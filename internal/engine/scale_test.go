package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPickVictims(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/owner/repo"
	for _, id := range []int{1, 2, 3, 4} {
		te.runner(id, u)
	}
	te.runner(5, "https://github.com/owner/other")
	te.setRunning(3, true)
	te.log(3, "2024-01-01 Running job: build")
	eq(t, "4 2 1 3", joinInts(e.pickVictims(u, 10)), "idle highest-first, then busy, other targets excluded")
	eq(t, "4 2", joinInts(e.pickVictims(u, 2)), "at most n")
	for _, id := range []int{1, 2, 4} {
		te.setRunning(id, true)
		te.log(id, "2024-01-01 Running job: build")
	}
	eq(t, "4 3 2 1", joinInts(e.pickVictims(u, 10)), "all busy: highest first")
	eq(t, "", joinInts(e.pickVictims("https://github.com/nobody/here", 5)), "no runners")
}

func TestNextFreeIDAndRunnerIDs(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	eq(t, 1, e.nextFreeID(), "no runners")
	te.runner(1, "")
	te.runner(2, "")
	eq(t, 3, e.nextFreeID(), "skips existing")
	os.RemoveAll(e.runnerDir(1))
	eq(t, 1, e.nextFreeID(), "fills the lowest gap")
	te.runner(10, "")
	te.runner(1, "")
	os.WriteFile(filepath.Join(e.base, "runner-7"), nil, 0o644) // a file, not a dir
	os.MkdirAll(filepath.Join(e.base, "runner-x"), 0o755)
	eq(t, "1 2 10", joinInts(e.runnerIDs()), "numeric sort, dirs only")
}

func TestScaleTarget(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/o/r"
	te.gh.set(func([]string) (string, string, int) { return "{}", "", 0 })
	e.startFn = func(id int) error {
		te.started = append(te.started, id)
		te.setRunning(id, true)
		return nil
	}
	te.runner(1, u)
	te.setRunning(1, true)
	te.log(1, "2024-01-01 Listening for Jobs")
	lines, err := e.scaleTarget(u, 1)
	eq(t, nil, err, "no-op scale")
	eq(t, 0, len(lines), "no lines for a no-op")

	// shrink: busy runners drain, idle ones are removed
	te.runner(2, u)
	te.setRunning(2, true)
	te.log(2, "2024-01-01 Running job: build")
	te.runner(3, u)
	lines, err = e.scaleTarget(u, 0)
	eq(t, nil, err, "shrink")
	eq(t, "3 1", joinInts(te.removed), "idle runners removed, highest first")
	eq(t, true, e.rt(2).draining, "busy runner drains")
	contains(t, strings.Join(lines, "\n"), "runner-2 is mid-job - draining", "drain line")

	// grow: cancel the drain first
	lines, err = e.scaleTarget(u, 1)
	eq(t, nil, err, "grow by cancelling a drain")
	eq(t, false, e.rt(2).draining, "drain cancelled")
	contains(t, strings.Join(lines, "\n"), "runner-2: drain cancelled, keeping it", "cancel line")

	// grow with an inaccessible target: error, want kept
	te.gh.set(func([]string) (string, string, int) { return "", "gh: Not Found (HTTP 404)", 1 })
	_, err = e.scaleTarget(u, 3)
	if err == nil {
		t.Fatal("inaccessible target scaled")
	}
	eq(t, 3, e.want["o+r"], "want kept after a failure")

	// grow: setup failure (no tarball) stops adding and keeps want
	te.gh.set(func([]string) (string, string, int) { return "tok", "", 0 })
	lines, err = e.scaleTarget(u, 3)
	if err == nil {
		t.Fatal("setup without a tarball succeeded")
	}
	contains(t, strings.Join(lines, "\n"), "failed - not adding more to o/r", "failure line")
	eq(t, 3, e.want["o+r"], "want kept")
	eq(t, false, e.runnerExists(3), "no half-set-up dir left")
}

func TestScaleAPI(t *testing.T) {
	te := newTestEnv(t, "MAX_RUNNERS=\"3\"\n")
	e := te.e
	u := "https://github.com/o/r"
	te.runner(1, u)
	te.runner(2, "https://github.com/o/other")
	if _, err := e.Scale(map[string]int{"bad target!!": 1}); err == nil {
		t.Error("invalid target accepted")
	}
	_, err := e.Scale(map[string]int{"o/r": 3})
	if err == nil {
		t.Fatal("over MAX_RUNNERS accepted")
	}
	contains(t, err.Error(), "that would make 4 runners across all projects, over MAX_RUNNERS (3)", "total guard")
	eq(t, 3, e.scaleTotalAfter(map[string]int{u: 1, "https://github.com/O/OTHER": 2}), "named targets replace current sizes")

	msg, err := e.Scale(map[string]int{"https://github.com/o/r.git": 1})
	eq(t, nil, err, "already there")
	eq(t, "o/r: already at 1 runner(s)", msg, "message")
	eq(t, true, e.targets.contains(u), "target listed")

	msg, err = e.Scale(map[string]int{"o/r": 0})
	eq(t, nil, err, "shrink")
	contains(t, msg, "runner-1 removed", "removed")
}

func TestRunnerAPIs(t *testing.T) {
	te := newTestEnv(t, "RUNNER_NAME_PREFIX=\"mac\"\n")
	e := te.e
	u := "https://github.com/o/r"
	te.runner(1, u)
	te.runner(2, u)
	e.startFn = func(id int) error {
		te.started = append(te.started, id)
		te.setRunning(id, true)
		return nil
	}
	e.rt(1).quarantined = true
	e.rt(1).fails = 5
	msg, err := e.Start(1)
	eq(t, nil, err, "start")
	eq(t, "runner-1 running (pid 10001)", msg, "start message")
	eq(t, false, e.rt(1).quarantined, "quarantine cleared")
	eq(t, 0, e.rt(1).fails, "fails cleared")

	msg, _ = e.Stop(1)
	eq(t, "runner-1 stopped (it stays down until started)", msg, "stop message")
	eq(t, true, e.isMarkedStopped(1), "marked stopped")

	msg, _ = e.StartAll()
	eq(t, "runner-1 running\nrunner-2 running", msg, "start all")
	eq(t, false, e.isMarkedStopped(1), "start clears the stop marker")
	msg, _ = e.StopAll()
	eq(t, "runner-1 stopped\nrunner-2 stopped", msg, "stop all")
	eq(t, true, e.isMarkedStopped(2), "all marked stopped")

	msg, _ = e.Remove(2)
	eq(t, "runner-2 removed", msg, "remove")
	if _, err := e.Remove(2); err == nil {
		t.Error("removing a missing runner should fail")
	}
	if _, err := e.Stop(0); err == nil {
		t.Error("runner-0 accepted")
	}
}
