package cli

import (
	"os"
	"path/filepath"
)

// Paths mirrors the directory layout runnermaxxer.sh computes at startup
// (runnermaxxer.sh:20,25,40,41,44), so the TUI can find state.json and the
// per-runner logs without asking the script.
type Paths struct {
	ScriptPath string // the located runnermaxxer.sh itself, for cli.Daemon
	ScriptDir  string // dir containing runnermaxxer.sh
	RunnerBase string // $RUNNER_BASE_DIR or ScriptDir/runners
	PIDDir     string // RunnerBase/.pids
	LogDir     string // RunnerBase/.logs
	StateFile  string // PIDDir/state.json
	DaemonPID  string // RunnerBase/.daemon.pid
	DaemonLog  string // LogDir/runnermaxxer.log (runnermaxxer.sh:46 DAEMON_LOG)
}

// NewPaths derives Paths from the location of runnermaxxer.sh, honouring
// RUNNER_BASE_DIR exactly like the script does.
func NewPaths(scriptPath string) Paths {
	scriptDir := filepath.Dir(scriptPath)
	runnerBase := os.Getenv("RUNNER_BASE_DIR")
	if runnerBase == "" {
		runnerBase = filepath.Join(scriptDir, "runners")
	}
	pidDir := filepath.Join(runnerBase, ".pids")
	logDir := filepath.Join(runnerBase, ".logs")
	return Paths{
		ScriptPath: scriptPath,
		ScriptDir:  scriptDir,
		RunnerBase: runnerBase,
		PIDDir:     pidDir,
		LogDir:     logDir,
		StateFile:  filepath.Join(pidDir, "state.json"),
		DaemonPID:  filepath.Join(runnerBase, ".daemon.pid"),
		DaemonLog:  filepath.Join(logDir, "runnermaxxer.log"),
	}
}
