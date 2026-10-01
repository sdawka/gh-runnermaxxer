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
	updated, cmd := m.update(msg)
	mm, ok := updated.(Model)
	if !ok {
		return updated, cmd
	}
	return mm.afterUpdate(cmd)
}

// afterUpdate runs the bookkeeping every message needs once it has been
// handled: keeping the table's scroll window following the cursor, the
// side pane's live tail following the cursor's runner (or the event log),
// and the engine's copy of the pending edits current.
func (m Model) afterUpdate(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.Screen == ScreenDashboard || m.Screen == ScreenProjects {
		m.TableOffset = m.tableWindow(m.layout().tableH).offset
	}
	if m.SharedPending != nil {
		m.SharedPending.set(m.Pending.Changes())
	}
	m, tailCmd := m.syncTail()
	return m, tea.Batch(cmd, tailCmd)
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		m.Pending.Reconcile(m.Snap)
		return m, waitForSnapshot(m.Engine.Snapshots())

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
		return m, nil

	case applyResultMsg:
		for _, key := range msg.keys {
			delete(m.Inflight, key)
		}
		m.noticeFromResult(msg.res)
		// Pending entries are left as-is here: they clear on their own via
		// Pending.Reconcile once a later snapshot's Have actually catches
		// up (new runners take time to spin up), not the moment the exec
		// that requested them returns.
		return m, nil

	// Log messages go to whichever pane they were started for: the explicit
	// 'l'/'L' view (m.Log) or the side pane's live tail (m.Tail). Anything
	// else is stale - that pane was switched or closed before it arrived.
	case logTailStartedMsg:
		switch {
		case m.Log.matches(msg.id, msg.stream):
			m.logUpdates = msg.updates
			m.Log.setLines(msg.lines)
		case m.Tail.matches(msg.id, msg.stream):
			m.tailUpdates = msg.updates
			m.Tail.setLines(msg.lines)
		default:
			return m, nil
		}
		return m, listenLogUpdates(msg.id, msg.stream, msg.updates)

	case logLinesMsg:
		pane, updates := m.Log, m.logUpdates
		if !m.Log.matches(msg.id, msg.stream) {
			if !m.Tail.matches(msg.id, msg.stream) {
				return m, nil
			}
			pane, updates = m.Tail, m.tailUpdates
		}
		if msg.reset {
			pane.setLines(msg.lines)
		} else {
			pane.appendLines(msg.lines)
		}
		return m, listenLogUpdates(msg.id, msg.stream, updates)

	case logErrMsg:
		switch {
		case m.Log.matches(msg.id, msg.stream):
			m.Log.Err = msg.err
		case m.Tail.matches(msg.id, msg.stream):
			m.Tail.Err = msg.err
		}
		return m, nil

	case setupResultMsg:
		return m.finishSetup(msg.res), nil

	case shutdownDoneMsg:
		return m, tea.Quit

	case ghStatusMsg:
		text := msg.text
		if msg.err != nil {
			if text != "" {
				text += "\n\n"
			}
			text += msg.err.Error()
		}
		m.GHStatusText = &text
		m.Screen = ScreenGHStatus
		return m, nil

	case tea.KeyPressMsg:
		if m.Quit != quitNone {
			return m.handleQuittingKey(msg)
		}
		if m.Confirm != nil {
			return m.handleConfirmKey(msg)
		}
		if m.Log != nil {
			if handled, updated, cmd := m.handleLogKey(msg); handled {
				return updated, cmd
			}
		}
		switch m.Screen {
		case ScreenSetup:
			return m.handleSetupKey(msg)
		case ScreenAddTarget:
			return m.handleAddTargetKey(msg)
		case ScreenBounds:
			return m.handleBoundsKey(msg)
		case ScreenConfig:
			return m.handleConfigKey(msg)
		case ScreenGHStatus:
			// §3.4: "q/Esc/any key closes" the gh-status modal.
			return m.closeGHStatus()
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	action := keyToAction(m.Screen, msg.String(), m.cursorOnHeaderRow())

	switch action.Kind {
	case ActionQuit:
		return m.requestQuit()

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

	case ActionCheckGH:
		return m.openGHStatus()

	case ActionDownload:
		return m.execDownload()

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

	case ActionEventLog:
		if m.Log != nil && m.Log.RunnerID == eventLogID {
			return m.closeLog(), nil
		}
		return m.openEventLog()
	}

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
