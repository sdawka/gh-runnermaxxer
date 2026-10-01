package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sdawka/gh-runnermaxxer/internal/engine"
)

func TestVersionFlag(t *testing.T) {
	stdout, _, code := captureRun(t, []string{"--version"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if want := "runnermaxxer " + engine.Version; !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
	}
}

func TestDirMustExist(t *testing.T) {
	_, stderr, code := captureRun(t, []string{"--dir", t.TempDir() + "/missing"})
	if code != 1 || !strings.Contains(stderr, "not a directory") {
		t.Fatalf("code = %d, stderr = %q; want 1 and a not-a-directory error", code, stderr)
	}
}

// A second instance on the same directory refuses to start (before any
// terminal handling) instead of fighting the first over the runners.
func TestSecondInstanceRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNNER_BASE_DIR", "")
	eng, err := engine.Open(context.Background(), engine.Options{Dir: dir, GH: "/nonexistent/gh"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Shutdown(context.Background(), engine.StopNow)

	_, stderr, code := captureRun(t, []string{"--dir", dir})
	if code != 1 || !strings.Contains(stderr, "already managing") {
		t.Fatalf("code = %d, stderr = %q; want 1 and an already-managing error", code, stderr)
	}
}

// captureRun runs main's run() with stdout/stderr redirected to temp files
// and returns their contents plus the exit code.
func captureRun(t *testing.T, args []string) (string, string, int) {
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
	return readAll(t, outFile), readAll(t, errFile), code
}

func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.ReadFrom(f); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
