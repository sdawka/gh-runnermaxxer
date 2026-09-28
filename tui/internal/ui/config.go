package ui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

// configItem indexes the six items edit_config offers (Go design doc
// §3.4). configItemProjects just switches screens; the three toggles apply
// immediately; the other two open a one-field textinput first.
type configItem int

const (
	configItemProjects configItem = iota
	configItemPrefix
	configItemSharedToolCache
	configItemEphemeral
	configItemAutoscale
	configItemIdleMinutes
)

var configItemLabels = [...]string{
	"Projects & runner counts (same as 't')",
	"Runner name prefix",
	"Toggle shared tool cache",
	"Toggle ephemeral runners for new runners",
	"Toggle autoscale for projects with bounds",
	"Autoscale idle minutes before scaling down",
}

// ConfigModel is the 'e' config screen: a 6-item list; Enter on a toggle
// applies immediately, Enter on prefix/idle-minutes opens Input first.
type ConfigModel struct {
	Cursor  int
	Editing bool
	Field   configItem
	Input   textinput.Model
}

func newConfigModel() *ConfigModel {
	return &ConfigModel{}
}

// configPrefixPattern mirrors valid_prefix exactly: letters, digits,
// underscore, hyphen, 1-60 characters.
func validConfigPrefix(s string) bool {
	if s == "" || len(s) > 60 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (c *ConfigModel) view(cfg state.Config) string {
	var b strings.Builder
	b.WriteString("Configuration\n\n")
	if c.Editing {
		switch c.Field {
		case configItemPrefix:
			fmt.Fprintf(&b, "  Runner name prefix (applies to runners added from now on, current: %s):\n\n  %s\n\n  Enter save   Esc cancel", cfg.RunnerNamePrefix, c.Input.View())
		case configItemIdleMinutes:
			fmt.Fprintf(&b, "  Idle minutes before an autoscaled project shrinks by one (current: %d):\n\n  %s\n\n  Enter save   Esc cancel", cfg.AutoscaleIdleMinutes, c.Input.View())
		}
		return b.String()
	}
	for i, label := range configItemLabels {
		marker := " "
		if i == c.Cursor {
			marker = ">"
		}
		fmt.Fprintf(&b, "  %s %d) %s%s\n", marker, i+1, label, configCurrentValue(configItem(i), cfg))
	}
	b.WriteString("\n  ↑↓ move   Enter select   q/Esc back")
	return b.String()
}

func configCurrentValue(item configItem, cfg state.Config) string {
	switch item {
	case configItemPrefix:
		return fmt.Sprintf(" (current: %s)", cfg.RunnerNamePrefix)
	case configItemSharedToolCache:
		return fmt.Sprintf(" (current: %s)", onOff(cfg.SharedToolCache))
	case configItemEphemeral:
		return fmt.Sprintf(" (current: %s)", onOff(cfg.EphemeralRunners))
	case configItemAutoscale:
		return fmt.Sprintf(" (current: %s)", onOff(cfg.Autoscale))
	case configItemIdleMinutes:
		return fmt.Sprintf(" (current: %d)", cfg.AutoscaleIdleMinutes)
	default:
		return ""
	}
}

func onOff(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// newPrefixInput/newIdleInput build the one-field textinput for their
// respective configItem, prefilled from the snapshot's current value.
func newPrefixInput(cfg state.Config) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "letters, digits, _ and - only, max 60 chars"
	ti.CharLimit = 60
	ti.SetWidth(60)
	ti.SetValue(cfg.RunnerNamePrefix)
	return ti
}

func newIdleInput(cfg state.Config) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "minutes"
	ti.CharLimit = 6
	ti.SetWidth(10)
	ti.SetValue(strconv.Itoa(cfg.AutoscaleIdleMinutes))
	return ti
}
