package ui

import "github.com/sdawka/gh-runner-swarm/tui/internal/state"

// RowKind identifies what a dashboard row shows.
type RowKind int

const (
	RowTarget RowKind = iota
	RowUnassignedHeader
	RowRunner
)

// Row is one line of the dashboard's grouped table: either a target's
// header row, the "Unconfigured" pseudo-group's header, or a runner row
// under one of those groups.
type Row struct {
	Kind   RowKind
	Target state.Target // valid when Kind == RowTarget
	Runner state.Runner // valid when Kind == RowRunner
}

// rows flattens a Snapshot into the dashboard's row list: each target
// followed by its runners, then (if any exist) the Unconfigured group and
// its runners. The cursor moves over *every* row, headers included (Go
// design doc §3.1), unlike the bash TUI's per-project cursor.
func rows(snap state.Snapshot) []Row {
	var out []Row
	for _, t := range snap.Targets {
		out = append(out, Row{Kind: RowTarget, Target: t})
		for _, r := range snap.RunnersFor(t.URL) {
			out = append(out, Row{Kind: RowRunner, Runner: r})
		}
	}
	if unassigned := snap.Unassigned(); len(unassigned) > 0 {
		out = append(out, Row{Kind: RowUnassignedHeader})
		for _, r := range unassigned {
			out = append(out, Row{Kind: RowRunner, Runner: r})
		}
	}
	return out
}

// rows returns the current dashboard rows for m.Snap.
func (m Model) rows() []Row {
	return rows(m.Snap)
}

// cursorRow returns the row the cursor is on, if any.
func (m Model) cursorRow() (Row, bool) {
	rs := m.rows()
	if m.Cursor < 0 || m.Cursor >= len(rs) {
		return Row{}, false
	}
	return rs[m.Cursor], true
}

// cursorOnHeaderRow reports whether the cursor is on a target's header row
// or the Unconfigured group's header, as opposed to a runner row.
func (m Model) cursorOnHeaderRow() bool {
	row, ok := m.cursorRow()
	return ok && row.Kind != RowRunner
}

// currentTargetURL resolves the cursor to the target its count keys should
// act on: the row's own target if it's a header, or its parent target if
// it's a runner row. Returns "" on the Unconfigured header or an
// out-of-range cursor (the count keys are then no-ops).
func (m Model) currentTargetURL() string {
	row, ok := m.cursorRow()
	if !ok {
		return ""
	}
	switch row.Kind {
	case RowTarget:
		return row.Target.URL
	case RowRunner:
		return row.Runner.Target
	default:
		return ""
	}
}

// currentRunnerID resolves the cursor to a runner id for the single-runner
// keys (d/x/s/r/l). ok is false on a header row.
func (m Model) currentRunnerID() (int, bool) {
	row, ok := m.cursorRow()
	if !ok || row.Kind != RowRunner {
		return 0, false
	}
	return row.Runner.ID, true
}
