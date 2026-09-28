package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
)

// openAddTargetForm opens the add-target form, reachable from either the
// dashboard or the projects screen (both map 'a' to ActionAddTarget),
// remembering which one to return to once it closes.
func (m Model) openAddTargetForm() (Model, tea.Cmd) {
	m.formReturn = m.Screen
	m.Screen = ScreenAddTarget
	f := newAddTargetForm()
	cmd := f.Input.Focus()
	m.AddTarget = f
	return m, cmd
}

func (m Model) closeAddTarget() Model {
	m.AddTarget = nil
	m.Screen = m.formReturn
	return m
}

// handleAddTargetKey routes keys while the add-target form is open: Esc
// cancels, Enter submits (an empty entry silently cancels, matching
// menu_add_target's bare `read` with no input), everything else goes to the
// textinput.
func (m Model) handleAddTargetKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m.closeAddTarget(), nil
	case "enter":
		entry := strings.TrimSpace(m.AddTarget.Input.Value())
		m = m.closeAddTarget()
		if entry == "" {
			return m, nil
		}
		return m.execAddTarget(entry)
	}
	var cmd tea.Cmd
	m.AddTarget.Input, cmd = m.AddTarget.Input.Update(msg)
	return m, cmd
}

func (m Model) execAddTarget(entry string) (Model, tea.Cmd) {
	key := "addtarget:" + entry
	op := Op{Verb: "add-target", Args: []string{entry}, Started: m.Now}
	client := m.Client
	return m.startOp(key, op, func(ctx context.Context) cli.Result { return client.AddTarget(ctx, entry) })
}

// openBoundsForm opens the bounds form for the target under the cursor
// (projects screen 'b'); no target under the cursor is a toast, not a
// no-op crash.
func (m Model) openBoundsForm() (Model, tea.Cmd) {
	url := m.currentTargetURL()
	if url == "" {
		m.notice("select a target", LevelWarn)
		return m, nil
	}
	t, ok := m.Snap.TargetByURL(url)
	if !ok {
		m.notice("select a target", LevelWarn)
		return m, nil
	}
	m.formReturn = m.Screen
	m.Screen = ScreenBounds
	f := newBoundsForm(t)
	cmd := f.Min.Focus()
	m.Bounds = f
	return m, cmd
}

func (m Model) closeBounds() Model {
	m.Bounds = nil
	m.Screen = m.formReturn
	return m
}

// handleBoundsKey routes keys while the bounds form is open: Esc cancels,
// Tab switches the focused field, Enter submits, everything else goes to
// whichever textinput is focused.
func (m Model) handleBoundsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m.closeBounds(), nil
	case "tab":
		f := m.Bounds
		if f.Focused == 0 {
			f.Min.Blur()
			f.Focused = 1
			return m, f.Max.Focus()
		}
		f.Max.Blur()
		f.Focused = 0
		return m, f.Min.Focus()
	case "enter":
		return m.submitBounds()
	}
	var cmd tea.Cmd
	f := m.Bounds
	if f.Focused == 0 {
		f.Min, cmd = f.Min.Update(msg)
	} else {
		f.Max, cmd = f.Max.Update(msg)
	}
	return m, cmd
}

// submitBounds validates client-side (both empty clears; otherwise two
// non-negative integers with min <= max) before exec-ing --set-bounds, so a
// malformed entry gets a friendly toast instead of the script's usage
// error. The MAX_RUNNERS-exceeds check is left to the script itself
// (cli_set_bounds already reports it), since that check needs the
// snapshot's live MaxRunners and the exec's own error surfaces the same
// way through noticeFromResult.
func (m Model) submitBounds() (Model, tea.Cmd) {
	f := m.Bounds
	minStr := strings.TrimSpace(f.Min.Value())
	maxStr := strings.TrimSpace(f.Max.Value())
	url, label := f.URL, f.Label

	if minStr == "" && maxStr == "" {
		m = m.closeBounds()
		return m.execSetBounds(url, 0, 0)
	}

	min, errMin := strconv.Atoi(minStr)
	max, errMax := strconv.Atoi(maxStr)
	if errMin != nil || errMax != nil || min < 0 || min > max {
		m.notice(fmt.Sprintf("invalid bounds for %s: expected two numbers, min <= max", label), LevelWarn)
		return m, nil
	}

	m = m.closeBounds()
	return m.execSetBounds(url, min, max)
}

func (m Model) execSetBounds(url string, min, max int) (Model, tea.Cmd) {
	key := targetKey(url)
	op := Op{Verb: "set-bounds", Args: []string{url}, Started: m.Now}
	client := m.Client
	return m.startOp(key, op, func(ctx context.Context) cli.Result { return client.SetBounds(ctx, url, min, max) })
}

// removeFromList implements 'x' in the projects screen: only removes the
// target under the cursor when it has no runners left and is actually in
// the targets file, else toasts the same messages menu_remove_target
// prints instead of exec-ing.
func (m Model) removeFromList() (Model, tea.Cmd) {
	url := m.currentTargetURL()
	if url == "" {
		m.notice("select a target", LevelWarn)
		return m, nil
	}
	t, ok := m.Snap.TargetByURL(url)
	if !ok {
		m.notice("select a target", LevelWarn)
		return m, nil
	}
	if t.Have > 0 {
		m.notice(fmt.Sprintf("%s still has %d runner(s) - set it to 0 and apply ('s') first", t.Label, t.Have), LevelWarn)
		return m, nil
	}
	if !t.Listed {
		m.notice(fmt.Sprintf("%s is not in the targets file", t.Label), LevelInfo)
		return m, nil
	}
	key := targetKey(url)
	op := Op{Verb: "remove-target", Args: []string{url}, Started: m.Now}
	client := m.Client
	return m.startOp(key, op, func(ctx context.Context) cli.Result { return client.RemoveTarget(ctx, url) })
}
