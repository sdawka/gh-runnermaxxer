package ui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runner-swarm/tui/internal/cli"
	"github.com/sdawka/gh-runner-swarm/tui/internal/state"
)

type nopRunner struct{}

func (nopRunner) Run(ctx context.Context, timeout time.Duration, args ...string) cli.Result {
	return cli.Result{}
}

func newTestModel() Model {
	client := cli.NewClient(nopRunner{})
	return New(context.Background(), client, cli.Paths{}, nil)
}

func TestInitReturnsACommand(t *testing.T) {
	m := newTestModel()
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init() returned a nil Cmd, want a batch of at least the tick/CLI-poll commands")
	}
}

func TestUpdateWindowSize(t *testing.T) {
	m := newTestModel()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	got := updated.(Model)
	if got.Width != 120 || got.Height != 40 {
		t.Fatalf("Width/Height = %d/%d, want 120/40", got.Width, got.Height)
	}
	if !got.ShowLog {
		t.Error("ShowLog = false at width 120, want true")
	}
	if got.Compact {
		t.Error("Compact = true at height 40, want false")
	}
}

func TestUpdateNarrowHidesLogPane(t *testing.T) {
	m := newTestModel()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	got := updated.(Model)
	if got.ShowLog {
		t.Error("ShowLog = true at width 80, want false (<100 hides the log pane)")
	}
	if !got.Compact {
		t.Error("Compact = false at height 20, want true (<30 collapses the header)")
	}
}

func TestUpdateSnapshotMsgReconcilesPending(t *testing.T) {
	m := newTestModel()
	m.Pending.values = map[string]int{urlA: 4}

	snap := state.Snapshot{Targets: []state.Target{{URL: urlA, Have: 4}}, DaemonPID: 123}
	updated, _ := m.Update(snapshotMsg{snap: snap})
	got := updated.(Model)

	if got.Pending.IsPending(urlA) {
		t.Error("pending entry survived a snapshot where Have caught up")
	}
	if !got.DaemonUp {
		t.Error("DaemonUp = false, want true (DaemonPID != 0)")
	}
}

func TestUpdateSnapshotErrKeepsLastGoodSnapshot(t *testing.T) {
	m := newTestModel()
	m.Snap = state.Snapshot{DaemonPID: 42}

	updated, _ := m.Update(snapshotErrMsg{err: context.DeadlineExceeded})
	got := updated.(Model)

	if got.Snap.DaemonPID != 42 {
		t.Error("Snap was cleared on a load error, want the last good snapshot kept")
	}
	if got.SnapErr == nil {
		t.Error("SnapErr = nil, want the error recorded")
	}
}

func TestUpdateQuitKey(t *testing.T) {
	m := newTestModel()
	_, cmd := m.Update(tea.KeyPressMsg{Text: "q"})
	if cmd == nil {
		t.Fatal("Update('q') returned a nil Cmd, want tea.Quit")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Errorf("Update('q') Cmd produced %T, want tea.QuitMsg", cmd())
	}
}

func TestUpdateHelpToggle(t *testing.T) {
	m := newTestModel()
	updated, _ := m.Update(tea.KeyPressMsg{Text: "?"})
	got := updated.(Model)
	if !got.Help {
		t.Error("Help = false after '?', want true")
	}
	updated, _ = got.Update(tea.KeyPressMsg{Text: "?"})
	got = updated.(Model)
	if got.Help {
		t.Error("Help = true after a second '?', want false")
	}
}

func TestUpdateCountKeysOnCursorTarget(t *testing.T) {
	m := newTestModel()
	m.Snap = state.Snapshot{MaxRunners: 20, Targets: []state.Target{{URL: urlA, Have: 2}}}
	m.Cursor = 0

	updated, _ := m.Update(tea.KeyPressMsg{Text: "+"})
	got := updated.(Model)
	if v := got.Pending.Get(urlA, 2); v != 3 {
		t.Errorf("after '+': pending = %d, want 3", v)
	}

	updated, _ = got.Update(tea.KeyPressMsg{Text: "esc"})
	got = updated.(Model)
	if !got.Pending.Empty() {
		t.Error("Pending not cleared after esc")
	}
}
