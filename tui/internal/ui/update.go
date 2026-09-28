package ui

import (
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// Update implements tea.Model. Dashboard/projects-specific rendering lands
// in later commits (Go plan §8, commits 7-10); this commit wires the
// message lifecycle (snapshot updates, the clock, resize) and the
// pending-count keys that don't need a rendered table to make sense.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		m.ShowLog = m.Width >= 100
		m.Compact = m.Height < 30
		return m, nil

	case tickMsg:
		m.Now = time.Time(msg)
		m.Notices = expireNotices(m.Notices, m.Now)
		return m, tickEvery(time.Second)

	case snapshotMsg:
		m.Snap = msg.snap
		m.SnapErr = nil
		m.Pending.Reconcile(m.Snap)
		m.DaemonUp = m.Snap.DaemonPID != 0
		return m, watchSnapshot(m.watcherEvents)

	case snapshotErrMsg:
		m.SnapErr = msg.err
		return m, watchSnapshot(m.watcherEvents)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.Spinner, cmd = m.Spinner.Update(msg)
		return m, cmd

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	action := keyToAction(m.Screen, msg.String(), m.cursorOnHeaderRow())

	switch action.Kind {
	case ActionQuit:
		// The confirm-before-quit modal (pending edits / in-flight ops,
		// §4.5) is added in commit 8 alongside the rest of the confirm
		// modal machinery; for now a quit with pending edits just discards
		// them rather than losing the keypress silently.
		if !m.Pending.Empty() {
			m.Pending.Clear()
		}
		return m, tea.Quit

	case ActionHelp:
		m.Help = !m.Help
		return m, nil

	case ActionUp:
		if m.Cursor > 0 {
			m.Cursor--
		}
		return m, nil

	case ActionDown:
		m.Cursor++
		return m, nil

	case ActionInc:
		m.adjustCursorTarget(1)
		return m, nil

	case ActionDec:
		m.adjustCursorTarget(-1)
		return m, nil

	case ActionSetCount:
		m.setCursorTarget(action.N)
		return m, nil

	case ActionDiscardOrBack:
		if !m.Pending.Empty() {
			m.Pending.Clear()
			m.notice("Pending changes discarded", LevelWarn)
		}
		return m, nil

	case ActionFilter:
		m.Filtering = !m.Filtering
		return m, nil
	}

	// ActionApply, ActionDrain*, ActionStop*, ActionStart*, ActionRestart*,
	// ActionProjects, ActionAddTarget, ActionLogs, ActionDaemonLog,
	// ActionCheckGH, ActionConfig, ActionDownload, ActionBounds and
	// ActionRemoveFromList are handled once the screens/exec wiring that
	// give them meaning land (commits 7-11).
	return m, nil
}

// cursorOnHeaderRow reports whether the cursor is on a target's header row
// as opposed to one of its runner rows. This is a placeholder over the flat
// Targets slice; it is superseded by the full rows() flattening (targets +
// runners + the Unconfigured pseudo-group) that the dashboard table adds in
// commit 7.
func (m Model) cursorOnHeaderRow() bool {
	return m.Cursor >= 0 && m.Cursor < len(m.Snap.Targets)
}

func (m Model) currentTargetURL() string {
	if m.Cursor >= 0 && m.Cursor < len(m.Snap.Targets) {
		return m.Snap.Targets[m.Cursor].URL
	}
	return ""
}

func (m *Model) adjustCursorTarget(delta int) {
	url := m.currentTargetURL()
	if url == "" {
		return
	}
	if ok, msg := m.Pending.Adjust(m.Snap, url, delta); !ok {
		m.notice(msg, LevelWarn)
	}
}

func (m *Model) setCursorTarget(n int) {
	url := m.currentTargetURL()
	if url == "" {
		return
	}
	if ok, msg := m.Pending.Set(m.Snap, url, n); !ok {
		m.notice(msg, LevelWarn)
	}
}

func (m *Model) notice(text string, level Level) {
	m.Notices = append(m.Notices, Notice{Text: text, Level: level, Until: m.Now.Add(4 * time.Second)})
}

func expireNotices(notices []Notice, now time.Time) []Notice {
	out := notices[:0]
	for _, n := range notices {
		if n.Until.IsZero() || n.Until.After(now) {
			out = append(out, n)
		}
	}
	return out
}
