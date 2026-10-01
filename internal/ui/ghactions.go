package ui

import (
	tea "charm.land/bubbletea/v2"
)

// execDownload implements 'g': only meaningful while the stale-tarball
// banner is showing (§3.4's "when the stale banner is showing"); otherwise
// it's a no-op toast rather than a pointless re-download.
func (m Model) execDownload() (Model, tea.Cmd) {
	if !m.Snap.Tarball.Stale {
		m.notice("tarball is already up to date", LevelInfo)
		return m, nil
	}
	op := Op{Verb: "download", Started: m.Now}
	return m.startOp("download", op, m.Engine.Download)
}

// openGHStatus implements 'c': fetches the GitHub status report and shows
// it in a modal (§3.4); closing the modal (any key) polls GitHub right
// away, so the dashboard reflects whatever the check found.
func (m Model) openGHStatus() (Model, tea.Cmd) {
	return m, ghStatus(m.Engine)
}

func (m Model) closeGHStatus() (Model, tea.Cmd) {
	m.GHStatusText = nil
	m.Screen = ScreenDashboard
	return m, runOp("poll", Op{Verb: "poll"}, m.Engine.Poll)
}
