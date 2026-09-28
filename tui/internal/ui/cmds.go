package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/logtail"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
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

// startedMany announces the same Op as in-flight under several keys at
// once, for an apply that touches more than one target in a single exec.
func startedMany(keys []string, op Op) tea.Cmd {
	cmds := make([]tea.Cmd, len(keys))
	for i, k := range keys {
		cmds[i] = started(k, op)
	}
	return tea.Batch(cmds...)
}

// applyVerb execs the pending-scale exec and reports its result against
// every key it covers, so Update can clear all of their spinners together.
func applyVerb(ctx context.Context, keys []string, op Op, exec func(context.Context) cli.Result) tea.Cmd {
	return func() tea.Msg {
		res := exec(ctx)
		return applyResultMsg{keys: keys, op: op, res: res}
	}
}

// DaemonStarter starts the daemon detached, satisfied by cli.Daemon; an
// interface here so tests can substitute a fake instead of spawning a real
// process (§4.1).
type DaemonStarter interface {
	Start() error
}

// startDaemon runs d.Start() and reports whether it launched successfully.
// A nil error here only means the exec succeeded, not that the daemon is
// actually up yet - the caller still needs to poll for that (§4.1).
func startDaemon(d DaemonStarter) tea.Cmd {
	return func() tea.Msg {
		return daemonStartedMsg{err: d.Start()}
	}
}

// ghStatus execs --gh-status and reports its text for the 'c' modal.
func ghStatus(ctx context.Context, c cli.Client) tea.Cmd {
	return func() tea.Msg {
		res := c.GHStatus(ctx)
		if res.Err != nil {
			return ghStatusMsg{err: res.Err}
		}
		if res.ExitCode != 0 {
			text := res.Stderr
			if text == "" {
				text = res.Stdout
			}
			return ghStatusMsg{text: text}
		}
		return ghStatusMsg{text: res.Stdout}
	}
}

// startLogTail opens a logtail.Tail on path and reports its initial lines
// plus the update channel to keep following it. ctx should be a
// child context the caller can cancel independently (switching runners, or
// closing the pane) without tearing down the rest of the program.
func startLogTail(ctx context.Context, id int, path string) tea.Cmd {
	return func() tea.Msg {
		lines, updates, err := logtail.Tail(ctx, path, logRingLimit)
		if err != nil {
			return logErrMsg{id: id, err: err}
		}
		return logTailStartedMsg{id: id, lines: lines, updates: updates}
	}
}

// listenLogUpdates reads the next Update off a logtail channel and wraps it
// as a tea.Msg; Update re-issues this after each message to keep listening,
// exactly like watchSnapshot does for the state.Watcher channel.
func listenLogUpdates(id int, updates <-chan logtail.Update) tea.Cmd {
	if updates == nil {
		return nil
	}
	return func() tea.Msg {
		upd, ok := <-updates
		if !ok {
			return nil
		}
		if upd.Err != nil {
			return logErrMsg{id: id, err: upd.Err}
		}
		return logLinesMsg{id: id, lines: upd.Lines, reset: upd.Reset}
	}
}
