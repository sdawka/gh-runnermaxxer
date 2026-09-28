package ui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
)

// execStartDaemon implements the no-daemon banner's 'S' action (§4.1): runs
// the script detached and, once the exec itself succeeds, requests an
// immediate CLI reload so the dashboard picks up DaemonPID as soon as the
// daemon's first tick writes state.json. It shares globalKey with the
// runner -all verbs since starting the daemon and, say, stopping every
// runner should not race each other.
func (m Model) execStartDaemon() (Model, tea.Cmd) {
	if _, busy := m.Inflight[globalKey]; busy {
		m.notice("an operation is already in progress", LevelWarn)
		return m, nil
	}
	if m.Daemon == nil {
		m.notice("no script location known - restart the TUI with --script", LevelError)
		return m, nil
	}
	op := Op{Verb: "start-daemon", Started: m.Now}
	m.Inflight[globalKey] = op
	d := m.Daemon
	return m, startDaemon(d)
}

// confirmInstallService implements 'I': a login service (launchd/systemd
// --user) that runs the daemon automatically, so it survives logout/reboot
// (§4.1). Multi-line result, hence a confirm modal rather than a bare toast.
func (m Model) confirmInstallService() (Model, tea.Cmd) {
	m.Confirm = &ConfirmModel{
		Title: "Install a login service that runs the daemon (launchd / systemd --user)?",
		Choices: []Choice{
			{Key: "y", Label: "Install"},
			{Key: "n", Label: "Cancel"},
		},
		Default: 1,
		OnChoice: func(mm Model, choice string) (Model, tea.Cmd) {
			if choice == "y" {
				return mm.execInstallService()
			}
			return mm, nil
		},
	}
	return m, nil
}

func (m Model) execInstallService() (Model, tea.Cmd) {
	op := Op{Verb: "install-service", Started: m.Now}
	client := m.Client
	return m.startOp(globalKey, op, func(ctx context.Context) cli.Result { return client.InstallService(ctx) })
}

// execDownload implements 'g': only meaningful while the stale-tarball
// banner is showing (§3.4's "when the stale banner is showing"); otherwise
// it's a no-op toast rather than a pointless re-download.
func (m Model) execDownload() (Model, tea.Cmd) {
	if !m.Snap.Tarball.Stale {
		m.notice("tarball is already up to date", LevelInfo)
		return m, nil
	}
	op := Op{Verb: "download", Started: m.Now}
	client := m.Client
	return m.startOp("download", op, func(ctx context.Context) cli.Result { return client.Download(ctx) })
}

// openGHStatus implements 'c': runs --gh-status and shows its text in a
// modal (§3.4); the modal closing (any key) runs --poll so the next tick
// re-polls immediately, matching SUPERVISOR_TICK=$((GH_HEALTH_TICKS - 1)).
func (m Model) openGHStatus() (Model, tea.Cmd) {
	return m, ghStatus(m.ctx, m.Client)
}

func (m Model) closeGHStatus() (Model, tea.Cmd) {
	m.GHStatusText = nil
	m.Screen = ScreenDashboard
	client := m.Client
	return m, func() tea.Msg {
		return cmdResultMsg{key: "poll", op: Op{Verb: "poll"}, res: client.Poll(m.ctx)}
	}
}
