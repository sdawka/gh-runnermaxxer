package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// assertFrameFits checks the frame invariant: never more than height lines
// or width columns.
func assertFrameFits(t *testing.T, label, frame string, width, height int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) > height {
		t.Errorf("%s: frame is %d lines, want <= %d:\n%s", label, len(lines), height, frame)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("%s: line %d is %d columns, want <= %d: %q", label, i, w, width, l)
		}
	}
}

var frameSizes = [][2]int{
	{160, 50}, {120, 40}, {100, 30}, {100, 12}, {99, 40}, {80, 25}, {80, 12},
	{60, 20}, {60, 8}, {45, 15}, {30, 6}, {20, 3}, {200, 5},
}

// TestFrameNeverExceedsTerminal renders every screen state the dashboard
// can be in (plain, help, confirm, explicit log, toast, projects) with a
// small and a large snapshot, at a spread of sizes, cursor at the top and
// the bottom, and checks the frame always fits.
func TestFrameNeverExceedsTerminal(t *testing.T) {
	snaps := map[string]state.Snapshot{
		"full": loadFullSnapshotForView(t),
		"many": manyRunnersSnapshot(),
		"none": {},
	}
	variants := map[string]func(Model) Model{
		"plain": func(m Model) Model { return m },
		"help":  func(m Model) Model { m.Help = true; return m },
		"confirm": func(m Model) Model {
			m.Confirm = &ConfirmModel{
				Title: "Runner mac-2 is busy running a job. Drain it (finish the job, then remove) or remove it now?",
				Body:  "Removing now cancels the job on GitHub and it will be retried elsewhere.",
				Choices: []Choice{
					{Key: "d", Label: "Drain"}, {Key: "D", Label: "Remove now", Danger: true}, {Key: "n", Label: "Cancel"},
				},
			}
			return m
		},
		"log": func(m Model) Model {
			m.Log = newLogPane("runner mac-2", 2, m.Width, m.Height)
			m.Log.setLines(strings.Split(strings.Repeat("a fairly long log line that will not fit in a narrow pane at all\n", 200), "\n"))
			return m
		},
		"toast": func(m Model) Model {
			m.notice(strings.Repeat("a very long toast message ", 10), LevelError)
			return m
		},
		"projects": func(m Model) Model { m.Screen = ScreenProjects; return m },
	}

	for snapName, snap := range snaps {
		for _, size := range frameSizes {
			w, h := size[0], size[1]
			base := dashboardModel(t, w, h, snap)
			for name, variant := range variants {
				for _, cursor := range []int{0, len(base.rows()) - 1} {
					m := variant(base)
					if cursor >= 0 {
						m.Cursor = cursor
					}
					updated, _ := m.Update(nudgeMsg{}) // let afterUpdate settle the scroll window
					m = updated.(Model)
					label := fmt.Sprintf("%s/%s/%dx%d/cursor %d", snapName, name, w, h, m.Cursor)
					assertFrameFits(t, label, m.renderString(), w, h)
					m.closeLog()
					m.stopTail()
				}
			}
		}
	}
}

// nudgeMsg is a message Update ignores, for running afterUpdate (scroll
// window and live-tail sync) without side effects or Cmds of its own.
type nudgeMsg struct{}

// TestCursorStaysVisibleWhenScrolling drives the cursor down every row of a
// snapshot too big for the terminal and back up, checking the cursor
// marker is always on screen and the indicators appear.
func TestCursorStaysVisibleWhenScrolling(t *testing.T) {
	for _, size := range [][2]int{{160, 50}, {120, 20}, {80, 25}, {60, 14}} {
		m := dashboardModel(t, size[0], size[1], manyRunnersSnapshot())
		n := len(m.rows())
		sawBelow, sawAbove := false, false
		check := func(dir string) {
			frame := m.renderString()
			row, _ := m.cursorRow()
			name := row.Target.Label
			if row.Kind == RowRunner {
				name = row.Runner.Name + " "
			}
			if len(name) > 12 {
				name = name[:12] // long labels are truncated in narrow tables
			}
			found := false
			for _, l := range strings.Split(frame, "\n") {
				plain := ansi.Strip(l)
				if strings.HasPrefix(plain, m.Glyphs.Selected) && strings.Contains(plain, name) {
					found = true
				}
			}
			if !found {
				t.Fatalf("%dx%d %s: cursor row %d (%s) not on screen:\n%s", size[0], size[1], dir, m.Cursor, name, frame)
			}
			sawBelow = sawBelow || strings.Contains(frame, m.Glyphs.MoreDown+" ")
			sawAbove = sawAbove || strings.Contains(frame, m.Glyphs.MoreUp+" ")
			assertFrameFits(t, dir, frame, size[0], size[1])
		}
		check("start")
		for i := 0; i < n; i++ {
			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			m = updated.(Model)
			check("down")
		}
		for i := 0; i < n; i++ {
			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
			m = updated.(Model)
			check("up")
		}
		if !sawAbove || !sawBelow {
			t.Errorf("%dx%d: expected both scroll indicators at some point (above=%v below=%v)", size[0], size[1], sawAbove, sawBelow)
		}
		m.stopTail()
	}
}

// TestSidePaneSwitchesLogSourceWithCursor: the live tail follows the
// cursor's runner, falls back to the daemon log on a header row, only
// restarts when the source changes, and drops lines from the old tail.
func TestSidePaneSwitchesLogSourceWithCursor(t *testing.T) {
	pathA := writeTempLog(t, "alpha one", "alpha two")
	pathB := writeTempLog(t, "bravo one")
	daemon := writeTempLog(t, "daemon one")
	snap := state.Snapshot{
		Targets: []state.Target{{URL: "u", Label: "org/repo", Have: 2, Want: 2, Listed: true}},
		Runners: []state.Runner{
			{ID: 1, Name: "mac-1", Target: "u", State: state.RunnerIdle, LogPath: pathA},
			{ID: 2, Name: "mac-2", Target: "u", State: state.RunnerIdle, LogPath: pathB},
		},
	}
	m := newLogTestModel(snap)
	m.EventLog = daemon

	// Cursor on the target header: the daemon log.
	updated, cmd := m.Update(nudgeMsg{})
	m = updated.(Model)
	if m.Tail == nil || m.Tail.RunnerID != eventLogID {
		t.Fatalf("header row: tail = %+v, want the daemon log", m.Tail)
	}
	m = runTailCmd(t, m, cmd)
	if got := strings.Join(m.Tail.Lines, "|"); got != "daemon one" {
		t.Errorf("daemon tail lines = %q", got)
	}

	// Down onto mac-1: switches to its log.
	updated, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(Model)
	if m.Tail.RunnerID != 1 || cmd == nil {
		t.Fatalf("runner row: tail id = %d cmd nil = %v, want runner 1 and a start Cmd", m.Tail.RunnerID, cmd == nil)
	}
	oldStream := m.Tail.stream
	m = runTailCmd(t, m, cmd)
	if got := strings.Join(m.Tail.Lines, "|"); got != "alpha one|alpha two" {
		t.Errorf("mac-1 tail lines = %q", got)
	}
	frame := m.renderString()
	if !strings.Contains(frame, "log: mac-1") || !strings.Contains(frame, "alpha two") {
		t.Errorf("side pane doesn't show mac-1's tail:\n%s", frame)
	}

	// A message that doesn't move the cursor must not restart the tail.
	updated, cmd = m.Update(nudgeMsg{})
	m = updated.(Model)
	if m.Tail.stream != oldStream {
		t.Error("a tick restarted the tail although the source didn't change")
	}
	_ = cmd

	// Down onto mac-2: switches again, and a late line from mac-1's tail
	// is dropped rather than landing in mac-2's buffer.
	updated, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(Model)
	if m.Tail.RunnerID != 2 {
		t.Fatalf("tail id = %d, want 2", m.Tail.RunnerID)
	}
	m = runTailCmd(t, m, cmd)
	updated, _ = m.Update(logLinesMsg{id: 1, stream: oldStream, lines: []string{"stale alpha"}})
	m = updated.(Model)
	if got := strings.Join(m.Tail.Lines, "|"); got != "bravo one" {
		t.Errorf("mac-2 tail lines = %q, want only bravo one", got)
	}
	m.stopTail()
}

// runTailCmd runs a start-tail Cmd (possibly batched with others) and
// feeds its logTailStartedMsg back through Update.
func runTailCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for _, msg := range flattenCmd(cmd) {
		if started, ok := msg.(logTailStartedMsg); ok {
			updated, _ := m.Update(started)
			return updated.(Model)
		}
	}
	t.Fatal("no logTailStartedMsg from the Cmd")
	return m
}

func flattenCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, flattenCmd(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// TestExplicitLogCollapsesDetailInSidePane: with 'l' open at >= 100
// columns, the side pane shows that log view instead of the detail block
// and the live tail is stopped (promoted into the view, no re-read).
func TestExplicitLogCollapsesDetailInSidePane(t *testing.T) {
	path := writeTempLog(t, "hello from mac-1")
	snap := state.Snapshot{
		Targets: []state.Target{{URL: "u", Label: "org/repo", Have: 1, Want: 1, Listed: true}},
		Runners: []state.Runner{{ID: 1, Name: "mac-1", Target: "u", State: state.RunnerIdle, LogPath: path, Version: "2.337.0"}},
	}
	m := newLogTestModel(snap)
	m.Cursor = 1
	updated, cmd := m.Update(nudgeMsg{})
	m = runTailCmd(t, updated.(Model), cmd)

	updated, cmd = m.Update(tea.KeyPressMsg{Text: "l", Code: 'l'})
	m = updated.(Model)
	if m.Log == nil || m.Tail != nil {
		t.Fatalf("after l: Log=%v Tail=%v, want the tail promoted into Log", m.Log != nil, m.Tail != nil)
	}
	if cmd != nil {
		t.Error("promoting the live tail should not start a new tail")
	}
	frame := m.renderString()
	if strings.Contains(frame, "version") {
		t.Errorf("detail block still shown with the explicit log open:\n%s", frame)
	}
	if !strings.Contains(frame, "runner mac-1") || !strings.Contains(frame, "hello from mac-1") {
		t.Errorf("side pane missing the explicit log view:\n%s", frame)
	}
	assertFrameFits(t, "explicit log", frame, m.Width, m.Height)
	m.closeLog()
}

// TestConfirmReplacesDetailArea: the confirm modal takes the detail
// block's place instead of growing the frame.
func TestConfirmReplacesDetailArea(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {80, 25}} {
		m := dashboardModel(t, size[0], size[1], loadFullSnapshotForView(t))
		m.Confirm = &ConfirmModel{Title: "Stop all runners?", Choices: []Choice{{Key: "y", Label: "Stop all", Danger: true}, {Key: "n", Label: "Cancel"}}, Default: 1}
		frame := ansi.Strip(m.renderString())
		if !strings.Contains(frame, "Stop all runners?") || !strings.Contains(frame, "[y] Stop all") {
			t.Errorf("%dx%d: confirm modal missing:\n%s", size[0], size[1], frame)
		}
		if strings.Contains(frame, "autoscale on") {
			t.Errorf("%dx%d: detail block still shown under the confirm modal:\n%s", size[0], size[1], frame)
		}
		assertFrameFits(t, "confirm", frame, size[0], size[1])
		m.stopTail()
	}
}

// TestNoUnicodeGlyphsInLayoutChrome: -no-unicode frames use the ASCII
// rules, separators, indicators and ellipsis.
func TestNoUnicodeGlyphsInLayoutChrome(t *testing.T) {
	for _, size := range [][2]int{{160, 50}, {80, 25}} {
		m := dashboardModel(t, size[0], size[1], manyRunnersSnapshot())
		m.Glyphs = NewGlyphs(true)
		m.Cursor = 30
		updated, _ := m.Update(nudgeMsg{})
		m = updated.(Model)
		frame := m.renderString()
		for _, g := range []string{"│", "─", "…", "▲", "▼", "●", "◂", "▾"} {
			if strings.Contains(frame, g) {
				t.Errorf("%dx%d: -no-unicode frame contains %q:\n%s", size[0], size[1], g, frame)
			}
		}
		m.stopTail()
	}
}
