package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/downloads"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/logs"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/pipeline"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// restartable are the services a client may restart. aria2 is deliberately
// absent: restarting it abandons every download in flight.
var restartable = map[string]bool{"comfyui": true, "ollama": true}

type items[T any] struct {
	Items []T `json:"items"`
}

func (s *Server) registerV2() {
	s.handle("GET /v2/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version, "phase": s.State.Phase()})
	})
	s.handle("GET /v2/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAPI)
	})
	s.handle("GET /v2/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			Version string `json:"version"`
			state.Summary
		}{s.Version, s.State.Summary()})
	})
	// One round trip for everything: the app reaches this over SSH exec, where
	// each request costs a fresh connection.
	s.handle("GET /v2/snapshot", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.snapshot())
	})
	s.handle("GET /v2/steps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, items[state.Step]{s.State.Steps()})
	})

	s.handle("GET /v2/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, items[state.Model]{s.State.Models()})
	})
	s.handle("GET /v2/models/{id}", func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.State.Model(r.PathValue("id"))
		if !ok {
			writeProblem(w, http.StatusNotFound, "model_not_found", "No such model.", "")
			return
		}
		writeJSON(w, http.StatusOK, m)
	})
	s.handle("POST /v2/models/retry-failed", func(w http.ResponseWriter, r *http.Request) {
		retried := s.Controller.RetryModels(r.Context(), nil)
		if retried == nil {
			retried = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"retried": retried})
	})
	s.handle("POST /v2/models/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m, ok := s.State.Model(id)
		if !ok {
			writeProblem(w, http.StatusNotFound, "model_not_found", "No such model.", "")
			return
		}
		if m.State != state.ModelError {
			writeProblem(w, http.StatusConflict, "model_not_failed", "Only a failed model can be retried.", "")
			return
		}
		if len(s.Controller.RetryModels(r.Context(), []string{id})) == 0 {
			writeProblem(w, http.StatusConflict, "retry_failed", "The model could not be re-queued.", "Check the supervisor log.")
			return
		}
		m, _ = s.State.Model(id)
		writeJSON(w, http.StatusAccepted, m)
	})

	s.handle("GET /v2/services", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, items[state.Service]{s.State.Services()})
	})
	s.handle("GET /v2/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		svc, ok := s.State.Service(r.PathValue("name"))
		if !ok {
			writeProblem(w, http.StatusNotFound, "service_not_found", "No such service.", "")
			return
		}
		writeJSON(w, http.StatusOK, svc)
	})
	s.handle("POST /v2/services/{name}/restart", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, ok := s.State.Service(name); !ok {
			writeProblem(w, http.StatusNotFound, "service_not_found", "No such service.", "")
			return
		}
		if !restartable[name] {
			writeProblem(w, http.StatusConflict, "not_restartable", name+" cannot be restarted through the API.", "")
			return
		}
		if err := s.Controller.RestartService(name); err != nil {
			writeProblem(w, http.StatusConflict, "restart_failed", err.Error(), "")
			return
		}
		svc, _ := s.State.Service(name)
		writeJSON(w, http.StatusAccepted, svc)
	})

	s.handle("GET /v2/manifest", func(w http.ResponseWriter, r *http.Request) {
		m := s.Controller.Manifest()
		if m.Models == nil {
			m.Models = []manifest.Model{}
		}
		if m.Extensions == nil {
			m.Extensions = []string{}
		}
		if m.OllamaModels == nil {
			m.OllamaModels = []string{}
		}
		writeJSON(w, http.StatusOK, m)
	})
	s.handle("PUT /v2/manifest", s.putManifest)

	s.handle("GET /v2/logs/{stream}", func(w http.ResponseWriter, r *http.Request) {
		stream := r.PathValue("stream")
		if !contains(logs.Streams, stream) {
			writeProblem(w, http.StatusNotFound, "unknown_stream", fmt.Sprintf("Streams: %v.", logs.Streams), "")
			return
		}
		tail := 200
		if v := r.URL.Query().Get("tail"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				writeProblem(w, http.StatusBadRequest, "bad_tail", "tail must be a positive integer.", "")
				return
			}
			tail = n
		}
		text, err := logs.Tail(s.LogPath(stream), tail)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "log_unreadable", err.Error(), "")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, s.Redactor.String(text))
	})

	s.handle("GET /v2/events", s.events)
}

func (s *Server) putManifest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var m manifest.Manifest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad_manifest", err.Error(), "")
		return
	}
	err := s.Controller.ApplyManifest(r.Context(), m)
	var disk *downloads.DiskError
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
	case errors.Is(err, pipeline.ErrNotReady):
		writeProblem(w, http.StatusConflict, "not_ready", "The instance is not ready yet.", "Wait for phase \"ready\".")
	case errors.Is(err, pipeline.ErrBusy):
		writeProblem(w, http.StatusConflict, "install_running", "Another install is still running.", "Retry when it finishes.")
	case errors.As(err, &disk):
		writeProblem(w, http.StatusConflict, "disk_full", err.Error(), "Relaunch with a larger disk.")
	default:
		writeProblem(w, http.StatusBadRequest, "bad_manifest", err.Error(), "")
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

type versionedSnapshot struct {
	Version string `json:"version"`
	state.Snapshot
}

func (s *Server) snapshot() versionedSnapshot {
	return versionedSnapshot{s.Version, s.State.Snapshot()}
}
