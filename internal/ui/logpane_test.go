package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

func writeTempLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner-1.log")
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newLogTestModel(snap state.Snapshot) Model {
	m := New(context.Background(), &fakeEngine{})
	m.Snap = snap
	m.Width, m.Height = 120, 40
	m.ShowLog = true
	return m
}

func TestActionLogsOpensAndTailsRunnerLog(t *testing.T) {
	path := writeTempLog(t, "line one", "line two")
	r := state.Runner{ID: 1, Name: "runner-1", LogPath: path}
	m := newLogTestModel(state.Snapshot{Runners: []state.Runner{r}})
	m.Cursor = 1 // row 0 is the Unconfigured header, row 1 is the runner

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "l"})
	got := updated.(Model)
	if got.Log == nil || got.Log.RunnerID != 1 {
		t.Fatal("'l' did not open a log pane for the current runner")
	}
	if cmd == nil {
		t.Fatal("openRunnerLog returned a nil Cmd")
	}

	msg := cmd()
	started, ok := msg.(logTailStartedMsg)
	if !ok {
		t.Fatalf("Cmd produced %T, want logTailStartedMsg", msg)
	}
	if len(started.lines) != 2 || started.lines[0] != "line one" {
		t.Errorf("initial lines = %v, want [line one line two]", started.lines)
	}

	updated2, _ := got.Update(started)
	got = updated2.(Model)
	if len(got.Log.Lines) != 2 {
		t.Errorf("Log.Lines = %v after logTailStartedMsg, want 2 lines", got.Log.Lines)
	}
	got.closeLog() // release the tail goroutine
}

func TestActionLogsTogglesClosedOnSameRunner(t *testing.T) {
	path := writeTempLog(t, "hello")
	r := state.Runner{ID: 1, Name: "runner-1", LogPath: path}
	m := newLogTestModel(state.Snapshot{Runners: []state.Runner{r}})
	m.Cursor = 1 // row 0 is the Unconfigured header, row 1 is the runner

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "l"})
	m = updated.(Model)
	if m.Log == nil {
		t.Fatal("expected a log pane to open")
	}

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "l"})
	m = updated.(Model)
	if m.Log != nil {
		t.Error("second 'l' on the same runner did not close the pane")
	}
	if cmd != nil {
		t.Error("closing the log produced a Cmd, want none")
	}
}

func TestActionLogsNoLogPathTostsInsteadOfOpening(t *testing.T) {
	r := state.Runner{ID: 1, Name: "runner-1", LogPath: ""}
	m := newLogTestModel(state.Snapshot{Runners: []state.Runner{r}})
	m.Cursor = 1 // row 0 is the Unconfigured header, row 1 is the runner

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "l"})
	got := updated.(Model)
	if got.Log != nil {
		t.Error("opened a log pane with no log file")
	}
	if cmd != nil {
		t.Error("expected no Cmd when there's no log file yet")
	}
	if len(got.Notices) == 0 {
		t.Error("expected a notice explaining there's no log file yet")
	}
}

func TestEscClosesOpenLogBeforeNormalDispatch(t *testing.T) {
	path := writeTempLog(t, "hello")
	r := state.Runner{ID: 1, Name: "runner-1", LogPath: path}
	m := newLogTestModel(state.Snapshot{Runners: []state.Runner{r}})
	m.Cursor = 1 // row 0 is the Unconfigured header, row 1 is the runner
	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "l"})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyPressMsg{Text: "esc"})
	got := updated.(Model)
	if got.Log != nil {
		t.Error("Esc did not close the open log pane")
	}
}

func TestLogLinesMsgAppendsAndIgnoresStaleRunner(t *testing.T) {
	r := state.Runner{ID: 1, Name: "runner-1", LogPath: "/dev/null"}
	m := newLogTestModel(state.Snapshot{Runners: []state.Runner{r}})
	m.Log = newLogPane("runner-1", 1, 80, 20)
	m.Log.setLines([]string{"a"})

	updated, _ := m.Update(logLinesMsg{id: 1, lines: []string{"b", "c"}})
	got := updated.(Model)
	if len(got.Log.Lines) != 3 {
		t.Fatalf("Lines = %v, want 3 lines after append", got.Log.Lines)
	}

	// A message for a runner id that's no longer the open pane (switched
	// away) must be dropped rather than corrupting the new pane's buffer.
	updated, _ = got.Update(logLinesMsg{id: 99, lines: []string{"stale"}})
	got2 := updated.(Model)
	if len(got2.Log.Lines) != 3 {
		t.Error("a stale-id logLinesMsg mutated the current pane")
	}
}

func TestLogRingBufferCaps(t *testing.T) {
	lp := newLogPane("t", 1, 80, 20)
	lines := make([]string, logRingLimit+100)
	for i := range lines {
		lines[i] = "line"
	}
	lp.setLines(lines)
	if len(lp.Lines) != logRingLimit {
		t.Errorf("len(Lines) = %d, want %d (capped)", len(lp.Lines), logRingLimit)
	}
}

func TestNavigateLogWrapsAcrossRunners(t *testing.T) {
	pathA := writeTempLog(t, "a")
	pathB := writeTempLog(t, "b")
	snap := state.Snapshot{Runners: []state.Runner{
		{ID: 1, Name: "runner-1", LogPath: pathA},
		{ID: 2, Name: "runner-2", LogPath: pathB},
	}}
	m := newLogTestModel(snap)
	m.Log = newLogPane("runner-1", 1, 80, 20)

	updated, cmd := m.navigateLog(true)
	if updated.Log.RunnerID != 2 {
		t.Errorf("RunnerID = %d after navigate(next), want 2", updated.Log.RunnerID)
	}
	if cmd == nil {
		t.Fatal("navigateLog returned a nil Cmd")
	}
	updated.closeLog()

	updated, _ = m.navigateLog(false)
	if updated.Log.RunnerID != 2 {
		t.Errorf("RunnerID = %d after navigate(prev) wrapping from the first runner, want 2", updated.Log.RunnerID)
	}
	updated.closeLog()
}

func TestFullScreenWhenNarrowSidePaneWhenWide(t *testing.T) {
	path := writeTempLog(t, "hi")
	r := state.Runner{ID: 1, Name: "runner-1", LogPath: path}

	wide := newLogTestModel(state.Snapshot{Runners: []state.Runner{r}})
	wide.ShowLog = true
	wide.Log = newLogPane("runner-1", 1, 80, 20)
	if strings.Contains(wide.renderString(), "Esc/q close") {
		t.Error("wide (ShowLog) render used the full-screen footer, want the side pane")
	}
	wide.closeLog()

	narrow := newLogTestModel(state.Snapshot{Runners: []state.Runner{r}})
	narrow.ShowLog = false
	narrow.Log = newLogPane("runner-1", 1, 80, 20)
	if !strings.Contains(narrow.renderString(), "Esc/q close") {
		t.Error("narrow (!ShowLog) render did not use the full-screen footer")
	}
	narrow.closeLog()
}

func TestDaemonLogTogglesIndependentlyOfRunnerLog(t *testing.T) {
	m := newLogTestModel(state.Snapshot{})
	m.EventLog = "/dev/null"

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "L"})
	got := updated.(Model)
	if got.Log == nil || got.Log.RunnerID != eventLogID {
		t.Fatal("'L' did not open the daemon log")
	}
	if cmd == nil {
		t.Fatal("openDaemonLog returned a nil Cmd")
	}

	updated, _ = got.handleKey(tea.KeyPressMsg{Text: "L"})
	got = updated.(Model)
	if got.Log != nil {
		t.Error("second 'L' did not close the daemon log")
	}
}

func TestFollowToggleStopsAutoScroll(t *testing.T) {
	lp := newLogPane("t", 1, 80, 5)
	lp.setLines([]string{"1", "2", "3", "4", "5", "6", "7", "8"})
	if !lp.Follow {
		t.Fatal("expected Follow to default true")
	}

	m := newLogTestModel(state.Snapshot{})
	m.Log = lp
	m.ShowLog = false // full-screen: 'f' is claimed by handleLogKey

	handled, updated, _ := m.handleLogKey(tea.KeyPressMsg{Text: "f"})
	if !handled {
		t.Fatal("'f' was not handled by handleLogKey")
	}
	if updated.Log.Follow {
		t.Error("Follow still true after pressing 'f'")
	}
}
