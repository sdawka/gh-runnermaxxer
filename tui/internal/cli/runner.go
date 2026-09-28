// Package cli locates runnermaxxer.sh and drives it through its scripting
// CLI. It never writes runner state itself; every mutation is one exec of
// the script with a typed verb's argv.
package cli

import (
	"bytes"
	"context"
	"os/exec"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// DefaultTimeout is the exec timeout for most verbs. --scale and --download
// get a longer one (they may set up runners / download a tarball).
const DefaultTimeout = 90 * time.Second

// LongTimeout is used for verbs that may take a while: --scale, --download.
const LongTimeout = 10 * time.Minute

// Result is the outcome of one script invocation.
type Result struct {
	Args     []string
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	Err      error // non-nil for launch failures (not for a non-zero exit)
}

// Runner executes runnermaxxer.sh with a set of arguments. It is an
// interface so tests can substitute a fake without touching a real script.
type Runner interface {
	Run(ctx context.Context, timeout time.Duration, args ...string) Result
}

// Exec is the real Runner: it execs the located script.
type Exec struct {
	ScriptPath string
}

// NewExec builds an Exec runner for the given script path.
func NewExec(scriptPath string) Exec {
	return Exec{ScriptPath: scriptPath}
}

// Run executes the script with args, killing it after timeout (or
// DefaultTimeout if timeout <= 0). Colour is stripped from stdout/stderr in
// case the script emits ANSI (it disables colour on non-tty stdout on its
// own, but the TUI can't assume the terminal check matches when it runs the
// script itself).
func (e Exec) Run(ctx context.Context, timeout time.Duration, args ...string) Result {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, e.ScriptPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// The script forks children (gh, ps, sleep). On timeout kill the whole
	// process group, and don't let a grandchild that still holds the pipes
	// keep Wait blocked past a short grace period.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	duration := time.Since(start)

	res := Result{
		Args:     args,
		Stdout:   ansi.Strip(stdout.String()),
		Stderr:   ansi.Strip(stderr.String()),
		Duration: duration,
	}

	if ctx.Err() == context.DeadlineExceeded {
		res.Err = context.DeadlineExceeded
		res.ExitCode = -1
		return res
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		return res
	}
	if err != nil {
		res.Err = err
		res.ExitCode = -1
		return res
	}
	res.ExitCode = 0
	return res
}
