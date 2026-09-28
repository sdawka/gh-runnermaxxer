package ui

import (
	"charm.land/bubbles/v2/viewport"
)

// logRingLimit is the log viewer's ring buffer size (Go design doc §3.4):
// old lines are dropped from the front once a runner's log has produced
// more than this many, so a long-lived busy runner can't grow the TUI's
// memory without bound.
const logRingLimit = 5000

// LogPane is the state behind an open log view: the README's 'l' (a
// runner's log) or 'L' (the daemon's log). It's rendered either as the
// permanent right-hand pane (when Model.ShowLog is true) or full-screen
// (otherwise) - the same LogPane either way, just a different View caller.
type LogPane struct {
	Title    string
	RunnerID int // the runner id this pane follows, or daemonLogID
	Lines    []string
	Viewport viewport.Model
	Follow   bool // true: auto-scroll to the newest line as it arrives
	Err      error

	// stream identifies the logtail.Tail feeding this pane, so messages
	// from a tail that has since been replaced (cursor moved, pane
	// switched) are dropped even when they carry the same runner id. path
	// is the file being followed ("" when there is none yet).
	stream int
	path   string
}

// matches reports whether a log message for (id, stream) belongs to lp.
// Safe on a nil pane.
func (lp *LogPane) matches(id, stream int) bool {
	return lp != nil && lp.RunnerID == id && lp.stream == stream
}

// newLogPane builds an empty pane sized to width x height, following the
// tail by default (§3.4's "follow-tail" behaviour).
func newLogPane(title string, runnerID, width, height int) *LogPane {
	return &LogPane{
		Title:    title,
		RunnerID: runnerID,
		Viewport: viewport.New(viewport.WithWidth(width), viewport.WithHeight(height)),
		Follow:   true,
	}
}

// resize adjusts the pane to a new width/height (e.g. a terminal resize, or
// switching between the side pane and full-screen sizes).
func (lp *LogPane) resize(width, height int) {
	lp.Viewport.SetWidth(width)
	lp.Viewport.SetHeight(height)
	if lp.Follow {
		lp.Viewport.GotoBottom()
	}
}

// setLines replaces the pane's whole buffer (the initial tail, or a reset
// after the underlying file was truncated by log rotation).
func (lp *LogPane) setLines(lines []string) {
	lp.Lines = capRing(lines)
	lp.refresh()
}

// appendLines adds newly tailed lines, trimming the ring buffer's front if
// it grows past logRingLimit.
func (lp *LogPane) appendLines(lines []string) {
	if len(lines) == 0 {
		return
	}
	lp.Lines = capRing(append(lp.Lines, lines...))
	lp.refresh()
}

func (lp *LogPane) refresh() {
	lp.Viewport.SetContentLines(lp.Lines)
	if lp.Follow {
		lp.Viewport.GotoBottom()
	}
}

func capRing(lines []string) []string {
	if len(lines) <= logRingLimit {
		return lines
	}
	return append([]string(nil), lines[len(lines)-logRingLimit:]...)
}

// view renders the pane: a title line (with a follow/paused indicator) over
// the viewport content.
func (lp *LogPane) view() string {
	status := "following"
	if !lp.Follow {
		status = "paused - press f to follow"
	}
	header := lp.Title + "  (" + status + ")"
	if lp.Err != nil {
		header += "  error: " + lp.Err.Error()
	}
	return header + "\n" + lp.Viewport.View()
}
