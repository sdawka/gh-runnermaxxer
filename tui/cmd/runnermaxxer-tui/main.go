// Command runnermaxxer-tui is the Go + Bubble Tea client for runnermaxxer.sh.
//
// It never writes runner state itself: it only reads the daemon's
// state.json snapshot (or falls back to `runnermaxxer.sh --status --json`)
// and drives the bash script's scripting CLI for every mutation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/ui"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("runnermaxxer-tui", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		showVersion bool
		script      string
		runnerDir   string
		noUnicode   bool
		debugLog    string
	)
	fs.BoolVar(&showVersion, "version", false, "print the client version and exit")
	fs.StringVar(&script, "script", "", "path to runnermaxxer.sh (default: $RUNNERMAXXER_SCRIPT, a sibling of this binary, or PATH)")
	fs.StringVar(&runnerDir, "runner-dir", "", "override the runner base directory")
	fs.BoolVar(&noUnicode, "no-unicode", false, "use ASCII glyphs instead of Unicode symbols")
	fs.StringVar(&debugLog, "debug", "", "write debug logging to this file")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if showVersion {
		fmt.Fprintf(stdout, "runnermaxxer-tui %s\n", version)
		return 0
	}

	if debugLog != "" {
		f, err := tea.LogToFile(debugLog, "runnermaxxer-tui")
		if err != nil {
			fmt.Fprintf(stderr, "runnermaxxer-tui: --debug %s: %v\n", debugLog, err)
			return 1
		}
		defer f.Close()
	}

	scriptPath, err := cli.Locate(script)
	if err != nil {
		fmt.Fprintf(stderr, "runnermaxxer-tui: %v\n", err)
		return 1
	}

	if runnerDir != "" {
		// NewPaths reads RUNNER_BASE_DIR from the environment (mirroring
		// the script's own precedence); --runner-dir is the TUI-only
		// equivalent for callers that would rather not export a var.
		os.Setenv("RUNNER_BASE_DIR", runnerDir)
	}
	paths := cli.NewPaths(scriptPath)

	client := cli.NewClient(cli.NewExec(scriptPath))

	// Only watch state.json when it already exists: a fresh install with no
	// daemon ever having run has nothing to watch yet, and Model.Init falls
	// back to polling --status --json in that case (§4.1). Once a daemon
	// starts (including one the TUI itself launches via 'S'), the next
	// pollCLI-driven reload happens to work fine without a watcher too,
	// since nothing here depends on watcher-vs-poll after startup beyond
	// which one delivers snapshotMsg.
	var watcher *state.Watcher
	if _, statErr := os.Stat(paths.StateFile); statErr == nil {
		watcher = state.NewWatcher(paths.StateFile)
		defer watcher.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := ui.New(ctx, client, paths, watcher)
	m.Glyphs = ui.NewGlyphs(noUnicode)
	m.Daemon = cli.Daemon{ScriptPath: scriptPath}

	p := tea.NewProgram(m, tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(stderr, "runnermaxxer-tui: %v\n", err)
		return 1
	}
	return 0
}
