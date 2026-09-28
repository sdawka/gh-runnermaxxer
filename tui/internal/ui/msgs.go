package ui

import (
	"time"

	"github.com/sdawka/gh-runner-swarm/tui/internal/cli"
	"github.com/sdawka/gh-runner-swarm/tui/internal/state"
)

// snapshotMsg carries a freshly loaded Snapshot, from the watcher or the
// CLI fallback poll.
type snapshotMsg struct{ snap state.Snapshot }

// snapshotErrMsg is a load/parse failure; the model keeps showing its last
// good snapshot rather than blanking the screen.
type snapshotErrMsg struct{ err error }

// tickMsg drives the 1s clock used for ages, elapsed columns and toast
// expiry.
type tickMsg time.Time

// cmdStartedMsg announces that a verb exec has begun, so the row/target it
// touches can show a spinner immediately rather than waiting for the exec
// to return.
type cmdStartedMsg struct {
	key string
	op  Op
}

// cmdResultMsg carries the outcome of a verb exec (any exit code - a
// non-zero exit is still a normal result, not a Go error).
type cmdResultMsg struct {
	key string
	op  Op
	res cli.Result
}

// applyResultMsg carries the outcome of applying pending scale edits: one
// exec covering every changed target, reported against all of their
// Inflight keys at once so each changed row's spinner clears together.
type applyResultMsg struct {
	keys []string
	op   Op
	res  cli.Result
}

// logLinesMsg carries new lines for the log pane/viewer. reset is true
// right after the underlying file was truncated (rotation): the consumer
// should replace its buffer instead of appending.
type logLinesMsg struct {
	id    int
	lines []string
	reset bool
}

// logErrMsg is a tail failure for runner id.
type logErrMsg struct {
	id  int
	err error
}

// ghStatusMsg carries the text of `--gh-status` for the 'c' modal.
type ghStatusMsg struct {
	text string
	err  error
}

// daemonStartedMsg reports whether StartDaemon succeeded.
type daemonStartedMsg struct {
	pid int
	err error
}

// errMsg is a generic error toast. sticky errors persist until a keypress
// instead of expiring after the usual toast timeout.
type errMsg struct {
	err    error
	sticky bool
}
