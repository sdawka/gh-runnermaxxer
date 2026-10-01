package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

func targetFixture(url string, have, want int, listed bool) state.Target {
	return state.Target{URL: url, Label: url, Listed: listed, Have: have, Want: want}
}

func TestActionProjectsSwitchesScreen(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{targetFixture("a/b", 1, 1, true)}})

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "t"})
	got := updated.(Model)
	if got.Screen != ScreenProjects {
		t.Fatalf("Screen = %v, want ScreenProjects", got.Screen)
	}
}

func TestProjectsDiscardOrBackReturnsToDashboardAndClearsPending(t *testing.T) {
	f := &fakeEngine{}
	snap := state.Snapshot{Targets: []state.Target{targetFixture("a/b", 1, 1, true)}}
	m := newActionsTestModel(f, snap)
	m.Screen = ScreenProjects
	m.Pending.Set(m.Snap, "a/b", 3)
	if m.Pending.Empty() {
		t.Fatal("test setup: expected a pending edit")
	}

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "q"})
	got := updated.(Model)
	if got.Screen != ScreenDashboard {
		t.Fatalf("Screen = %v, want ScreenDashboard", got.Screen)
	}
	if !got.Pending.Empty() {
		t.Fatal("pending edits were not discarded on projects q/Esc")
	}
}

func TestProjectsApplyRunsScaleAndReturnsToDashboard(t *testing.T) {
	f := &fakeEngine{}
	snap := state.Snapshot{Targets: []state.Target{targetFixture("a/b", 1, 1, true)}}
	m := newActionsTestModel(f, snap)
	m.Screen = ScreenProjects
	m.Pending.Set(m.Snap, "a/b", 3)

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "enter"})
	got := updated.(Model)
	if got.Screen != ScreenDashboard {
		t.Fatalf("Screen = %v, want ScreenDashboard after apply", got.Screen)
	}
	runCmdSync(cmd)
	calls := f.calls()
	if len(calls) != 1 || calls[0][0] != "Scale" {
		t.Fatalf("calls = %v, want one Scale call", calls)
	}
}

func TestProjectsHLAreCountKeysUnlikeDashboard(t *testing.T) {
	f := &fakeEngine{}
	snap := state.Snapshot{Targets: []state.Target{targetFixture("a/b", 1, 1, true)}}
	m := newActionsTestModel(f, snap)
	m.Screen = ScreenProjects
	m.Cursor = 0

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "l"})
	got := updated.(Model)
	if got.Pending.Get("a/b", 1) != 2 {
		t.Fatalf("pending after 'l' = %d, want 2 (h/l are count keys in projects)", got.Pending.Get("a/b", 1))
	}
}

func TestActionAddTargetOpensFormAndRemembersReturnScreen(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{})
	m.Screen = ScreenProjects

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "a"})
	got := updated.(Model)
	if got.Screen != ScreenAddTarget || got.AddTarget == nil {
		t.Fatalf("Screen = %v, AddTarget = %v, want ScreenAddTarget open", got.Screen, got.AddTarget)
	}
	if got.formReturn != ScreenProjects {
		t.Fatalf("formReturn = %v, want ScreenProjects", got.formReturn)
	}
}

func TestAddTargetEscCancelsWithoutExec(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openAddTargetForm())

	updated, _ := m.handleAddTargetKey(tea.KeyPressMsg{Text: "esc"})
	got := updated.(Model)
	if got.Screen != ScreenDashboard || got.AddTarget != nil {
		t.Fatalf("Esc did not close the form cleanly: screen=%v form=%v", got.Screen, got.AddTarget)
	}
	if len(f.calls()) != 0 {
		t.Fatal("Esc must not call anything")
	}
}

func TestAddTargetEmptyEntrySilentlyCancels(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openAddTargetForm())

	updated, cmd := m.handleAddTargetKey(tea.KeyPressMsg{Text: "enter"})
	got := updated.(Model)
	if got.Screen != ScreenDashboard {
		t.Fatalf("Screen = %v, want ScreenDashboard", got.Screen)
	}
	if cmd != nil {
		t.Fatal("empty entry must not start a call")
	}
}

func TestAddTargetEnterRunsAddTargetExec(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{})
	m, _ = mustModel(m.openAddTargetForm())
	m.AddTarget.Input.SetValue("owner/repo")

	_, cmd := m.handleAddTargetKey(tea.KeyPressMsg{Text: "enter"})
	runCmdSync(cmd)
	calls := f.calls()
	if len(calls) != 1 || calls[0][0] != "AddTarget" || calls[0][1] != "owner/repo" {
		t.Fatalf("calls = %v, want one AddTarget owner/repo call", calls)
	}
}

func TestBoundsFormPrefilledFromTargetWhenAutoscale(t *testing.T) {
	f := &fakeEngine{}
	t1 := state.Target{URL: "a/b", Label: "a/b", Listed: true, Autoscale: true, Min: 1, Max: 5}
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{t1}})
	m.Screen = ScreenProjects
	m.Cursor = 0

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "b"})
	got := updated.(Model)
	if got.Screen != ScreenBounds || got.Bounds == nil {
		t.Fatalf("Screen = %v, Bounds = %v, want ScreenBounds open", got.Screen, got.Bounds)
	}
	if got.Bounds.Min.Value() != "1" || got.Bounds.Max.Value() != "5" {
		t.Fatalf("bounds prefilled = %q/%q, want 1/5", got.Bounds.Min.Value(), got.Bounds.Max.Value())
	}
}

func TestBoundsFormNoTargetUnderCursorToasts(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{}) // no targets, no rows
	m.Screen = ScreenProjects

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "b"})
	got := updated.(Model)
	if got.Screen == ScreenBounds {
		t.Fatal("bounds form opened with no target under the cursor")
	}
	if len(got.Notices) == 0 {
		t.Fatal("expected a toast when no target is under the cursor")
	}
}

func TestBoundsSubmitBothEmptyClears(t *testing.T) {
	f := &fakeEngine{}
	t1 := state.Target{URL: "a/b", Label: "a/b", Listed: true, Autoscale: true, Min: 1, Max: 5}
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{t1}})
	m.Screen = ScreenProjects
	m, _ = mustModel(m.openBoundsForm())
	m.Bounds.Min.SetValue("")
	m.Bounds.Max.SetValue("")

	_, cmd := m.handleBoundsKey(tea.KeyPressMsg{Text: "enter"})
	runCmdSync(cmd)
	calls := f.calls()
	if len(calls) != 1 || strings.Join(calls[0], " ") != "SetBounds a/b 0 0" {
		t.Fatalf("calls = %v, want SetBounds a/b 0 0 (clears)", calls)
	}
}

func TestBoundsSubmitInvalidRangeToastsWithoutExec(t *testing.T) {
	f := &fakeEngine{}
	t1 := state.Target{URL: "a/b", Label: "a/b", Listed: true}
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{t1}})
	m.Screen = ScreenProjects
	m, _ = mustModel(m.openBoundsForm())
	m.Bounds.Min.SetValue("5")
	m.Bounds.Max.SetValue("1") // min > max

	updated, cmd := m.handleBoundsKey(tea.KeyPressMsg{Text: "enter"})
	got := updated.(Model)
	if cmd != nil {
		t.Fatal("invalid bounds must not start a call")
	}
	if got.Screen != ScreenBounds {
		t.Fatal("invalid bounds should keep the form open for correction")
	}
	if len(got.Notices) == 0 {
		t.Fatal("expected a toast for invalid bounds")
	}
}

func TestBoundsSubmitValidRangeRunsSetBounds(t *testing.T) {
	f := &fakeEngine{}
	t1 := state.Target{URL: "a/b", Label: "a/b", Listed: true}
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{t1}})
	m.Screen = ScreenProjects
	m, _ = mustModel(m.openBoundsForm())
	m.Bounds.Min.SetValue("1")
	m.Bounds.Max.SetValue("5")

	_, cmd := m.handleBoundsKey(tea.KeyPressMsg{Text: "enter"})
	runCmdSync(cmd)
	calls := f.calls()
	if len(calls) != 1 || strings.Join(calls[0], " ") != "SetBounds a/b 1 5" {
		t.Fatalf("calls = %v, want SetBounds a/b 1 5", calls)
	}
}

func TestBoundsTabSwitchesFocusedField(t *testing.T) {
	f := &fakeEngine{}
	t1 := state.Target{URL: "a/b", Label: "a/b", Listed: true}
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{t1}})
	m.Screen = ScreenProjects
	m, _ = mustModel(m.openBoundsForm())
	if m.Bounds.Focused != 0 {
		t.Fatalf("Focused = %d, want 0 (Min) on open", m.Bounds.Focused)
	}

	updated, _ := m.handleBoundsKey(tea.KeyPressMsg{Text: "tab"})
	got := updated.(Model)
	if got.Bounds.Focused != 1 {
		t.Fatalf("Focused = %d, want 1 (Max) after tab", got.Bounds.Focused)
	}
}

func TestRemoveFromListRefusesWhenStillHasRunners(t *testing.T) {
	f := &fakeEngine{}
	t1 := targetFixture("a/b", 2, 2, true)
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{t1}})
	m.Screen = ScreenProjects
	m.Cursor = 0

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "x"})
	got := updated.(Model)
	if len(f.calls()) != 0 {
		t.Fatal("must not call RemoveTarget while the target still has runners")
	}
	if len(got.Notices) == 0 || !strings.Contains(got.Notices[len(got.Notices)-1].Text, "still has") {
		t.Fatalf("Notices = %v, want a 'still has N runner(s)' toast", got.Notices)
	}
}

func TestRemoveFromListRefusesWhenNotListed(t *testing.T) {
	f := &fakeEngine{}
	t1 := targetFixture("a/b", 0, 0, false)
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{t1}})
	m.Screen = ScreenProjects
	m.Cursor = 0

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "x"})
	got := updated.(Model)
	if len(f.calls()) != 0 {
		t.Fatal("must not call RemoveTarget for an unlisted target")
	}
	if len(got.Notices) == 0 || !strings.Contains(got.Notices[len(got.Notices)-1].Text, "not in the targets file") {
		t.Fatalf("Notices = %v, want a 'not in the targets file' toast", got.Notices)
	}
}

func TestRemoveFromListRunsRemoveTargetWhenEligible(t *testing.T) {
	f := &fakeEngine{}
	t1 := targetFixture("a/b", 0, 0, true)
	m := newActionsTestModel(f, state.Snapshot{Targets: []state.Target{t1}})
	m.Screen = ScreenProjects
	m.Cursor = 0

	_, cmd := m.handleKey(tea.KeyPressMsg{Text: "x"})
	runCmdSync(cmd)
	calls := f.calls()
	if len(calls) != 1 || calls[0][0] != "RemoveTarget" || calls[0][1] != "a/b" {
		t.Fatalf("calls = %v, want one RemoveTarget a/b call", calls)
	}
}

// mustModel adapts a (Model, tea.Cmd) pair (as returned by the form-opening
// helpers) into two plain return values for test setup convenience.
func mustModel(m Model, cmd tea.Cmd) (Model, tea.Cmd) { return m, cmd }
