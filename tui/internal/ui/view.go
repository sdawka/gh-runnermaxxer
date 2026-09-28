package ui

import (
	"strings"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// View implements tea.Model: status bar, banners, the grouped runner table,
// and a footer that is either the short help line or the latest toast.
// Below 12 rows (§3.2) the footer help line is dropped to leave room for
// the table; '?' still opens the full help overlay.
func (m Model) View() tea.View {
	return tea.NewView(m.renderString())
}

// logPaneWidth is how much of the frame's width an open side log pane
// takes, when the terminal is wide enough for one (ShowLog).
func (m Model) logPaneWidth() int {
	w := m.Width / 3
	if w < 30 {
		w = 30
	}
	if w > m.Width-20 {
		w = m.Width - 20
	}
	if w < 1 {
		w = 1
	}
	return w
}

// renderString builds the full frame as plain text; split out from View so
// tests can assert on it directly without unwrapping a tea.View. When a log
// view is open and the terminal isn't wide enough for a side pane (§3.4),
// the log view takes over the whole frame instead.
func (m Model) renderString() string {
	if m.Log != nil && !m.ShowLog {
		return m.renderLogFullScreen()
	}
	if m.Screen == ScreenAddTarget && m.AddTarget != nil {
		return m.AddTarget.view()
	}
	if m.Screen == ScreenBounds && m.Bounds != nil {
		return m.Bounds.view()
	}
	if m.Screen == ScreenConfig && m.Config != nil {
		return m.Config.view(m.Snap.Config)
	}
	if m.Screen == ScreenGHStatus && m.GHStatusText != nil {
		return "GitHub status\n\n" + *m.GHStatusText + "\n\n" + m.Theme.Dim.Render("any key closes")
	}

	var b strings.Builder

	for _, line := range m.renderHeader() {
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, line := range m.renderBanners() {
		b.WriteString(line)
		b.WriteString("\n")
	}

	if m.Screen == ScreenProjects {
		b.WriteString(m.Theme.Dim.Render("-- Projects --"))
		b.WriteString("\n")
	}

	tableWidth := m.Width
	if m.Log != nil {
		tableWidth = m.Width - m.logPaneWidth() - 1
	}
	table := m.renderTable(tableWidth)
	if m.Log != nil {
		height := m.Height - 6
		if height < 5 {
			height = 5
		}
		m.Log.resize(m.logPaneWidth(), height)
		table = lipgloss.JoinHorizontal(lipgloss.Top, table, " "+m.Log.view())
	}
	b.WriteString(table)

	if m.Confirm != nil {
		b.WriteString("\n\n")
		b.WriteString(m.Confirm.view(m.Theme))
	}

	if footer := m.renderFooter(); footer != "" {
		b.WriteString("\n")
		b.WriteString(footer)
	}

	if m.Help {
		b.WriteString("\n\n")
		h := help.New()
		h.SetWidth(m.Width)
		b.WriteString(h.View(m.Keys))
	}

	return b.String()
}

// renderLogFullScreen replaces the whole frame with the open log view, for
// terminals too narrow to also show the side pane (§3.4).
func (m Model) renderLogFullScreen() string {
	height := m.Height - 2
	if height < 5 {
		height = 5
	}
	m.Log.resize(m.Width, height)
	footer := "Esc/q close  f follow  n/p next/prev runner  ↑↓ scroll"
	if m.Log.RunnerID == daemonLogID {
		footer = "Esc/q close  f follow  ↑↓ scroll"
	}
	return m.Log.view() + "\n" + m.Theme.Dim.Render(footer)
}

// renderFooter shows the most recent toast if one is active, else the
// short help line (suppressed below 12 rows so the table gets the space).
func (m Model) renderFooter() string {
	if len(m.Notices) > 0 {
		n := m.Notices[len(m.Notices)-1]
		style := m.Theme.Dim
		switch n.Level {
		case LevelWarn:
			style = m.Theme.Warn
		case LevelError:
			style = m.Theme.Error
		}
		return style.Render(n.Text)
	}
	if m.Height > 0 && m.Height < 12 {
		return ""
	}
	if m.Screen == ScreenProjects {
		return m.Theme.Dim.Render("↑↓ move  ←→/hl count  Enter apply & back  a add  x remove  b bounds  q/Esc back  ? help")
	}
	return m.Theme.Dim.Render("↑↓ move  ←→ count  Enter apply  Esc discard  d drain  x stop  s start  r restart  t projects  ? help")
}
