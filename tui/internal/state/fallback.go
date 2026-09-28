package state

import (
	"context"
	"fmt"
	"strings"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
)

// CLIRunner is the minimal surface FromCLI needs, satisfied by
// cli.Client.StatusJSON. Declared as an interface here (rather than taking
// a concrete cli.Client) so tests can substitute a fake without an exec.
type CLIRunner interface {
	StatusJSON(ctx context.Context) cli.Result
}

// FromCLI loads a Snapshot by running `runnermaxxer.sh --status --json`
// ([bash-2]), for use when there is no state.json to watch (no daemon has
// ever run) or as an immediate reload right after a mutating verb, before
// the next tick would otherwise rewrite the file.
func FromCLI(ctx context.Context, c CLIRunner) (Snapshot, error) {
	res := c.StatusJSON(ctx)
	if res.Err != nil {
		return Snapshot{}, fmt.Errorf("running --status --json: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return Snapshot{}, fmt.Errorf("--status --json exited %d: %s", res.ExitCode, lastNonEmptyLine(res.Stderr))
	}
	return Parse([]byte(res.Stdout), SourceCLI)
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}
