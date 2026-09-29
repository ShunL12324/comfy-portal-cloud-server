// Package api is the supervisor's HTTP surface.
//
// /v2 is the API. /v1 is a frozen compatibility layer for app builds that
// predate it; it is served from the same state and will be removed once those
// builds are gone.
package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// Controller is what the API needs from the rest of the supervisor.
type Controller interface {
	// RetryModels re-queues failed models; nil retries all. Returns the IDs queued.
	RetryModels(ctx context.Context, ids []string) []string
	// ApplyManifest adds to the running instance. See pipeline.Apply.
	ApplyManifest(ctx context.Context, m manifest.Manifest) error
	Manifest() manifest.Manifest
	// RestartService restarts a supervised service by name.
	RestartService(name string) error
}

type Options struct {
	State      *state.State
	Controller Controller
	Redactor   *redact.Redactor
	// Token is the bearer token. Empty disables auth (development only).
	Token string
	// LogPath maps a stream name to its file.
	LogPath func(stream string) string
	Version string
}

type Server struct {
	Options
	mux    *http.ServeMux
	routes []string
}

//go:embed openapi.json
var openAPI []byte

func New(o Options) *Server {
	s := &Server{Options: o, mux: http.NewServeMux()}
	s.registerV2()
	s.registerV1()
	return s
}

// Routes lists the registered "METHOD /path" patterns, for the spec-drift test.
func (s *Server) Routes() []string { return append([]string(nil), s.routes...) }

func (s *Server) handle(pattern string, h http.HandlerFunc) {
	s.routes = append(s.routes, pattern)
	s.mux.HandleFunc(pattern, h)
}

// Handler wraps the mux with panic recovery, auth and security headers.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.auth(s.mux))
}

// public paths answer without a token: they prove the port is published and
// reveal nothing.
func isPublic(path string) bool {
	return path == "/v2/health" || path == "/v1/health" || path == "/v2/openapi.json"
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.Token != "" && !isPublic(r.URL.Path) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") ||
				subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="comfy-portal"`)
				if strings.HasPrefix(r.URL.Path, "/v1/") {
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorised"})
				} else {
					writeProblem(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid bearer token.", "")
				}
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("handler panic", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeProblem(w, http.StatusInternalServerError, "internal", "Internal error.", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Problem is an RFC 9457 problem document.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
	Hint   string `json:"hint,omitempty"`
}

func writeProblem(w http.ResponseWriter, status int, code, detail, hint string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type: "urn:comfy-portal:problem:" + code, Title: http.StatusText(status),
		Status: status, Detail: detail, Code: code, Hint: hint,
	})
}
