package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/engine"
	"github.com/sdawka/gh-runnermaxxer/internal/logtail"
	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// tickEvery drives the model's 1s clock.
func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// waitForSnapshot blocks for the engine's next snapshot and wraps it as a
// tea.Msg. Update re-issues this Cmd after each snapshot to keep listening;
// once the engine shuts down and closes the channel it returns nil, so the
// loop simply ends.
func waitForSnapshot(snaps <-chan state.Snapshot) tea.Cmd {
	if snaps == nil {
		return nil
	}
	return func() tea.Msg {
		snap, ok := <-snaps
		if !ok {
			return nil
		}
		return snapshotMsg{snap: snap}
	}
}

// opResult is what an engine call returns: the message to toast on
// success, or the error to toast on failure.
type opResult struct {
	msg string
	err error
}

// runOp runs a single engine call and reports its result. key identifies
// the in-flight operation ("target:<url>", "runner:<id>", or "global") so the
// model can look it up in m.inflight and unblock the row's spinner.
func runOp(key string, op Op, call func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		msg, err := call()
		return cmdResultMsg{key: key, op: op, res: opResult{msg, err}}
	}
}

// started immediately announces an Op before its call's result comes back,
// so the row can show a spinner without waiting a full round trip.
func started(key string, op Op) tea.Cmd {
	return func() tea.Msg { return cmdStartedMsg{key: key, op: op} }
}

// startedMany announces the same Op as in-flight under several keys at
// once, for an apply that touches more than one target in a single call.
func startedMany(keys []string, op Op) tea.Cmd {
	cmds := make([]tea.Cmd, len(keys))
	for i, k := range keys {
		cmds[i] = started(k, op)
	}
	return tea.Batch(cmds...)
}

// applyOp runs the pending-scale call and reports its result against
// every key it covers, so Update can clear all of their spinners together.
func applyOp(keys []string, op Op, call func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		msg, err := call()
		return applyResultMsg{keys: keys, op: op, res: opResult{msg, err}}
	}
}

// ghStatus fetches the GitHub status report for the 'c' modal.
func ghStatus(eng Engine) tea.Cmd {
	return func() tea.Msg {
		text, err := eng.GHStatus()
		return ghStatusMsg{text: text, err: err}
	}
}

// shutdown runs the engine's Shutdown and reports when it returns; Update
// quits the program on that message.
func shutdown(ctx context.Context, eng Engine, mode engine.ShutdownMode) tea.Cmd {
	return func() tea.Msg {
		return shutdownDoneMsg{err: eng.Shutdown(ctx, mode)}
	}
}

// startLogTail opens a logtail.Tail on path and reports its initial lines
// plus the update channel to keep following it. ctx should be a
// child context the caller can cancel independently (switching runners, or
// closing the pane) without tearing down the rest of the program.
// stream is the LogPane.stream of the pane it feeds, carried on every
// message so Update can route it (explicit log view vs. the side pane's
// live tail) and drop it once that pane has moved on.
func startLogTail(ctx context.Context, id, stream int, path string) tea.Cmd {
	return func() tea.Msg {
		lines, updates, err := logtail.Tail(ctx, path, logRingLimit)
		if err != nil {
			return logErrMsg{id: id, stream: stream, err: err}
		}
		return logTailStartedMsg{id: id, stream: stream, lines: lines, updates: updates}
	}
}

// listenLogUpdates reads the next Update off a logtail channel and wraps it
// as a tea.Msg; Update re-issues this after each message to keep listening,
// exactly like waitForSnapshot does for the engine's snapshots.
func listenLogUpdates(id, stream int, updates <-chan logtail.Update) tea.Cmd {
	if updates == nil {
		return nil
	}
	return func() tea.Msg {
		upd, ok := <-updates
		if !ok {
			return nil
		}
		if upd.Err != nil {
			return logErrMsg{id: id, stream: stream, err: upd.Err}
		}
		return logLinesMsg{id: id, stream: stream, lines: upd.Lines, reset: upd.Reset}
	}
}
