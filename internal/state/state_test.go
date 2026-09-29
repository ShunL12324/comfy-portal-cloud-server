package state

import (
	"testing"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
)

func newState() *State { return New("", redact.New()) }

func TestStepRecordsDuration(t *testing.T) {
	s := newState()
	clock := time.Unix(1000, 0)
	s.SetClock(func() time.Time { return clock })
	s.Step("a", StepRunning, "")
	if s.Steps()[0].Ms != nil {
		t.Fatal("running step must have null ms")
	}
	clock = clock.Add(1500 * time.Millisecond)
	s.Step("a", StepDone, "")
	if ms := s.Steps()[0].Ms; ms == nil || *ms != 1500 {
		t.Fatalf("ms = %v", ms)
	}
}

func TestTotalsAndETA(t *testing.T) {
	s := newState()
	s.PutModel(Model{ID: "1", Total: 1000, Completed: 400, Speed: 100, State: ModelActive})
	s.PutModel(Model{ID: "2", Total: 500, Completed: 500, Speed: 999, State: ModelDone})
	got := s.Summary().Totals
	if got.Bytes != 1500 || got.Completed != 900 || got.Speed != 100 {
		t.Fatalf("%+v", got)
	}
	if got.ETASeconds == nil || *got.ETASeconds != 6 {
		t.Fatalf("eta = %v", got.ETASeconds)
	}
}

func TestStallDetection(t *testing.T) {
	s := newState()
	clock := time.Unix(0, 0)
	s.SetClock(func() time.Time { return clock })
	clock = clock.Add(10 * time.Minute)
	s.Progress(false, true, 5*time.Minute)
	if !s.Summary().Stalled {
		t.Fatal("should be stalled")
	}
	s.Progress(true, true, 5*time.Minute)
	if s.Summary().Stalled {
		t.Fatal("progress must clear stalled")
	}
	clock = clock.Add(10 * time.Minute)
	s.Progress(false, false, 5*time.Minute)
	if s.Summary().Stalled {
		t.Fatal("idle is not a stall")
	}
}

func TestUpdateModelOnlyPublishesChanges(t *testing.T) {
	s := newState()
	s.PutModel(Model{ID: "1", State: ModelActive})
	before := s.Hub.LastID()
	s.UpdateModel("1", func(m *Model) { m.State = ModelActive })
	if s.Hub.LastID() != before {
		t.Fatal("no-op update published an event")
	}
	s.UpdateModel("1", func(m *Model) { m.Completed = 5 })
	if s.Hub.LastID() != before+1 {
		t.Fatal("change did not publish")
	}
}

func TestHubReplayAndGap(t *testing.T) {
	h := NewHub()
	for i := 0; i < 3; i++ {
		h.publish("x", i)
	}
	replay, _, cancel, gap := h.Subscribe(1)
	defer cancel()
	if gap || len(replay) != 2 || replay[0].ID != 2 {
		t.Fatalf("replay=%v gap=%v", replay, gap)
	}
	for i := 0; i < ringSize+10; i++ {
		h.publish("x", i)
	}
	_, _, cancel2, gap := h.Subscribe(1)
	defer cancel2()
	if !gap {
		t.Fatal("expected a gap")
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	h := NewHub()
	_, ch, _, _ := h.Subscribe(0)
	for i := 0; i < 300; i++ {
		h.publish("x", i)
	}
	closed := false
	for range ch {
	}
	closed = true
	if !closed {
		t.Fatal("channel should be closed")
	}
}

func TestFailRedacts(t *testing.T) {
	s := New("", redact.New("hunter2-secret"))
	s.Fail("x", "boom hunter2-secret", "")
	if s.Summary().Error.Message != "boom ***" || s.Phase() != PhaseFailed {
		t.Fatalf("%+v", s.Summary().Error)
	}
}
