package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/sdawka/gh-runner-swarm/tui/internal/state"
)

// renderTable renders the grouped runner table: a header row followed by
// one line per Row (target/Unconfigured headers plus their runners), the
// cursor's row highlighted. width narrows the label column and drops the
// JOB column below 70 columns (§3.2).
func (m Model) renderTable(width int) string {
	rs := m.rows()
	if len(rs) == 0 {
		return "  (no targets configured yet - press 'a' to add one)"
	}

	showJob := width >= 70
	labelWidth := 40
	if width < 70 {
		labelWidth = 24
	}

	lines := make([]string, 0, len(rs)+1)
	lines = append(lines, m.tableColumnHeader(showJob))
	for i, row := range rs {
		selected := i == m.Cursor
		switch row.Kind {
		case RowTarget:
			lines = append(lines, m.renderTargetRow(row.Target, selected, labelWidth))
		case RowUnassignedHeader:
			lines = append(lines, m.renderUnassignedHeader(selected))
		case RowRunner:
			lines = append(lines, m.renderRunnerRow(row.Runner, selected, showJob))
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) tableColumnHeader(showJob bool) string {
	if showJob {
		return "  PROJECT / RUNNER                        PID     STATE      JOB"
	}
	return "  PROJECT / RUNNER              PID     STATE"
}

func (m Model) cursorMarker(selected bool) string {
	if selected {
		return m.Glyphs.Selected
	}
	return " "
}

func (m Model) renderTargetRow(t state.Target, selected bool, labelWidth int) string {
	marker := m.cursorMarker(selected)
	label := truncate(t.Label, labelWidth)

	have := t.Have
	pending := m.Pending.Get(t.URL, have)
	countStr := fmt.Sprintf("%d", have)
	if pending != have {
		countStr = m.Theme.Pending.Render(fmt.Sprintf("%d -> %d", have, pending))
	}

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
		tail = append(tail, m.Theme.Error.Render(fmt.Sprintf("%s %s", m.Glyphs.Warn, t.Error.Message)))
	}

	line := fmt.Sprintf("%s %s %-*s  %s/%d", marker, m.Glyphs.HeaderOpen, labelWidth, label, countStr, have)
	if len(tail) > 0 {
		line += "  " + strings.Join(tail, "  ")
	}
	return line
}

func (m Model) renderUnassignedHeader(selected bool) string {
	marker := m.cursorMarker(selected)
	return fmt.Sprintf("%s %s Unconfigured (incomplete setup - remove with d)", marker, m.Glyphs.HeaderOpen)
}

func (m Model) renderRunnerRow(r state.Runner, selected bool, showJob bool) string {
	marker := m.cursorMarker(selected)
	glyph, style := m.runnerGlyph(r)

	pid := "-"
	if r.PID != 0 {
		pid = fmt.Sprintf("%d", r.PID)
	}

	status := string(r.State)
	switch {
	case r.Quarantined:
		status = "quarantined"
	case r.Draining:
		status = fmt.Sprintf("draining: %s", r.Status)
	case r.Backoff(m.Now):
		status = fmt.Sprintf("backoff %ds", int(r.NextRetry-m.Now.Unix()))
	case r.Status != "":
		status = r.Status
	}

	line := fmt.Sprintf("%s   %s %-8s %-7s %s", marker, style.Render(glyph), r.Name, pid, status)

	if showJob && r.Job != "" {
		elapsed := r.Elapsed
		if r.JobStarted > 0 {
			elapsed = int(m.Now.Unix() - r.JobStarted)
		}
		line += "  " + fmt.Sprintf("%s (%s)", r.Job, formatDuration(elapsed))
	}
	if r.Ephemeral {
		line += "  " + m.Theme.Dim.Render("ephemeral")
	}
	if (r.Quarantined || r.LastErr != "") && r.LastErr != "" {
		line += "  " + m.Theme.Dim.Render(r.LastErr)
	}
	return line
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
