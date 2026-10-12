// Package progress prints one short, machine-readable line about the launch to
// the container's log as it goes, for whatever reads that log before the API
// is reachable: vast's console, `vastai logs`, docker logs.
//
//	CP phase=downloading steps=6/7 step=extensions models=0/2 bytes=2147483648/7516192768 rate=220200960 t=12
//
// Each line stands alone (a reader that keeps only the last line still has the
// whole picture), keys are fixed, numbers are plain integers (bytes, bytes per
// second, seconds), and nothing in it comes from the manifest: no URLs, file
// names or tokens. Lines are redacted all the same.
package progress

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// Prefix starts every line, so a reader can pick them out of a mixed log.
const Prefix = "CP "

// Line describes a snapshot in one line. t is the seconds since the launch
// began; it is left out of the comparison that decides whether to print.
func Line(snap state.Snapshot) string {
	var b strings.Builder
	b.WriteString(Prefix)
	b.WriteString(body(snap))
	fmt.Fprintf(&b, " t=%d", snap.Elapsed)
	return b.String()
}

// body is the line without its clock: what has to change for a new line.
func body(snap state.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "phase=%s", snap.Phase)

	done, running := 0, ""
	for _, st := range snap.Steps {
		switch st.State {
		case state.StepDone:
			done++
		case state.StepRunning:
			if running == "" {
				running = st.ID
			}
		}
	}
	fmt.Fprintf(&b, " steps=%d/%d", done, len(snap.Steps))
	if running != "" {
		// "extension:<url>" is per item; the kind is enough here, and keeps
		// the manifest's URLs out of the log.
		kind, _, _ := strings.Cut(running, ":")
		fmt.Fprintf(&b, " step=%s", kind)
	}

	if len(snap.Models) > 0 {
		modelsDone := 0
		for _, m := range snap.Models {
			if m.State == state.ModelDone {
				modelsDone++
			}
		}
		fmt.Fprintf(&b, " models=%d/%d bytes=%d/%d rate=%d",
			modelsDone, len(snap.Models), snap.Totals.Completed, snap.Totals.Bytes, snap.Totals.Speed)
	}
	if snap.Error != nil {
		fmt.Fprintf(&b, " error=%s", snap.Error.Code)
	}
	return b.String()
}

// Reporter prints a line whenever the launch changes, at most once per Every.
// A change of phase, or the launch settling, is printed at most one second
// late, however long Every is, so the last line is never stale.
type Reporter struct {
	St       *state.State
	Out      io.Writer
	Redactor *redact.Redactor
	// Every is the least time between two lines about byte counts or steps.
	// Defaults to 2 s: vast keeps only the last line or two of a log.
	Every time.Duration
	// Tick is how often the state is read. Defaults to 250 ms; tests shorten it.
	Tick time.Duration
	now  func() time.Time
}

// Run prints the first line at once, then follows the state until ctx ends.
func (r *Reporter) Run(ctx context.Context) {
	every, tick := r.Every, r.Tick
	if every <= 0 {
		every = 2 * time.Second
	}
	if tick <= 0 {
		tick = 250 * time.Millisecond
	}
	now := r.now
	if now == nil {
		now = time.Now
	}

	var lastBody string
	var lastPhase state.Phase
	var lastAt time.Time
	emit := func(force bool) {
		snap := r.St.Snapshot()
		b := body(snap)
		if b == lastBody {
			return
		}
		gap := every
		if snap.Phase != lastPhase {
			gap = time.Second
		}
		if !force && now().Sub(lastAt) < gap {
			return
		}
		lastBody, lastPhase, lastAt = b, snap.Phase, now()
		line := Line(snap)
		if r.Redactor != nil {
			line = r.Redactor.String(line)
		}
		_, _ = io.WriteString(r.Out, line+"\n")
	}

	emit(true)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			emit(true)
			return
		case <-t.C:
			emit(false)
		}
	}
}

// ContainerLog is where the lines go: the container's own stdout, which is
// what docker logs. The supervisor is usually not PID 1 there: the app starts
// it from vast's onstart with its output sent to a file, so it writes to PID
// 1's stdout instead. Falls back to its own stdout (PID 1 under the image's
// entrypoint, or a local run).
func ContainerLog() io.Writer {
	if os.Getpid() != 1 {
		if f, err := os.OpenFile("/proc/1/fd/1", os.O_WRONLY|os.O_APPEND, 0); err == nil {
			return f
		}
	}
	return os.Stdout
}
