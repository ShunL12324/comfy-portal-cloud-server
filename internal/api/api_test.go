package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/downloads"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/pipeline"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

const token = "test-token-abcdefgh"

type fakeCtl struct {
	mu        sync.Mutex
	retried   [][]string
	applyErr  error
	applied   []manifest.Manifest
	restarted []string
}

func (f *fakeCtl) RetryModels(_ context.Context, ids []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried = append(f.retried, ids)
	if ids == nil {
		return []string{manifest.ID("loras", "bad.bin")}
	}
	return ids
}
func (f *fakeCtl) ApplyManifest(_ context.Context, m manifest.Manifest) error {
	f.applied = append(f.applied, m)
	return f.applyErr
}
func (f *fakeCtl) Manifest() manifest.Manifest { return manifest.Manifest{} }
func (f *fakeCtl) RestartService(name string) error {
	f.restarted = append(f.restarted, name)
	return nil
}

type rig struct {
	*httptest.Server
	st  *state.State
	ctl *fakeCtl
	dir string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := redact.New(token, "hf_supersecret")
	st := state.New("", r)
	ctl := &fakeCtl{}
	dir := t.TempDir()
	s := New(Options{
		State: st, Controller: ctl, Redactor: r, Token: token, Version: "test",
		LogPath: func(stream string) string { return filepath.Join(dir, stream+".log") },
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &rig{srv, st, ctl, dir}
}

func (r *rig) do(t *testing.T, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, r.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func decode(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("bad JSON %q: %v", body, err)
	}
}

func (r *rig) seedFailedModel() string {
	id := manifest.ID("loras", "bad.bin")
	r.st.PutModel(state.Model{ID: id, Name: "bad.bin", Folder: "loras", State: state.ModelError, Error: "HTTP 404", ErrorCode: "model_not_found"})
	return id
}

func TestAuth(t *testing.T) {
	r := newRig(t)
	for _, path := range []string{"/v2/health", "/v1/health", "/v2/openapi.json"} {
		resp, err := http.Get(r.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s should be public: %v %v", path, resp, err)
		}
	}
	resp, _ := http.Get(r.URL + "/v2/state")
	if resp.StatusCode != 401 || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("v2: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, _ = http.Get(r.URL + "/v1/status")
	if resp.StatusCode != 401 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("v1: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", r.URL+"/v2/state", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatal("wrong token accepted")
	}
	req.Header.Set("Authorization", token) // no Bearer prefix
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatal("token without Bearer accepted")
	}
}

func TestReadEndpoints(t *testing.T) {
	r := newRig(t)
	id := r.seedFailedModel()
	r.st.Step("aria2", state.StepDone, "")
	r.st.UpdateService("comfyui", func(s *state.Service) { s.State = state.ServiceRunning })

	_, body := r.do(t, "GET", "/v2/models", "")
	var list struct{ Items []state.Model }
	decode(t, body, &list)
	if len(list.Items) != 1 || list.Items[0].ID != id {
		t.Fatal(body)
	}
	if resp, _ := r.do(t, "GET", "/v2/models/"+id, ""); resp.StatusCode != 200 {
		t.Fatal("model by id")
	}
	if resp, body := r.do(t, "GET", "/v2/models/nope", ""); resp.StatusCode != 404 || !strings.Contains(body, `"code":"model_not_found"`) {
		t.Fatal(body)
	}
	_, body = r.do(t, "GET", "/v2/snapshot", "")
	var snap struct {
		Version  string
		Models   []state.Model
		Services []state.Service
		Steps    []state.Step
	}
	decode(t, body, &snap)
	if snap.Version != "test" || len(snap.Models) != 1 || len(snap.Services) != 1 || len(snap.Steps) != 1 {
		t.Fatal(body)
	}
	if resp, _ := r.do(t, "GET", "/v2/state", ""); resp.StatusCode != 200 {
		t.Fatal("state")
	}
}

func TestRetry(t *testing.T) {
	r := newRig(t)
	id := r.seedFailedModel()
	r.st.PutModel(state.Model{ID: "okmodel", State: state.ModelDone})

	if resp, _ := r.do(t, "POST", "/v2/models/okmodel/retry", ""); resp.StatusCode != 409 {
		t.Fatalf("retry of healthy model: %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "POST", "/v2/models/missing/retry", ""); resp.StatusCode != 404 {
		t.Fatal("missing")
	}
	if resp, _ := r.do(t, "POST", "/v2/models/"+id+"/retry", ""); resp.StatusCode != 202 {
		t.Fatal("retry")
	}
	_, body := r.do(t, "POST", "/v2/models/retry-failed", "")
	if !strings.Contains(body, id) {
		t.Fatal(body)
	}
}

func TestRestart(t *testing.T) {
	r := newRig(t)
	r.st.UpdateService("comfyui", func(s *state.Service) {})
	r.st.UpdateService("aria2", func(s *state.Service) {})
	if resp, _ := r.do(t, "POST", "/v2/services/comfyui/restart", ""); resp.StatusCode != 202 || len(r.ctl.restarted) != 1 {
		t.Fatal("comfyui restart")
	}
	if resp, _ := r.do(t, "POST", "/v2/services/aria2/restart", ""); resp.StatusCode != 409 {
		t.Fatal("aria2 must not be restartable")
	}
	if resp, _ := r.do(t, "POST", "/v2/services/ghost/restart", ""); resp.StatusCode != 404 {
		t.Fatal("unknown service")
	}
}

func TestPutManifestMapsErrors(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		err  error
		body string
		want int
		code string
	}{
		{nil, `{"extensions":["https://github.com/u/x"]}`, 202, ""},
		{pipeline.ErrNotReady, `{}`, 409, "not_ready"},
		{pipeline.ErrBusy, `{}`, 409, "install_running"},
		{&downloads.DiskError{Needed: 2 << 30, Free: 1 << 30}, `{}`, 409, "disk_full"},
		{errors.New("models[0]: bad"), `{}`, 400, "bad_manifest"},
		{nil, `{"bogus":1}`, 400, "bad_manifest"},
		{nil, `not json`, 400, "bad_manifest"},
	}
	for _, c := range cases {
		r.ctl.applyErr = c.err
		resp, body := r.do(t, "PUT", "/v2/manifest", c.body)
		if resp.StatusCode != c.want || (c.code != "" && !strings.Contains(body, `"code":"`+c.code+`"`)) {
			t.Errorf("%v / %s: got %d %s", c.err, c.body, resp.StatusCode, body)
		}
	}
}

func TestLogsAreRedacted(t *testing.T) {
	r := newRig(t)
	_ = os.WriteFile(filepath.Join(r.dir, "comfyui.log"), []byte("a\nAuthorization: Bearer "+token+"\nhf_supersecret\n"), 0o644)
	resp, body := r.do(t, "GET", "/v2/logs/comfyui?tail=5", "")
	if resp.StatusCode != 200 || strings.Contains(body, token) || strings.Contains(body, "hf_supersecret") || !strings.Contains(body, "***") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if resp, _ := r.do(t, "GET", "/v2/logs/passwd", ""); resp.StatusCode != 404 {
		t.Fatal("unknown stream must 404, never read arbitrary files")
	}
	if resp, _ := r.do(t, "GET", "/v2/logs/comfyui?tail=x", ""); resp.StatusCode != 400 {
		t.Fatal("bad tail")
	}
	_, body = r.do(t, "GET", "/v1/log?stream=comfyui&tail=5", "")
	if strings.Contains(body, token) {
		t.Fatal("v1 log leaked the token")
	}
}

// readEvents reads SSE frames until n arrive or the deadline passes.
func readEvents(t *testing.T, resp *http.Response, n int) []map[string]string {
	t.Helper()
	var out []map[string]string
	cur := map[string]string{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if len(cur) > 0 {
					out = append(out, cur)
					cur = map[string]string{}
					if len(out) == n {
						return
					}
				}
			case strings.HasPrefix(line, ":"):
			default:
				k, v, _ := strings.Cut(line, ": ")
				cur[k] = v
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("only got %d of %d events: %v", len(out), n, out)
	}
	return out
}

func TestEventsSnapshotThenChanges(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest("GET", r.URL+"/v2/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Header)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.st.PutModel(state.Model{ID: "m1", Name: "m", State: state.ModelWaiting})
	}()
	evs := readEvents(t, resp, 2)
	if evs[0]["event"] != "snapshot" || evs[1]["event"] != "model.updated" || !strings.Contains(evs[1]["data"], `"m1"`) {
		t.Fatalf("%v", evs)
	}
}

func TestEventsResumeWithLastEventID(t *testing.T) {
	r := newRig(t)
	r.st.PutModel(state.Model{ID: "a", State: state.ModelDone})
	mark := r.st.Hub.LastID()
	r.st.PutModel(state.Model{ID: "b", State: state.ModelDone})

	req, _ := http.NewRequest("GET", r.URL+"/v2/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Last-Event-ID", strconv.FormatUint(mark, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	evs := readEvents(t, resp, 1)
	if evs[0]["event"] != "model.updated" || !strings.Contains(evs[0]["data"], `"b"`) {
		t.Fatalf("resume should replay only what was missed: %v", evs)
	}
}

// The v1 shape is a contract with app builds already in the wild.
func TestV1StatusKeepsTheLegacyShape(t *testing.T) {
	r := newRig(t)
	r.st.PutModel(state.Model{ID: "1", Name: "x.bin", Folder: "loras", Total: 10, State: state.ModelDone})
	r.st.PutModel(state.Model{ID: "2", Name: "y.bin", Folder: "loras", State: state.ModelError, Error: "boom", ErrorCode: "model_failed"})
	r.st.UpdateService("comfyui", func(s *state.Service) { s.State = state.ServiceRunning })
	r.st.UpdateService("aria2", func(s *state.Service) {})
	r.st.Step("s", state.StepRunning, "")

	resp, body := r.do(t, "GET", "/v1/status", "")
	if resp.Header.Get("Deprecation") != "true" {
		t.Fatal("v1 must be marked deprecated")
	}
	var snap map[string]json.RawMessage
	decode(t, body, &snap)
	for _, k := range []string{"supervisorVersion", "phase", "startedAt", "elapsed", "stalled", "lastProgressAt", "steps", "models", "totals", "services", "error"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("snapshot missing %q", k)
		}
	}
	var models []map[string]any
	decode(t, string(snap["models"]), &models)
	for _, k := range []string{"name", "folder", "total", "completed", "speed", "state", "error"} {
		if _, ok := models[0][k]; !ok {
			t.Errorf("model missing %q", k)
		}
	}
	if models[0]["error"] != nil || models[1]["error"] != "boom" {
		t.Errorf("error must be null or a string: %v", models)
	}
	var services map[string]any
	decode(t, string(snap["services"]), &services)
	if _, ok := services["aria2"]; ok || services["comfyui"] == nil {
		t.Errorf("services = %v", services)
	}
	if string(snap["error"]) != "null" {
		t.Errorf("error = %s", snap["error"])
	}
}

func TestV1RetryTranslatesKeys(t *testing.T) {
	r := newRig(t)
	r.seedFailedModel()
	_, body := r.do(t, "POST", "/v1/models/retry", `{"keys":["loras/bad.bin","nokeyhere"]}`)
	if !strings.Contains(body, `"loras/bad.bin"`) {
		t.Fatal(body)
	}
	if got := r.ctl.retried[0]; len(got) != 1 || got[0] != manifest.ID("loras", "bad.bin") {
		t.Fatalf("ids = %v", got)
	}
	r.do(t, "POST", "/v1/models/retry", `{}`)
	if r.ctl.retried[1] != nil {
		t.Fatal("empty body must retry everything")
	}
	if resp, body := r.do(t, "POST", "/v1/comfyui/restart", ""); resp.StatusCode != 200 || !strings.Contains(body, "true") {
		t.Fatal(body)
	}
}

func TestOpenAPIDocumentsEveryRoute(t *testing.T) {
	r := newRig(t)
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPI, &spec); err != nil {
		t.Fatal(err)
	}
	s := New(Options{State: r.st, Controller: r.ctl, Redactor: redact.New(), LogPath: func(string) string { return "" }})
	brace := regexp.MustCompile(`\{[^}]+\}`)
	for _, route := range s.Routes() {
		method, path, _ := strings.Cut(route, " ")
		ops, ok := spec.Paths[path]
		if !ok {
			t.Errorf("%s is not in openapi.json", route)
			continue
		}
		if _, ok := ops[strings.ToLower(method)]; !ok {
			t.Errorf("%s is not in openapi.json", route)
		}
		_ = brace
	}
	total := 0
	for _, ops := range spec.Paths {
		total += len(ops)
	}
	if total != len(s.Routes()) {
		t.Errorf("spec has %d operations but the server registers %d", total, len(s.Routes()))
	}
}
