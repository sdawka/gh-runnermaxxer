package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLocateFlagWins(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "runnermaxxer.sh")
	writeExecutable(t, script)

	got, err := Locate(script)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != script {
		t.Errorf("Locate = %q, want %q", got, script)
	}
}

func TestLocateEnvVar(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "runnermaxxer.sh")
	writeExecutable(t, script)
	t.Setenv("RUNNERMAXXER_SCRIPT", script)

	got, err := Locate("")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != script {
		t.Errorf("Locate = %q, want %q", got, script)
	}
}

func TestLocateFlagBeatsEnv(t *testing.T) {
	dir := t.TempDir()
	flagScript := filepath.Join(dir, "flag.sh")
	envScript := filepath.Join(dir, "env.sh")
	writeExecutable(t, flagScript)
	writeExecutable(t, envScript)
	t.Setenv("RUNNERMAXXER_SCRIPT", envScript)

	got, err := Locate(flagScript)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != flagScript {
		t.Errorf("Locate = %q, want the flag value %q", got, flagScript)
	}
}

func TestLocatePATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH executable lookup test assumes a POSIX shell")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "runnermaxxer.sh")
	writeExecutable(t, script)
	t.Setenv("RUNNERMAXXER_SCRIPT", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := Locate("")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != script {
		t.Errorf("Locate = %q, want %q", got, script)
	}
}

func TestLocateSiblingOfExecutable(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "runnermaxxer.sh")
	writeExecutable(t, script)
	t.Setenv("RUNNERMAXXER_SCRIPT", "")
	t.Setenv("PATH", t.TempDir())

	fakeExe := filepath.Join(dir, "runnermaxxer-tui")
	restore := executablePath
	executablePath = func() (string, error) { return fakeExe, nil }
	defer func() { executablePath = restore }()

	got, err := Locate("")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != script {
		t.Errorf("Locate = %q, want sibling %q", got, script)
	}
}

func TestLocateParentOfExecutable(t *testing.T) {
	// A checkout: script at the repo root, binary built in tui/.
	root := t.TempDir()
	script := filepath.Join(root, "runnermaxxer.sh")
	writeExecutable(t, script)
	t.Setenv("RUNNERMAXXER_SCRIPT", "")
	t.Setenv("PATH", t.TempDir())
	t.Chdir(t.TempDir())

	fakeExe := filepath.Join(root, "tui", "runnermaxxer-tui")
	restore := executablePath
	executablePath = func() (string, error) { return fakeExe, nil }
	defer func() { executablePath = restore }()

	got, err := Locate("")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != script {
		t.Errorf("Locate = %q, want parent-dir script %q", got, script)
	}
}

func TestLocateCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "runnermaxxer.sh")
	writeExecutable(t, script)
	t.Setenv("RUNNERMAXXER_SCRIPT", "")
	t.Setenv("PATH", t.TempDir())
	t.Chdir(dir)

	restore := executablePath
	executablePath = func() (string, error) { return filepath.Join(t.TempDir(), "runnermaxxer-tui"), nil }
	defer func() { executablePath = restore }()

	got, err := Locate("")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != script {
		t.Errorf("Locate = %q, want cwd script %q", got, script)
	}
}

func TestLocateNotFound(t *testing.T) {
	t.Setenv("RUNNERMAXXER_SCRIPT", "")
	t.Setenv("PATH", t.TempDir())

	_, err := Locate("")
	if err == nil {
		t.Fatal("Locate succeeded, want a not-found error")
	}
}
