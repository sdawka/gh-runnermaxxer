// Command runnermaxxer runs a fleet of self-hosted GitHub Actions runners
// from a terminal dashboard.
//
// The runner manager (internal/engine) lives in this process: runners are
// started, supervised and autoscaled for as long as the program runs, and
// quitting stops them (letting running jobs finish first if asked). A
// runner left behind by a crash is adopted by the next launch.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/engine"
	"github.com/sdawka/gh-runnermaxxer/internal/ui"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("runnermaxxer", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		showVersion bool
		dir         string
		noUnicode   bool
		debugLog    string
	)
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")
	fs.StringVar(&dir, "dir", ".", "directory holding .runnermaxxer.conf, .runnermaxxer.targets and the runner tarball (runners go in DIR/runners unless $RUNNER_BASE_DIR is set)")
	fs.BoolVar(&noUnicode, "no-unicode", false, "use ASCII glyphs instead of Unicode symbols")
	fs.StringVar(&debugLog, "debug", "", "write debug logging to this file")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if showVersion {
		fmt.Fprintf(stdout, "runnermaxxer %s\n", engine.Version)
		return 0
	}

	if debugLog != "" {
		f, err := tea.LogToFile(debugLog, "runnermaxxer")
		if err != nil {
			fmt.Fprintf(stderr, "runnermaxxer: --debug %s: %v\n", debugLog, err)
			return 1
		}
		defer f.Close()
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "runnermaxxer: --dir %s: %v\n", dir, err)
		return 1
	}
	if st, err := os.Stat(absDir); err != nil || !st.IsDir() {
		fmt.Fprintf(stderr, "runnermaxxer: --dir %s: not a directory\n", dir)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pending := &ui.SharedPending{}
	eng, err := engine.Open(ctx, engine.Options{Dir: absDir, Pending: pending.Get})
	if err != nil {
		fmt.Fprintf(stderr, "runnermaxxer: %v\n", err)
		return 1
	}
	// Runners only live as long as this process: whatever way run returns
	// (including a panic unwinding through here), stop them. Shutdown is
	// idempotent, so after a normal quit this is a no-op.
	defer eng.Shutdown(context.Background(), engine.StopNow)
	// The UI's context: cancelling it escalates a quit that is waiting for
	// jobs to finish to stopping right away, so the Shutdown calls here
	// never wait behind a drain. Deferred after the Shutdown above so it
	// runs first.
	uiCtx, escalate := context.WithCancel(ctx)
	defer escalate()

	m := ui.New(uiCtx, eng)
	m.Glyphs = ui.NewGlyphs(noUnicode)
	m.SharedPending = pending

	// Signals are ours rather than Bubble Tea's, so that SIGTERM, SIGHUP
	// (terminal closed) and SIGINT stop the runners before the program
	// exits. p.Quit then lets Run return normally, restoring the terminal.
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithoutSignalHandler())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigs:
			escalate()
			eng.Shutdown(context.Background(), engine.StopNow)
			p.Quit()
		case <-done:
		}
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(stderr, "runnermaxxer: %v\n", err)
		return 1
	}
	return 0
}
