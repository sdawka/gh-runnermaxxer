package ui

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// Golden-frame tests for the dashboard, at the four breakpoint sizes from
// the Go design doc's §3.2/§5 (120x40 full, 99x40 just under the log-pane
// breakpoint, 80x25 narrow, 60x20 compact+narrow).
//
// Deviation from the design doc: rather than driving a full teatest
// pty-emulated tea.Program (which introduces real time/ticker-driven
// nondeterminism from the 1s clock and spinner ticks racing the capture),
// these tests call Model.renderString() directly after synchronously
// feeding it a WindowSizeMsg and a snapshotMsg built from the same
// internal/state/testdata/full.json golden fixture used by the loader
// tests. That exercises the exact rendering code View() calls, with a
// pinned m.Now so the "tick Ns ago" text is deterministic, without any
// wall-clock race. teatest itself is still a real dependency (verified at
// scaffold time) for a later commit's fuller integration coverage if
// needed; it isn't required to get golden-frame protection for the layout
// logic added in this commit.
var updateGolden = flag.Bool("update", false, "update golden files")

func loadFullSnapshotForView(t *testing.T) state.Snapshot {
	t.Helper()
	snap, err := state.Load(filepath.Join("..", "state", "testdata", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func renderDashboard(t *testing.T, width, height int) string {
	t.Helper()
	return dashboardModel(t, width, height, loadFullSnapshotForView(t)).renderString()
}

// dashboardModel builds a sized model showing snap under NO_COLOR, with
// the clock pinned and the side pane's live tail fed a fixed set of lines
// (the tail's Cmd is never run, so no file is read).
func dashboardModel(t *testing.T, width, height int, snap state.Snapshot) Model {
	t.Helper()
	t.Setenv("NO_COLOR", "1")

	m := New(context.Background(), &fakeEngine{eventLog: "/abs/runners/.logs/runnermaxxer.log"})
	m.Theme = NewTheme()                                   // rebuild now that NO_COLOR is set for this test
	m.Now = time.Unix(snap.TickTS, 0).Add(2 * time.Second) // pin the clock: deterministic "tick 2s ago"

	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m = updated.(Model)
	updated, _ = m.Update(snapshotMsg{snap: snap})
	m = updated.(Model)
	return feedTail(t, m)
}

// feedTail delivers a canned logTailStartedMsg to the model's live tail.
func feedTail(t *testing.T, m Model) Model {
	t.Helper()
	if m.Tail == nil {
		t.Fatal("no live tail after sizing the dashboard")
	}
	var lines []string
	if m.Tail.RunnerID == eventLogID {
		lines = []string{
			"2026-09-28T12:00:40 tick: 3 targets, 5 runners, 1 busy",
			"2026-09-28T12:00:41 myorg/myrepo: queued 3, scaling 2 -> 2 (max 5)",
			"2026-09-28T12:00:42 mac-4: exited unexpectedly (fails 2), retry in 20s",
			"2026-09-28T12:00:43 myorg (org): token lacks admin:org",
			"2026-09-28T12:00:45 tick: 3 targets, 5 runners, 1 busy",
		}
	} else {
		lines = []string{
			"√ Connected to GitHub",
			"Current runner version: '2.337.0'",
			"2026-09-28 12:00:01Z: Listening for Jobs",
			"2026-09-28 12:04:12Z: Running job: build",
		}
	}
	updated, _ := m.Update(logTailStartedMsg{id: m.Tail.RunnerID, stream: m.Tail.stream, lines: lines})
	return updated.(Model)
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v (run with -update to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("frame for %s did not match golden.\n--- got ---\n%s\n--- want ---\n%s", name, got, string(want))
	}
}

func TestDashboardGolden120x40(t *testing.T) {
	assertGolden(t, "dashboard_120x40", renderDashboard(t, 120, 40))
}

func TestDashboardGolden99x40(t *testing.T) {
	assertGolden(t, "dashboard_99x40", renderDashboard(t, 99, 40))
}

func TestDashboardGolden80x25(t *testing.T) {
	assertGolden(t, "dashboard_80x25", renderDashboard(t, 80, 25))
}

func TestDashboardGolden60x20(t *testing.T) {
	assertGolden(t, "dashboard_60x20", renderDashboard(t, 60, 20))
}

func TestDashboardNoColorHasNoEscapeSequences(t *testing.T) {
	got := renderDashboard(t, 120, 40)
	if strings.Contains(got, "\x1b[3") {
		t.Errorf("frame contains a colour escape sequence under NO_COLOR:\n%s", got)
	}
}

func TestDashboardShowsBusyRunnerAndScopeError(t *testing.T) {
	got := renderDashboard(t, 120, 40)
	if !strings.Contains(got, "build") {
		t.Errorf("frame missing the busy runner's job name:\n%s", got)
	}
	// With the side pane showing a detail block, the row keeps only the
	// error class; the full message is in the detail block once the cursor
	// is on that target.
	if !strings.Contains(got, "⚠ scope") {
		t.Errorf("frame missing the target's error marker:\n%s", got)
	}
	if !strings.Contains(got, "myorg/myrepo") {
		t.Errorf("frame missing the repo target label:\n%s", got)
	}

	m := dashboardModel(t, 120, 40, loadFullSnapshotForView(t))
	m.Cursor = 7 // the myorg (org) target row
	if got := m.renderString(); !strings.Contains(got, "scope: token lacks admin:org") {
		t.Errorf("detail block missing the target scope error:\n%s", got)
	}
}

// TestNarrowWithoutRoomForDetailKeepsInlineErrors: when the table takes
// every row (too short for a detail block), rows fall back to showing a
// truncated error inline.
func TestNarrowWithoutRoomForDetailKeepsInlineErrors(t *testing.T) {
	got := dashboardModel(t, 90, 11, loadFullSnapshotForView(t)).renderString()
	if !strings.Contains(got, "session conflict") || !strings.Contains(got, "exited unexpectedly") {
		t.Errorf("expected inline errors with no detail area:\n%s", got)
	}
}

func TestDashboardCompactHeaderBelow30Rows(t *testing.T) {
	got := renderDashboard(t, 80, 25)
	// Compact mode collapses the header to one line: the fleet summary
	// line ("tarball ...") should be gone.
	if strings.Contains(got, "tarball") {
		t.Errorf("expected the compact header to drop the tarball summary line at height 25:\n%s", got)
	}
}

// A full-height frame drawn inline scrolls its top line into scrollback on
// every redraw (the status bar piles up above the dashboard), so View must
// ask for the alternate screen.
func TestViewUsesAltScreen(t *testing.T) {
	if !dashboardModel(t, 80, 25, loadFullSnapshotForView(t)).View().AltScreen {
		t.Error("View() is not on the alternate screen")
	}
}
