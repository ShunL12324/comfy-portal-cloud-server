// Package state is the single source of truth behind the API.
//
// Every mutation publishes an event, so the SSE stream and the polling
// endpoints can never disagree.
package state

import (
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
)

type Phase string

const (
	PhasePreparing   Phase = "preparing"
	PhaseDownloading Phase = "downloading"
	PhaseStarting    Phase = "starting"
	PhaseReady       Phase = "ready"
	PhaseFailed      Phase = "failed"
)

type StepState string

const (
	StepRunning StepState = "running"
	StepDone    StepState = "done"
	StepFailed  StepState = "failed"
)

type ModelState string

const (
	ModelWaiting ModelState = "waiting"
	ModelActive  ModelState = "active"
	ModelDone    ModelState = "done"
	ModelError   ModelState = "error"
	ModelPaused  ModelState = "paused"
	ModelRemoved ModelState = "removed"
)

type ServiceState string

const (
	ServiceStarting   ServiceState = "starting"
	ServiceRunning    ServiceState = "running"
	ServiceRestarting ServiceState = "restarting"
	ServiceStopped    ServiceState = "stopped"
)

type Step struct {
	ID     string    `json:"id"`
	State  StepState `json:"state"`
	Detail string    `json:"detail"`
	// Ms is the wall time once the step settles; null while it runs.
	Ms *int64 `json:"ms"`
	// StartedAt is unix milliseconds, so steps that overlap can be laid out
	// on one timeline.
	StartedAt int64 `json:"startedAt"`
	started   time.Time
}

// PhaseTime is one phase the launch passed through.
type PhaseTime struct {
	Phase     Phase `json:"phase"`
	StartedAt int64 `json:"startedAt"` // unix milliseconds
	// Ms is the time spent in the phase; null while it is the current one.
	Ms *int64 `json:"ms"`
}

type Model struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Folder string `json:"folder"`
	// Bytes. 0 until the host reports a length.
	Total     int64      `json:"total"`
	Completed int64      `json:"completed"`
	Speed     int64      `json:"speed"`
	State     ModelState `json:"state"`
	Error     string     `json:"error,omitempty"`
	ErrorCode string     `json:"errorCode,omitempty"`
	Hint      string     `json:"hint,omitempty"`
	// StartedAt and FinishedAt are unix milliseconds: when the download went
	// active and when it completed. AvgSpeed is the bytes fetched in between
	// over that time, so a resumed file is not credited with what was on disk.
	StartedAt  int64 `json:"startedAt,omitempty"`
	FinishedAt int64 `json:"finishedAt,omitempty"`
	AvgSpeed   int64 `json:"avgSpeed,omitempty"`
	// resumedFrom is Completed when the download went active; queuedAt is
	// when it was handed to the engine, for files too small to be seen active.
	resumedFrom int64
	queuedAt    int64
}

type Service struct {
	Name       string       `json:"name"`
	State      ServiceState `json:"state"`
	PID        int          `json:"pid,omitempty"`
	Restarts   int          `json:"restarts"`
	LastExit   *int         `json:"lastExit,omitempty"`
	AnsweredAt int64        `json:"answeredAt,omitempty"`
	Models     []string     `json:"models,omitempty"`
	// ReadyMs is how long the last start took to answer its ready URL.
	ReadyMs *int64 `json:"readyMs,omitempty"`
}

type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type Totals struct {
	Bytes      int64  `json:"bytes"`
	Completed  int64  `json:"completed"`
	Speed      int64  `json:"speed"`
	ETASeconds *int64 `json:"etaSeconds"`
}

// Summary is everything except the per-resource lists.
type Summary struct {
	Phase           Phase    `json:"phase"`
	StartedAt       int64    `json:"startedAt"`
	Elapsed         int64    `json:"elapsed"`
	Stalled         bool     `json:"stalled"`
	LastProgressAt  int64    `json:"lastProgressAt"`
	RestartRequired bool     `json:"restartRequired"`
	Totals          Totals   `json:"totals"`
	Error           *Problem `json:"error"`
	// Phases is every phase so far, oldest first.
	Phases []PhaseTime `json:"phases"`
}

// Snapshot is the full state, persisted to disk after transitions.
type Snapshot struct {
	Summary
	Steps    []Step    `json:"steps"`
	Models   []Model   `json:"models"`
	Services []Service `json:"services"`
}

type State struct {
	Hub *Hub

	mu              sync.Mutex
	now             func() time.Time
	redactor        *redact.Redactor
	path            string
	phase           Phase
	phases          []PhaseTime
	startedAt       time.Time
	steps           []*Step
	models          map[string]*Model
	modelOrder      []string
	services        map[string]*Service
	serviceOrder    []string
	problem         *Problem
	lastProgress    time.Time
	stalled         bool
	restartRequired bool
}

// New creates a State that persists to path ("" disables persistence).
func New(path string, r *redact.Redactor) *State {
	s := &State{
		Hub:      NewHub(),
		now:      time.Now,
		redactor: r,
		path:     path,
		phase:    PhasePreparing,
		models:   map[string]*Model{},
		services: map[string]*Service{},
	}
	s.startedAt = s.now()
	s.lastProgress = s.startedAt
	s.phases = []PhaseTime{{Phase: PhasePreparing, StartedAt: s.startedAt.UnixMilli()}}
	return s
}

// SetClock replaces the time source. Tests only.
func (s *State) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
	s.startedAt = now()
	s.lastProgress = s.startedAt
	s.phases = []PhaseTime{{Phase: s.phase, StartedAt: s.startedAt.UnixMilli()}}
}

func (s *State) SetPhase(p Phase) {
	s.mu.Lock()
	s.enterPhase(p)
	s.publishPhase()
	s.mu.Unlock()
	slog.Info("phase", "phase", p)
	s.Persist()
}

// Fail records a fatal problem and moves to the failed phase.
func (s *State) Fail(code, message, hint string) {
	s.mu.Lock()
	s.enterPhase(PhaseFailed)
	s.problem = &Problem{Code: code, Message: s.redactor.String(message), Hint: hint}
	s.publishPhase()
	s.mu.Unlock()
	slog.Error("failed", "code", code, "message", message)
	s.Persist()
}

// enterPhase closes the current phase's timing and opens p's.
func (s *State) enterPhase(p Phase) {
	now := s.now()
	n := len(s.phases)
	if n > 0 && s.phases[n-1].Phase == p && s.phases[n-1].Ms == nil {
		s.phase = p
		return // already in it
	}
	if n > 0 && s.phases[n-1].Ms == nil {
		ms := now.UnixMilli() - s.phases[n-1].StartedAt
		s.phases[n-1].Ms = &ms
	}
	s.phase = p
	s.phases = append(s.phases, PhaseTime{Phase: p, StartedAt: now.UnixMilli()})
}

func (s *State) publishPhase() {
	s.Hub.publish("phase.changed", map[string]any{"phase": s.phase, "error": s.problem})
}

// Phase returns the current phase.
func (s *State) Phase() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// Step creates or updates a named step. Settling it (done/failed) records its
// wall time.
func (s *State) Step(id string, st StepState, detail string) {
	s.mu.Lock()
	var step *Step
	for _, existing := range s.steps {
		if existing.ID == id {
			step = existing
			break
		}
	}
	if step == nil {
		now := s.now()
		step = &Step{ID: id, started: now, StartedAt: now.UnixMilli()}
		s.steps = append(s.steps, step)
	}
	step.State, step.Detail = st, detail
	if st != StepRunning {
		ms := s.now().Sub(step.started).Milliseconds()
		step.Ms = &ms
	} else {
		step.Ms = nil
	}
	s.Hub.publish("step.updated", step)
	s.mu.Unlock()
	slog.Info("step", "id", id, "state", st, "detail", detail)
	s.Persist()
}

// PutModel inserts or replaces a model entry.
func (s *State) PutModel(m Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.models[m.ID]; !ok {
		s.modelOrder = append(s.modelOrder, m.ID)
	}
	cp := m
	if cp.State == ModelWaiting {
		cp.queuedAt = s.now().UnixMilli()
	}
	s.models[m.ID] = &cp
	s.Hub.publish("model.updated", cp)
}

// UpdateModel mutates an existing model and reports whether it changed.
// Unchanged models publish nothing, so a steady download stream is one event
// per progress tick rather than per poll.
func (s *State) UpdateModel(id string, fn func(*Model)) (Model, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	if !ok {
		return Model{}, false
	}
	before := *m
	fn(m)
	s.timeModel(before, m)
	if *m != before {
		s.Hub.publish("model.updated", *m)
	}
	return *m, true
}

// timeModel records when a download went active and finished, and its
// average rate. Only the transitions count, so a stalled poll changes nothing.
func (s *State) timeModel(before Model, m *Model) {
	now := s.now()
	if m.State == ModelActive && before.State != ModelActive && m.StartedAt == 0 {
		m.StartedAt, m.resumedFrom = now.UnixMilli(), before.Completed
	}
	if m.State == ModelDone && before.State != ModelDone && m.StartedAt == 0 && m.queuedAt > 0 {
		// Finished between two polls without ever being seen active.
		m.StartedAt = m.queuedAt
	}
	if m.State == ModelDone && before.State != ModelDone && m.StartedAt > 0 {
		m.FinishedAt = now.UnixMilli()
		if ms := m.FinishedAt - m.StartedAt; ms > 0 {
			m.AvgSpeed = (m.Completed - m.resumedFrom) * 1000 / ms
		}
	}
}

func (s *State) Model(id string) (Model, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	if !ok {
		return Model{}, false
	}
	return *m, true
}

func (s *State) Models() []Model {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelsLocked()
}

func (s *State) modelsLocked() []Model {
	out := make([]Model, 0, len(s.modelOrder))
	for _, id := range s.modelOrder {
		out = append(out, *s.models[id])
	}
	return out
}

// UpdateService creates or mutates a service and publishes the result.
func (s *State) UpdateService(name string, fn func(*Service)) Service {
	s.mu.Lock()
	svc, ok := s.services[name]
	if !ok {
		svc = &Service{Name: name, State: ServiceStarting}
		s.services[name] = svc
		s.serviceOrder = append(s.serviceOrder, name)
	}
	fn(svc)
	out := *svc
	s.Hub.publish("service.updated", out)
	s.mu.Unlock()
	s.Persist()
	return out
}

func (s *State) Service(name string) (Service, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[name]
	if !ok {
		return Service{}, false
	}
	return *svc, true
}

func (s *State) Services() []Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.servicesLocked()
}

func (s *State) servicesLocked() []Service {
	out := make([]Service, 0, len(s.serviceOrder))
	for _, name := range s.serviceOrder {
		out = append(out, *s.services[name])
	}
	return out
}

// SetRestartRequired flags that ComfyUI must restart to pick up new
// extensions.
func (s *State) SetRestartRequired(v bool) {
	s.mu.Lock()
	changed := s.restartRequired != v
	s.restartRequired = v
	if changed {
		s.Hub.publish("restart-required.changed", map[string]bool{"restartRequired": v})
	}
	s.mu.Unlock()
}

// Progress records whether bytes moved on this poll and whether a download is
// in flight, and derives the stalled flag from it.
func (s *State) Progress(moved, downloading bool, stallAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	stalled := s.stalled
	switch {
	case moved:
		s.lastProgress = now
		stalled = false
	case downloading && now.Sub(s.lastProgress) > stallAfter:
		stalled = true
	case !downloading:
		stalled = false
	}
	if stalled != s.stalled {
		s.stalled = stalled
		s.Hub.publish("stalled.changed", map[string]bool{"stalled": stalled})
	}
}

func (s *State) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summaryLocked()
}

func (s *State) summaryLocked() Summary {
	var t Totals
	for _, id := range s.modelOrder {
		m := s.models[id]
		t.Bytes += m.Total
		t.Completed += m.Completed
		if m.State == ModelActive {
			t.Speed += m.Speed
		}
	}
	if t.Speed > 0 && t.Bytes > t.Completed {
		eta := (t.Bytes - t.Completed) / t.Speed
		t.ETASeconds = &eta
	}
	return Summary{
		Phase:           s.phase,
		StartedAt:       s.startedAt.Unix(),
		Elapsed:         int64(s.now().Sub(s.startedAt).Seconds()),
		Stalled:         s.stalled,
		LastProgressAt:  s.lastProgress.Unix(),
		RestartRequired: s.restartRequired,
		Totals:          t,
		Error:           s.problem,
		Phases:          append([]PhaseTime(nil), s.phases...),
	}
}

func (s *State) Steps() []Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stepsLocked()
}

func (s *State) stepsLocked() []Step {
	out := make([]Step, 0, len(s.steps))
	for _, st := range s.steps {
		out = append(out, *st)
	}
	return out
}

func (s *State) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		Summary:  s.summaryLocked(),
		Steps:    s.stepsLocked(),
		Models:   s.modelsLocked(),
		Services: s.servicesLocked(),
	}
}

// FailedModelIDs lists models currently in the error state, sorted.
func (s *State) FailedModelIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, m := range s.models {
		if m.State == ModelError {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Persist writes the snapshot atomically: a half-written state file read
// after a crash would be worse than none.
func (s *State) Persist() {
	if s.path == "" {
		return
	}
	payload, err := json.Marshal(s.Snapshot())
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		slog.Warn("could not persist state", "err", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Warn("could not persist state", "err", err)
	}
}
