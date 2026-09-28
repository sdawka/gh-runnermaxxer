package ui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

type fakeDaemonStarter struct {
	err     error
	started bool
}

func (f *fakeDaemonStarter) Start() error {
	f.started = true
	return f.err
}

func TestActionStartAllWhenDaemonDownStartsDaemonInstead(t *testing.T) {
	f := &fakeRunner{}
	d := &fakeDaemonStarter{}
	m := newActionsTestModel(f, state.Snapshot{DaemonPID: 0})
	m.Daemon = d

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "S"})
	got := updated.(Model)
	if _, busy := got.Inflight[globalKey]; !busy {
		t.Fatal("expected globalKey marked in-flight for start-daemon")
	}
	msg := runCmdSync(cmd)
	if !d.started {
		t.Fatal("Daemon.Start was not called")
	}
	if _, ok := msg.(daemonStartedMsg); !ok {
		t.Fatalf("msg = %T, want daemonStartedMsg", msg)
	}
	if len(f.callArgs()) != 0 {
		t.Fatal("must not exec --start-all when the daemon is down")
	}
}

func TestActionStartAllWhenDaemonUpRunsStartAll(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{DaemonPID: 123})

	_, cmd := m.handleKey(tea.KeyPressMsg{Text: "S"})
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--start-all" {
		t.Fatalf("calls = %v, want one --start-all exec", calls)
	}
}

func TestDaemonStartedMsgErrorShowsStickyError(t *testing.T) {
	f := &fakeRunner{}
	d := &fakeDaemonStarter{err: errors.New("boom")}
	m := newActionsTestModel(f, state.Snapshot{DaemonPID: 0})
	m.Daemon = d
	m.Inflight[globalKey] = Op{}

	updated, _ := m.Update(daemonStartedMsg{err: errors.New("boom")})
	got := updated.(Model)
	if _, busy := got.Inflight[globalKey]; busy {
		t.Fatal("globalKey should clear on daemonStartedMsg regardless of outcome")
	}
	if len(got.Notices) == 0 {
		t.Fatal("expected an error notice")
	}
}

func TestActionInstallServiceOpensConfirmWhenNoDaemon(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{DaemonPID: 0})

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "I"})
	got := updated.(Model)
	if got.Confirm == nil {
		t.Fatal("expected a confirm modal for install-service")
	}
}

func TestActionInstallServiceNoopWhenDaemonAlreadyUp(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{DaemonPID: 123})

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "I"})
	got := updated.(Model)
	if got.Confirm != nil {
		t.Fatal("must not offer install-service while a daemon is already running")
	}
	if len(got.Notices) == 0 {
		t.Fatal("expected an info toast instead")
	}
}

func TestInstallServiceConfirmYesExecsInstallService(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{DaemonPID: 0})
	m, _ = mustModel(m.confirmInstallService())

	_, cmd := m.handleConfirmKey(tea.KeyPressMsg{Text: "y"})
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--install-service" {
		t.Fatalf("calls = %v, want one --install-service exec", calls)
	}
}

func TestActionDownloadNoopWhenTarballNotStale(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{Tarball: state.Tarball{Stale: false}})

	updated, cmd := m.handleKey(tea.KeyPressMsg{Text: "g"})
	got := updated.(Model)
	if cmd != nil {
		t.Fatal("must not exec --download when the tarball is not stale")
	}
	if len(got.Notices) == 0 {
		t.Fatal("expected an info toast instead")
	}
}

func TestActionDownloadRunsDownloadWhenStale(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{Tarball: state.Tarball{Stale: true}})

	_, cmd := m.handleKey(tea.KeyPressMsg{Text: "g"})
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--download" {
		t.Fatalf("calls = %v, want one --download exec", calls)
	}
}

func TestActionCheckGHOpensGHStatusScreenWithOutput(t *testing.T) {
	f := &fakeRunner{override: map[string]cli.Result{
		argsKey([]string{"--gh-status"}): {Stdout: "a/b: 2 registered runners\n", ExitCode: 0},
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
		t.Fatalf("GHStatusText = %q, want it to contain the script's output", *got.GHStatusText)
	}
}

func TestGHStatusAnyKeyClosesAndRunsPoll(t *testing.T) {
	f := &fakeRunner{}
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
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--poll" {
		t.Fatalf("calls = %v, want one --poll exec on close", calls)
	}
}
