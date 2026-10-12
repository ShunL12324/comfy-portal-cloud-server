package procs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

func newSup(t *testing.T) (*Supervisor, *state.State) {
	t.Helper()
	st := state.New("", redact.New())
	s := New(st)
	s.MinBackoff, s.MaxBackoff, s.StopTimeout = 10*time.Millisecond, 50*time.Millisecond, 2*time.Second
	return s, st
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCrashedProcessIsRestarted(t *testing.T) {
	s, st := newSup(t)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx, Spec{Name: "flaky", Argv: []string{"sh", "-c", "exit 3"}, LogPath: filepath.Join(t.TempDir(), "l")}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "two restarts", func() bool {
		svc, _ := st.Service("flaky")
		return svc.Restarts >= 2 && svc.LastExit != nil && *svc.LastExit == 3
	})
	cancel()
	s.Wait()
	if svc, _ := st.Service("flaky"); svc.State != state.ServiceStopped {
		t.Fatalf("state = %s", svc.State)
	}
}

func TestManualRestartAndShutdownKillTheProcess(t *testing.T) {
	s, st := newSup(t)
	ctx, cancel := context.WithCancel(context.Background())
	_ = s.Start(ctx, Spec{Name: "sleeper", Argv: []string{"sleep", "60"}, LogPath: filepath.Join(t.TempDir(), "l")})
	eventually(t, "running", func() bool { svc, _ := st.Service("sleeper"); return svc.State == state.ServiceRunning && svc.PID > 0 })
	first, _ := st.Service("sleeper")

	if err := s.Restart("sleeper"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "new pid", func() bool {
		svc, _ := st.Service("sleeper")
		return svc.Restarts == 1 && svc.State == state.ServiceRunning && svc.PID != first.PID && svc.PID > 0
	})
	if err := s.Restart("nope"); err == nil {
		t.Fatal("unknown service must error")
	}
	cancel()
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not stop the process")
	}
}

func TestReadyURLGatesRunning(t *testing.T) {
	s, st := newSup(t)
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.Wait() }()
	_ = s.Start(ctx, Spec{Name: "svc", Argv: []string{"sleep", "60"}, LogPath: filepath.Join(t.TempDir(), "l"), ReadyURL: srv.URL})
	eventually(t, "starting", func() bool { svc, _ := st.Service("svc"); return svc.State == state.ServiceStarting && svc.PID > 0 })
	up.Store(true)
	if err := s.WaitRunning(ctx, "svc", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if svc, _ := st.Service("svc"); svc.AnsweredAt == 0 {
		t.Fatal("answeredAt not set")
	}
}
