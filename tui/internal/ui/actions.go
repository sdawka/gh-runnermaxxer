package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runner-swarm/tui/internal/cli"
	"github.com/sdawka/gh-runner-swarm/tui/internal/state"
)

// runnerKey builds the Inflight/Confirm key for a single runner's op.
func runnerKey(id int) string { return "runner:" + strconv.Itoa(id) }

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
// and run its exec, refusing to start a second op under the same key while
// one is already running (§2.4).
func (m Model) startOp(key string, op Op, exec func(context.Context) cli.Result) (Model, tea.Cmd) {
	if _, busy := m.Inflight[key]; busy {
		m.notice(fmt.Sprintf("%s already in progress", key), LevelWarn)
		return m, nil
	}
	return m, tea.Batch(started(key, op), runVerb(m.ctx, key, op, exec))
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
	return m.startOp(key, op, func(ctx context.Context) cli.Result { return m.Client.Drain(ctx, id) })
}

func (m Model) execStop(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "stop", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	return m.startOp(key, op, func(ctx context.Context) cli.Result { return m.Client.Stop(ctx, id) })
}

func (m Model) execStart(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "start", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	return m.startOp(key, op, func(ctx context.Context) cli.Result { return m.Client.Start(ctx, id) })
}

func (m Model) execRemove(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "remove", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	return m.startOp(key, op, func(ctx context.Context) cli.Result { return m.Client.Remove(ctx, id) })
}

// execRestart runs Stop then Start against the same id, sequentially inside
// one exec Cmd (there is no native "restart" verb on the bash side). Both
// calls block inside the Cmd's own goroutine, so this adds no nondeterminism
// to Update; the caller only ever sees the final result.
func (m Model) execRestart(id int) (Model, tea.Cmd) {
	key := runnerKey(id)
	op := Op{Verb: "restart", Args: []string{strconv.Itoa(id)}, Started: m.Now}
	client := m.Client
	return m.startOp(key, op, func(ctx context.Context) cli.Result {
		stopRes := client.Stop(ctx, id)
		if stopRes.Err != nil || stopRes.ExitCode != 0 {
			return stopRes
		}
		return client.Start(ctx, id)
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

// sequentialRunnerVerb runs verb for each id in turn inside the caller's Cmd
// goroutine, aggregating into one cli.Result (concatenated stdout/stderr,
// the last non-zero exit code wins) so the whole batch reports as a single
// cmdResultMsg.
func sequentialRunnerVerb(ctx context.Context, ids []int, verb func(context.Context, int) cli.Result) cli.Result {
	var agg cli.Result
	for _, id := range ids {
		res := verb(ctx, id)
		agg.Stdout += res.Stdout
		if res.ExitCode != 0 || res.Err != nil {
			agg.ExitCode = res.ExitCode
			agg.Stderr += res.Stderr
			if res.Err != nil {
				agg.Err = res.Err
			}
		}
	}
	return agg
}

func (m Model) execStopAll() (Model, tea.Cmd) {
	op := Op{Verb: "stop-all", Started: m.Now}
	ids := allRunnerIDs(m.Snap)
	client := m.Client
	busy := map[int]bool{}
	for _, id := range busyRunnerIDs(m.Snap) {
		busy[id] = true
	}
	return m.startOp(globalKey, op, func(ctx context.Context) cli.Result {
		return sequentialRunnerVerb(ctx, ids, func(ctx context.Context, id int) cli.Result {
			if busy[id] {
				return client.Drain(ctx, id)
			}
			return client.Stop(ctx, id)
		})
	})
}

func (m Model) execStartAll() (Model, tea.Cmd) {
	op := Op{Verb: "start-all", Started: m.Now}
	ids := allRunnerIDs(m.Snap)
	client := m.Client
	return m.startOp(globalKey, op, func(ctx context.Context) cli.Result {
		return sequentialRunnerVerb(ctx, ids, client.Start)
	})
}

func (m Model) execRestartAll() (Model, tea.Cmd) {
	op := Op{Verb: "restart-all", Started: m.Now}
	ids := allRunnerIDs(m.Snap)
	client := m.Client
	busy := map[int]bool{}
	for _, id := range busyRunnerIDs(m.Snap) {
		busy[id] = true
	}
	return m.startOp(globalKey, op, func(ctx context.Context) cli.Result {
		return sequentialRunnerVerb(ctx, ids, func(ctx context.Context, id int) cli.Result {
			if busy[id] {
				return client.Drain(ctx, id)
			}
			stopRes := client.Stop(ctx, id)
			if stopRes.Err != nil || stopRes.ExitCode != 0 {
				return stopRes
			}
			return client.Start(ctx, id)
		})
	})
}

// noticeFromResult turns an exec's cli.Result into a toast: the script's
// last stdout line on success, its last stderr line (or the bare exit code)
// on failure, per §2.4's "toast with the script's stdout last line".
func (m *Model) noticeFromResult(res cli.Result) {
	if res.Err != nil {
		m.notice(res.Err.Error(), LevelError)
		return
	}
	if res.ExitCode == 0 {
		if line := lastLine(res.Stdout); line != "" {
			m.notice(line, LevelInfo)
		}
		return
	}
	if line := lastLine(res.Stderr); line != "" {
		m.notice(line, LevelError)
		return
	}
	m.notice(fmt.Sprintf("exit %d", res.ExitCode), LevelError)
}

// lastLine returns the last non-empty line of s, or "".
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}
