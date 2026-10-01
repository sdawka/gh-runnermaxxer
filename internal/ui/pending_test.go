package ui

import (
	"testing"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

func snapWithTargets(maxRunners int, targets ...state.Target) state.Snapshot {
	return state.Snapshot{MaxRunners: maxRunners, Targets: targets}
}

const urlA = "https://github.com/a/a"
const urlB = "https://github.com/b/b"

func TestPendingAdjustBasic(t *testing.T) {
	snap := snapWithTargets(20, state.Target{URL: urlA, Want: 2, Have: 2})
	p := NewPendingCounts()

	ok, msg := p.Adjust(snap, urlA, 1)
	if !ok {
		t.Fatalf("Adjust(+1) rejected: %s", msg)
	}
	if got := p.Get(urlA, 2); got != 3 {
		t.Errorf("Get = %d, want 3", got)
	}
	if !p.IsPending(urlA) {
		t.Error("IsPending = false, want true")
	}

	// Adjusting back to Want drops the pending entry (menu_reload semantics).
	ok, _ = p.Adjust(snap, urlA, -1)
	if !ok {
		t.Fatal("Adjust(-1) rejected")
	}
	if p.IsPending(urlA) {
		t.Error("IsPending = true after returning to Want, want false")
	}
	if p.Get(urlA, 2) != 2 {
		t.Errorf("Get = %d, want 2 (back to Want)", p.Get(urlA, 2))
	}
}

func TestPendingSetClampsNegative(t *testing.T) {
	snap := snapWithTargets(20, state.Target{URL: urlA, Want: 2, Have: 2})
	p := NewPendingCounts()

	ok, msg := p.Set(snap, urlA, -5)
	if !ok {
		t.Fatalf("Set(-5) rejected: %s", msg)
	}
	if got := p.Get(urlA, 2); got != 0 {
		t.Errorf("Get = %d, want 0 (clamped)", got)
	}
}

func TestPendingMaxRunnersGuard(t *testing.T) {
	snap := snapWithTargets(5,
		state.Target{URL: urlA, Want: 3, Have: 3},
		state.Target{URL: urlB, Want: 2, Have: 2},
	)
	p := NewPendingCounts()

	// Total is already at Want=5=MaxRunners; +1 on A should be rejected.
	ok, msg := p.Adjust(snap, urlA, 1)
	if ok {
		t.Fatal("Adjust(+1) accepted, want rejected (MAX_RUNNERS)")
	}
	if msg == "" {
		t.Error("expected a MAX_RUNNERS message")
	}
	if p.IsPending(urlA) {
		t.Error("a rejected Adjust must not create a pending entry")
	}
}

func TestPendingTotalAfter(t *testing.T) {
	snap := snapWithTargets(20,
		state.Target{URL: urlA, Want: 2, Have: 2},
		state.Target{URL: urlB, Want: 3, Have: 3},
	)
	p := NewPendingCounts()
	if got := p.TotalAfter(snap); got != 5 {
		t.Fatalf("TotalAfter (no pending) = %d, want 5", got)
	}

	if ok, msg := p.Set(snap, urlA, 4); !ok {
		t.Fatalf("Set rejected: %s", msg)
	}
	if got := p.TotalAfter(snap); got != 7 { // 4 + 3
		t.Errorf("TotalAfter (A pending 4) = %d, want 7", got)
	}
}

func TestPendingReconcileDropsWhenWantCatchesUp(t *testing.T) {
	// User set a pending count of 4; before it applies, the daemon accepts
	// it (Want moves to 4) or autoscale requests 4 on its own. The pending
	// entry must disappear so the table doesn't show a phantom change, even
	// though new runners may still be spinning up (Have hasn't caught up).
	snap := snapWithTargets(20, state.Target{URL: urlA, Want: 2, Have: 2})
	p := NewPendingCounts()
	if ok, msg := p.Set(snap, urlA, 4); !ok {
		t.Fatalf("Set rejected: %s", msg)
	}
	if !p.IsPending(urlA) {
		t.Fatal("expected a pending entry before reconciliation")
	}

	moved := snapWithTargets(20, state.Target{URL: urlA, Want: 4, Have: 4})
	p.Reconcile(moved)
	if p.IsPending(urlA) {
		t.Error("IsPending = true after Want caught up, want false")
	}
}

func TestPendingReconcileDropsOnWantEvenWhileHaveLags(t *testing.T) {
	// The specific case the daemon's schema 2 wants distinguished: Want has
	// accepted the apply, but Have is still catching up (new runners take
	// time to register). Reconciling against Have here would leave a slow
	// or failed convergence looking like an unapplied edit forever.
	snap := snapWithTargets(20, state.Target{URL: urlA, Want: 2, Have: 2})
	p := NewPendingCounts()
	if ok, _ := p.Set(snap, urlA, 5); !ok {
		t.Fatal("Set rejected")
	}

	stillSpinningUp := snapWithTargets(20, state.Target{URL: urlA, Want: 5, Have: 3})
	p.Reconcile(stillSpinningUp)
	if p.IsPending(urlA) {
		t.Error("IsPending = true after Want caught up (even though Have hasn't), want false")
	}
}

func TestPendingReconcileKeepsUnmatchedEdits(t *testing.T) {
	snap := snapWithTargets(20, state.Target{URL: urlA, Want: 2, Have: 2})
	p := NewPendingCounts()
	if ok, _ := p.Set(snap, urlA, 5); !ok {
		t.Fatal("Set rejected")
	}

	// Want hasn't moved to 5 yet: still pending (nothing has accepted the
	// edit), regardless of what Have happens to be.
	notYetAccepted := snapWithTargets(20, state.Target{URL: urlA, Want: 2, Have: 2})
	p.Reconcile(notYetAccepted)
	if !p.IsPending(urlA) {
		t.Error("IsPending = false, want true (Want hasn't accepted the pending 5 yet)")
	}
}

func TestPendingDDecrementIsANoOp(t *testing.T) {
	// §4.4: "d on a non-busy runner ... decrementing that target's pending
	// count if it equalled Have ... in the Go model this is a no-op because
	// pending only holds explicit edits and reconcilePending drops entries
	// equal to the new Want." Removing a runner (Want/Have: 2 -> 1) with no
	// pending edit must not create one.
	p := NewPendingCounts()
	if !p.Empty() {
		t.Fatal("expected no pending edits initially")
	}

	after := snapWithTargets(20, state.Target{URL: urlA, Want: 1, Have: 1}) // a runner was removed
	p.Reconcile(after)
	if !p.Empty() {
		t.Error("Reconcile created a pending entry where there was none")
	}
}

func TestPendingClearAndChanges(t *testing.T) {
	snap := snapWithTargets(20,
		state.Target{URL: urlA, Want: 2, Have: 2},
		state.Target{URL: urlB, Want: 1, Have: 1},
	)
	p := NewPendingCounts()
	if ok, _ := p.Set(snap, urlA, 3); !ok {
		t.Fatal("Set rejected")
	}
	if ok, _ := p.Set(snap, urlB, 0); !ok {
		t.Fatal("Set rejected")
	}

	changes := p.Changes()
	if len(changes) != 2 || changes[urlA] != 3 || changes[urlB] != 0 {
		t.Errorf("Changes() = %v, want {%s:3 %s:0}", changes, urlA, urlB)
	}

	p.Clear()
	if !p.Empty() {
		t.Error("Empty() = false after Clear()")
	}
}
