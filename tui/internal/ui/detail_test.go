package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

func newDetailTestModel(t *testing.T, snap state.Snapshot, cursor int) Model {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	m := New(context.Background(), cli.NewClient(nopRunner{}), cli.Paths{}, nil)
	m.Theme = NewTheme()
	m.Snap = snap
	m.Cursor = cursor
	m.Now = time.Unix(1_000_000, 0)
	return m
}

func TestDetailWrapsLongLastErr(t *testing.T) {
	lastErr := "session conflict: another process holds this runner's registration (cloned dir? second manager?) - remove and re-add"
	snap := state.Snapshot{
		Targets: []state.Target{{URL: "u", Label: "org/repo", Have: 1, Want: 1}},
		Runners: []state.Runner{{
			ID: 1, Name: "mac-1", Target: "u", State: state.RunnerQuarantined, Quarantined: true,
			Fails: 5, NextRetry: 1_000_030, LastErr: lastErr, LogPath: "/abs/runners/.logs/runner-1.log", WorkMB: 12,
		}},
	}
	m := newDetailTestModel(t, snap, 1)

	for _, compact := range []bool{false, true} {
		for _, width := range []int{30, 40, 60} {
			lines := m.renderDetail(width, compact)
			for _, l := range lines {
				if w := ansi.StringWidth(l); w > width {
					t.Errorf("compact=%v width=%d: line %q is %d wide", compact, width, l, w)
				}
			}
			joined := ansi.Strip(strings.Join(lines, "\n"))
			// Every word of the error survives (wrapped, not truncated).
			flat := strings.Join(strings.Fields(joined), " ")
			if !strings.Contains(flat, "second manager?) - remove and re-add") {
				t.Errorf("compact=%v width=%d: lasterr tail lost:\n%s", compact, width, joined)
			}
			for _, want := range []string{"mac-1 · org/repo", "retry in 30s", "12 MB", "quarantined"} {
				if !strings.Contains(joined, want) {
					t.Errorf("compact=%v width=%d: detail missing %q:\n%s", compact, width, want, joined)
				}
			}
		}
	}
}

func TestDetailTargetShowsErrorAndCounts(t *testing.T) {
	snap := state.Snapshot{Targets: []state.Target{{
		URL: "https://github.com/myorg", Label: "myorg (org)", Type: "org", Listed: true,
		Want: 1, Have: 0, Min: 1, Max: 4, Autoscale: true, Queued: 2, Unsatisfiable: 1,
		Error: &state.TargetError{Class: "scope", Message: "token lacks admin:org", Since: 1_000_000 - 3600},
	}}}
	m := newDetailTestModel(t, snap, 0)
	joined := ansi.Strip(strings.Join(m.renderDetail(40, false), "\n"))
	for _, want := range []string{"https://github.com/myorg", "have 0 · want 1", "1-4 · autoscale on", "queued 2 · unsatisfiable 1", "scope: token lacks admin:org", "since 1h0m ago"} {
		if !strings.Contains(strings.Join(strings.Fields(joined), " "), want) {
			t.Errorf("target detail missing %q:\n%s", want, joined)
		}
	}
}
