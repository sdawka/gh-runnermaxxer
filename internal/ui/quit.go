package ui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/engine"
	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// quitState is how far along quitting is. Runners only live as long as the
// program, so quitting means stopping them: right away, or once their jobs
// finish.
type quitState int

const (
	quitNone     quitState = iota
	quitStopping           // Shutdown(StopNow) is running
	quitDraining           // Shutdown(DrainThenStop) is running; ctrl+c escalates
)

// runningRunnerIDs returns the ids of runners with a live process.
func runningRunnerIDs(snap state.Snapshot) []int {
	var ids []int
	for _, r := range snap.Runners {
		if r.PID != 0 {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// requestQuit is q / ctrl+c: confirm first when there are pending edits or
// in-flight operations that quitting would lose, then beginQuit.
func (m Model) requestQuit() (Model, tea.Cmd) {
	if !m.Pending.Empty() || len(m.Inflight) > 0 {
		m.Confirm = &ConfirmModel{
			Title: "Discard pending changes and quit?",
			Choices: []Choice{
				{Key: "y", Label: "Quit", Danger: true},
				{Key: "n", Label: "Cancel"},
			},
			Default: 1,
			OnChoice: func(mm Model, choice string) (Model, tea.Cmd) {
				if choice == "y" {
					return mm.beginQuit()
				}
				return mm, nil
			},
		}
		return m, nil
	}
	return m.beginQuit()
}

// beginQuit shuts the engine down and quits once it has. With no jobs
// running every runner is stopped right away; with jobs running the user
// chooses between cancelling them, letting them finish first, or staying.
func (m Model) beginQuit() (Model, tea.Cmd) {
	busy := busyRunnerIDs(m.Snap)
	if len(busy) == 0 {
		return m.quitStopNow()
	}
	m.Confirm = &ConfirmModel{
		Title: fmt.Sprintf("%d runner(s) are running jobs. Quitting stops every runner.", len(busy)),
		Choices: []Choice{
			{Key: "s", Label: fmt.Sprintf("Stop now (cancels %d job(s))", len(busy)), Danger: true},
			{Key: "w", Label: "Wait for jobs, then quit"},
			{Key: "c", Label: "Cancel"},
		},
		Default: 1,
		OnChoice: func(mm Model, choice string) (Model, tea.Cmd) {
			switch choice {
			case "s":
				return mm.quitStopNow()
			case "w":
				return mm.quitDrain()
			default:
				return mm, nil
			}
		},
	}
	return m, nil
}

func (m Model) quitStopNow() (Model, tea.Cmd) {
	m.Quit = quitStopping
	return m, shutdown(m.ctx, m.Engine, engine.StopNow)
}

// quitDrain lets every running job finish, stopping each runner once it is
// idle; the dashboard stays up showing progress until the last one stops.
func (m Model) quitDrain() (Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.Quit = quitDraining
	m.quitCancel = cancel
	return m, shutdown(ctx, m.Engine, engine.DrainThenStop)
}

// handleQuittingKey: while shutting down only ctrl+c does anything, and
// only while waiting for jobs, where it cancels them and stops right away.
func (m Model) handleQuittingKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" && m.Quit == quitDraining {
		m.Quit = quitStopping
		if m.quitCancel != nil {
			m.quitCancel()
		}
	}
	return m, nil
}

// quitLines is the progress overlay shown while quitting.
func (m Model) quitLines() []string {
	n := len(runningRunnerIDs(m.Snap))
	switch m.Quit {
	case quitDraining:
		return []string{
			m.Theme.Bold.Render(fmt.Sprintf("Waiting for %d runner(s) to finish their jobs, then quitting...", n)),
			"Each runner stops as soon as it is idle.",
			m.Theme.Dim.Render("ctrl+c stops them now (cancels the jobs)"),
		}
	case quitStopping:
		if n == 0 {
			return []string{m.Theme.Bold.Render("Quitting...")}
		}
		return []string{m.Theme.Bold.Render(fmt.Sprintf("Stopping %d runner(s)...", n))}
	}
	return nil
}
