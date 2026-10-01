package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSetupRunner(t *testing.T) {
	te := newTestEnv(t, "RUNNER_NAME_PREFIX=\"mac\"\nEPHEMERAL_RUNNERS=\"0\"\n")
	e := te.e
	repo := "https://github.com/o/r"
	makeFakeTarball(t, e, "2.334.0")
	argvPath := filepath.Join(e.base, "config.argv")
	te.gh.set(func([]string) (string, string, int) { return "tok123\n", "", 0 })

	if err := e.setupRunner(1, repo); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(argvPath)
	contains(t, string(argv), "--token tok123", "registration token")
	if strings.Contains(string(argv), "--pat") {
		t.Error("config.sh got --pat")
	}
	contains(t, string(argv), "--name mac-1 --url "+repo, "name and url")
	contains(t, string(argv), "--labels macos,arm64", "labels")
	eq(t, "2.334.0", readTrim(e.pidPath(1, "version")), "version from the tarball name")
	eq(t, repo, readTrim(e.pidPath(1, "target")), "target saved")
	eq(t, "mac-1", readTrim(e.pidPath(1, "name")), "name saved")
	eq(t, false, e.isEphemeral(1), "not ephemeral")
	eq(t, 1, te.gh.count("registration-token"), "one token per runner")
	eq(t, nil, e.setupRunner(1, repo), "already configured -> no-op")

	// token failure: nothing extracted
	te.gh.set(func([]string) (string, string, int) { return "", "gh: Must have admin rights (HTTP 403)", 1 })
	err := e.setupRunner(2, repo)
	if err == nil {
		t.Fatal("setup without a token succeeded")
	}
	contains(t, err.Error(), "403", "error names the 403")
	eq(t, false, e.runnerExists(2), "no dir on a token failure")
	e.targetClearError(repo, "")

	// config.sh fails after writing .runner: unregistered with a remove token
	te.gh.set(func([]string) (string, string, int) { return "tok123\n", "", 0 })
	os.WriteFile(filepath.Join(e.base, "config.rc"), []byte("1"), 0o644)
	if err := e.setupRunner(3, repo); err == nil {
		t.Fatal("setup with a failing config.sh succeeded")
	}
	os.Remove(filepath.Join(e.base, "config.rc"))
	eq(t, 1, te.gh.count("remove-token"), "remove token minted for the half registration")
	argv, _ = os.ReadFile(argvPath)
	contains(t, string(argv), "remove --token tok123", "config.sh remove called")
	eq(t, false, e.runnerExists(3), "dir removed")
	eq(t, false, exists(e.orphansPath()), "no orphan when unregistering worked")

	// ephemeral
	e.cfg.vals["EPHEMERAL_RUNNERS"] = "1"
	if err := e.setupRunner(4, repo); err != nil {
		t.Fatal(err)
	}
	eq(t, true, e.isEphemeral(4), "ephemeral marker")
	argv, _ = os.ReadFile(argvPath)
	contains(t, string(argv), "--ephemeral", "--ephemeral passed")

	// a leftover unconfigured dir is rebuilt
	os.MkdirAll(e.runnerDir(5), 0o755)
	os.WriteFile(filepath.Join(e.runnerDir(5), "junk"), nil, 0o644)
	if err := e.setupRunner(5, repo); err != nil {
		t.Fatal(err)
	}
	eq(t, false, exists(filepath.Join(e.runnerDir(5), "junk")), "rebuilt")
}

func TestReconfigureRunner(t *testing.T) {
	te := newTestEnv(t, "RUNNER_NAME_PREFIX=\"newprefix\"\n")
	e := te.e
	repo := "https://github.com/o/r"
	makeFakeTarball(t, e, "2.334.0")
	te.gh.set(func([]string) (string, string, int) { return "tok\n", "", 0 })
	e.cfg.vals["EPHEMERAL_RUNNERS"] = "1"
	if err := e.setupRunner(1, repo); err != nil {
		t.Fatal(err)
	}
	writeTrim(e.pidPath(1, "name"), "oldprefix-1")
	os.Remove(filepath.Join(e.runnerDir(1), ".runner")) // job done
	os.Remove(filepath.Join(e.base, "config.argv"))
	if err := e.reconfigureRunner(1); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(e.base, "config.argv"))
	contains(t, string(argv), "--name oldprefix-1 --url "+repo, "keeps the saved name")
	contains(t, string(argv), "--ephemeral", "ephemeral")
	logb, _ := os.ReadFile(e.logPath(1))
	contains(t, string(logb), "[runnermaxxer] re-registering ephemeral runner-1", "marker line")
	eq(t, true, e.isConfigured(1), "configured again")

	os.Remove(filepath.Join(e.runnerDir(1), ".runner"))
	te.gh.set(func([]string) (string, string, int) { return "", "gh: Must have admin rights (HTTP 403)", 1 })
	if err := e.reconfigureRunner(1); err == nil {
		t.Fatal("reconfigure without a token succeeded")
	}
	contains(t, e.rt(1).lastErr, "re-register failed", "lasterr")
	os.Remove(e.pidPath(1, "target"))
	if err := e.reconfigureRunner(1); err == nil {
		t.Fatal("reconfigure without a saved target succeeded")
	}
	eq(t, "not configured (no saved target)", e.rt(1).lastErr, "no target")
}

func TestRemoveRunnerReal(t *testing.T) {
	te := newTestEnv(t, "RUNNER_NAME_PREFIX=\"mac\"\n")
	e := te.e
	e.removeFn = e.removeRunnerReal
	repo := "https://github.com/o/r"
	makeFakeTarball(t, e, "2.334.0")
	te.gh.set(func([]string) (string, string, int) { return "tok\n", "", 0 })
	for _, id := range []int{1, 2, 3} {
		if err := e.setupRunner(id, repo); err != nil {
			t.Fatal(err)
		}
		te.log(id, "x")
	}
	eq(t, "", e.removeRunner(1), "clean removal")
	eq(t, false, e.runnerExists(1), "dir gone")
	eq(t, false, exists(e.logPath(1)), "log gone")
	eq(t, false, exists(e.pidPath(1, "target")), "state gone")
	eq(t, "1", joinInts(te.stopped), "stopped first")

	// target deleted on GitHub: treated as unregistered
	te.gh.set(func([]string) (string, string, int) { return "", "gh: Not Found (HTTP 404)", 1 })
	eq(t, "", e.removeRunner(2), "404 target counts as unregistered")
	eq(t, false, exists(e.orphansPath()), "no orphan")

	// remove token refused: recorded as an orphan
	te.gh.set(func([]string) (string, string, int) { return "", "gh: Must have admin rights (HTTP 403)", 1 })
	w := e.removeRunner(3)
	contains(t, w, "failed to unregister mac-3", "warning")
	orph, _ := os.ReadFile(e.orphansPath())
	eq(t, "mac-3\n", string(orph), "orphan recorded")
	eq(t, false, e.runnerExists(3), "dir removed anyway")
}

func TestStartRunnerReal(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	te := newTestEnv(t, "RUNNER_NAME_PREFIX=\"mac\"\n")
	e := te.e
	e.listProcs = psList
	e.startFn = e.startRunnerReal
	e.stopProcsFn = e.stopProcsReal
	repo := "https://github.com/o/r"
	makeFakeTarball(t, e, "2.334.0")
	te.gh.set(func([]string) (string, string, int) { return "tok\n", "", 0 })
	if err := e.setupRunner(1, repo); err != nil {
		t.Fatal(err)
	}
	old := startGrace
	startGrace = 300 * time.Millisecond
	defer func() { startGrace = old }()

	e.markStopped(1)
	if err := e.startRunner(1); err != nil {
		t.Fatal(err)
	}
	defer e.stopProcs([]int{1})
	e.invalidateProcs()
	eq(t, true, e.isRunning(1), "running")
	eq(t, false, e.isMarkedStopped(1), "stop marker cleared")
	pid := e.readPID(1)
	ps := e.runnerProcs(1)
	if len(ps) == 0 || !strings.Contains(ps[0].cmd, e.runnerDir(1)+"/run.sh") {
		t.Errorf("command line lacks the absolute run.sh path: %+v", ps)
	}
	eq(t, nil, e.startRunner(1), "already running -> no-op")
	eq(t, pid, e.readPID(1), "same pid")

	// adopt: lose the pid file, start finds the live tree
	os.Remove(e.pidPath(1, "pid"))
	e.invalidateProcs()
	eq(t, nil, e.startRunner(1), "adopt")
	eq(t, pid, e.readPID(1), "adopted the same process")
	contains(t, strings.Join(e.Events(), "\n"), "adopted running process "+strconv.Itoa(pid), "adopt event")

	e.stopRunners([]int{1})
	e.invalidateProcs()
	eq(t, false, e.isRunning(1), "stopped")
	eq(t, 0, len(e.runnerProcs(1)), "no processes left")
	eq(t, true, e.isMarkedStopped(1), "marked stopped")

	// crash at start
	os.WriteFile(filepath.Join(e.runnerDir(1), "crash"), nil, 0o644)
	err := e.startRunner(1)
	if err == nil {
		t.Fatal("crashing runner started")
	}
	eq(t, "process exited immediately (see its log)", e.rt(1).lastErr, "lasterr")
	eq(t, false, exists(e.pidPath(1, "pid")), "pid file removed")

	os.Remove(filepath.Join(e.runnerDir(1), ".runner"))
	if err := e.startRunner(1); err == nil {
		t.Fatal("unconfigured runner started")
	}
	eq(t, "not configured (incomplete setup)", e.rt(1).lastErr, "unconfigured")
	if err := e.startRunner(9); err == nil {
		t.Fatal("missing runner started")
	}
	eq(t, "runner directory missing", e.rt(9).lastErr, "missing dir")
}
