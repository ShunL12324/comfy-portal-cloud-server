package state

import (
	"fmt"
	"slices"
	"sort"
)

// Report is the launch's timings as plain lines: per phase, per step, per
// model with its average rate, and the downloads' aggregate rate. The
// supervisor logs it when the launch settles and `cpd report` prints it, so
// the same numbers explain a slow launch whether you have the log or a shell.
func (snap Snapshot) Report() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	total := snap.Elapsed * 1000
	if n := len(snap.Phases); n > 1 && (snap.Phase == PhaseReady || snap.Phase == PhaseFailed) {
		total = snap.Phases[n-1].StartedAt - snap.Phases[0].StartedAt // to ready, not to now
	}
	add("total %s  phase=%s", seconds(total), snap.Phase)
	for _, p := range snap.Phases {
		add("phase %-12s %s", p.Phase, msOrRunning(p.Ms))
	}

	steps := slices.Clone(snap.Steps)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].StartedAt < steps[j].StartedAt })
	for _, st := range steps {
		at := ""
		if len(snap.Phases) > 0 && st.StartedAt > 0 {
			at = "at +" + seconds(st.StartedAt-snap.Phases[0].StartedAt)
		}
		add("step  %-9s %-8s %-12s %s", msOrRunning(st.Ms), st.State, at, st.ID)
	}

	var first, last, fetched int64
	for _, m := range snap.Models {
		line := fmt.Sprintf("model %-8s %9s", m.State, bytes(m.Completed))
		if m.StartedAt > 0 && m.FinishedAt > 0 {
			line += fmt.Sprintf(" in %-7s %s/s", seconds(m.FinishedAt-m.StartedAt), bytes(m.AvgSpeed))
			if first == 0 || m.StartedAt < first {
				first = m.StartedAt
			}
			last = max(last, m.FinishedAt)
			fetched += m.AvgSpeed * (m.FinishedAt - m.StartedAt) / 1000
		} else if m.State == ModelDone {
			line += " already on disk"
		}
		add("%s  %s/%s", line, m.Folder, m.Name)
	}
	if last > first {
		add("downloads %s in %s wall, %s/s aggregate", bytes(fetched), seconds(last-first), bytes(fetched*1000/(last-first)))
	}

	for _, svc := range snap.Services {
		if svc.ReadyMs != nil {
			add("service %-8s answered %s after its last start (restarts %d)", svc.Name, seconds(*svc.ReadyMs), svc.Restarts)
		}
	}
	return out
}

func msOrRunning(ms *int64) string {
	if ms == nil {
		return "running"
	}
	return seconds(*ms)
}

func seconds(ms int64) string { return fmt.Sprintf("%.1fs", float64(ms)/1000) }

func bytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d B", n)
}
