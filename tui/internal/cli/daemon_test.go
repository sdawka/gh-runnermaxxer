package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDaemonStartLaunchesDetached uses a tiny shell script standing in for
// runnermaxxer.sh: it writes a marker file after a moment, proving Start
// actually launched the process rather than merely building the Cmd.
func TestDaemonStartLaunchesDetached(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	script := filepath.Join(dir, "fake-runnermaxxer.sh")

	body := "#!/bin/sh\ntouch \"" + marker + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	d := Daemon{ScriptPath: script}
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("marker file was never created: the detached process did not run")
}
