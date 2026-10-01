package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// queueInfo is the last queue depth seen for a target: jobs this host could
// run, and (repos only) queued jobs whose labels no runner here has.
type queueInfo struct {
	sat, unsat int
	hasUnsat   bool
}

// autoscaleDecide is the pure scaling decision: the desired runner count.
//   - queued jobs and no idle runner: grow by queued
//   - an idle runner for >= threshold minutes and cur > min: shrink by 1
//   - clamped to min..max; growth is also clamped to cap when hasCap (the
//     most this target may have without the fleet exceeding MAX_RUNNERS),
//     but cap never forces a shrink
func autoscaleDecide(cur, busy, queued, mn, mx, elapsed, threshold, cap int, hasCap bool) int {
	idle := cur - busy
	if idle < 0 {
		idle = 0
	}
	want := cur
	if queued > 0 && idle == 0 {
		want = cur + queued
	} else if idle > 0 && cur > mn && elapsed >= threshold {
		want = cur - 1
	}
	if want > mx {
		want = mx
	}
	if want < mn {
		want = mn
	}
	if hasCap && want > cur && want > cap {
		want = cap
		if want < cur {
			want = cur
		}
	}
	return want
}

// fleetLabels are the labels a job's runs-on may name that this host's
// runners carry: self-hosted, OS, arch and the detected ones (lowercase).
func (e *Engine) fleetLabels() string {
	os := e.plat.runnerOS
	if os == "osx" {
		os = "macos"
	}
	return strings.ToLower(strings.Join(strings.Fields("self-hosted "+os+" "+e.plat.runnerArch+" "+strings.ReplaceAll(e.labels(), ",", " ")), " "))
}

// labelSatisfiable: every label of the job ("a,b,c") is one the fleet has
// (space-separated), case-insensitively.
func labelSatisfiable(job, fleet string) bool {
	have := map[string]bool{}
	for _, l := range strings.Fields(strings.ToLower(fleet)) {
		have[l] = true
	}
	for _, l := range strings.Split(strings.ToLower(job), ",") {
		l = strings.TrimSpace(l)
		if l != "" && !have[l] {
			return false
		}
	}
	return true
}

// targetQueueDepth counts queued jobs this host could run. Repos: the
// queued jobs of up to 10 queued runs, split by label fit. Orgs have no
// queue API for org runners, so the signal is "all runners busy" (= 1).
// ok is false when an API call failed.
func (e *Engine) targetQueueDepth(u string, cur, busy int) (q queueInfo, ok bool) {
	if targetType(u) != "repo" {
		if cur > 0 && busy >= cur {
			q.sat = 1
		}
		return q, true
	}
	p := strings.TrimPrefix(u, ghPrefix)
	out, r := e.ghAPI("repos/"+p+"/actions/runs?status=queued&per_page=20", "--jq", ".workflow_runs[].id")
	if !r.ok() {
		e.targetSetError(u, r.Class, r.Msg)
		return q, false
	}
	fleet := e.fleetLabels()
	n := 0
	for _, id := range strings.Fields(out) {
		if !reDigits.MatchString(id) {
			continue
		}
		n++
		if n > 10 {
			break
		}
		jobs, r := e.ghAPI("repos/"+p+"/actions/runs/"+id+"/jobs", "--jq", `.jobs[] | select(.status=="queued") | (.labels | join(","))`)
		if !r.ok() {
			e.targetSetError(u, r.Class, r.Msg)
			return q, false
		}
		for _, line := range strings.Split(jobs, "\n") {
			if line == "" {
				continue
			}
			if labelSatisfiable(line, fleet) {
				q.sat++
			} else {
				q.unsat++
			}
		}
	}
	q.hasUnsat = true
	return q, true
}

// pendingFor reports whether the UI has an unapplied count for t.
func (e *Engine) pendingFor(t string) bool {
	if e.pending == nil {
		return false
	}
	for u := range e.pending() {
		if sameTarget(targetEntryToURL(u), t) || sameTarget(u, t) {
			return true
		}
	}
	return false
}

// autoscaleTick is one autoscaling pass over every target with bounds
// (AUTOSCALE=1 only); runs right after a GitHub poll.
func (e *Engine) autoscaleTick() {
	if e.cfg.vals["AUTOSCALE"] != "1" {
		return
	}
	maxr := e.cfg.int("MAX_RUNNERS")
	threshold := e.cfg.int("AUTOSCALE_IDLE_MINUTES")
	now := e.now().Unix()
	total := 0
	for _, t := range e.knownTargets() {
		total += e.countRunnersForTarget(t)
	}
	for _, t := range e.knownTargets() {
		mn, mx, ok := e.targets.bounds(t)
		if !ok {
			continue
		}
		// Never override a count the user has changed but not applied yet
		if e.pendingFor(t) {
			continue
		}
		cur := e.countRunnersForTarget(t)
		// Busy flags must come from this poll (a target with no runners has
		// none to report, so it can still be brought up to its minimum)
		if cur != 0 && !e.ghDataFresh() {
			continue
		}
		// Only a running, non-busy runner is idle; quarantined, stopped or
		// between-jobs runners count as busy so they never block growth or
		// trigger a shrink
		idle := 0
		for _, id := range e.runnerIDsForTarget(t) {
			if e.rt(id).draining || !e.isRunning(id) {
				continue
			}
			if !e.isBusy(id) {
				idle++
			}
		}
		busy := cur - idle
		if busy < 0 {
			busy = 0
		}
		key := targetKey(t)
		q, ok := e.targetQueueDepth(t, cur, busy)
		if !ok {
			continue
		}
		e.queue[key] = q

		idle = cur - busy
		elapsed := 0
		if idle > 0 {
			since, ok := e.idleSince[key]
			if !ok {
				since = now
				e.idleSince[key] = now
			}
			elapsed = int((now - since) / 60)
		} else {
			delete(e.idleSince, key)
		}

		cap := maxr - (total - cur)
		want := autoscaleDecide(cur, busy, q.sat, mn, mx, elapsed, threshold, cap, true)
		if want == cur {
			continue
		}
		var reason string
		switch {
		case want > cur && q.sat > 0 && idle == 0 && cur >= mn:
			reason = strconv.Itoa(q.sat) + " queued"
		case want < cur && cur <= mx:
			reason = fmt.Sprintf("idle %dm", elapsed)
		default:
			reason = fmt.Sprintf("bounds %d-%d", mn, mx)
		}
		_, err := e.scaleTarget(t, want)
		nw := e.countRunnersForTarget(t)
		total = total - cur + nw
		// A shrink restarts the idle clock: at most one removal per period
		if want < cur {
			e.idleSince[key] = now
		}
		note := fmt.Sprintf("autoscale: %s %d → %d (%s)", targetLabel(t), cur, want, reason)
		if err != nil {
			note += fmt.Sprintf(" - failed, now %d", nw)
		}
		e.dlog(note)
	}
}
