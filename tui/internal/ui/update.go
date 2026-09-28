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

	case cmdStartedMsg:
		m.Inflight[msg.key] = msg.op
		return m, nil

	case cmdResultMsg:
		delete(m.Inflight, msg.key)
		m.noticeFromResult(msg.res)
		// Reload immediately rather than waiting for the next daemon tick
		// or watcher event, so the mutation's effect shows up right away.
		return m, pollCLI(m.ctx, m.Client)

	case applyResultMsg:
		for _, key := range msg.keys {
			delete(m.Inflight, key)
		}
		m.noticeFromResult(msg.res)
		// Pending entries are left as-is here: they clear on their own via
		// Pending.Reconcile once a later snapshot's Have actually catches
		// up (new runners take time to spin up), not the moment the exec
		// that requested them returns.
		return m, pollCLI(m.ctx, m.Client)

	case tea.KeyPressMsg:
		if m.Confirm != nil {
			return m.handleConfirmKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	action := keyToAction(m.Screen, msg.String(), m.cursorOnHeaderRow())

	switch action.Kind {
	case ActionQuit:
		// §4.5: confirm before quitting with pending edits or in-flight ops
		// rather than losing them silently.
		if !m.Pending.Empty() || len(m.Inflight) > 0 {
			m.Confirm = &ConfirmModel{
				Title: "Discard pending changes and quit?",
				Choices: []Choice{
					{Key: "y", Label: "Quit", Danger: true},
					{Key: "n", Label: "Cancel"},
				},
				Default: 1,
				OnChoice: func(mm Model, choice string) (Model, tea.Cmd) {
					if choice == "y" {
						return mm, tea.Quit
					}
					return mm, nil
				},
			}
			return m, nil
		}
		return m, tea.Quit

	case ActionDrain:
		id, ok := m.currentRunnerID()
		if !ok {
			m.notice("select a runner", LevelWarn)
			return m, nil
		}
		return m.guardBusy(id,
			func(mm Model) (Model, tea.Cmd) { return mm.execDrain(id) },
			func(mm Model) (Model, tea.Cmd) { return mm.execRemove(id) },
		)

	case ActionDrainPicker:
		return m.confirmDrainPicker()

	case ActionRemoveNow:
		id, ok := m.currentRunnerID()
		if !ok {
			m.notice("select a runner", LevelWarn)
			return m, nil
		}
		return m.confirmRemoveNow(id)

	case ActionStop:
		id, ok := m.currentRunnerID()
		if !ok {
			m.notice("select a runner", LevelWarn)
			return m, nil
		}
		return m.guardBusy(id,
			func(mm Model) (Model, tea.Cmd) { return mm.execStop(id) },
			func(mm Model) (Model, tea.Cmd) { return mm.execStop(id) },
		)

	case ActionStart:
		id, ok := m.currentRunnerID()
		if !ok {
			m.notice("select a runner", LevelWarn)
			return m, nil
		}
		return m.execStart(id)

	case ActionRestart:
		id, ok := m.currentRunnerID()
		if !ok {
			m.notice("select a runner", LevelWarn)
			return m, nil
		}
		return m.guardBusy(id,
			func(mm Model) (Model, tea.Cmd) { return mm.execRestart(id) },
			func(mm Model) (Model, tea.Cmd) { return mm.execRestart(id) },
		)

	case ActionStopAll:
		return m.confirmAllVariant("stop", func(mm Model) (Model, tea.Cmd) { return mm.execStopAll() })

	case ActionStartAll:
		return m.execStartAll()

	case ActionRestartAll:
		return m.confirmAllVariant("restart", func(mm Model) (Model, tea.Cmd) { return mm.execRestartAll() })

	case ActionApply:
		return m.applyPending()

	case ActionHelp:
		m.Help = !m.Help
		return m, nil

	case ActionUp:
		if m.Cursor > 0 {
			m.Cursor--
		}
		return m, nil

	case ActionDown:
		if m.Cursor < len(m.rows())-1 {
			m.Cursor++
		}
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

	// ActionProjects, ActionAddTarget, ActionLogs, ActionDaemonLog,
	// ActionCheckGH, ActionConfig, ActionDownload, ActionBounds and
	// ActionRemoveFromList are handled once the screens/exec wiring that
	// give them meaning land (commits 10-11).
	return m, nil
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
