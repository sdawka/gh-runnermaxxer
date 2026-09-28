package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

func TestActionConfigOpensConfigScreen(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "e"})
	got := updated.(Model)
	if got.Screen != ScreenConfig || got.Config == nil {
		t.Fatalf("Screen = %v, Config = %v, want ScreenConfig open", got.Screen, got.Config)
	}
}

func TestConfigCursorMovesWithinBounds(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openConfigScreen())

	updated, _ := m.handleConfigKey(tea.KeyPressMsg{Text: "down"})
	got := updated.(Model)
	if got.Config.Cursor != 1 {
		t.Fatalf("Cursor = %d, want 1 after one down", got.Config.Cursor)
	}

	updated, _ = got.handleConfigKey(tea.KeyPressMsg{Text: "up"})
	got = updated.(Model)
	if got.Config.Cursor != 0 {
		t.Fatalf("Cursor = %d, want 0 after up", got.Config.Cursor)
	}
}

func TestConfigItemProjectsJumpsToProjectsScreen(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openConfigScreen())
	m.Config.Cursor = int(configItemProjects)

	updated, _ := m.handleConfigKey(tea.KeyPressMsg{Text: "enter"})
	got := updated.(Model)
	if got.Screen != ScreenProjects {
		t.Fatalf("Screen = %v, want ScreenProjects", got.Screen)
	}
	if got.Config != nil {
		t.Fatal("Config model should be cleared when jumping to projects")
	}
}

func TestConfigToggleSharedToolCacheExecsSetConfig(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Config: state.Config{SharedToolCache: false}}
	m := newActionsTestModel(f, snap)
	m, _ = mustModel(m.openConfigScreen())
	m.Config.Cursor = int(configItemSharedToolCache)

	_, cmd := m.handleConfigKey(tea.KeyPressMsg{Text: "enter"})
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--set-config" || calls[0][1] != "SHARED_TOOL_CACHE=1" {
		t.Fatalf("calls = %v, want one --set-config SHARED_TOOL_CACHE=1 exec", calls)
	}
}

func TestConfigToggleAutoscaleFlipsOffWhenCurrentlyOn(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Config: state.Config{Autoscale: true}}
	m := newActionsTestModel(f, snap)
	m, _ = mustModel(m.openConfigScreen())
	m.Config.Cursor = int(configItemAutoscale)

	_, cmd := m.handleConfigKey(tea.KeyPressMsg{Text: "enter"})
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][1] != "AUTOSCALE=0" {
		t.Fatalf("calls = %v, want one --set-config AUTOSCALE=0 exec", calls)
	}
}

func TestConfigPrefixEntersEditingAndValidates(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openConfigScreen())
	m.Config.Cursor = int(configItemPrefix)

	updated, _ := m.handleConfigKey(tea.KeyPressMsg{Text: "enter"})
	got := updated.(Model)
	if !got.Config.Editing || got.Config.Field != configItemPrefix {
		t.Fatalf("Editing = %v, Field = %v, want editing configItemPrefix", got.Config.Editing, got.Config.Field)
	}

	got.Config.Input.SetValue("bad prefix!")
	updated2, cmd := got.handleConfigKey(tea.KeyPressMsg{Text: "enter"})
	got2 := updated2.(Model)
	if cmd != nil {
		t.Fatal("invalid prefix must not exec")
	}
	if len(got2.Notices) == 0 {
		t.Fatal("expected a validation toast for an invalid prefix")
	}
}

func TestConfigPrefixValidValueExecsSetConfig(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openConfigScreen())
	m.Config.Cursor = int(configItemPrefix)
	m, _ = mustModel(m.selectConfigItemForTest())
	m.Config.Input.SetValue("gh-ci")

	_, cmd := m.handleConfigKey(tea.KeyPressMsg{Text: "enter"})
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--set-config" || calls[0][1] != "RUNNER_NAME_PREFIX=gh-ci" {
		t.Fatalf("calls = %v, want one --set-config RUNNER_NAME_PREFIX=gh-ci exec", calls)
	}
}

func TestConfigIdleMinutesRejectsNonNumeric(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openConfigScreen())
	m.Config.Cursor = int(configItemIdleMinutes)
	m, _ = mustModel(m.selectConfigItemForTest())
	m.Config.Input.SetValue("soon")

	updated, cmd := m.handleConfigKey(tea.KeyPressMsg{Text: "enter"})
	got := updated.(Model)
	if cmd != nil {
		t.Fatal("non-numeric idle minutes must not exec")
	}
	if len(got.Notices) == 0 {
		t.Fatal("expected a validation toast for non-numeric idle minutes")
	}
}

func TestConfigEditingEscReturnsToListWithoutExec(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openConfigScreen())
	m.Config.Cursor = int(configItemIdleMinutes)
	m, _ = mustModel(m.selectConfigItemForTest())

	updated, _ := m.handleConfigKey(tea.KeyPressMsg{Text: "esc"})
	got := updated.(Model)
	if got.Config == nil || got.Config.Editing {
		t.Fatalf("Config = %v, want editing cleared but screen still open", got.Config)
	}
	if len(f.callArgs()) != 0 {
		t.Fatal("Esc while editing must not exec anything")
	}
}

func TestConfigQEscOnListClosesScreen(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openConfigScreen())

	updated, _ := m.handleConfigKey(tea.KeyPressMsg{Text: "q"})
	got := updated.(Model)
	if got.Screen != ScreenDashboard || got.Config != nil {
		t.Fatalf("Screen = %v, Config = %v, want closed back to dashboard", got.Screen, got.Config)
	}
}

// selectConfigItemForTest exposes selectConfigItem's (tea.Model, tea.Cmd)
// result as (Model, tea.Cmd) for test setup convenience.
func (m Model) selectConfigItemForTest() (Model, tea.Cmd) {
	updated, cmd := m.selectConfigItem()
	return updated.(Model), cmd
}
