package engine

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

// pickVictims chooses up to n runners of a target to remove: idle/stopped
// ones first, then busy ones; highest id first within each class. Draining
// runners are skipped (they are going away anyway).
func (e *Engine) pickVictims(u string, n int) []int {
	ids := e.runnerIDsForTarget(u)
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	var idle, busy []int
	for _, id := range ids {
		if e.rt(id).draining {
			continue
		}
		if e.isBusy(id) {
			busy = append(busy, id)
		} else {
			idle = append(idle, id)
		}
	}
	out := append(idle, busy...)
	if n < 0 {
		n = 0
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// nextFreeID is the lowest N with no runner-N directory.
func (e *Engine) nextFreeID() int {
	id := 1
	for {
		if _, err := os.Stat(e.runnerDir(id)); err != nil {
			return id
		}
		id++
	}
}

// scaleTarget brings one target to exactly want runners (draining ones
// excluded). Busy runners are drained rather than killed. Returns progress
// lines; the error is set when access failed or a runner failed to set up
// or start.
func (e *Engine) scaleTarget(u string, want int) ([]string, error) {
	label := targetLabel(u)
	key := targetKey(u)
	cur := e.countRunnersForTarget(u)
	// The requested count shows in the snapshot while this runs and after a
	// partial failure (want 5, have 3); dropped once the target gets there
	e.want[key] = want
	var lines []string

	// Cheapest way back up: cancel pending drains first
	if want > cur {
		for _, id := range e.runnerIDsForTarget(u) {
			if cur >= want {
				break
			}
			if r := e.rt(id); r.draining {
				r.draining = false
				cur++
				lines = append(lines, fmt.Sprintf("runner-%d: drain cancelled, keeping it", id))
			}
		}
	}

	var failed error
	switch {
	case want > cur:
		if r := e.targetAccessible(u); !r.ok() {
			msg := fmt.Sprintf("%s: %s", label, ghErrorNote(r.Class, r.Msg))
			return append(lines, msg), errors.New(msg)
		}
		lines = append(lines, fmt.Sprintf("%s: adding %d runner(s)", label, want-cur))
		for k := cur; k < want; k++ {
			id := e.nextFreeID()
			e.clearFailureState(id)
			err := e.setupRunner(id, u)
			if err == nil {
				err = e.startRunner(id)
			}
			if err != nil {
				msg := fmt.Sprintf("runner-%d failed - not adding more to %s: %v", id, label, err)
				e.dlog(msg)
				lines = append(lines, msg)
				failed = errors.New(msg)
				break
			}
			lines = append(lines, fmt.Sprintf("runner-%d added", id))
		}
	case want < cur:
		lines = append(lines, fmt.Sprintf("%s: removing %d runner(s)", label, cur-want))
		for _, id := range e.pickVictims(u, cur-want) {
			if e.isBusy(id) {
				e.rt(id).draining = true
				e.dlog(fmt.Sprintf("runner-%d: draining - removed when its job finishes", id))
				lines = append(lines, fmt.Sprintf("runner-%d is mid-job - draining, it will be removed when the job finishes", id))
				continue
			}
			if w := e.removeRunner(id); w != "" {
				lines = append(lines, fmt.Sprintf("runner-%d removed (%s)", id, w))
			} else {
				lines = append(lines, fmt.Sprintf("runner-%d removed", id))
			}
		}
	}
	if failed == nil {
		delete(e.want, key)
	}
	return lines, failed
}

// scaleTotalAfter is the runner total across all targets after applying
// counts (targets not named keep their current size, drains excluded).
func (e *Engine) scaleTotalAfter(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	for _, t := range e.knownTargets() {
		named := false
		for u := range counts {
			if sameTarget(t, u) {
				named = true
			}
		}
		if !named {
			total += e.countRunnersForTarget(t)
		}
	}
	return total
}
