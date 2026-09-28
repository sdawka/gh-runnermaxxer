package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScript writes a shell script to dir that records its argv to
// argvFile (one arg per line) and exits with code.
func fakeScript(t *testing.T, dir string, code int, extra string) string {
	t.Helper()
	path := filepath.Join(dir, "fake.sh")
	argvFile := filepath.Join(dir, "argv.txt")
	body := fmt.Sprintf("#!/bin/sh\n: > %q\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\n%s\nexit %d\n", argvFile, argvFile, extra, code)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecRunArgvAndExitCode(t *testing.T) {
	dir := t.TempDir()
	script := fakeScript(t, dir, 0, `echo "out line"; echo "err line" >&2`)
	e := NewExec(script)

	res := e.Run(context.Background(), 0, "--scale", "a/b=3")
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (err=%v stderr=%q)", res.ExitCode, res.Err, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "out line") {
		t.Errorf("Stdout = %q, want it to contain 'out line'", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "err line") {
		t.Errorf("Stderr = %q, want it to contain 'err line'", res.Stderr)
	}

	argv, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "--scale\na/b=3\n"
	if string(argv) != want {
		t.Errorf("argv = %q, want %q", string(argv), want)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	script := fakeScript(t, dir, 2, "")
	e := NewExec(script)

	res := e.Run(context.Background(), 0, "--bogus")
	if res.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", res.ExitCode)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil (a non-zero exit is not a launch failure)", res.Err)
	}
}

func TestExecTimeout(t *testing.T) {
	dir := t.TempDir()
	script := fakeScript(t, dir, 0, "sleep 5")
	e := NewExec(script)

	start := time.Now()
	res := e.Run(context.Background(), 100*time.Millisecond, "--daemon")
	if res.Err == nil {
		t.Fatal("Err = nil, want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("Run took %s, want it killed near the 100ms timeout", elapsed)
	}
}

func TestExecStripsANSI(t *testing.T) {
	dir := t.TempDir()
	script := fakeScript(t, dir, 0, `printf '\033[31mred text\033[0m\n'`)
	e := NewExec(script)

	res := e.Run(context.Background(), 0)
	if strings.Contains(res.Stdout, "\x1b[") {
		t.Errorf("Stdout = %q, want ANSI stripped", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "red text") {
		t.Errorf("Stdout = %q, want it to still contain 'red text'", res.Stdout)
	}
}
