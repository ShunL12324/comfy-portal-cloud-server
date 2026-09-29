package downloads

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/aria2"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

type fakeEngine struct {
	mu       sync.Mutex
	added    []added
	statuses map[string]aria2.Status
	err      error
}

type added struct {
	URI  string
	Opts aria2.Options
	GID  string
}

func (f *fakeEngine) AddURI(_ context.Context, uri string, o aria2.Options) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	gid := fmt.Sprintf("gid%d", len(f.added)+1)
	f.added = append(f.added, added{uri, o, gid})
	return gid, nil
}

func (f *fakeEngine) TellStatus(_ context.Context, gid string) (aria2.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.statuses[gid]
	if !ok {
		return aria2.Status{}, aria2.ErrGIDNotFound
	}
	return st, nil
}

func setup(t *testing.T, cfg Config) (*Manager, *state.State, *fakeEngine) {
	t.Helper()
	cfg.Workspace = t.TempDir()
	cfg.PollInterval = 10 * time.Millisecond
	if cfg.StallAfter == 0 {
		cfg.StallAfter = time.Minute
	}
	r := redact.New(cfg.HFToken, cfg.CivitaiAPIKey)
	st := state.New("", r)
	eng := &fakeEngine{statuses: map[string]aria2.Status{}}
	return New(cfg, st, eng, r), st, eng
}

func model(url, folder, name string) manifest.Model {
	return manifest.Model{URL: url, Folder: folder, Filename: name}
}

func TestCompleteFileIsSkippedPartialIsResumed(t *testing.T) {
	m, st, eng := setup(t, Config{})
	dir := filepath.Join(m.cfg.Workspace, "models", "loras")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "done.bin"), []byte("12345"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "part.bin"), []byte("12"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "part.bin.aria2"), []byte("ctl"), 0o644)

	m.Queue(context.Background(), []manifest.Model{
		model("https://h.example/done.bin", "loras", "done.bin"),
		model("https://h.example/part.bin", "loras", "part.bin"),
	})

	if len(eng.added) != 1 || eng.added[0].Opts.Out != "part.bin" {
		t.Fatalf("added = %+v", eng.added)
	}
	done, _ := st.Model(manifest.ID("loras", "done.bin"))
	if done.State != state.ModelDone || done.Completed != 5 {
		t.Fatalf("%+v", done)
	}
	part, _ := st.Model(manifest.ID("loras", "part.bin"))
	if part.State != state.ModelWaiting {
		t.Fatalf("%+v", part)
	}
}

func TestQueueIsIdempotent(t *testing.T) {
	m, _, eng := setup(t, Config{})
	ms := []manifest.Model{model("https://h.example/a.bin", "loras", "a.bin")}
	m.Queue(context.Background(), ms)
	m.Queue(context.Background(), ms)
	if len(eng.added) != 1 {
		t.Fatalf("added %d times", len(eng.added))
	}
}

func TestOneFailureDoesNotBlockOthersAndRetryWorks(t *testing.T) {
	m, st, eng := setup(t, Config{})
	m.Queue(context.Background(), []manifest.Model{
		model("https://h.example/bad.bin", "loras", "bad.bin"),
		model("https://h.example/good.bin", "loras", "good.bin"),
	})
	eng.statuses["gid1"] = aria2.Status{State: "error", ErrorMessage: "HTTP 404 not found"}
	eng.statuses["gid2"] = aria2.Status{State: "complete", Completed: 10, Total: 10}
	m.poll(context.Background())

	failed, err := m.Wait(context.Background())
	if err != nil || len(failed) != 1 {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
	bad, _ := st.Model(failed[0])
	if bad.ErrorCode != "model_not_found" || bad.State != state.ModelError {
		t.Fatalf("%+v", bad)
	}
	good, _ := st.Model(manifest.ID("loras", "good.bin"))
	if good.State != state.ModelDone {
		t.Fatalf("%+v", good)
	}

	retried := m.Retry(context.Background(), nil)
	if len(retried) != 1 || retried[0] != failed[0] || len(eng.added) != 3 {
		t.Fatalf("retried=%v added=%d", retried, len(eng.added))
	}
	if again := m.Retry(context.Background(), []string{good.ID}); len(again) != 0 {
		t.Fatal("retry must ignore models that did not fail")
	}
}

func TestEngineRestartMarksModelRetryable(t *testing.T) {
	m, st, _ := setup(t, Config{})
	m.Queue(context.Background(), []manifest.Model{model("https://h.example/a.bin", "loras", "a.bin")})
	m.poll(context.Background()) // fake engine has forgotten gid1
	got, _ := st.Model(manifest.ID("loras", "a.bin"))
	if got.ErrorCode != "engine_restarted" || !m.Idle() {
		t.Fatalf("%+v idle=%v", got, m.Idle())
	}
}

func TestHFTokenOnlyGoesToHuggingFace(t *testing.T) {
	m, _, eng := setup(t, Config{HFToken: "hf_supersecrettoken"})
	m.Queue(context.Background(), []manifest.Model{
		model("https://huggingface.co/a/b/resolve/main/x.bin", "loras", "x.bin"),
		model("https://evil.example/x.bin", "loras", "y.bin"),
	})
	if len(eng.added[0].Opts.Headers) != 1 {
		t.Fatalf("hf download should carry the token: %+v", eng.added[0])
	}
	if len(eng.added[1].Opts.Headers) != 0 {
		t.Fatalf("token leaked to another host: %+v", eng.added[1])
	}
}

func TestCivitaiIsResolvedToSignedURL(t *testing.T) {
	signed := httptest.NewServer(http.NotFoundHandler())
	defer signed.Close()
	var gotAuth string
	m, _, _ := setup(t, Config{CivitaiAPIKey: "civitai-key-12345"})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, signed.URL+"/signed?x=1", http.StatusFound)
	}))
	defer api.Close()

	got, err := m.resolveCivitai(context.Background(), api.URL+"/api/download/models/1")
	if err != nil || got != signed.URL+"/signed?x=1" || gotAuth != "Bearer civitai-key-12345" {
		t.Fatalf("got=%q err=%v auth=%q", got, err, gotAuth)
	}
}

func TestPreflight(t *testing.T) {
	head := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Linked-Size", "3000")
	}))
	defer head.Close()

	m, _, _ := setup(t, Config{FreeBytes: func(string) (uint64, error) { return 1000, nil }})
	models := []manifest.Model{{URL: head.URL + "/a.bin"}, {URL: head.URL + "/b.bin", SizeBytes: 50}}
	needed, err := m.Preflight(context.Background(), models)
	if needed != 3050 {
		t.Fatalf("needed = %d", needed)
	}
	if _, ok := err.(*DiskError); !ok {
		t.Fatalf("err = %v", err)
	}
}

func TestStallRaisedWhenNothingMoves(t *testing.T) {
	m, st, eng := setup(t, Config{StallAfter: time.Nanosecond})
	m.Queue(context.Background(), []manifest.Model{model("https://h.example/a.bin", "loras", "a.bin")})
	eng.statuses["gid1"] = aria2.Status{State: "active", Completed: 0, Total: 10}
	time.Sleep(5 * time.Millisecond)
	m.poll(context.Background())
	if !st.Summary().Stalled {
		t.Fatal("expected stalled")
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"HTTP 401":           "model_auth_failed",
		"no space left":      "disk_full",
		"weird":              "model_failed",
		"resource not found": "model_not_found",
	}
	for msg, want := range cases {
		if got, _ := Classify(msg); got != want {
			t.Errorf("%q: got %s want %s", msg, got, want)
		}
	}
}
