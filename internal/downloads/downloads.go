// Package downloads queues model downloads on aria2 and mirrors their
// progress into the supervisor's state.
package downloads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/aria2"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// Engine is the part of aria2 the manager needs; tests substitute a fake.
type Engine interface {
	AddURI(ctx context.Context, uri string, o aria2.Options) (string, error)
	TellStatus(ctx context.Context, gid string) (aria2.Status, error)
}

type Config struct {
	Workspace     string
	HFToken       string
	CivitaiAPIKey string
	StallAfter    time.Duration
	PollInterval  time.Duration
	// FreeBytes reports free space at a path. Defaults to statfs.
	FreeBytes func(path string) (uint64, error)
}

type Manager struct {
	cfg      Config
	st       *state.State
	eng      Engine
	redactor *redact.Redactor
	http     *http.Client

	mu      sync.Mutex
	specs   map[string]manifest.Resolved // by model ID, so retry needs no manifest
	pending map[string]string            // aria2 gid -> model ID
}

func New(cfg Config, st *state.State, eng Engine, r *redact.Redactor) *Manager {
	if cfg.FreeBytes == nil {
		cfg.FreeBytes = freeBytes
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Manager{
		cfg: cfg, st: st, eng: eng, redactor: r,
		http:    &http.Client{Timeout: 30 * time.Second},
		specs:   map[string]manifest.Resolved{},
		pending: map[string]string{},
	}
}

// Preflight fails when the models cannot fit, before any bytes are spent.
// It fills in sizes the manifest left at zero by asking the host.
func (m *Manager) Preflight(ctx context.Context, models []manifest.Model) (needed int64, err error) {
	for i := range models {
		if models[i].SizeBytes == 0 {
			models[i].SizeBytes = m.headSize(ctx, models[i].URL)
		}
		needed += models[i].SizeBytes
	}
	free, ferr := m.cfg.FreeBytes(m.cfg.Workspace)
	if ferr != nil {
		slog.Warn("could not read free disk space", "err", ferr)
		return needed, nil
	}
	slog.Info("disk check", "neededGiB", float64(needed)/(1<<30), "freeGiB", float64(free)/(1<<30))
	if needed > 0 && uint64(needed) > free {
		return needed, &DiskError{Needed: needed, Free: int64(free)}
	}
	return needed, nil
}

type DiskError struct{ Needed, Free int64 }

func (e *DiskError) Error() string {
	return fmt.Sprintf("Models need %.0f GiB but only %.0f GiB is free.", float64(e.Needed)/(1<<30), float64(e.Free)/(1<<30))
}

// Queue hands models to aria2. Ones already known and not failed are skipped,
// so applying the same manifest twice is harmless.
func (m *Manager) Queue(ctx context.Context, models []manifest.Model) {
	for _, model := range models {
		r := model.Resolve()
		m.mu.Lock()
		_, known := m.specs[r.ID]
		m.mu.Unlock()
		if known {
			if cur, ok := m.st.Model(r.ID); ok && cur.State != state.ModelError {
				continue
			}
		}
		if err := m.add(ctx, r); err != nil {
			m.st.PutModel(state.Model{
				ID: r.ID, Name: r.Name, Folder: r.Folder, State: state.ModelError,
				Error: m.redactor.String(err.Error()), ErrorCode: "queue_failed", Hint: "Retry, or check the URL.",
			})
			slog.Error("queue failed", "model", r.Key(), "err", err)
		}
	}
}

// add hands one model to aria2 and records its gid.
//
// A file that is already complete on disk is marked done without being
// fetched, which is the normal case after a restart: /workspace survives, so a
// launch interrupted at 40 of 47 GiB resumes rather than starting over. A
// partial file has aria2's ".aria2" control file beside it and must be handed
// back to aria2 to continue, not mistaken for a finished one.
func (m *Manager) add(ctx context.Context, r manifest.Resolved) error {
	dir := filepath.Join(m.cfg.Workspace, "models", filepath.FromSlash(r.Folder))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m.mu.Lock()
	m.specs[r.ID] = r
	m.mu.Unlock()

	file := filepath.Join(dir, r.Name)
	if info, err := os.Stat(file); err == nil && info.Size() > 0 {
		if _, ctl := os.Stat(file + ".aria2"); errors.Is(ctl, os.ErrNotExist) {
			m.st.PutModel(state.Model{
				ID: r.ID, Name: r.Name, Folder: r.Folder,
				Total: info.Size(), Completed: info.Size(), State: state.ModelDone,
			})
			slog.Info("skip, already on disk", "model", r.Key())
			return nil
		}
	}

	uri := r.URL
	opts := aria2.Options{Dir: dir, Out: r.Name}
	switch host := hostOf(uri); {
	case isCivitai(host):
		// No header at all on the signed URL: see resolveCivitai.
		resolved, err := m.resolveCivitai(ctx, uri)
		if err != nil {
			slog.Warn("civitai resolve failed", "model", r.Key(), "err", m.redactor.String(err.Error()))
		} else {
			uri = resolved
		}
	case isHuggingFace(host) && m.cfg.HFToken != "":
		opts.Headers = []string{"Authorization: Bearer " + m.cfg.HFToken}
	}

	gid, err := m.eng.AddURI(ctx, uri, opts)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.pending[gid] = r.ID
	m.mu.Unlock()
	m.st.PutModel(state.Model{
		ID: r.ID, Name: r.Name, Folder: r.Folder, Total: r.SizeBytes, State: state.ModelWaiting,
	})
	return nil
}

// Retry re-queues failed models. With no ids it retries every failed one.
// A single bad LoRA should never cost someone the whole instance, so a failure
// marks that one model and the rest carry on; this is how the app turns that
// into a retry button rather than a relaunch.
func (m *Manager) Retry(ctx context.Context, ids []string) (retried []string) {
	if ids == nil {
		ids = m.st.FailedModelIDs()
	}
	for _, id := range ids {
		cur, ok := m.st.Model(id)
		if !ok || cur.State != state.ModelError {
			continue
		}
		m.mu.Lock()
		spec, ok := m.specs[id]
		m.mu.Unlock()
		if !ok {
			continue
		}
		slog.Info("retrying", "model", spec.Key())
		if err := m.add(ctx, spec); err != nil {
			m.st.UpdateModel(id, func(mm *state.Model) {
				mm.Error, mm.ErrorCode = m.redactor.String(err.Error()), "queue_failed"
			})
			continue
		}
		retried = append(retried, id)
	}
	return retried
}

// Spec returns the resolved manifest entry behind a model.
func (m *Manager) Spec(id string) (manifest.Resolved, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.specs[id]
	return r, ok
}

// Run mirrors aria2's view into the state until ctx ends.
//
// It runs for the life of the process rather than until the queue empties, so
// a retry issued long after the first pass is tracked like the first attempt.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.poll(ctx)
		}
	}
}

func (m *Manager) poll(ctx context.Context) {
	m.mu.Lock()
	current := make(map[string]string, len(m.pending))
	for gid, id := range m.pending {
		current[gid] = id
	}
	m.mu.Unlock()

	moved := false
	for gid, id := range current {
		status, err := m.eng.TellStatus(ctx, gid)
		if errors.Is(err, aria2.ErrGIDNotFound) {
			m.settle(gid)
			m.st.UpdateModel(id, func(mm *state.Model) {
				mm.State, mm.Speed = state.ModelError, 0
				mm.Error, mm.ErrorCode = "The download engine restarted and lost this download.", "engine_restarted"
				mm.Hint = "Retry."
			})
			continue
		}
		if err != nil {
			slog.Warn("tellStatus failed", "model", id, "err", m.redactor.String(err.Error()))
			continue
		}
		m.st.UpdateModel(id, func(mm *state.Model) {
			if status.Completed > mm.Completed {
				moved = true
			}
			mm.Completed = status.Completed
			if status.Total > 0 {
				mm.Total = status.Total
			}
			mm.Speed = status.Speed
			switch status.State {
			case "complete":
				mm.State, mm.Speed = state.ModelDone, 0
			case "error":
				msg := m.redactor.String(status.ErrorMessage)
				if msg == "" {
					msg = "download failed"
				}
				mm.State, mm.Speed, mm.Error = state.ModelError, 0, msg
				mm.ErrorCode, mm.Hint = Classify(msg)
			default:
				mm.State = state.ModelState(status.State)
			}
		})
		if status.State == "complete" || status.State == "error" {
			m.settle(gid)
			slog.Info("download finished", "model", id, "state", status.State)
		}
	}
	m.st.Progress(moved, len(current) > 0, m.cfg.StallAfter)
	if len(current) > 0 {
		m.st.Persist()
	}
}

func (m *Manager) settle(gid string) {
	m.mu.Lock()
	delete(m.pending, gid)
	m.mu.Unlock()
}

// Idle reports whether nothing is in flight.
func (m *Manager) Idle() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending) == 0
}

// Wait blocks until nothing is in flight and returns the IDs that failed.
// Failures do not block; they are reported.
func (m *Manager) Wait(ctx context.Context) ([]string, error) {
	for !m.Idle() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.cfg.PollInterval / 2):
		}
	}
	return m.st.FailedModelIDs(), nil
}

// Classify turns aria2's error text into a code and a hint the app can show.
func Classify(message string) (code, hint string) {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "401") || strings.Contains(lower, "403") || strings.Contains(lower, "authorization"):
		return "model_auth_failed", "Check the API key for that host."
	case strings.Contains(lower, "404") || strings.Contains(lower, "not found"):
		return "model_not_found", "The URL no longer resolves to a file."
	case strings.Contains(lower, "no space"):
		return "disk_full", "Relaunch with a larger disk."
	}
	return "model_failed", "Retry, or check the URL."
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func isCivitai(host string) bool {
	return host == "civitai.com" || strings.HasSuffix(host, ".civitai.com")
}
func isHuggingFace(host string) bool {
	return host == "huggingface.co" || strings.HasSuffix(host, ".huggingface.co") || host == "hf.co"
}

// headSize is the content length without fetching the body. Zero on failure:
// the pre-check is advisory and must not block a launch.
func (m *Manager) headSize(ctx context.Context, raw string) int64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, raw, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	switch host := hostOf(raw); {
	case isCivitai(host) && m.cfg.CivitaiAPIKey != "":
		req.Header.Set("Authorization", "Bearer "+m.cfg.CivitaiAPIKey)
	case isHuggingFace(host) && m.cfg.HFToken != "":
		req.Header.Set("Authorization", "Bearer "+m.cfg.HFToken)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	// HuggingFace reports an LFS object's real size here; Content-Length would
	// be the pointer file's.
	if v := resp.Header.Get("X-Linked-Size"); v != "" {
		var n int64
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n
		}
	}
	return max(resp.ContentLength, 0)
}

// resolveCivitai turns a Civitai download URL into the signed URL it redirects
// to.
//
// Resolving here rather than letting aria2 follow the redirect is deliberate:
// aria2 applies --header across redirects unconditionally, and Civitai lands
// on either a B2- or an R2-backed signed URL depending on the asset. B2
// tolerates the stray Authorization; R2 rejects it with a flat 400, because
// its signature covers only `host`. Handing aria2 the bare signed URL sidesteps
// both and keeps all 16 connections instead of dropping to one.
func (m *Manager) resolveCivitai(ctx context.Context, raw string) (string, error) {
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if m.cfg.CivitaiAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.CivitaiAPIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
		if loc := resp.Header.Get("Location"); loc != "" {
			next, err := req.URL.Parse(loc)
			if err != nil {
				return "", err
			}
			return next.String(), nil
		}
	case 200:
		return raw, nil
	}
	return "", fmt.Errorf("civitai answered HTTP %d", resp.StatusCode)
}
