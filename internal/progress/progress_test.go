package progress

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

func TestLine(t *testing.T) {
	snap := state.Snapshot{
		Summary: state.Summary{
			Phase: state.PhaseDownloading, Elapsed: 12,
			Totals: state.Totals{Bytes: 7516192768, Completed: 2147483648, Speed: 220200960},
		},
		Steps: []state.Step{
			{ID: "link-directories", State: state.StepDone},
			{ID: "aria2", State: state.StepDone},
			{ID: "extension:https://github.com/rgthree/rgthree-comfy", State: state.StepRunning},
			{ID: "extensions", State: state.StepRunning},
		},
		Models: []state.Model{
			{Name: "sd_xl_base_1.0.safetensors", State: state.ModelActive},
			{Name: "lora.safetensors", State: state.ModelDone},
		},
	}
	want := "CP phase=downloading steps=2/4 step=extension models=1/2 bytes=2147483648/7516192768 rate=220200960 t=12"
	if got := Line(snap); got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	for _, leak := range []string{"github.com", "safetensors"} {
		if strings.Contains(Line(snap), leak) {
			t.Fatalf("line carries manifest text %q", leak)
		}
	}
}

func TestLineBeforeAnything(t *testing.T) {
	got := Line(state.Snapshot{Summary: state.Summary{Phase: state.PhasePreparing}})
	if got != "CP phase=preparing steps=0/0 t=0" {
		t.Fatalf("%q", got)
	}
}

func TestLineFailed(t *testing.T) {
	got := Line(state.Snapshot{Summary: state.Summary{
		Phase: state.PhaseFailed, Elapsed: 3, Error: &state.Problem{Code: "disk_full", Message: "no room"},
	}})
	if got != "CP phase=failed steps=0/0 error=disk_full t=3" {
		t.Fatalf("%q", got)
	}
}

// syncBuffer is written by the reporter's goroutine and read by the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Split(strings.TrimSpace(s.b.String()), "\n")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReporterRateLimitsAndFollowsPhases(t *testing.T) {
	st := state.New("", redact.New())
	var mu sync.Mutex
	clock := time.Unix(1000, 0)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }

	out := &syncBuffer{}
	ctx := t.Context()
	r := &Reporter{St: st, Out: out, Every: 2 * time.Second, Tick: time.Millisecond, now: now}
	go r.Run(ctx)

	// The first line is printed at once, before anything has happened.
	waitFor(t, func() bool { return len(out.lines()) == 1 && out.lines()[0] != "" })
	if !strings.HasPrefix(out.lines()[0], "CP phase=preparing steps=0/0") {
		t.Fatalf("first line %q", out.lines()[0])
	}

	// A step change inside the 2 s window waits for it.
	st.Step("link-directories", state.StepRunning, "")
	time.Sleep(30 * time.Millisecond)
	if n := len(out.lines()); n != 1 {
		t.Fatalf("printed %d lines inside the window", n)
	}
	advance(2 * time.Second)
	waitFor(t, func() bool { return len(out.lines()) == 2 })
	if !strings.Contains(out.lines()[1], "steps=0/1 step=link-directories") {
		t.Fatalf("%q", out.lines()[1])
	}

	// A new phase only waits a second.
	st.SetPhase(state.PhaseDownloading)
	time.Sleep(30 * time.Millisecond)
	if n := len(out.lines()); n != 2 {
		t.Fatalf("phase change printed early: %d lines", n)
	}
	advance(time.Second)
	waitFor(t, func() bool { return len(out.lines()) == 3 })
	if !strings.Contains(out.lines()[2], "phase=downloading") {
		t.Fatalf("%q", out.lines()[2])
	}

	// Nothing changed: nothing printed, however long it has been.
	advance(time.Minute)
	time.Sleep(30 * time.Millisecond)
	if n := len(out.lines()); n != 3 {
		t.Fatalf("printed an unchanged line: %d lines", n)
	}
}

func TestReporterRedacts(t *testing.T) {
	st := state.New("", redact.New())
	st.Fail("secret-token-12345", "boom", "")
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	r := &Reporter{St: st, Out: out, Redactor: redact.New("secret-token-12345"), Tick: time.Millisecond}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	waitFor(t, func() bool { return out.lines()[0] != "" })
	cancel()
	<-done
	for _, l := range out.lines() {
		if strings.Contains(l, "secret-token") {
			t.Fatalf("leaked: %q", l)
		}
	}
}
