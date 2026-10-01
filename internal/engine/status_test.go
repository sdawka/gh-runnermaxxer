package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

func TestLogRunnerStatus(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	eq(t, "no logs", e.getRunnerStatus(1), "missing log")
	os.WriteFile(e.logPath(2), nil, 0o644)
	eq(t, "no logs", e.getRunnerStatus(2), "empty log")
	cases := []struct {
		lines []string
		want  string
	}{
		{[]string{"2024-01-01 Starting Runner listener", "2024-01-01 Listening for Jobs"}, "idle"},
		{[]string{"2024-01-01 Listening for Jobs", "2024-01-01 Running job: build"}, "running: build"},
		{[]string{"2024-01-01 Listening for Jobs", "2024-01-01 Running job: build", "2024-01-01 Job build completed with result: Succeeded"}, "idle (last: Succeeded)"},
		{[]string{"2024-01-01 Could not connect to GitHub"}, "connection error"},
		{[]string{"2024-01-01 Authentication failed"}, "auth error"},
		{[]string{"2024-01-01 Starting Runner listener"}, "starting..."},
		{[]string{"2024-01-01 Exiting runner"}, "exiting"},
		{[]string{"2024-01-01 some unrelated chatter"}, "unknown"},
		{[]string{"2024-01-01 Running job: this-is-a-very-long-job-name-that-should-be-truncated"}, "running: this-is-a-very-long-job-n"},
	}
	for i, c := range cases {
		id := 10 + i
		te.log(id, c.lines...)
		eq(t, c.want, e.getRunnerStatus(id), c.want)
	}
}

func TestElapsed(t *testing.T) {
	ep, ok := logTSToEpoch("2023-11-14 22:13:20Z")
	eq(t, true, ok, "parses")
	eq(t, int64(1700000000), ep, "round-trips a known epoch")
	_, ok = logTSToEpoch("not a timestamp")
	eq(t, false, ok, "garbage fails")
	for secs, want := range map[int64]string{45: "45s", 720: "12m", 3900: "1h05m", 0: "0s"} {
		eq(t, want, formatDuration(secs), want)
	}
	te := newTestEnv(t, "")
	ts := te.now.Add(-65 * time.Second).UTC().Format("2006-01-02 15:04:05")
	te.log(1, ts+"Z: Running job: build")
	eq(t, "running: build (1m)", te.e.getRunnerStatus(1), "elapsed suffix")
	te.log(2, "not-a-real-timestamp Running job: build")
	eq(t, "running: build", te.e.getRunnerStatus(2), "no suffix for a malformed timestamp")
	job, start := te.e.logLastJob(1)
	eq(t, "build", job, "last job name")
	eq(t, te.now.Unix()-65, start, "last job start")
}

func TestExitReason(t *testing.T) {
	L := "2024-01-01 00:00:00Z"
	cases := []struct {
		lines         []string
		class, detail string
		name          string
	}{
		{[]string{L + ": Listening for Jobs", "Runner listener exit with Session Conflict error, stop the service, no retry needed."}, "conflict", "", "session conflict"},
		{[]string{"Runner listener exit with deprecated version exit code: 7."}, "deprecated", "", "deprecated"},
		{[]string{"Runner listener exit with terminated error, stop the service, no retry needed."}, "terminated", "", "terminated"},
		{[]string{"Exiting with unknown error code: 42"}, "unknown", "42", "unknown code"},
		{[]string{"Runner listener exit with retryable error, re-launch runner in 5 seconds."}, "", "", "retryable"},
		{[]string{"Runner listener exit because of updating, re-launch runner after successful update"}, "", "", "update"},
		{[]string{"Runner listener exit with Session Conflict error, stop the service, no retry needed.", L + ": Starting Runner listener", L + ": Listening for Jobs"}, "", "", "lifecycle after the reason"},
		{[]string{L + ": Running job: build", L + ": Job build completed with result: Succeeded", "Exiting with unknown error code: 3"}, "unknown", "3", "reason after a finished job"},
		{nil, "", "", "no log"},
		{[]string{L + ": Listening for Jobs", L + ": Authentication failed with status code 401"}, "creds", "", "credentials"},
	}
	for _, c := range cases {
		cl, d := exitReason(c.lines)
		eq(t, c.class+"/"+c.detail, cl+"/"+d, c.name)
	}
	eq(t, "runner credentials rejected - remove and re-add", exitNote("creds", ""), "note creds")
	if !strings.HasPrefix(exitNote("conflict", ""), "session conflict") {
		t.Error("conflict note")
	}
	contains(t, exitNote("deprecated", ""), "download", "deprecated note")
	eq(t, "listener exited with code 42 - see log", exitNote("unknown", "42"), "unknown note")
}

func TestRunnerVersion(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	te.runner(5, "")
	d := e.runnerDir(5)
	os.MkdirAll(filepath.Join(d, "bin.2.330.0"), 0o755)
	os.Symlink(filepath.Join(d, "bin.2.330.0"), filepath.Join(d, "bin"))
	writeTrim(e.pidPath(5, "version"), "2.320.0")
	eq(t, "2.330.0", e.runnerVersion(5), "symlink wins")
	os.Remove(filepath.Join(d, "bin"))
	os.Symlink("bin.2.331.0/", filepath.Join(d, "bin"))
	eq(t, "2.331.0", e.runnerVersion(5), "relative symlink with trailing slash")
	te.runner(6, "")
	writeTrim(e.pidPath(6, "version"), "2.320.0")
	eq(t, "2.320.0", e.runnerVersion(6), "version file")
	te.runner(7, "")
	os.MkdirAll(filepath.Join(e.runnerDir(7), "bin"), 0o755)
	os.WriteFile(filepath.Join(e.runnerDir(7), "bin", "Runner.Listener"), []byte("#!/bin/sh\necho 2.329.0\n"), 0o755)
	eq(t, "2.329.0", e.runnerVersion(7), "asks Runner.Listener")
	eq(t, "2.329.0", readTrim(e.pidPath(7, "version")), "cached")
	os.WriteFile(filepath.Join(e.runnerDir(7), "bin", "Runner.Listener"), []byte("#!/bin/sh\necho garbage\n"), 0o755)
	os.Remove(e.pidPath(7, "version"))
	delete(e.versionTried, 7)
	eq(t, "", e.runnerVersion(7), "non-version output ignored")
	eq(t, "", e.runnerVersion(8), "unknown runner")
}

func TestMainPIDAndProcs(t *testing.T) {
	eq(t, 0, mainPID(nil), "no processes")
	ps := []proc{
		{pid: 20, ppid: 10, cmd: "/x/runner-1/bin/Runner.Listener run"},
		{pid: 10, ppid: 1, cmd: "/bin/bash /x/runner-1/run-helper.sh"},
	}
	eq(t, 10, mainPID(ps), "tree root, not the first match")
	ps = append(ps, proc{pid: 30, ppid: 1, cmd: "/bin/bash /x/runner-1/run.sh"})
	eq(t, 30, mainPID(ps), "prefers the run.sh root")

	all := []proc{
		{pid: 1, cmd: "/bin/bash /b/runner-1/run.sh"},
		{pid: 2, cmd: "/bin/bash /b/runner-10/run.sh"},
		{pid: 3, cmd: "du -sk /b/runner-1/_work"},
	}
	eq(t, 1, len(matchProcs(all, "/b/runner-1/")), "runner-1 matches neither runner-10 nor du")

	te := newTestEnv(t, "")
	e := te.e
	te.runner(1, "")
	eq(t, false, e.isRunning(1), "no pid file")
	writeTrim(e.pidPath(1, "pid"), "4242")
	te.procs = []proc{{pid: 4242, ppid: 1, cmd: "/usr/bin/something-else"}}
	e.invalidateProcs()
	eq(t, false, e.isRunning(1), "pid reused by another command")
	te.procs = []proc{{pid: 4242, ppid: 1, cmd: "/bin/bash " + e.runnerDir(1) + "/run.sh"}}
	e.invalidateProcs()
	eq(t, true, e.isRunning(1), "running")
	writeTrim(e.pidPath(2, "pid"), "999999")
	eq(t, false, e.isRunning(2), "pid that doesn't exist")
}

func TestPsList(t *testing.T) {
	ps, err := psList()
	if err != nil {
		t.Fatal(err)
	}
	me := os.Getpid()
	for _, p := range ps {
		if p.pid == me {
			if p.cmd == "" {
				t.Error("own command line is empty")
			}
			return
		}
	}
	t.Error("own process not in the table")
}

func TestGHBusy(t *testing.T) {
	te := newTestEnv(t, "GH_HEALTH_TICKS=\"12\"\nREFRESH_INTERVAL=\"5\"\n")
	e := te.e
	eq(t, false, e.ghDataFresh(), "no poll yet")
	e.ghs.pollTS = te.now.Unix()
	eq(t, true, e.ghDataFresh(), "fresh")
	e.ghs.pollTS = te.now.Unix() - 121
	eq(t, false, e.ghDataFresh(), "stale")
	e.ghs.pollTS = te.now.Unix()
	e.cfg.vals["GH_HEALTH_TICKS"] = "0"
	eq(t, false, e.ghDataFresh(), "polling off -> never fresh")
	e.cfg.vals["GH_HEALTH_TICKS"] = "12"

	eq(t, false, e.ghKnown(1), "not seen")
	e.rt(1).ghSeen = true
	eq(t, true, e.ghKnown(1), "seen + fresh")
	e.ghs.pollTS = te.now.Unix() - 121
	eq(t, false, e.ghKnown(1), "stale data")
	e.ghs.pollTS = te.now.Unix()

	for _, id := range []int{2, 3, 4, 5} {
		te.runner(id, "https://github.com/o/r")
		te.setRunning(id, true)
	}
	te.log(2, "2024-01-01 Listening for Jobs")
	e.rt(2).ghSeen, e.rt(2).ghBusy = true, true
	eq(t, true, e.isBusy(2), "GitHub busy beats an idle log")
	e.rt(2).ghBusy = false
	te.log(2, "2024-01-01 Running job: build")
	eq(t, true, e.isBusy(2), "running log beats a not-busy flag")
	te.log(2, "2024-01-01 Listening for Jobs")
	eq(t, false, e.isBusy(2), "not busy by either")
	te.log(3, "2024-01-01 Running job: build")
	eq(t, true, e.isBusy(3), "unknown to GitHub: log running")
	te.log(3, "2024-01-01 Listening for Jobs")
	eq(t, false, e.isBusy(3), "unknown to GitHub: log idle")

	e.rt(4).ghSeen = true
	te.log(4, "2024-01-01 Running job: build")
	eq(t, "running: build", e.getRunnerStatus(4), "log running wins")
	e.rt(5).ghSeen, e.rt(5).ghBusy = true, true
	te.log(5, "2024-01-01 Listening for Jobs")
	eq(t, "busy (per GitHub)", e.getRunnerStatus(5), "GitHub upgrades to busy")
	te.setRunning(5, false)
	eq(t, false, e.isBusy(5), "not running is never busy")
}

func TestCheckGithubHealthMarkersAndRate(t *testing.T) {
	te := newTestEnv(t, "GH_HEALTH_TICKS=\"12\"\n")
	e := te.e
	u := "https://github.com/owner/repo"
	te.runner(1, u)
	te.runner(2, u)
	te.setRunning(1, true)
	te.setRunning(2, true)
	e.rt(2).ghSeen, e.rt(2).ghBusy = true, true
	e.ghs.probeState = "ok"
	te.gh.set(func(args []string) (string, string, int) {
		if strings.Contains(strings.Join(args, " "), "rate_limit") {
			return "4000\t0\n", "", 0
		}
		return "runner-1\tonline\ttrue\n", "", 0
	})
	e.checkGithubHealth()
	eq(t, true, e.rt(1).ghSeen, "seen")
	eq(t, true, e.rt(1).ghBusy, "busy")
	eq(t, te.now.Unix(), e.ghs.pollTS, "poll time recorded")
	eq(t, false, e.rt(2).ghSeen, "missing runner loses seen")
	eq(t, false, e.rt(2).ghBusy, "missing runner loses busy")
	eq(t, int64(4000), e.ghs.rateRem, "rate recorded")

	reset := te.now.Unix() + 600
	te.gh.set(func(args []string) (string, string, int) {
		if strings.Contains(strings.Join(args, " "), "rate_limit") {
			return "3\t" + strconv.FormatInt(reset, 10) + "\n", "", 0
		}
		return "runner-1\tonline\tfalse\n", "", 0
	})
	e.checkGithubHealth()
	eq(t, 0, te.gh.count("actions/runners"), "no listing with 3 left")
	eq(t, "ratelimit", e.ghState(), "ratelimit state")
	contains(t, e.ghs.errMsg, "resumes", "says when it resumes")
	eq(t, true, e.rt(1).ghBusy, "markers left alone")
	te.gh.set(te.gh.handler)
	e.checkGithubHealth()
	eq(t, 0, len(te.gh.calls), "until reset, not even rate_limit is called")
	te.now = time.Unix(reset+1, 0)
	te.gh.set(func(args []string) (string, string, int) {
		if strings.Contains(strings.Join(args, " "), "rate_limit") {
			return "4000\t0\n", "", 0
		}
		return "runner-1\tonline\tfalse\n", "", 0
	})
	e.checkGithubHealth()
	eq(t, true, te.gh.count("actions/runners") > 0, "poll runs again after the reset")
	eq(t, "ok", e.ghState(), "ratelimit clears")

	eq(t, "12 12 12 24 48", joinInts([]int{effectiveHealthTicks(12, 0), effectiveHealthTicks(12, 1), effectiveHealthTicks(12, 10), effectiveHealthTicks(12, 11), effectiveHealthTicks(12, 35)}), "effective health ticks")
}

func TestListingErrorDropsMarkers(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/o/r"
	te.runner(1, u)
	te.setRunning(1, true)
	e.rt(1).ghSeen, e.rt(1).ghBusy = true, true
	e.ghs.probeState = "ok"
	te.gh.set(func(args []string) (string, string, int) {
		if strings.Contains(strings.Join(args, " "), "rate_limit") {
			return "4000\t0\n", "", 0
		}
		return "", "gh: Not Found (HTTP 404)", 1
	})
	e.checkGithubHealth()
	eq(t, false, e.rt(1).ghSeen || e.rt(1).ghBusy, "markers dropped")
	got, _ := e.targetError(u)
	eq(t, "notfound", got.class, "target error recorded")
	eq(t, int64(0), e.ghs.pollTS, "no successful poll")
}

func TestSnapshot(t *testing.T) {
	te := newTestEnv(t, "RUNNER_NAME_PREFIX=\"mac\"\nAUTOSCALE=\"1\"\n")
	e := te.e
	u := "https://github.com/o/r"
	te.targets("o/r min=1 max=4\nmyorg\n")
	te.runner(1, u)
	te.runner(2, u)
	te.runner(3, u)
	te.runner(4, "")
	te.setRunning(1, true)
	te.setRunning(2, true)
	te.log(1, "2024-01-01 00:00:00Z: Listening for Jobs")
	ts := te.now.Add(-30 * time.Second).UTC().Format("2006-01-02 15:04:05")
	te.log(2, ts+"Z: Running job: build")
	e.markStopped(3)
	writeTrim(e.pidPath(1, "ephemeral"), "")
	writeTrim(e.pidPath(1, "version"), "2.334.0")
	e.rt(2).fails = 1
	e.want["o+r"] = 5
	e.queue["o+r"] = queueInfo{sat: 2, unsat: 1, hasUnsat: true}
	e.targetSetError("https://github.com/myorg", "scope", "token lacks the admin:org scope")
	e.appendOrphan("mac-9")

	s := e.buildSnapshot()
	eq(t, 2, s.Schema, "schema")
	eq(t, "tui", s.Writer, "writer")
	eq(t, os.Getpid(), s.DaemonPID, "daemon pid is this process")
	eq(t, te.now.Unix(), s.TickTS, "tick")
	eq(t, Version, s.ScriptVersion, "version")
	eq(t, "mac", s.Config.RunnerNamePrefix, "config")
	eq(t, true, s.Config.Autoscale, "config autoscale")
	eq(t, "github.com", s.GH.Host, "gh host")
	eq(t, state.GHStateUnknown, s.GH.State, "gh state")
	eq(t, 2, len(s.Targets), "targets")
	tg := s.Targets[0]
	eq(t, u, tg.URL, "target url")
	eq(t, "o/r", tg.Label, "label")
	eq(t, "repo", tg.Type, "type")
	eq(t, true, tg.Listed, "listed")
	eq(t, 5, tg.Want, "want from the pending scale")
	eq(t, 3, tg.Have, "have")
	eq(t, 2, tg.Running, "running")
	eq(t, 1, tg.Busy, "busy")
	eq(t, 1, tg.Min, "min")
	eq(t, 4, tg.Max, "max")
	eq(t, true, tg.Autoscale, "autoscale")
	eq(t, 2, tg.Queued, "queued")
	eq(t, 1, tg.Unsatisfiable, "unsatisfiable")
	if s.Targets[1].Error == nil || s.Targets[1].Error.Class != "scope" {
		t.Errorf("org error: %+v", s.Targets[1].Error)
	}
	eq(t, 4, len(s.Runners), "runners")
	byID := map[int]state.Runner{}
	for _, r := range s.Runners {
		byID[r.ID] = r
	}
	r1 := byID[1]
	eq(t, state.RunnerIdle, r1.State, "runner-1 idle")
	eq(t, 10001, r1.PID, "pid")
	eq(t, "runner-1", r1.Name, "name from .runner")
	eq(t, true, r1.Ephemeral, "ephemeral")
	eq(t, "2.334.0", r1.Version, "version")
	eq(t, e.logPath(1), r1.LogPath, "log path")
	r2 := byID[2]
	eq(t, state.RunnerBusy, r2.State, "runner-2 busy")
	eq(t, "build", r2.Job, "job")
	eq(t, 30, r2.Elapsed, "elapsed")
	eq(t, 1, r2.Fails, "fails")
	eq(t, state.RunnerStopped, byID[3].State, "runner-3 stopped")
	eq(t, "", byID[4].Target, "unassigned runner")
	eq(t, state.RunnerRestarting, byID[4].State, "unassigned not running -> restarting")
	contains(t, strings.Join(s.Warnings, "\n"), "orphaned GitHub registrations", "orphan warning")

	e.rt(3).quarantined = true
	e.unmarkStopped(3)
	e.rt(3).lastErr = "crash-looped 5x"
	s = e.buildSnapshot()
	for _, r := range s.Runners {
		if r.ID == 3 {
			eq(t, state.RunnerQuarantined, r.State, "quarantined")
			eq(t, "crash-looped 5x", r.LastErr, "lasterr")
		}
	}
	e.rt(4).lastStart = te.now.Unix()
	e.rt(4).fails = 2
	s = e.buildSnapshot()
	for _, r := range s.Runners {
		if r.ID == 4 {
			eq(t, te.now.Unix()+20, r.NextRetry, "next retry = laststart + 5*2^fails")
		}
	}
	e.quitting = true
	s = e.buildSnapshot()
	for _, r := range s.Runners {
		if r.ID == 1 {
			eq(t, state.RunnerDraining, r.State, "quitting: running shows draining")
		}
		if r.ID == 3 {
			eq(t, state.RunnerStopped, r.State, "quitting: others show stopped")
		}
	}
}
