package cli

import (
	"path/filepath"
	"testing"
)

func TestNewPathsDefault(t *testing.T) {
	t.Setenv("RUNNER_BASE_DIR", "")
	p := NewPaths("/opt/runnermaxxer/runnermaxxer.sh")

	if p.RunnerBase != filepath.Join("/opt/runnermaxxer", "runners") {
		t.Errorf("RunnerBase = %q", p.RunnerBase)
	}
	if p.PIDDir != filepath.Join(p.RunnerBase, ".pids") {
		t.Errorf("PIDDir = %q", p.PIDDir)
	}
	if p.LogDir != filepath.Join(p.RunnerBase, ".logs") {
		t.Errorf("LogDir = %q", p.LogDir)
	}
	if p.StateFile != filepath.Join(p.PIDDir, "state.json") {
		t.Errorf("StateFile = %q", p.StateFile)
	}
	if p.DaemonPID != filepath.Join(p.RunnerBase, ".daemon.pid") {
		t.Errorf("DaemonPID = %q", p.DaemonPID)
	}
	if p.DaemonLog != filepath.Join(p.LogDir, "runnermaxxer.log") {
		t.Errorf("DaemonLog = %q", p.DaemonLog)
	}
}

func TestNewPathsRunnerBaseOverride(t *testing.T) {
	t.Setenv("RUNNER_BASE_DIR", "/custom/runners")
	p := NewPaths("/opt/runnermaxxer/runnermaxxer.sh")

	if p.RunnerBase != "/custom/runners" {
		t.Errorf("RunnerBase = %q, want /custom/runners", p.RunnerBase)
	}
}
