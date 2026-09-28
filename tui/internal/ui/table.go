package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

// Table column geometry (§3.2). The marker, PID and STATE columns are
// fixed; the PROJECT / RUNNER label column grows with the available width
// (tableLabelMin..tableLabelMax) and JOB takes whatever is left, truncated
// with an ellipsis rather than wrapping.
const (
	tableMarkerW  = 2 // cursor marker + space
	tablePIDW     = 7
	tableStateW   = 12
	tableLabelMin = 24
	tableLabelMax = 60
	tableJobMin   = 20 // spare width JOB keeps before the label column grows
	tableJobDrop  = 6  // below this JOB is dropped entirely
	scrollMargin  = 2  // rows kept visible above/below the cursor when scrolling
)

// tableCols is the resolved column layout for one render of the table.
type tableCols struct {
	width int // total table width; every row is truncated to it
	label int // PROJECT / RUNNER column width
	job   int // JOB column width, 0 when the table is too narrow for one
	// inlineErr appends a truncated lasterr / target error to rows; it's
	// off whenever a detail block is on screen to show the full text.
	inlineErr bool
}

// tableColumns sizes the table for width columns: the label column gets
// as much of the spare width (beyond the fixed columns and JOB's minimum)
// as the longest label needs, within tableLabelMin..tableLabelMax, and
// JOB gets everything else.
func (m Model) tableColumns(width int, inlineErr bool) tableCols {
	return tableColumns(width, m.widestLabel(), inlineErr)
}

// widestLabel is the label column width the current rows would need to
// show every target label / runner name untruncated.
func (m Model) widestLabel() int {
	widest := 0
	for _, row := range m.rows() {
		w := 0
		switch row.Kind {
		case RowTarget:
			w = 2 + ansi.StringWidth(row.Target.Label)
		case RowRunner:
			w = 4 + ansi.StringWidth(row.Runner.Name)
		}
		if w > widest {
			widest = w
		}
	}
	return widest
}

func tableColumns(width, widest int, inlineErr bool) tableCols {
	fixed := tableMarkerW + 1 + tablePIDW + 1 + tableStateW + 1
	label := width - fixed - tableJobMin
	if label > widest {
		label = widest
	}
	if label < tableLabelMin {
		label = tableLabelMin
	}
	if label > tableLabelMax {
		label = tableLabelMax
	}
	job := width - fixed - label
	if job < tableJobDrop {
		job = 0
	}
	return tableCols{width: width, label: label, job: job, inlineErr: inlineErr}
}

// tableLineCount is how many lines the table wants with no height limit:
// the column header plus one per row (or the single empty-state line).
func (m Model) tableLineCount() int {
	n := len(m.rows())
	if n == 0 {
		return 1
	}
	return n + 1
}

// tableWindow resolves which rows are on screen for a table given height
// lines in total (column header included; <= 0 means unbounded).
func (m Model) tableWindow(height int) scrollWindow {
	rowsHeight := height - 1
	if height <= 0 {
		rowsHeight = 0
	}
	return scrollTable(len(m.rows()), rowsHeight, m.Cursor, m.TableOffset, scrollMargin)
}

// renderTable renders the grouped runner table: a column header followed
// by the rows (target/Unconfigured headers plus their runners), the
// cursor's row highlighted. height is how many lines the table may use in
// total (header included); when there are more rows than fit, the window
// scrolls to keep the cursor visible and "N more" indicators mark what's
// cut off above/below. height <= 0 means unbounded. Every line is
// truncated to cols.width so nothing wraps.
func (m Model) renderTable(cols tableCols, height int) []string {
	rs := m.rows()
	if len(rs) == 0 {
		return []string{m.fit("  (no targets configured yet - press 'a' to add one)", cols.width)}
	}

	lines := []string{m.fit(m.tableColumnHeader(cols), cols.width)}
	win := m.tableWindow(height)
	if win.showAbove {
		lines = append(lines, m.fit(m.Theme.Dim.Render(fmt.Sprintf("  %s %d more", m.Glyphs.MoreUp, win.offset)), cols.width))
	}
	for i := win.offset; i < win.offset+win.count; i++ {
		lines = append(lines, m.fit(m.renderRow(rs[i], i == m.Cursor, cols), cols.width))
	}
	if win.showBelow {
		lines = append(lines, m.fit(m.Theme.Dim.Render(fmt.Sprintf("  %s %d more", m.Glyphs.MoreDown, len(rs)-win.offset-win.count)), cols.width))
	}
	return lines
}

func (m Model) renderRow(row Row, selected bool, cols tableCols) string {
	switch row.Kind {
	case RowTarget:
		return m.renderTargetRow(row.Target, selected, cols)
	case RowUnassignedHeader:
		return m.renderUnassignedHeader(selected)
	default:
		return m.renderRunnerRow(row.Runner, selected, cols)
	}
}

func (m Model) tableColumnHeader(cols tableCols) string {
	line := "  " + padRight("PROJECT / RUNNER", cols.label) + " " + padRight("PID", tablePIDW) + " " + padRight("STATE", tableStateW)
	if cols.job > 0 {
		line += " JOB"
	}
	return strings.TrimRight(line, " ")
}

func (m Model) cursorMarker(selected bool) string {
	if selected {
		return m.Glyphs.Selected
	}
	return " "
}

func (m Model) renderTargetRow(t state.Target, selected bool, cols tableCols) string {
	marker := m.cursorMarker(selected)
	label := m.truncate(t.Label, cols.label-2)

	have := t.Have
	pending := m.Pending.Get(t.URL, have)
	countStr := fmt.Sprintf("%d", have)
	switch {
	case pending != have:
		// A local edit differs from what's currently running: show where
		// it's headed once applied.
		countStr = m.Theme.Pending.Render(fmt.Sprintf("%d -> %d", have, pending))
	case t.Want != have:
		// No local edit, but the daemon's own Want differs from Have: a
		// scale it already accepted (ours or someone else's, or autoscale)
		// is still converging. Distinct from a TUI pending edit (§ gap
		// flagged after commit 7).
		countStr = m.Theme.Dim.Render(fmt.Sprintf("%d/%d", have, t.Want))
	}
	if _, inflight := m.Inflight[targetKey(t.URL)]; inflight {
		countStr = m.Spinner.View() + " " + countStr
	}
	count := fmt.Sprintf("%s/%d", countStr, have)

	var tail []string
	if !t.Listed {
		tail = append(tail, m.Theme.Dim.Render("(not in targets file)"))
	}
	if t.Autoscale {
		tail = append(tail, m.Theme.Dim.Render(fmt.Sprintf("auto %d-%d", t.Min, t.Max)))
	}
	if t.Queued > 0 {
		tail = append(tail, m.Theme.Dim.Render(fmt.Sprintf("queued %d", t.Queued)))
	}
	if t.Error != nil {
		// With a detail block on screen the row keeps only a short marker
		// (the error class); the detail block carries the full message.
		text := orDefault(t.Error.Class, "error")
		if cols.inlineErr {
			text = t.Error.Message
		}
		tail = append(tail, m.Theme.Error.Render(fmt.Sprintf("%s %s", m.Glyphs.Warn, text)))
	}

	line := marker + " " + padRight(m.Glyphs.HeaderOpen+" "+label, cols.label) + " " + padRight(count, tablePIDW+1+tableStateW)
	if len(tail) > 0 {
		line += " " + strings.Join(tail, "  ")
	}
	return strings.TrimRight(line, " ")
}

func (m Model) renderUnassignedHeader(selected bool) string {
	marker := m.cursorMarker(selected)
	return fmt.Sprintf("%s %s Unconfigured (incomplete setup - remove with d)", marker, m.Glyphs.HeaderOpen)
}

// runnerStateWord is the short STATE column text for a runner.
func (m Model) runnerStateWord(r state.Runner) string {
	switch {
	case r.Quarantined:
		return "quarantined"
	case r.Draining:
		return "draining"
	case r.Backoff(m.Now):
		return fmt.Sprintf("backoff %ds", int(r.NextRetry-m.Now.Unix()))
	case r.State != "":
		return string(r.State)
	default:
		return orDefault(r.Status, "?")
	}
}

// runnerJobText is the JOB column: the running job and its elapsed time,
// or else the daemon's free-text status when it says more than the STATE
// word does (e.g. "idle (last: Succeeded)").
func (m Model) runnerJobText(r state.Runner) string {
	if r.Job != "" {
		return fmt.Sprintf("%s (%s)", r.Job, formatDuration(m.runnerElapsed(r)))
	}
	status := strings.TrimRight(r.Status, ".")
	if status != "" && status != m.runnerStateWord(r) && status != string(r.State) {
		return m.Theme.Dim.Render(r.Status)
	}
	return ""
}

// runnerElapsed is how long the runner's current job has been going.
func (m Model) runnerElapsed(r state.Runner) int {
	if r.JobStarted > 0 {
		return int(m.Now.Unix() - r.JobStarted)
	}
	return r.Elapsed
}

func (m Model) renderRunnerRow(r state.Runner, selected bool, cols tableCols) string {
	marker := m.cursorMarker(selected)
	glyph, style := m.runnerGlyph(r)
	if _, inflight := m.Inflight[runnerKey(r.ID)]; inflight {
		glyph, style = m.Spinner.View(), m.Theme.Dim
	}

	pid := "-"
	if r.PID != 0 {
		pid = fmt.Sprintf("%d", r.PID)
	}

	name := "  " + style.Render(glyph) + " " + m.truncate(r.Name, cols.label-4)
	line := marker + " " + padRight(name, cols.label) + " " + padRight(pid, tablePIDW) + " " + padRight(m.runnerStateWord(r), tableStateW)

	var tail []string
	if cols.job > 0 {
		if job := m.runnerJobText(r); job != "" {
			tail = append(tail, job)
		}
	}
	if r.Ephemeral {
		tail = append(tail, m.Theme.Dim.Render("ephemeral"))
	}
	if cols.inlineErr && r.LastErr != "" {
		tail = append(tail, m.Theme.Dim.Render(r.LastErr))
	}
	if len(tail) > 0 {
		line += " " + strings.Join(tail, "  ")
	}
	return strings.TrimRight(line, " ")
}

// runnerGlyph returns the glyph and style for a runner's state, mirroring
// render_runner_line: green running/idle/busy, yellow draining/backoff, red
// stopped/quarantined.
func (m Model) runnerGlyph(r state.Runner) (string, lipgloss.Style) {
	switch {
	case r.Quarantined:
		return m.Glyphs.Quarantined, m.Theme.Error
	case r.Draining:
		return m.Glyphs.Running, m.Theme.Draining
	case r.Backoff(m.Now):
		return m.Glyphs.Backoff, m.Theme.Warn
	case r.State == state.RunnerBusy:
		return m.Glyphs.Running, m.Theme.Busy
	case r.State == state.RunnerRunning, r.State == state.RunnerIdle:
		return m.Glyphs.Running, m.Theme.Running
	case r.State == state.RunnerStopped:
		return m.Glyphs.Stopped, m.Theme.Stopped
	case r.State == state.RunnerRestarting:
		return m.Glyphs.Backoff, m.Theme.Warn
	default:
		return m.Glyphs.Stopped, m.Theme.Dim
	}
}
