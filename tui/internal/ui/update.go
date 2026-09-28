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

	case logTailStartedMsg:
		if m.Log == nil || m.Log.RunnerID != msg.id {
			return m, nil // stale: the pane was switched or closed before this arrived
		}
		m.logUpdates = msg.updates
		m.Log.setLines(msg.lines)
		return m, listenLogUpdates(msg.id, msg.updates)

	case logLinesMsg:
		if m.Log == nil || m.Log.RunnerID != msg.id {
			return m, nil
		}
		if msg.reset {
			m.Log.setLines(msg.lines)
		} else {
			m.Log.appendLines(msg.lines)
		}
		return m, listenLogUpdates(msg.id, m.logUpdates)

	case logErrMsg:
		if m.Log != nil && m.Log.RunnerID == msg.id {
			m.Log.Err = msg.err
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.Confirm != nil {
			return m.handleConfirmKey(msg)
		}
		if m.Log != nil {
			if handled, updated, cmd := m.handleLogKey(msg); handled {
				return updated, cmd
			}
		}
		switch m.Screen {
		case ScreenAddTarget:
			return m.handleAddTargetKey(msg)
		case ScreenBounds:
			return m.handleBoundsKey(msg)
		case ScreenConfig:
			return m.handleConfigKey(msg)
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
		mm, cmd := m.applyPending()
		if m.Screen == ScreenProjects {
			mm.Screen = ScreenDashboard
		}
		return mm, cmd

	case ActionProjects:
		m.Screen = ScreenProjects
		return m, nil

	case ActionAddTarget:
		return m.openAddTargetForm()

	case ActionBounds:
		return m.openBoundsForm()

	case ActionRemoveFromList:
		return m.removeFromList()

	case ActionConfig:
		return m.openConfigScreen()

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
		// Projects screen: 'q'/Esc discard silently and return to the
		// dashboard (§3.4: "bash discards silently on q ... keep that").
		// Dashboard: Esc discards with a toast; there's nowhere to "back" to.
		if m.Screen == ScreenProjects {
			m.Pending.Clear()
			m.Screen = ScreenDashboard
			return m, nil
		}
		if !m.Pending.Empty() {
			m.Pending.Clear()
			m.notice("Pending changes discarded", LevelWarn)
		}
		return m, nil

	case ActionFilter:
		m.Filtering = !m.Filtering
		return m, nil

	case ActionLogs:
		id, ok := m.currentRunnerID()
		if !ok {
			m.notice("select a runner", LevelWarn)
			return m, nil
		}
		if m.Log != nil && m.Log.RunnerID == id {
			return m.closeLog(), nil
		}
		r, _ := m.Snap.RunnerByID(id)
		return m.openRunnerLog(id, r.Name, r.LogPath)

	case ActionDaemonLog:
		if m.Log != nil && m.Log.RunnerID == daemonLogID {
			return m.closeLog(), nil
		}
		return m.openDaemonLog()
	}

	// ActionCheckGH and ActionDownload are handled once the screens/exec
	// wiring that give them meaning land (commit 13).
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
