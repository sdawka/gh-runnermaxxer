package ui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

func newTestModel() Model {
	return New(context.Background(), &fakeEngine{})
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

	snap := state.Snapshot{Targets: []state.Target{{URL: urlA, Want: 4, Have: 4}}}
	updated, _ := m.Update(snapshotMsg{snap: snap})
	got := updated.(Model)

	if got.Pending.IsPending(urlA) {
		t.Error("pending entry survived a snapshot where Want caught up")
	}
}

// Each snapshot re-arms the wait for the next one, and the loop ends
// quietly once the engine closes the channel.
func TestSnapshotsAreStreamedUntilClosed(t *testing.T) {
	f := &fakeEngine{snaps: make(chan state.Snapshot, 1)}
	m := New(context.Background(), f)

	f.snaps <- state.Snapshot{MaxRunners: 7}
	msg := waitForSnapshot(f.Snapshots())()
	updated, cmd := m.Update(msg)
	got := updated.(Model)
	if got.Snap.MaxRunners != 7 {
		t.Fatalf("Snap.MaxRunners = %d, want 7", got.Snap.MaxRunners)
	}
	if cmd == nil {
		t.Fatal("snapshotMsg did not re-arm the snapshot wait")
	}

	close(f.snaps)
	if msg := waitForSnapshot(f.Snapshots())(); msg != nil {
		t.Errorf("wait on a closed channel = %T, want nil", msg)
	}
}

// SharedPending mirrors the pending edits after every update, for the
// engine's autoscaler.
func TestSharedPendingFollowsEdits(t *testing.T) {
	m := newTestModel()
	m.SharedPending = &SharedPending{}
	m.Snap = state.Snapshot{MaxRunners: 20, Targets: []state.Target{{URL: urlA, Want: 2, Have: 2}}}

	updated, _ := m.Update(tea.KeyPressMsg{Text: "+"})
	if got := m.SharedPending.Get(); got[urlA] != 3 {
		t.Fatalf("SharedPending = %v, want %s=3", got, urlA)
	}
	updated.(Model).Update(tea.KeyPressMsg{Text: "esc"})
	if got := m.SharedPending.Get(); len(got) != 0 {
		t.Errorf("SharedPending = %v after esc, want empty", got)
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
	m.Snap = state.Snapshot{MaxRunners: 20, Targets: []state.Target{{URL: urlA, Want: 2, Have: 2}}}
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
