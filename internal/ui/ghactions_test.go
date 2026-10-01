package ui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// 'S' is plain start-all: the supervisor runs in-process, so there is no
// daemon to start instead.
func TestActionStartAllRunsStartAll(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{})

	_, cmd := m.handleKey(tea.KeyPressMsg{Text: "S"})
	runCmdSync(cmd)
	calls := f.calls()
	if len(calls) != 1 || calls[0][0] != "StartAll" {
		t.Fatalf("calls = %v, want one StartAll call", calls)
	}
}

// 'I' (install a login service for the daemon) is gone.
func TestInstallServiceKeyDoesNothing(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{})

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "I"})
	if got := updated.(Model); got.Confirm != nil || cmd != nil {
		t.Fatalf("'I' opened a modal or ran a command (confirm %v)", got.Confirm)
	}
	if len(f.calls()) != 0 {
		t.Fatalf("calls = %v, want none", f.calls())
	}
}

func TestActionDownloadNoopWhenTarballNotStale(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{Tarball: state.Tarball{Stale: false}})

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "g"})
	got := updated.(Model)
	if cmd != nil {
		t.Fatal("must not call Download when the tarball is not stale")
	}
	if len(got.Notices) == 0 {
		t.Fatal("expected an info toast instead")
	}
}

func TestActionDownloadRunsDownloadWhenStale(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{Tarball: state.Tarball{Stale: true}})

	_, cmd := m.handleKey(tea.KeyPressMsg{Text: "g"})
	runCmdSync(cmd)
	calls := f.calls()
	if len(calls) != 1 || calls[0][0] != "Download" {
		t.Fatalf("calls = %v, want one Download call", calls)
	}
}

func TestActionCheckGHOpensGHStatusScreenWithOutput(t *testing.T) {
	f := &fakeEngine{results: map[string]opResult{
		"GHStatus": {msg: "a/b: 2 registered runners"},
	}}
	m := newActionsTestModel(f, state.Snapshot{})

	_, cmd := m.handleKey(tea.KeyPressMsg{Text: "c"})
	msg := runCmdSync(cmd)
	updated, _ := m.Update(msg)
	got := updated.(Model)
	if got.Screen != ScreenGHStatus || got.GHStatusText == nil {
		t.Fatalf("Screen = %v, GHStatusText = %v, want ScreenGHStatus open", got.Screen, got.GHStatusText)
	}
	if !strings.Contains(*got.GHStatusText, "registered runners") {
		t.Fatalf("GHStatusText = %q, want it to contain the report", *got.GHStatusText)
	}
}

func TestActionCheckGHShowsPartialReportAndError(t *testing.T) {
	f := &fakeEngine{results: map[string]opResult{
		"GHStatus": {msg: "a/b: 2 registered runners", err: errors.New("could not fetch every target")},
	}}
	m := newActionsTestModel(f, state.Snapshot{})

	_, cmd := m.handleKey(tea.KeyPressMsg{Text: "c"})
	updated, _ := m.Update(runCmdSync(cmd))
	text := *updated.(Model).GHStatusText
	if !strings.Contains(text, "registered runners") || !strings.Contains(text, "could not fetch") {
		t.Fatalf("GHStatusText = %q, want both the report and the error", text)
	}
}

func TestGHStatusAnyKeyClosesAndRunsPoll(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{})
	m.Screen = ScreenGHStatus
	text := "some status"
	m.GHStatusText = &text

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	got := updated.(Model)
	if got.Screen != ScreenDashboard || got.GHStatusText != nil {
		t.Fatalf("Screen = %v, GHStatusText = %v, want closed back to dashboard", got.Screen, got.GHStatusText)
	}
	runCmdSync(cmd)
	calls := f.calls()
	if len(calls) != 1 || calls[0][0] != "Poll" {
		t.Fatalf("calls = %v, want one Poll call on close", calls)
	}
}
