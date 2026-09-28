package ui

import "testing"

func TestDashboardKeyToAction(t *testing.T) {
	cases := []struct {
		key         string
		onHeaderRow bool
		want        Action
	}{
		{"up", false, Action{Kind: ActionUp}},
		{"k", false, Action{Kind: ActionUp}},
		{"down", false, Action{Kind: ActionDown}},
		{"j", false, Action{Kind: ActionDown}},
		{"left", false, Action{Kind: ActionDec}},
		{"right", false, Action{Kind: ActionInc}},
		{"+", false, Action{Kind: ActionInc}},
		{"=", false, Action{Kind: ActionInc}},
		{"-", false, Action{Kind: ActionDec}},
		{"_", false, Action{Kind: ActionDec}},
		{"0", false, Action{Kind: ActionSetCount, N: 0}},
		{"7", false, Action{Kind: ActionSetCount, N: 7}},
		{"enter", false, Action{Kind: ActionApply}},
		{"esc", false, Action{Kind: ActionDiscardOrBack}},
		// 'd' depends on whether the cursor is on a target header row.
		{"d", false, Action{Kind: ActionDrain}},
		{"d", true, Action{Kind: ActionDrainPicker}},
		{"D", false, Action{Kind: ActionRemoveNow}},
		{"x", false, Action{Kind: ActionStop}},
		{"X", false, Action{Kind: ActionStopAll}},
		{"s", false, Action{Kind: ActionStart}},
		{"S", false, Action{Kind: ActionStartAll}},
		{"r", false, Action{Kind: ActionRestart}},
		{"R", false, Action{Kind: ActionRestartAll}},
		{"t", false, Action{Kind: ActionProjects}},
		{"n", false, Action{Kind: ActionProjects}},
		{"a", false, Action{Kind: ActionAddTarget}},
		{"l", false, Action{Kind: ActionLogs}},
		{"L", false, Action{Kind: ActionDaemonLog}},
		{"c", false, Action{Kind: ActionCheckGH}},
		{"e", false, Action{Kind: ActionConfig}},
		{"g", false, Action{Kind: ActionDownload}},
		{"/", false, Action{Kind: ActionFilter}},
		{"?", false, Action{Kind: ActionHelp}},
		{"q", false, Action{Kind: ActionQuit}},
		{"ctrl+c", false, Action{Kind: ActionQuit}},
		{"z", false, Action{Kind: ActionNone}},
	}
	for _, tc := range cases {
		got := keyToAction(ScreenDashboard, tc.key, tc.onHeaderRow)
		if got != tc.want {
			t.Errorf("keyToAction(Dashboard, %q, onHeaderRow=%v) = %+v, want %+v", tc.key, tc.onHeaderRow, got, tc.want)
		}
	}
}

func TestDashboardHDoesNotAdjustCount(t *testing.T) {
	// On the dashboard, h/l are NOT count keys (l is "open logs"); only
	// ←/→ and +/- adjust the count there (§3.4 note on h/l).
	if got := keyToAction(ScreenDashboard, "h", false); got.Kind != ActionNone {
		t.Errorf("keyToAction(Dashboard, h) = %+v, want ActionNone", got)
	}
	if got := keyToAction(ScreenDashboard, "l", false); got.Kind != ActionLogs {
		t.Errorf("keyToAction(Dashboard, l) = %+v, want ActionLogs", got)
	}
}

func TestProjectsKeyToAction(t *testing.T) {
	cases := []struct {
		key  string
		want Action
	}{
		{"up", Action{Kind: ActionUp}},
		{"k", Action{Kind: ActionUp}},
		{"down", Action{Kind: ActionDown}},
		{"j", Action{Kind: ActionDown}},
		// On the projects screen, unlike the dashboard, h/l DO adjust the count.
		{"h", Action{Kind: ActionDec}},
		{"l", Action{Kind: ActionInc}},
		{"left", Action{Kind: ActionDec}},
		{"right", Action{Kind: ActionInc}},
		{"+", Action{Kind: ActionInc}},
		{"-", Action{Kind: ActionDec}},
		{"3", Action{Kind: ActionSetCount, N: 3}},
		{"a", Action{Kind: ActionAddTarget}},
		{"x", Action{Kind: ActionRemoveFromList}},
		{"b", Action{Kind: ActionBounds}},
		{"enter", Action{Kind: ActionApply}},
		{"s", Action{Kind: ActionApply}},
		{"q", Action{Kind: ActionDiscardOrBack}},
		{"esc", Action{Kind: ActionDiscardOrBack}},
		{"z", Action{Kind: ActionNone}},
	}
	for _, tc := range cases {
		got := keyToAction(ScreenProjects, tc.key, false)
		if got != tc.want {
			t.Errorf("keyToAction(Projects, %q) = %+v, want %+v", tc.key, got, tc.want)
		}
	}
}

func TestDefaultKeyMapHelp(t *testing.T) {
	km := DefaultKeyMap()
	if len(km.ShortHelp()) == 0 {
		t.Error("ShortHelp() is empty")
	}
	if len(km.FullHelp()) == 0 {
		t.Error("FullHelp() is empty")
	}
}
