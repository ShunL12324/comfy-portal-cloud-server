package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/logs"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// legacyVersion is the supervisorVersion the Python supervisor reported.
const legacyVersion = 2

// The v1 shapes mirror exactly what the Python supervisor sent, because app
// builds in the wild parse them. Do not change them; add to /v2 instead.
type v1Step struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	Ms     *int64 `json:"ms"`
}

type v1Model struct {
	Name      string  `json:"name"`
	Folder    string  `json:"folder"`
	Total     int64   `json:"total"`
	Completed int64   `json:"completed"`
	Speed     int64   `json:"speed"`
	State     string  `json:"state"`
	Error     *string `json:"error"`
	ErrorCode string  `json:"errorCode,omitempty"`
	Hint      string  `json:"hint,omitempty"`
}

type v1Service struct {
	State      string   `json:"state"`
	PID        int      `json:"pid,omitempty"`
	Restarts   int      `json:"restarts"`
	LastExit   *int     `json:"lastExit,omitempty"`
	AnsweredAt int64    `json:"answeredAt,omitempty"`
	Models     []string `json:"models,omitempty"`
}

type v1Snapshot struct {
	SupervisorVersion int                  `json:"supervisorVersion"`
	Phase             state.Phase          `json:"phase"`
	StartedAt         int64                `json:"startedAt"`
	Elapsed           int64                `json:"elapsed"`
	Stalled           bool                 `json:"stalled"`
	LastProgressAt    int64                `json:"lastProgressAt"`
	Steps             []v1Step             `json:"steps"`
	Models            []v1Model            `json:"models"`
	Totals            state.Totals         `json:"totals"`
	Services          map[string]v1Service `json:"services"`
	Error             *state.Problem       `json:"error"`
}

func (s *Server) legacySnapshot() v1Snapshot {
	snap := s.State.Snapshot()
	out := v1Snapshot{
		SupervisorVersion: legacyVersion, Phase: snap.Phase, StartedAt: snap.StartedAt,
		Elapsed: snap.Elapsed, Stalled: snap.Stalled, LastProgressAt: snap.LastProgressAt,
		Steps: []v1Step{}, Models: []v1Model{}, Totals: snap.Totals,
		Services: map[string]v1Service{}, Error: snap.Error,
	}
	for _, st := range snap.Steps {
		out.Steps = append(out.Steps, v1Step{st.ID, string(st.State), st.Detail, st.Ms})
	}
	for _, m := range snap.Models {
		lm := v1Model{
			Name: m.Name, Folder: m.Folder, Total: m.Total, Completed: m.Completed, Speed: m.Speed,
			State: string(m.State), ErrorCode: m.ErrorCode, Hint: m.Hint,
		}
		if m.Error != "" {
			e := m.Error
			lm.Error = &e
		}
		out.Models = append(out.Models, lm)
	}
	for _, svc := range snap.Services {
		if svc.Name == "aria2" {
			continue // v1 clients only know about comfyui and ollama
		}
		out.Services[svc.Name] = v1Service{string(svc.State), svc.PID, svc.Restarts, svc.LastExit, svc.AnsweredAt, svc.Models}
	}
	return out
}

func (s *Server) registerV1() {
	// deprecate marks every v1 response so a curious client can tell.
	deprecate := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Deprecation", "true")
			w.Header().Set("Link", `</v2/openapi.json>; rel="successor-version"`)
			h(w, r)
		}
	}
	s.handle("GET /v1/health", deprecate(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "supervisorVersion": legacyVersion})
	}))
	s.handle("GET /v1/status", deprecate(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.legacySnapshot())
	}))
	s.handle("GET /v1/log", deprecate(func(w http.ResponseWriter, r *http.Request) {
		stream := r.URL.Query().Get("stream")
		if !contains(logs.Streams, stream) {
			stream = "supervisor" // v1 fell back rather than failing
		}
		tail := 200
		if n, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && n > 0 {
			tail = n
		}
		text, _ := logs.Tail(s.LogPath(stream), tail)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, s.Redactor.String(text))
	}))
	s.handle("GET /v1/events", deprecate(s.legacyEvents))
	s.handle("POST /v1/models/retry", deprecate(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Keys []string `json:"keys"`
		}
		if r.ContentLength != 0 {
			_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) // v1 ignored a bad body
		}
		var ids []string // nil means all failed
		if body.Keys != nil {
			ids = []string{}
			for _, k := range body.Keys {
				if i := strings.LastIndex(k, "/"); i > 0 {
					ids = append(ids, manifest.ID(k[:i], k[i+1:]))
				}
			}
		}
		retried := []string{}
		for _, id := range s.Controller.RetryModels(r.Context(), ids) {
			if m, ok := s.State.Model(id); ok {
				retried = append(retried, m.Folder+"/"+m.Name)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"retried": retried})
	}))
	s.handle("POST /v1/comfyui/restart", deprecate(func(w http.ResponseWriter, r *http.Request) {
		_ = s.Controller.RestartService("comfyui")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}

// legacyEvents repeats the full v1 snapshot every two seconds, as v1 did.
func (s *Server) legacyEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func() bool {
		payload, _ := json.Marshal(s.legacySnapshot())
		fmt.Fprintf(w, "data: %s\n\n", payload)
		return rc.Flush() == nil
	}
	if !send() {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}
