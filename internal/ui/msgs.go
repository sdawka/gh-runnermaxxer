package ui

import (
	"time"

	"github.com/sdawka/gh-runnermaxxer/internal/logtail"
	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// snapshotMsg carries a fresh Snapshot from the engine.
type snapshotMsg struct{ snap state.Snapshot }

// tickMsg drives the 1s clock used for ages, elapsed columns and toast
// expiry.
type tickMsg time.Time

// cmdStartedMsg announces that an engine call has begun, so the row/target it
// touches can show a spinner immediately rather than waiting for the call
// to return.
type cmdStartedMsg struct {
	key string
	op  Op
}

// cmdResultMsg carries the outcome of an engine call.
type cmdResultMsg struct {
	key string
	op  Op
	res opResult
}

// applyResultMsg carries the outcome of applying pending scale edits: one
// Scale call covering every changed target, reported against all of their
// Inflight keys at once so each changed row's spinner clears together.
type applyResultMsg struct {
	keys []string
	op   Op
	res  opResult
}

// logTailStartedMsg reports the initial lines and follow channel from a
// freshly started logtail.Tail for id (a runner id, or eventLogID).
type logTailStartedMsg struct {
	id      int
	stream  int // LogPane.stream of the pane this tail was started for
	lines   []string
	updates <-chan logtail.Update
}

// logLinesMsg carries new lines for the log pane/viewer. reset is true
// right after the underlying file was truncated (rotation): the consumer
// should replace its buffer instead of appending.
type logLinesMsg struct {
	id     int
	stream int
	lines  []string
	reset  bool
}

// logErrMsg is a tail failure for runner id.
type logErrMsg struct {
	id     int
	stream int
	err    error
}

// ghStatusMsg carries the GitHub status report for the 'c' modal; text may
// be set alongside err (a partial report).
type ghStatusMsg struct {
	text string
	err  error
}

// setupResultMsg reports the first-run Setup call.
type setupResultMsg struct{ res opResult }

// shutdownDoneMsg reports that the engine's Shutdown returned: every runner
// is stopped (or finished its job) and the program can exit.
type shutdownDoneMsg struct{ err error }

// errMsg is a generic error toast. sticky errors persist until a keypress
// instead of expiring after the usual toast timeout.
type errMsg struct {
	err    error
	sticky bool
}
