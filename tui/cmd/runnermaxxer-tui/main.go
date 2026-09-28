// Command runnermaxxer-tui is the Go + Bubble Tea client for runnermaxxer.sh.
//
// It never writes runner state itself: it only reads the daemon's
// state.json snapshot (or falls back to `runnermaxxer.sh --status --json`)
// and drives the bash script's scripting CLI for every mutation.
package main

import (
	"flag"
	"fmt"
	"os"
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

	// Startup wiring (script location, snapshot loading, tea.Program) lands
	// in later commits as internal/cli and internal/ui grow. For now the
	// scaffold only proves the module builds and --version works.
	_ = script
	_ = runnerDir
	_ = noUnicode
	_ = debugLog

	fmt.Fprintln(stderr, "runnermaxxer-tui: not yet implemented beyond --version")
	return 1
}
