package ui

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// handleLogKey intercepts keys meant for an open LogPane before the normal
// dashboard/projects dispatch sees them: Esc/q close it, f toggles
// follow-tail, n/p move to the next/previous runner's log (meaningless for
// the daemon log, so left to fall through), and - only in full-screen mode,
// since the side pane leaves the table as the primary focus - the
// navigation keys scroll the viewport instead of moving the cursor. handled
// is false when the key wasn't claimed and should go through the normal
// path (e.g. digits, or n/p on the daemon log).
func (m Model) handleLogKey(msg tea.KeyPressMsg) (handled bool, updated Model, cmd tea.Cmd) {
	switch msg.String() {
	case "esc":
		return true, m.closeLog(), nil
	case "f":
		m.Log.Follow = !m.Log.Follow
		if m.Log.Follow {
			m.Log.Viewport.GotoBottom()
		}
		return true, m, nil
	case "n":
		if m.Log.RunnerID == daemonLogID {
			return false, m, nil
		}
		mm, c := m.navigateLog(true)
		return true, mm, c
	case "p":
		if m.Log.RunnerID == daemonLogID {
			return false, m, nil
		}
		mm, c := m.navigateLog(false)
		return true, mm, c
	}

	if !m.ShowLog {
		// Full-screen: the log view owns scrolling/quit since there's no
		// table visible to move a cursor over.
		switch msg.String() {
		case "up", "k":
			m.Log.Viewport.ScrollUp(1)
			return true, m, nil
		case "down", "j":
			m.Log.Viewport.ScrollDown(1)
			return true, m, nil
		case "pgup":
			m.Log.Viewport.PageUp()
			return true, m, nil
		case "pgdown":
			m.Log.Viewport.PageDown()
			return true, m, nil
		case "q":
			return true, m.closeLog(), nil
		}
	}
	return false, m, nil
}

// closeLog cancels the tail goroutine (if any) and drops the pane.
func (m Model) closeLog() Model {
	if m.logCancel != nil {
		m.logCancel()
	}
	m.Log = nil
	m.logCancel = nil
	m.logUpdates = nil
	return m
}

// openRunnerLog starts tailing a runner's log file, replacing whatever log
// view (if any) was previously open. An empty path (the runner hasn't
// produced a log yet) is a toast, not an empty pane.
func (m Model) openRunnerLog(id int, name, path string) (Model, tea.Cmd) {
	if path == "" {
		if m.logCancel != nil {
			m.logCancel()
		}
		m.Log = nil
		m.logCancel = nil
		m.logUpdates = nil
		m.notice("no log file yet for "+orDefault(name, "that runner"), LevelWarn)
		return m, nil
	}
	return m.openLog("runner "+name, id, path)
}

// openDaemonLog starts tailing the daemon's event log (runnermaxxer.sh's
// DAEMON_LOG, README's 'L').
func (m Model) openDaemonLog() (Model, tea.Cmd) {
	return m.openLog("daemon log", daemonLogID, m.Paths.DaemonLog)
}

// openLog opens the explicit log view on path. When the side pane's live
// tail is already following that same file it is promoted into the view
// as-is (buffer, goroutine and all) instead of re-reading the file.
func (m Model) openLog(title string, id int, path string) (Model, tea.Cmd) {
	if m.logCancel != nil {
		m.logCancel()
	}
	if t := m.Tail; t != nil && t.RunnerID == id && t.path == path && t.stream > 0 {
		t.Title = title
		t.resize(m.Width, m.Height)
		m.Log, m.logCancel, m.logUpdates = t, m.tailCancel, m.tailUpdates
		m.Tail, m.tailCancel, m.tailUpdates = nil, nil, nil
		return m, nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.logCancel = cancel
	m.logUpdates = nil
	m.logSeq++
	m.Log = newLogPane(title, id, m.Width, m.Height)
	m.Log.stream, m.Log.path = m.logSeq, path
	return m, startLogTail(ctx, id, m.Log.stream, path)
}

// tailSource is what the side pane's live tail should follow for the
// current cursor: the runner's own log on a runner row, the daemon log on
// a target/Unconfigured header row (or with no rows at all).
func (m Model) tailSource() (id int, path, title string) {
	if row, ok := m.cursorRow(); ok && row.Kind == RowRunner {
		return row.Runner.ID, row.Runner.LogPath, "log: " + row.Runner.Name
	}
	return daemonLogID, m.Paths.DaemonLog, "daemon log"
}

// syncTail points the live tail at tailSource, starting a new logtail only
// when the source actually changed (so a snapshot tick or a keypress that
// doesn't move the cursor never re-reads the file). The tail is stopped
// while an explicit log view is open, and left alone on the form/config
// screens. Nothing starts before the first WindowSizeMsg.
func (m Model) syncTail() (Model, tea.Cmd) {
	if m.Width <= 0 || m.ctx == nil {
		return m, nil
	}
	if m.Screen != ScreenDashboard && m.Screen != ScreenProjects {
		return m, nil
	}
	if m.Log != nil {
		return m.stopTail(), nil
	}
	id, path, title := m.tailSource()
	if m.Tail != nil && m.Tail.RunnerID == id && m.Tail.path == path {
		m.Tail.Title = title
		return m, nil
	}
	m = m.stopTail()
	m.Tail = newLogPane(title, id, 0, 0)
	m.Tail.path = path
	m.Tail.stream = -1 // no tail running: matches no message
	if path == "" {
		return m, nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.tailCancel = cancel
	m.logSeq++
	m.Tail.stream = m.logSeq
	return m, startLogTail(ctx, id, m.Tail.stream, path)
}

// stopTail cancels the live tail's goroutine (if any) and drops it.
func (m Model) stopTail() Model {
	if m.tailCancel != nil {
		m.tailCancel()
	}
	m.Tail, m.tailCancel, m.tailUpdates = nil, nil, nil
	return m
}

// navigateLog switches the open runner log to the next (or, with next
// false, previous) runner in the snapshot's listed order, wrapping around.
func (m Model) navigateLog(next bool) (Model, tea.Cmd) {
	ids := allRunnerIDs(m.Snap)
	if len(ids) == 0 {
		return m, nil
	}
	idx := indexOfInt(ids, m.Log.RunnerID)
	switch {
	case idx < 0:
		idx = 0
	case next:
		idx = (idx + 1) % len(ids)
	default:
		idx = (idx - 1 + len(ids)) % len(ids)
	}
	id := ids[idx]
	r, ok := m.Snap.RunnerByID(id)
	if !ok {
		return m, nil
	}
	return m.openRunnerLog(id, r.Name, r.LogPath)
}

func indexOfInt(xs []int, v int) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}
