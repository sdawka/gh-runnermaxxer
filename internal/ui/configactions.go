package ui

import (
	"fmt"
	"strconv"

	tea "charm.land/bubbletea/v2"
)

// configKey is the Inflight key every SetConfig call shares: the config
// screen only ever changes one field at a time, so there is never a reason
// for two to run concurrently.
const configKey = "config"

// openConfigScreen implements 'e': opens the 6-item config list (§3.4).
func (m Model) openConfigScreen() (Model, tea.Cmd) {
	m.Screen = ScreenConfig
	m.Config = newConfigModel()
	return m, nil
}

func (m Model) closeConfigScreen() Model {
	m.Config = nil
	m.Screen = ScreenDashboard
	return m
}

// handleConfigKey routes keys on the config screen: list navigation/select
// when not editing, or the active textinput plus Enter/Esc when editing a
// field (prefix, idle minutes).
func (m Model) handleConfigKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.Config.Editing {
		switch msg.String() {
		case "esc":
			m.Config.Editing = false
			return m, nil
		case "enter":
			return m.submitConfigField()
		}
		var cmd tea.Cmd
		m.Config.Input, cmd = m.Config.Input.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "up", "k":
		if m.Config.Cursor > 0 {
			m.Config.Cursor--
		}
		return m, nil
	case "down", "j":
		if m.Config.Cursor < len(configItemLabels)-1 {
			m.Config.Cursor++
		}
		return m, nil
	case "1", "2", "3", "4", "5", "6":
		m.Config.Cursor = int(msg.Text[0] - '1')
		return m.selectConfigItem()
	case "enter":
		return m.selectConfigItem()
	case "q", "esc":
		return m.closeConfigScreen(), nil
	}
	return m, nil
}

// selectConfigItem runs the item under the cursor: item 0 jumps to the
// projects screen, the three toggles apply immediately, and the other two
// open a one-field textinput first (§3.4, mirroring edit_config's cases).
func (m Model) selectConfigItem() (tea.Model, tea.Cmd) {
	switch configItem(m.Config.Cursor) {
	case configItemProjects:
		m.Config = nil
		m.Screen = ScreenProjects
		return m, nil
	case configItemPrefix:
		m.Config.Editing = true
		m.Config.Field = configItemPrefix
		m.Config.Input = newPrefixInput(m.Snap.Config)
		return m, m.Config.Input.Focus()
	case configItemSharedToolCache:
		return m.toggleConfigBool("SHARED_TOOL_CACHE", !m.Snap.Config.SharedToolCache,
			"Shared tool cache: %s (takes effect as runners restart)")
	case configItemEphemeral:
		return m.toggleConfigBool("EPHEMERAL_RUNNERS", !m.Snap.Config.EphemeralRunners,
			"Ephemeral runners: %s (runners added from now on; remove and re-add existing ones to switch)")
	case configItemAutoscale:
		return m.toggleConfigBool("AUTOSCALE", !m.Snap.Config.Autoscale,
			"Autoscale: %s (only projects with bounds - set them with 'b' in the projects screen)")
	case configItemIdleMinutes:
		m.Config.Editing = true
		m.Config.Field = configItemIdleMinutes
		m.Config.Input = newIdleInput(m.Snap.Config)
		return m, m.Config.Input.Focus()
	}
	return m, nil
}

func (m Model) toggleConfigBool(key string, newVal bool, noteFmt string) (Model, tea.Cmd) {
	v := "0"
	if newVal {
		v = "1"
	}
	m.notice(fmt.Sprintf(noteFmt, v), LevelInfo)
	return m.execSetConfig(key, v)
}

// submitConfigField validates and submits the field currently being
// edited (prefix or idle minutes), matching edit_config's own validation
// messages, then closes the input back to the list.
func (m Model) submitConfigField() (tea.Model, tea.Cmd) {
	value := m.Config.Input.Value()
	field := m.Config.Field
	m.Config.Editing = false

	switch field {
	case configItemPrefix:
		if value == "" {
			return m, nil // edit_config: empty input leaves the prefix unchanged
		}
		if !validConfigPrefix(value) {
			m.notice("Invalid prefix (letters, numbers, _, - only; at most 60 characters)", LevelWarn)
			return m, nil
		}
		return m.execSetConfig("RUNNER_NAME_PREFIX", value)
	case configItemIdleMinutes:
		if value == "" {
			return m, nil
		}
		if _, err := strconv.Atoi(value); err != nil {
			m.notice("Expected a whole number of minutes", LevelWarn)
			return m, nil
		}
		return m.execSetConfig("AUTOSCALE_IDLE_MINUTES", value)
	}
	return m, nil
}

func (m Model) execSetConfig(key, value string) (Model, tea.Cmd) {
	op := Op{Verb: "set-config", Args: []string{key + "=" + value}, Started: m.Now}
	eng := m.Engine
	return m.startOp(configKey, op, func() (string, error) { return eng.SetConfig(key, value) })
}
