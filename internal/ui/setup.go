package ui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// setupKey is the Inflight key of the first-run Setup call.
const setupKey = "setup"

// handleSetupKey routes keys on the first-run form: Tab/arrows switch
// field, Enter submits, ctrl+c quits, everything else goes to the focused
// textinput. There is deliberately no Esc: without a configuration there
// is nothing to go back to.
func (m Model) handleSetupKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	f := m.Setup
	switch msg.String() {
	case "ctrl+c":
		return m.requestQuit()
	case "tab", "shift+tab", "up", "down":
		f.Focused = 1 - f.Focused
		return m, f.focus()
	case "enter":
		return m.submitSetup()
	}
	var cmd tea.Cmd
	if f.Focused == 0 {
		f.Prefix, cmd = f.Prefix.Update(msg)
	} else {
		f.Max, cmd = f.Max.Update(msg)
	}
	return m, cmd
}

// submitSetup validates the form and calls Setup; an empty prefix is left
// for the engine to fill in with the host name.
func (m Model) submitSetup() (Model, tea.Cmd) {
	if _, busy := m.Inflight[setupKey]; busy {
		return m, nil
	}
	f := m.Setup
	prefix := strings.TrimSpace(f.Prefix.Value())
	if prefix != "" && !validConfigPrefix(prefix) {
		f.Err = "Invalid prefix (letters, numbers, _, - only; at most 60 characters)"
		return m, nil
	}
	max, err := strconv.Atoi(strings.TrimSpace(f.Max.Value()))
	if err != nil || max < 1 {
		f.Err = "Max runners must be a whole number, 1 or more"
		return m, nil
	}
	f.Err = ""
	m.Inflight[setupKey] = Op{Verb: "setup", Started: m.Now}
	eng := m.Engine
	return m, func() tea.Msg {
		msg, err := eng.Setup(prefix, max)
		return setupResultMsg{res: opResult{msg, err}}
	}
}

// finishSetup handles Setup's result: a failure stays on the form with the
// error under the fields; success opens the (empty) dashboard.
func (m Model) finishSetup(res opResult) Model {
	delete(m.Inflight, setupKey)
	if m.Setup == nil {
		return m
	}
	if res.err != nil {
		m.Setup.Err = oneLine(res.err.Error())
		return m
	}
	m.Setup = nil
	m.Screen = ScreenDashboard
	m.notice(oneLine(res.msg)+" - press a to add a project", LevelInfo)
	return m
}
