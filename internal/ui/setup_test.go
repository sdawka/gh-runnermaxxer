package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		updated, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = updated.(Model)
	}
	return m
}

func press(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(msg)
	return updated.(Model), cmd
}

func setupModel(f *fakeEngine) Model {
	f.needsSetup = true
	m := New(context.Background(), f)
	m.Width, m.Height = 100, 30
	return m
}

func TestNeedsSetupOpensSetupForm(t *testing.T) {
	m := setupModel(&fakeEngine{})
	if m.Screen != ScreenSetup || m.Setup == nil {
		t.Fatalf("Screen = %v, want the setup form", m.Screen)
	}
	frame := m.renderString()
	for _, want := range []string{"Runner name prefix", "Max runners", "20"} {
		if !strings.Contains(frame, want) {
			t.Errorf("setup frame lacks %q:\n%s", want, frame)
		}
	}
	// Esc and q don't get past it: q is just typed into the prefix.
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = typeText(t, m, "q")
	if m.Screen != ScreenSetup || m.Setup.Prefix.Value() != "q" {
		t.Fatalf("Screen = %v, prefix %q; want the form still open with q typed", m.Screen, m.Setup.Prefix.Value())
	}
}

func TestSetupSubmitCallsSetupThenOpensDashboard(t *testing.T) {
	f := &fakeEngine{results: map[string]opResult{"Setup ci 5": {msg: "Configuration saved to .runnermaxxer.conf"}}}
	m := setupModel(f)

	m = typeText(t, m, "ci")
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = typeText(t, m, "5")
	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not submit")
	}
	m, _ = press(t, m, runCmdSync(cmd))

	if calls := f.calls(); len(calls) != 1 || strings.Join(calls[0], " ") != "Setup ci 5" {
		t.Fatalf("calls = %v, want Setup ci 5", calls)
	}
	if m.Screen != ScreenDashboard || m.Setup != nil {
		t.Fatalf("Screen = %v, want the dashboard after setup", m.Screen)
	}
	if n := m.Notices[len(m.Notices)-1].Text; !strings.Contains(n, "press a to add a project") {
		t.Errorf("notice = %q, want a hint to add a project", n)
	}
}

// An empty prefix is passed through for the engine to fill in with the
// host name.
func TestSetupDefaultsSubmitEmptyPrefix(t *testing.T) {
	f := &fakeEngine{}
	m := setupModel(f)
	_, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	runCmdSync(cmd)
	if calls := f.calls(); len(calls) != 1 || strings.Join(calls[0], " ") != "Setup  20" {
		t.Fatalf("calls = %v, want Setup with an empty prefix and 20", calls)
	}
}

func TestSetupValidatesBeforeCallingSetup(t *testing.T) {
	f := &fakeEngine{}
	m := setupModel(f)
	m = typeText(t, m, "bad name")
	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || m.Setup.Err == "" {
		t.Fatalf("invalid prefix: cmd %v, Err %q; want an error and no call", cmd, m.Setup.Err)
	}
	if len(f.calls()) != 0 {
		t.Fatalf("calls = %v, want none", f.calls())
	}
}

func TestSetupFailureStaysOnForm(t *testing.T) {
	f := &fakeEngine{results: map[string]opResult{"Setup  20": {err: errors.New("disk full")}}}
	m := setupModel(f)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = press(t, m, runCmdSync(cmd))
	if m.Screen != ScreenSetup || m.Setup == nil || m.Setup.Err != "disk full" {
		t.Fatalf("Screen = %v, want the form kept with the error shown", m.Screen)
	}
	if !strings.Contains(m.renderString(), "disk full") {
		t.Error("setup frame does not show the error")
	}
}

func TestSetupCtrlCQuits(t *testing.T) {
	f := &fakeEngine{}
	m := setupModel(f)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !isQuit(finishQuit(t, m, cmd)) {
		t.Fatal("ctrl+c on the setup form did not quit")
	}
	if calls := f.calls(); len(calls) != 1 || strings.Join(calls[0], " ") != "Shutdown StopNow" {
		t.Fatalf("calls = %v, want Shutdown StopNow", calls)
	}
}
