package ui

import (
	"fmt"

	"github.com/sdawka/gh-runner-swarm/tui/internal/state"
)

// PendingCounts tracks in-progress scale edits the way the bash TUI's
// M_WANT/M_CUR pair does (Go design doc §2.5): an entry for a target exists
// only while it differs from that target's server-reported Want (the count
// the daemon was last asked for), not its Have (the count actually running).
// Reconciling against Want means an edit clears the moment the daemon
// accepts it, even if new runners are still spinning up - and a slow or
// failed convergence (Have lagging Want) never looks like an unapplied
// local edit. An autoscale change happening underneath the user, or another
// client's own apply, drops the entry the same way.
type PendingCounts struct {
	values map[string]int
}

// NewPendingCounts returns an empty PendingCounts.
func NewPendingCounts() *PendingCounts {
	return &PendingCounts{values: map[string]int{}}
}

// Get returns the pending count for url, or have if there is no pending edit.
func (p *PendingCounts) Get(url string, have int) int {
	if v, ok := p.values[url]; ok {
		return v
	}
	return have
}

// IsPending reports whether url currently has an edit distinct from its Have.
func (p *PendingCounts) IsPending(url string) bool {
	_, ok := p.values[url]
	return ok
}

// Empty reports whether there are no pending edits at all.
func (p *PendingCounts) Empty() bool {
	return len(p.values) == 0
}

// Clear discards every pending edit (Esc on the dashboard, or 'q' on the
// projects screen).
func (p *PendingCounts) Clear() {
	p.values = map[string]int{}
}

// Changes returns a copy of the pending map, for building one
// `--scale url=N` exec per entry.
func (p *PendingCounts) Changes() map[string]int {
	out := make(map[string]int, len(p.values))
	for k, v := range p.values {
		out[k] = v
	}
	return out
}

// store keeps n as url's pending value unless it equals want (the target's
// current server-reported Want), in which case the entry is dropped - this
// is what makes reconciliation automatic instead of needing a special case
// ("d decrements pending", §4.4).
func (p *PendingCounts) store(url string, want, n int) {
	if n == want {
		delete(p.values, url)
		return
	}
	p.values[url] = n
}

// Adjust changes url's pending count by delta (the +/-/←/→ keys), clamped
// to >= 0 and guarded so the fleet total never exceeds snap.MaxRunners. ok
// is false (with a message to toast) when the guard rejects the change,
// matching menu_adjust's MAX_RUNNERS message.
func (p *PendingCounts) Adjust(snap state.Snapshot, url string, delta int) (ok bool, msg string) {
	current := p.Get(url, targetWant(snap, url))
	return p.Set(snap, url, current+delta)
}

// Set assigns url's pending count to n directly (the 0-9 digit keys),
// applying the same clamp and MAX_RUNNERS guard as Adjust.
func (p *PendingCounts) Set(snap state.Snapshot, url string, n int) (ok bool, msg string) {
	if n < 0 {
		n = 0
	}
	want := targetWant(snap, url)
	total := p.TotalAfter(snap) - p.Get(url, want) + n
	if snap.MaxRunners > 0 && total > snap.MaxRunners {
		return false, fmt.Sprintf("MAX_RUNNERS (%d) reached - raise it in .runnermaxxer.conf", snap.MaxRunners)
	}
	p.store(url, want, n)
	return true, ""
}

// Reconcile drops any pending entry that now matches the snapshot's Want -
// the daemon accepted the apply (even if Have hasn't caught up yet), or
// autoscale (or another actor) requested that value on its own.
func (p *PendingCounts) Reconcile(snap state.Snapshot) {
	for url, n := range p.values {
		if t, ok := snap.TargetByURL(url); ok && t.Want == n {
			delete(p.values, url)
		}
	}
}

// TotalAfter sums, across every target in snap, the pending count if one
// exists else Want - i.e. what the fleet will be asked to converge to if
// applied now.
func (p *PendingCounts) TotalAfter(snap state.Snapshot) int {
	total := 0
	for _, t := range snap.Targets {
		total += p.Get(t.URL, t.Want)
	}
	return total
}

func targetWant(snap state.Snapshot, url string) int {
	if t, ok := snap.TargetByURL(url); ok {
		return t.Want
	}
	return 0
}
