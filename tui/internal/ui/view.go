package ui

import tea "charm.land/bubbletea/v2"

// View implements tea.Model. The real dashboard layout (status bar, banners,
// grouped runner table, log pane) lands in commit 7; until then this proves
// the model/update/key/theme/pending wiring runs under a real tea.Program.
func (m Model) View() tea.View {
	return tea.NewView("runnermaxxer-tui: dashboard view lands in a later commit")
}
