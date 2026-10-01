package engine

import (
	"strings"
	"testing"
	"time"
)

func TestAutoscaleDecide(t *testing.T) {
	cases := []struct {
		cur, busy, queued, mn, mx, elapsed, threshold, cap int
		hasCap                                             bool
		want                                               int
		name                                               string
	}{
		{1, 1, 2, 1, 5, 0, 10, 0, false, 3, "grow by queued when all busy"},
		{3, 3, 9, 1, 5, 0, 10, 0, false, 5, "growth capped at max"},
		{2, 1, 4, 1, 5, 0, 10, 0, false, 2, "no growth while a runner is idle"},
		{2, 2, 0, 1, 5, 0, 10, 0, false, 2, "all busy, nothing queued"},
		{3, 1, 0, 1, 5, 10, 10, 0, false, 2, "shrink after the idle period"},
		{3, 1, 0, 1, 5, 9, 10, 0, false, 3, "no shrink before the idle period"},
		{1, 0, 0, 1, 5, 60, 10, 0, false, 1, "never below min"},
		{0, 0, 0, 1, 5, 0, 10, 0, false, 1, "brought up to min"},
		{7, 7, 0, 1, 5, 0, 10, 0, false, 5, "brought down to max"},
		{0, 0, 2, 0, 5, 0, 10, 0, false, 2, "min=0 grows from nothing on a queue"},
		{2, 2, 5, 1, 8, 0, 10, 4, true, 4, "growth capped by MAX_RUNNERS headroom"},
		{2, 2, 5, 1, 8, 0, 10, 1, true, 2, "global cap never forces a shrink"},
		{3, 0, 0, 1, 5, 0, 0, 0, false, 2, "AUTOSCALE_IDLE_MINUTES=0 shrinks immediately"},
	}
	for _, c := range cases {
		eq(t, c.want, autoscaleDecide(c.cur, c.busy, c.queued, c.mn, c.mx, c.elapsed, c.threshold, c.cap, c.hasCap), c.name)
	}
}

func TestLabels(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	e.plat.runnerOS, e.plat.runnerArch = "osx", "arm64"
	e.labelsStr = "macos arm64 apple-silicon"
	fleet := e.fleetLabels()
	eq(t, "self-hosted macos arm64 macos arm64 apple-silicon", fleet, "implicit + detected, lowercase")
	eq(t, true, labelSatisfiable("Self-Hosted,macOS,ARM64", fleet), "case-insensitive")
	eq(t, false, labelSatisfiable("self-hosted,gpu", fleet), "missing label")
	eq(t, true, labelSatisfiable("", fleet), "no labels")
	eq(t, true, labelSatisfiable("apple-silicon", fleet), "detected custom label")
}

func TestTargetQueueDepth(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	te.gh.set(func(args []string) (string, string, int) {
		a := strings.Join(args, " ")
		if strings.Contains(a, "status=queued") {
			var ids []string
			for i := 1; i <= 15; i++ {
				ids = append(ids, itoa(i))
			}
			return strings.Join(ids, "\n") + "\n", "", 0
		}
		return "self-hosted,macos\n", "", 0
	})
	q, ok := e.targetQueueDepth("https://github.com/auto/repo", 1, 1)
	eq(t, true, ok, "ok")
	eq(t, 10, q.sat, "no more than 10 runs")
	q, _ = e.targetQueueDepth("https://github.com/myorg", 2, 2)
	eq(t, 1, q.sat, "org: all busy = 1")
	q, _ = e.targetQueueDepth("https://github.com/myorg", 2, 1)
	eq(t, 0, q.sat, "org: an idle runner = 0")
	q, _ = e.targetQueueDepth("https://github.com/myorg", 0, 0)
	eq(t, 0, q.sat, "org: no runners = 0")
}

// autoscaleEnv: AUTOSCALE=1, auto/repo bounded 1-5 with one busy runner,
// fixed/repo unbounded; queue: 2 satisfiable jobs + 1 needing a gpu.
func autoscaleEnv(t *testing.T) *testEnv {
	te := newTestEnv(t, "AUTOSCALE=\"1\"\nMAX_RUNNERS=\"20\"\nAUTOSCALE_IDLE_MINUTES=\"10\"\n")
	e := te.e
	te.targets("auto/repo min=1 max=5\nfixed/repo\n")
	te.runner(1, "https://github.com/auto/repo")
	te.setRunning(1, true)
	te.log(1, "2024-01-01 Running job: build")
	te.runner(2, "https://github.com/fixed/repo")
	te.setRunning(2, true)
	e.ghs.pollTS = te.now.Unix()
	te.gh.set(func(args []string) (string, string, int) {
		a := strings.Join(args, " ")
		switch {
		case strings.Contains(a, "status=queued"):
			return "100\n", "", 0
		case strings.Contains(a, "runs/100/jobs"):
			return "self-hosted,macos\nself-hosted\nself-hosted,gpu\n", "", 0
		}
		return "", "", 0
	})
	return te
}

func TestAutoscaleTick(t *testing.T) {
	te := autoscaleEnv(t)
	e := te.e
	e.cfg.vals["AUTOSCALE"] = "0"
	e.autoscaleTick()
	eq(t, 0, te.gh.count("status=queued"), "AUTOSCALE=0 does nothing")
	e.cfg.vals["AUTOSCALE"] = "1"

	// the grow path needs setup; count the attempt through the want map
	e.autoscaleTick()
	eq(t, 3, e.want["auto+repo"], "scaled the bounded target by the satisfiable queue")
	q := e.queue["auto+repo"]
	eq(t, "2/1", itoa(q.sat)+"/"+itoa(q.unsat), "queue cached")
	_, ok := e.queue["fixed+repo"]
	eq(t, false, ok, "no queue for an unbounded target")
	contains(t, strings.Join(e.Events(), "\n"), "autoscale: auto/repo 1 → 3 (2 queued)", "event")
}

func TestAutoscaleSkips(t *testing.T) {
	te := autoscaleEnv(t)
	e := te.e
	e.pending = func() map[string]int { return map[string]int{"auto/repo": 4} }
	e.autoscaleTick()
	eq(t, 0, te.gh.count("status=queued"), "pending manual change skipped")
	e.pending = nil

	e.ghs.pollTS = te.now.Unix() - 3600
	e.autoscaleTick()
	eq(t, 0, te.gh.count("status=queued"), "waits for fresh API data")
	e.ghs.pollTS = te.now.Unix()

	te.gh.set(func([]string) (string, string, int) { return "", "gh: Server Error (HTTP 500)", 1 })
	e.autoscaleTick()
	_, ok := e.want["auto+repo"]
	eq(t, false, ok, "unreadable queue skipped")
}

func TestAutoscaleIdleShrink(t *testing.T) {
	te := autoscaleEnv(t)
	e := te.e
	te.runner(3, "https://github.com/auto/repo")
	te.setRunning(3, true)
	te.log(1, "2024-01-01 Listening for Jobs")
	te.log(3, "2024-01-01 Listening for Jobs")
	te.gh.set(func(args []string) (string, string, int) { return "", "", 0 })
	e.autoscaleTick()
	eq(t, 0, len(te.removed), "no shrink on the first idle tick")
	_, ok := e.idleSince["auto+repo"]
	eq(t, true, ok, "idle-since recorded")
	te.now = te.now.Add(11 * time.Minute)
	e.ghs.pollTS = te.now.Unix()
	e.autoscaleTick()
	eq(t, 1, len(te.removed), "shrinks by one after AUTOSCALE_IDLE_MINUTES")
	contains(t, strings.Join(e.Events(), "\n"), "2 → 1 (idle 11m)", "event")
	eq(t, te.now.Unix(), e.idleSince["auto+repo"], "shrink restarts the idle clock")
}

func TestAutoscaleGlobalCap(t *testing.T) {
	te := autoscaleEnv(t)
	e := te.e
	e.cfg.vals["MAX_RUNNERS"] = "3"
	e.autoscaleTick()
	eq(t, 2, e.want["auto+repo"], "growth limited so the total stays within MAX_RUNNERS")
}
