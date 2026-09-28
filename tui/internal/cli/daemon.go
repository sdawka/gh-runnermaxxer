package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// Daemon starts runnermaxxer.sh --daemon as a detached background process
// (Go design doc §4.1). It is deliberately not a Client verb: every other
// verb execs synchronously and captures output through Runner.Run, but the
// daemon is meant to keep running after the TUI exits, logging to its own
// file (DaemonLog) rather than to a captured buffer, so it gets its own
// exec.Command with no timeout and no wait.
type Daemon struct {
	ScriptPath string
}

// Start execs the script detached: a new session (Setsid) so it survives
// the TUI's process group, stdio redirected to /dev/null, then released
// immediately rather than waited on. The caller finds out whether it
// actually came up the same way `--status`/the watcher would: by polling
// for DaemonPID/state.json afterward (§4.1: "waits up to 10s for
// .daemon.pid and a first state.json").
func (d Daemon) Start() error {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	cmd := exec.Command(d.ScriptPath, "--daemon")
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
