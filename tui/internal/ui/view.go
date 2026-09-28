package ui

import (
	"strings"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"
)

// View implements tea.Model: status bar, banners, the grouped runner table,
// and a footer that is either the short help line or the latest toast.
// Below 12 rows (§3.2) the footer help line is dropped to leave room for
// the table; '?' still opens the full help overlay.
func (m Model) View() tea.View {
	return tea.NewView(m.renderString())
}

// renderString builds the full frame as plain text; split out from View so
// tests can assert on it directly without unwrapping a tea.View.
func (m Model) renderString() string {
	var b strings.Builder

	for _, line := range m.renderHeader() {
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, line := range m.renderBanners() {
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString(m.renderTable(m.Width))

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
	return m.Theme.Dim.Render("↑↓ move  ←→ count  Enter apply  Esc discard  d drain  x stop  s start  r restart  t projects  ? help")
}
