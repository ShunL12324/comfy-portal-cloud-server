package pipeline

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/aria2"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/config"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/downloads"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/procs"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// TestMain doubles as the fake ComfyUI: when re-executed with CP_FAKE_COMFY
// set it serves /system_stats on the port given after --port.
func TestMain(m *testing.M) {
	if os.Getenv("CP_FAKE_COMFY") != "" {
		args := os.Args
		port := args[len(args)-1]
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			os.Exit(2)
		}
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{}") }))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// instantEngine completes every download immediately.
type instantEngine struct {
	mu   sync.Mutex
	n    int
	fail map[string]bool
	uris map[string]string
}

func (e *instantEngine) AddURI(_ context.Context, uri string, o aria2.Options) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	gid := fmt.Sprintf("g%d", e.n)
	if e.uris == nil {
		e.uris = map[string]string{}
	}
	e.uris[gid] = o.Out
	return gid, nil
}

func (e *instantEngine) TellStatus(_ context.Context, gid string) (aria2.Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail[e.uris[gid]] {
		return aria2.Status{State: "error", ErrorMessage: "HTTP 404"}, nil
	}
	return aria2.Status{State: "complete", Completed: 100, Total: 100}, nil
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newPipeline(t *testing.T, eng *instantEngine) (*Pipeline, *state.State, context.Context) {
	t.Helper()
	ws, comfy := t.TempDir(), t.TempDir()
	bin := filepath.Join(comfy, "venv", "bin")
	_ = os.MkdirAll(bin, 0o755)
	self, _ := os.Executable()
	script := fmt.Sprintf("#!/bin/sh\nCP_FAKE_COMFY=1 exec %s \"$@\"\n", self)
	if err := os.WriteFile(filepath.Join(bin, "python"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Workspace: ws, ComfyDir: comfy, ComfyPort: freePort(t),
		PollInterval: 10 * time.Millisecond, StallAfter: time.Minute,
		ComfyStartTimeout: 20 * time.Second, InstallTimeout: 20 * time.Second,
	}
	r := redact.New()
	st := state.New("", r)
	dl := downloads.New(downloads.Config{
		Workspace: ws, PollInterval: 10 * time.Millisecond, StallAfter: time.Minute,
		FreeBytes: func(string) (uint64, error) { return 1 << 40, nil },
	}, st, eng, r)
	ctx, cancel := context.WithCancel(context.Background())
	sup := procs.New(st)
	t.Cleanup(func() { cancel(); sup.Wait() })
	_ = ctx
	return &Pipeline{
		Cfg: cfg, St: st, Procs: sup, DL: dl,
		Run:         func(context.Context, string, ...string) (string, error) { return "", nil },
		StartEngine: func(context.Context) error { return nil },
	}, st, ctx
}

func bootWith(t *testing.T, ctx context.Context, p *Pipeline, m manifest.Manifest) {
	t.Helper()
	done := make(chan struct{})
	go func() { p.Boot(ctx, m); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("boot did not finish")
	}
}

func TestBootReachesReadyDespiteOneFailedModel(t *testing.T) {
	eng := &instantEngine{fail: map[string]bool{"bad.bin": true}}
	p, st, ctx := newPipeline(t, eng)
	bootWith(t, ctx, p, manifest.Manifest{Models: []manifest.Model{
		{URL: "https://h.example/good.bin", Folder: "loras", SizeBytes: 100},
		{URL: "https://h.example/bad.bin", Folder: "loras", SizeBytes: 100},
	}})

	if st.Phase() != state.PhaseReady {
		t.Fatalf("phase = %s, error = %+v", st.Phase(), st.Summary().Error)
	}
	if got := st.FailedModelIDs(); len(got) != 1 {
		t.Fatalf("failed = %v", got)
	}
	if svc, _ := st.Service("comfyui"); svc.State != state.ServiceRunning || svc.AnsweredAt == 0 {
		t.Fatalf("%+v", svc)
	}
}

func TestBootFailsWhenDiskIsTooSmall(t *testing.T) {
	p, st, ctx := newPipeline(t, &instantEngine{})
	p.DL = downloads.New(downloads.Config{
		Workspace: p.Cfg.Workspace, FreeBytes: func(string) (uint64, error) { return 10, nil },
	}, st, &instantEngine{}, redact.New())
	bootWith(t, ctx, p, manifest.Manifest{Models: []manifest.Model{{URL: "https://h.example/a.bin", SizeBytes: 1 << 30}}})
	if st.Phase() != state.PhaseFailed || st.Summary().Error.Code != "disk_full" {
		t.Fatalf("phase=%s err=%+v", st.Phase(), st.Summary().Error)
	}
}

func TestBootFailsWhenExtensionCannotBeInstalled(t *testing.T) {
	p, st, ctx := newPipeline(t, &instantEngine{})
	p.Run = func(context.Context, string, ...string) (string, error) { return "nope", fmt.Errorf("exit 1") }
	bootWith(t, ctx, p, manifest.Manifest{Extensions: []string{"https://github.com/u/broken"}})
	if st.Summary().Error == nil || st.Summary().Error.Code != "environment_install_failed" {
		t.Fatalf("%+v", st.Summary().Error)
	}
}

func TestApplyAddsModelsAndFlagsRestartForExtensions(t *testing.T) {
	eng := &instantEngine{}
	p, st, ctx := newPipeline(t, eng)
	bootWith(t, ctx, p, manifest.Manifest{})

	var cloned []string
	p.Run = func(_ context.Context, _ string, argv ...string) (string, error) {
		if argv[0] == "git" {
			dest := argv[len(argv)-1]
			_ = os.MkdirAll(dest, 0o755)
			cloned = append(cloned, dest)
		}
		return "", nil
	}
	go p.DL.Run(ctx)
	err := p.Apply(ctx, manifest.Manifest{
		Models:     []manifest.Model{{URL: "https://h.example/new.bin", Folder: "loras", SizeBytes: 5}},
		Extensions: []string{"https://github.com/u/NewNode"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.WaitBackground()
	if len(st.Models()) != 1 || len(cloned) != 1 || !st.Summary().RestartRequired {
		t.Fatalf("models=%d cloned=%v restart=%v", len(st.Models()), cloned, st.Summary().RestartRequired)
	}
	if got := p.Manifest(); len(got.Models) != 1 || len(got.Extensions) != 1 {
		t.Fatalf("%+v", got)
	}
	// Applying the same thing again changes nothing.
	_ = p.Apply(ctx, manifest.Manifest{Models: []manifest.Model{{URL: "https://h.example/new.bin", Folder: "loras"}}})
	p.WaitBackground()
	if len(eng.uris) != 1 {
		t.Fatalf("re-applied model was queued again: %v", eng.uris)
	}
}

func TestApplyBeforeReadyIsRejected(t *testing.T) {
	p, _, ctx := newPipeline(t, &instantEngine{})
	if err := p.Apply(ctx, manifest.Manifest{}); err != ErrNotReady {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyRejectsInvalidManifest(t *testing.T) {
	p, _, ctx := newPipeline(t, &instantEngine{})
	if err := p.Apply(ctx, manifest.Manifest{Models: []manifest.Model{{URL: "nope"}}}); err == nil || err == ErrNotReady {
		t.Fatalf("err = %v", err)
	}
}

func TestMerge(t *testing.T) {
	a := manifest.Manifest{Extensions: []string{"x"}, Models: []manifest.Model{{URL: "https://h/a.bin"}}}
	b := manifest.Manifest{Extensions: []string{"x", "y"}, Models: []manifest.Model{{URL: "https://h/a.bin"}, {URL: "https://h/b.bin"}}}
	got := merge(a, b)
	if strings.Join(got.Extensions, ",") != "x,y" || len(got.Models) != 2 {
		t.Fatalf("%+v", got)
	}
}
