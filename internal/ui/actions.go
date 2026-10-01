package ui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// runnerKey builds the Inflight/Confirm key for a single runner's op.
func runnerKey(id int) string { return "runner:" + strconv.Itoa(id) }

// targetKey builds the Inflight key for a target's scale-apply op.
func targetKey(url string) string { return "target:" + url }

// globalKey is the Inflight key for a fleet-wide op (an -all variant).
const globalKey = "global"

// allRunnerIDs returns every runner id in the snapshot, in listed order.
func allRunnerIDs(snap state.Snapshot) []int {
	ids := make([]int, 0, len(snap.Runners))
	for _, r := range snap.Runners {
		ids = append(ids, r.ID)
	}
	return ids
}

// busyRunnerIDs returns the ids of runners currently running a job.
func busyRunnerIDs(snap state.Snapshot) []int {
	var ids []int
	for _, r := range snap.Runners {
		if r.State == state.RunnerBusy {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// startOp records an Op as in-flight and issues the Cmds that announce it
// and run its engine call, refusing to start a second op under the same key while
// one is already running (§2.4).
func (m Model) startOp(key string, op Op, call func() (string, error)) (Model, tea.Cmd) {
	if _, busy := m.Inflight[key]; busy {
		m.notice(fmt.Sprintf("%s already in progress", key), LevelWarn)
		return m, nil
	}
	return m, tea.Batch(started(key, op), runOp(key, op, call))
}

// guardBusy is the busy-runner guard from the Go design doc's §4.4: stopping,
// restarting or draining a runner that is currently running a job first
// shows a confirm modal naming the job, offering to drain (let the job
// finish, then apply the requested action) or kill now (run aggressive
// immediately). A non-busy runner skips the modal and runs primary directly.
func (m Model) guardBusy(id int, primary, aggressive func(Model) (Model, tea.Cmd)) (Model, tea.Cmd) {
	r, ok := m.Snap.RunnerByID(id)
	if !ok {
		m.notice("select a runner", LevelWarn)
		return m, nil
	}
	if r.State != state.RunnerBusy {
		return primary(m)
	}
	job := orDefault(r.Job, "a job")
	m.Confirm = &ConfirmModel{
		Title: fmt.Sprintf("%s is running %q. Stopping cancels the job.", r.Name, job),
		Choices: []Choice{
			{Key: "d", Label: "Drain: remove after the job finishes"},
			{Key: "k", Label: "Kill now", Danger: true},
			{Key: "c", Label: "Cancel"},
		},
		Default: 0,
		OnChoice: func(mm Model, choice string) (Model, tea.Cmd) {
			switch choice {
			case "d":
				return mm.execDrain(id)
			case "k":
				return aggressive(mm)
			default:
				return mm, nil
			}
		},
	}
	return m, nil
}

func (m Model) execDrain(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "drain", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	eng := m.Engine
	return m.startOp(key, op, func() (string, error) { return eng.Drain(id) })
}

func (m Model) execStop(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "stop", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	eng := m.Engine
	return m.startOp(key, op, func() (string, error) { return eng.Stop(id) })
}

func (m Model) execStart(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "start", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	eng := m.Engine
	return m.startOp(key, op, func() (string, error) { return eng.Start(id) })
}

func (m Model) execRemove(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "remove", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	eng := m.Engine
	return m.startOp(key, op, func() (string, error) { return eng.Remove(id) })
}

// execRestart runs Stop then Start against the same id, sequentially inside
// one Cmd (the engine has no restart operation). Both
// calls block inside the Cmd's own goroutine, so this adds no nondeterminism
// to Update; the caller only ever sees the final result.
func (m Model) execRestart(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "restart", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	eng := m.Engine
	return m.startOp(key, op, func() (string, error) {
		if msg, err := eng.Stop(id); err != nil {
			return msg, err
		}
		return eng.Start(id)
	})
}

// confirmRemoveNow implements 'D': always confirms, busy or not, since it
// skips draining and removes the runner immediately (README/§3.4: "remove
// now (no drain), always confirms").
func (m Model) confirmRemoveNow(id int) (Model, tea.Cmd) {
	r, ok := m.Snap.RunnerByID(id)
	if !ok {
		m.notice("select a runner", LevelWarn)
		return m, nil
	}
	title := fmt.Sprintf("Remove %s now (no drain)?", r.Name)
	if r.State == state.RunnerBusy {
		title = fmt.Sprintf("%s is running %q. Removing now cancels the job.", r.Name, orDefault(r.Job, "a job"))
	}
	m.Confirm = &ConfirmModel{
		Title: title,
		Choices: []Choice{
			{Key: "y", Label: "Remove now", Danger: true},
			{Key: "n", Label: "Cancel"},
		},
		Default: 1,
		OnChoice: func(mm Model, choice string) (Model, tea.Cmd) {
			if choice == "y" {
				return mm.execRemove(id)
			}
			return mm, nil
		},
	}
	return m, nil
}

// confirmDrainPicker implements 'd' on a target header row. A full runner
// picker screen is out of scope for this commit (deviation, noted to
// "main"); for now it just tells the user to move onto a runner row first.
func (m Model) confirmDrainPicker() (Model, tea.Cmd) {
	m.notice("move onto a runner under this target, then press d", LevelInfo)
	return m, nil
}

// confirmAllVariant is the X/S/R-all guard (§4.4): when any runner is busy,
// confirm before draining the busy ones and running verb against the rest;
// with nothing busy, run immediately with no modal.
func (m Model) confirmAllVariant(verb string, run func(Model) (Model, tea.Cmd)) (Model, tea.Cmd) {
	busy := busyRunnerIDs(m.Snap)
	if len(busy) == 0 {
		return run(m)
	}
	m.Confirm = &ConfirmModel{
		Title: fmt.Sprintf("%d runner(s) are busy. Drain them and %s the rest?", len(busy), verb),
		Choices: []Choice{
			{Key: "y", Label: fmt.Sprintf("Drain busy, %s the rest", verb)},
			{Key: "n", Label: "Cancel"},
		},
		Default: 0,
		OnChoice: func(mm Model, choice string) (Model, tea.Cmd) {
			if choice == "y" {
				return run(mm)
			}
			return mm, nil
		},
	}
	return m, nil
}

// execStopAll, execStartAll and execRestartAll act on the whole fleet in
// one engine call each; confirmAllVariant has already got the user's
// go-ahead when any runner is busy. Restart is stop-all then start-all.
func (m Model) execStopAll() (Model, tea.Cmd) {
	op := Op{Verb: "stop-all", Started: m.Now}
	return m.startOp(globalKey, op, m.Engine.StopAll)
}

func (m Model) execStartAll() (Model, tea.Cmd) {
	op := Op{Verb: "start-all", Started: m.Now}
	return m.startOp(globalKey, op, m.Engine.StartAll)
}

func (m Model) execRestartAll() (Model, tea.Cmd) {
	op := Op{Verb: "restart-all", Started: m.Now}
	eng := m.Engine
	return m.startOp(globalKey, op, func() (string, error) {
		if msg, err := eng.StopAll(); err != nil {
			return msg, err
		}
		return eng.StartAll()
	})
}

// applyPending runs every pending scale edit in one Scale call, marking
// each changed target in-flight so its row can show a spinner. A no-op
// (nothing pending) or an apply already running for one of the changed
// targets is a toast, not a second call.
func (m Model) applyPending() (Model, tea.Cmd) {
	changes := m.Pending.Changes()
	if len(changes) == 0 {
		return m, nil
	}
	keys := make([]string, 0, len(changes))
	for url := range changes {
		key := targetKey(url)
		if _, busy := m.Inflight[key]; busy {
			m.notice("apply already in progress for "+url, LevelWarn)
			return m, nil
		}
		keys = append(keys, key)
	}
	op := Op{Verb: "scale", Started: m.Now}
	eng := m.Engine
	return m, tea.Batch(
		startedMany(keys, op),
		applyOp(keys, op, func() (string, error) { return eng.Scale(changes) }),
	)
}

// noticeFromResult turns an engine call's result into a toast: its message
// on success, its error on failure. Either can span several lines (a scale
// reports one line per target), which the one-line footer joins up.
func (m *Model) noticeFromResult(res opResult) {
	if res.err != nil {
		m.notice(oneLine(res.err.Error()), LevelError)
		return
	}
	if line := oneLine(res.msg); line != "" {
		m.notice(line, LevelInfo)
	}
}

// oneLine joins the non-empty lines of s with "; ".
func oneLine(s string) string {
	var parts []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, "; ")
}
