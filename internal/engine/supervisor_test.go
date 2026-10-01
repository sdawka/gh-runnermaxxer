package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSuperviseExitReasons(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/o/r"
	for _, id := range []int{2, 3, 4} {
		te.runner(id, u)
	}
	te.log(2, "Runner listener exit with Session Conflict error, stop the service, no retry needed.")
	te.log(3, "Runner listener exit with deprecated version exit code: 7.")
	te.log(4, "Runner listener exit with terminated error, stop the service, no retry needed.")
	e.superviseRunners()
	eq(t, true, e.rt(2).quarantined, "conflict quarantined at once")
	eq(t, true, e.rt(3).quarantined, "deprecated quarantined at once")
	if !strings.HasPrefix(e.rt(2).lastErr, "session conflict") || !strings.HasSuffix(e.rt(2).lastErr, "- quarantined") {
		t.Errorf("conflict lasterr: %q", e.rt(2).lastErr)
	}
	eq(t, false, e.rt(4).quarantined, "terminated not quarantined")
	eq(t, "4", joinInts(te.started), "only the terminated runner restarted")
	eq(t, "listener exited (terminated, exit 1) - see log", e.rt(4).lastErr, "terminated lasterr")
	eq(t, 0, e.rt(2).fails, "no fail charged to a conflict")
}

func TestSuperviseBackoffAndQuarantine(t *testing.T) {
	te := newTestEnv(t, "MAX_RESTART_ATTEMPTS=\"3\"\n")
	e := te.e
	te.runner(1, "https://github.com/o/r")
	start := te.now
	step := func(d time.Duration) {
		te.now = te.now.Add(d)
		e.invalidateProcs()
		e.superviseRunners()
	}
	step(0)
	eq(t, "1", joinInts(te.started), "first restart right away")
	eq(t, 1, e.rt(1).fails, "attempt counted")
	step(4 * time.Second)
	eq(t, 1, len(te.started), "within 5*2^1=10s: no restart")
	step(6 * time.Second)
	eq(t, 2, len(te.started), "after 10s: restart")
	step(19 * time.Second)
	eq(t, 2, len(te.started), "within 20s: no restart")
	step(1 * time.Second)
	eq(t, 3, len(te.started), "after 20s: restart")
	step(time.Hour)
	eq(t, 3, len(te.started), "MAX_RESTART_ATTEMPTS reached: no more")
	eq(t, true, e.rt(1).quarantined, "quarantined")
	contains(t, e.rt(1).lastErr, "crash-looped 3x - quarantined", "lasterr")
	step(time.Hour)
	eq(t, 3, len(te.started), "quarantined runners stay down")

	// manual start clears; sustained uptime clears fails
	e.clearFailureState(1)
	eq(t, false, e.rt(1).quarantined, "clear failure state")
	e.rt(1).fails = 2
	e.rt(1).lastStart = te.now.Unix()
	te.setRunning(1, true)
	step(30 * time.Second)
	eq(t, 2, e.rt(1).fails, "running < 60s keeps fails")
	step(30 * time.Second)
	eq(t, 0, e.rt(1).fails, "running >= 60s clears fails")
	_ = start

	// a stopped runner is left alone
	te.setRunning(1, false)
	e.markStopped(1)
	te.started = nil
	step(time.Hour)
	eq(t, 0, len(te.started), "stopped on purpose -> not restarted")
}

func TestSuperviseDrain(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/owner/repo"
	for _, id := range []int{10, 11, 12} {
		te.runner(id, u)
	}
	eq(t, 3, e.countRunnersForTarget(u), "count before draining")
	e.rt(11).draining = true
	eq(t, "10 11 12", joinInts(e.runnerIDsForTarget(u)), "a draining runner is still listed")
	eq(t, 2, e.countRunnersForTarget(u), "but not counted")
	eq(t, "12 10", joinInts(e.pickVictims(u, 10)), "victims skip the draining runner")
	e.clearRunnerState(11)
	eq(t, false, e.rt(11).draining, "clearRunnerState drops the drain")

	te2 := newTestEnv(t, "")
	te2.runner(20, u)
	te2.e.rt(20).draining = true
	te2.e.superviseRunners()
	eq(t, "20", joinInts(te2.removed), "idle draining runner removed")
	eq(t, 0, len(te2.started), "never restarted")

	te3 := newTestEnv(t, "")
	te3.runner(21, u)
	te3.setRunning(21, true)
	te3.log(21, "2024-01-01 Running job: build")
	te3.e.rt(21).draining = true
	te3.e.superviseRunners()
	eq(t, 0, len(te3.removed), "busy draining runner left alone")
	eq(t, true, te3.e.rt(21).draining, "still draining")
	eq(t, 0, len(te3.started), "not restarted")
}

func TestDrainAPI(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/o/r"
	te.runner(1, u)
	te.runner(2, u)
	te.setRunning(2, true)
	te.log(2, "2024-01-01 Running job: build")
	if _, err := e.Drain(9); err == nil {
		t.Error("no such runner accepted")
	}
	msg, err := e.Drain(1)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "runner-1 was not running a job: removed", msg, "idle drain removes")
	msg, _ = e.Drain(2)
	contains(t, msg, "mid-job: draining", "busy drain waits")
	eq(t, true, e.rt(2).draining, "marked draining")
	eq(t, "1", joinInts(te.removed), "only the idle one removed")
}

func TestWakeGuard(t *testing.T) {
	te := newTestEnv(t, "REFRESH_INTERVAL=\"5\"\n")
	e := te.e
	e.lastAlive.Store(te.now.Unix() - 4)
	e.noteTickTime()
	eq(t, int64(0), e.wakeAt, "normal gap is not a wake")
	eq(t, te.now.Unix(), e.lastAlive.Load(), "alive time refreshed")
	e.lastAlive.Store(te.now.Unix() - 60)
	e.noteTickTime()
	eq(t, te.now.Unix(), e.wakeAt, "60s gap at 5s interval is a wake")
	contains(t, strings.Join(e.Events(), "\n"), "clock jumped", "logged")
	eq(t, true, e.recentlyWoke(), "recently woke")
	e.wakeAt = te.now.Unix() - 121
	eq(t, false, e.recentlyWoke(), "not after 2 minutes")
	e.wakeAt = 0
	e.lastAlive.Store(0)
	e.noteTickTime()
	eq(t, int64(0), e.wakeAt, "first tick is not a wake")

	// recycle guards
	u := "https://github.com/owner/repo"
	te.runner(1, u)
	te.setRunning(1, true)
	e.ghs.probeState = "ok"
	busy := "false"
	te.gh.set(func(args []string) (string, string, int) {
		if strings.Contains(strings.Join(args, " "), "rate_limit") {
			return "4000\t0\n", "", 0
		}
		return "runner-1\toffline\t" + busy + "\n", "", 0
	})
	offlineTwice := func() {
		te.stopped, te.started = nil, nil
		te.setRunning(1, true)
		e.rt(1).ghOffline = 1
		e.rt(1).lastStart = te.now.Unix() - 600
		e.checkGithubHealth()
	}
	te.log(1, "2024-01-01 00:00:00Z: Listening for Jobs")
	offlineTwice()
	eq(t, "1", joinInts(te.stopped), "offline twice -> recycled")
	eq(t, "1", joinInts(te.started), "and restarted")
	eq(t, "GitHub reported offline - recycled", e.rt(1).lastErr, "lasterr")
	e.wakeAt = te.now.Unix()
	offlineTwice()
	eq(t, "", joinInts(te.stopped), "just woke -> not recycled")
	eq(t, 1, e.rt(1).ghOffline, "counter not advanced")
	eq(t, true, e.rt(1).ghSeen, "markers still updated")
	e.wakeAt = 0
	te.log(1, "2024-01-01 00:00:00Z: Running job: build")
	offlineTwice()
	eq(t, "", joinInts(te.stopped), "running job in the log -> not recycled")
	te.log(1, "2024-01-01 00:00:00Z: Listening for Jobs")
	busy = "true"
	offlineTwice()
	eq(t, "", joinInts(te.stopped), "GitHub busy -> not recycled")
	busy = "false"
	e.rt(1).ghOffline = 1
	te.stopped = nil
	e.rt(1).lastStart = te.now.Unix() - 60
	e.checkGithubHealth()
	eq(t, "", joinInts(te.stopped), "started < 120s ago -> grace period")
	offlineTwice()
	eq(t, "1", joinInts(te.stopped), "guards lifted -> recycled")
}

func TestEphemeral(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/owner/repo"
	te.runner(1, "")
	writeTrim(e.pidPath(1, "target"), u)
	eq(t, u, e.registeredURL(1), "falls back to runner-N.target")
	te.runner(2, u)
	writeTrim(e.pidPath(2, "target"), "https://github.com/owner/other")
	eq(t, u, e.registeredURL(2), ".runner wins")
	te.runner(3, "")
	eq(t, "", e.registeredURL(3), "neither")
	eq(t, false, e.isEphemeral(4), "no marker")
	writeTrim(e.pidPath(4, "ephemeral"), "")
	eq(t, true, e.isEphemeral(4), "marker")

	te.runner(5, "")
	te.log(5, "2024-01-01 12:00:00Z: Job build completed with result: Succeeded")
	eq(t, false, e.ephemeralJobFinished(5), "not ephemeral")
	writeTrim(e.pidPath(6, "ephemeral"), "")
	te.runner(6, u)
	te.log(6, "2024-01-01 12:00:00Z: Job build completed with result: Succeeded")
	eq(t, false, e.ephemeralJobFinished(6), ".runner still there")
	writeTrim(e.pidPath(7, "ephemeral"), "")
	te.runner(7, "")
	te.log(7, "2024-01-01 11:00:00Z: [runnermaxxer] re-registering ephemeral runner-7 (2024-01-01 11:00:00)", "2024-01-01 12:00:00Z: Job build completed with result: Succeeded")
	eq(t, true, e.ephemeralJobFinished(7), "completed newer than the marker")
	writeTrim(e.pidPath(8, "ephemeral"), "")
	te.runner(8, "")
	te.log(8, "2024-01-01 12:00:00Z: Job build completed with result: Succeeded", "2024-01-01 13:00:00Z: [runnermaxxer] re-registering ephemeral runner-8 (2024-01-01 13:00:00)")
	eq(t, false, e.ephemeralJobFinished(8), "marker newest")

	// supervise: finished ephemeral is relaunched without a fail or backoff
	e.rt(7).fails = 2
	e.rt(7).lastStart = te.now.Unix()
	e.superviseRunners()
	contains(t, " "+joinInts(te.started)+" ", " 7 ", "ephemeral relaunched")
	eq(t, 0, e.rt(7).fails, "no fail counted")
}

func TestRunnerEnv(t *testing.T) {
	te := newTestEnv(t, "SHARED_TOOL_CACHE=\"1\"\n")
	envOf := func() map[string]string {
		m := map[string]string{}
		for _, kv := range te.e.runnerEnv() {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}
	cache := filepath.Join(te.e.base, ".toolcache")
	m := envOf()
	eq(t, cache, m["RUNNER_TOOL_CACHE"], "RUNNER_TOOL_CACHE")
	eq(t, cache, m["AGENT_TOOLSDIRECTORY"], "AGENT_TOOLSDIRECTORY")
	eq(t, true, exists(cache), "cache dir created")
	eq(t, "1", m["ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE"], "deprecated exit code")
	te.e.cfg.vals["SHARED_TOOL_CACHE"] = "0"
	m = envOf()
	if _, ok := m["RUNNER_TOOL_CACHE"]; ok && os.Getenv("RUNNER_TOOL_CACHE") == "" {
		t.Error("RUNNER_TOOL_CACHE set with SHARED_TOOL_CACHE=0")
	}
	eq(t, "1", m["ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE"], "always set")
}

func TestRotateLog(t *testing.T) {
	te := newTestEnv(t, "MAX_LOG_SIZE_MB=\"1\"\n")
	e := te.e
	big := strings.Repeat("x", 1024*1024+10)
	os.WriteFile(e.logPath(1), []byte(big), 0o644)
	e.rotateLog(1)
	fi, _ := os.Stat(e.logPath(1))
	eq(t, int64(0), fi.Size(), "truncated in place")
	fi, _ = os.Stat(e.logPath(1) + ".1")
	eq(t, int64(len(big)), fi.Size(), "copied to .log.1")
	os.WriteFile(e.logPath(2), []byte("small\n"), 0o644)
	e.rotateLog(2)
	fi, _ = os.Stat(e.logPath(2))
	eq(t, int64(6), fi.Size(), "small log untouched")
}

func TestTickPollsFirstThenEveryN(t *testing.T) {
	te := newTestEnv(t, "GH_HEALTH_TICKS=\"3\"\n")
	e := te.e
	te.gh.set(func([]string) (string, string, int) { return "", "", 0 })
	e.tick()
	eq(t, 1, te.gh.count("rate_limit"), "first tick polls")
	e.tick()
	e.tick()
	eq(t, 1, te.gh.count("rate_limit"), "then waits")
	e.tick()
	eq(t, 2, te.gh.count("rate_limit"), "every GH_HEALTH_TICKS ticks")
	e.cfg.vals["GH_HEALTH_TICKS"] = "0"
	for i := 0; i < 5; i++ {
		e.tick()
	}
	eq(t, 2, te.gh.count("rate_limit"), "GH_HEALTH_TICKS=0: no polls")
	if _, err := e.Poll(); err != nil {
		t.Fatal(err)
	}
	eq(t, 3, te.gh.count("rate_limit"), "Poll polls anyway")
}

func TestEvents(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	for i := 0; i < eventRing+20; i++ {
		e.dlog("event")
	}
	eq(t, eventRing, len(e.Events()), "ring is bounded")
	data, _ := os.ReadFile(e.EventLogPath())
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	eq(t, eventRing+20, len(lines), "every event appended to the file")
	eq(t, te.now.Format("2006-01-02 15:04:05")+" event", lines[0], "line format")
}
