package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	stdout, err := captureRun(t, []string{"--version"})
	if err != 0 {
		t.Fatalf("exit code = %d, want 0", err)
	}
	if !strings.Contains(stdout, "runnermaxxer-tui") {
		t.Fatalf("stdout = %q, want it to contain runnermaxxer-tui", stdout)
	}
}

func TestUnimplementedExitsNonZero(t *testing.T) {
	_, err := captureRun(t, []string{})
	if err == 0 {
		t.Fatalf("exit code = 0, want non-zero for the not-yet-implemented path")
	}
}

// captureRun runs main's run() with stdout/stderr redirected to temp files
// and returns stdout's contents plus the exit code.
func captureRun(t *testing.T, args []string) (string, int) {
	t.Helper()
	outFile, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer errFile.Close()

	code := run(args, outFile, errFile)

	var buf bytes.Buffer
	if _, err := outFile.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.ReadFrom(outFile); err != nil {
		t.Fatal(err)
	}
	return buf.String(), code
}
