package ui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// manyRunnersSnapshot is 4 targets x 12 runners, more rows than a 50-row
// terminal holds, for the scrolling/wide-layout golden.
func manyRunnersSnapshot() state.Snapshot {
	snap := state.Snapshot{
		ScriptVersion: "4.0.0", DaemonPID: 500, TickTS: 1790612448,
		GH: state.GH{State: state.GHStateOK, User: "sahil", Scopes: []string{"repo"}, RateRemaining: 4990},
	}
	snap.Tarball.Version = "2.337.0"
	snap.Disk.PctFree = 63
	names := []string{"acme/api", "acme/web-frontend-monorepo", "acme/infra", "acme (org)"}
	id := 1
	for ti, label := range names {
		url := fmt.Sprintf("https://github.com/t%d", ti)
		tgt := state.Target{URL: url, Label: label, Type: "repo", Listed: true, Want: 12, Have: 12, Running: 12}
		if ti == 3 {
			tgt.Type = "org"
			tgt.Error = &state.TargetError{Class: "scope", Message: "token lacks admin:org; runners can't register", Since: 1790600000}
		}
		snap.Targets = append(snap.Targets, tgt)
		for i := 0; i < 12; i++ {
			r := state.Runner{
				ID: id, Name: fmt.Sprintf("mac-%d", id), Target: url, PID: 7000 + id,
				State: state.RunnerIdle, Status: "idle", Version: "2.337.0",
				LogPath: fmt.Sprintf("/abs/runners/.logs/runner-%d.log", id), WorkMB: 10 * id,
			}
			switch id % 7 {
			case 0:
				r.State, r.Status, r.Job, r.JobStarted = state.RunnerBusy, "running: integration-tests (linux, arm64, full matrix)", "integration-tests (linux, arm64, full matrix)", 1790612000
			case 3:
				r.State, r.Status, r.PID, r.Fails, r.NextRetry = state.RunnerRestarting, "restarting...", 0, 2, 1790612460
				r.LastErr = "exited unexpectedly: Runner listener exit with retryable error, re-launch runner in 5 seconds"
			case 5:
				r.Ephemeral = true
			}
			snap.Runners = append(snap.Runners, r)
			id++
		}
	}
	return snap
}

func TestDashboardGolden160x50ManyRunners(t *testing.T) {
	m := dashboardModel(t, 160, 50, manyRunnersSnapshot())
	// Walk the cursor past the bottom of the first screenful (the table
	// has 52 rows for 46 visible) and back up three, onto mac-38 (row 41,
	// a restarting runner with a long lasterr): the window has scrolled,
	// and stays put on the way back up since the cursor keeps its margin.
	for i := 0; i < 44; i++ {
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(Model)
	}
	for i := 0; i < 3; i++ {
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		m = updated.(Model)
	}
	m = feedTail(t, m)
	assertGolden(t, "dashboard_160x50_many", m.renderString())
}
