package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runner-swarm/tui/internal/cli"
	"github.com/sdawka/gh-runner-swarm/tui/internal/state"
)

// tickEvery drives the model's 1s clock.
func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// watchSnapshot reads the next event off a state.Watcher's channel and
// wraps it as a tea.Msg. Update re-issues this Cmd after each message to
// keep listening; when events is nil (no watcher yet) or the channel has
// been closed, it returns nil so the batch simply drops it.
func watchSnapshot(events <-chan state.Event) tea.Cmd {
	if events == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-events
		if !ok {
			return nil
		}
		if ev.Err != nil {
			return snapshotErrMsg{err: ev.Err}
		}
		return snapshotMsg{snap: ev.Snapshot}
	}
}

// pollCLI runs the --status --json fallback once (for when there is no
// state.json to watch, per §4.1) and reports the result as a snapshot
// message just like the watcher does.
func pollCLI(ctx context.Context, c state.CLIRunner) tea.Cmd {
	return func() tea.Msg {
		snap, err := state.FromCLI(ctx, c)
		if err != nil {
			return snapshotErrMsg{err: err}
		}
		return snapshotMsg{snap: snap}
	}
}

// runVerb execs a single verb and reports its result. key identifies the
// in-flight operation ("target:<url>", "runner:<id>", or "global") so the
// model can look it up in m.inflight and unblock the row's spinner.
func runVerb(ctx context.Context, key string, op Op, exec func(context.Context) cli.Result) tea.Cmd {
	return func() tea.Msg {
		res := exec(ctx)
		return cmdResultMsg{key: key, op: op, res: res}
	}
}

// started immediately announces an Op before its exec's result comes back,
// so the row can show a spinner without waiting a full round trip.
func started(key string, op Op) tea.Cmd {
	return func() tea.Msg { return cmdStartedMsg{key: key, op: op} }
}
