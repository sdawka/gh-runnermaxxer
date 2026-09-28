package ui

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

// fakeRunner records every exec's argv, in call order, and returns a canned
// Result (zero value = exit 0, no output) unless a per-args override is set.
type fakeRunner struct {
	mu       sync.Mutex
	calls    [][]string
	override map[string]cli.Result
}

func (f *fakeRunner) Run(ctx context.Context, timeout time.Duration, args ...string) cli.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]string(nil), args...)
	f.calls = append(f.calls, cp)
	if f.override != nil {
		if res, ok := f.override[argsKey(args)]; ok {
			return res
		}
	}
	return cli.Result{ExitCode: 0}
}

func (f *fakeRunner) callArgs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

func argsKey(args []string) string {
	s := ""
	for _, a := range args {
		s += a + " "
	}
	return s
}

func newActionsTestModel(f *fakeRunner, snap state.Snapshot) Model {
	m := New(context.Background(), cli.NewClient(f), cli.Paths{}, nil)
	m.Snap = snap
	m.DaemonUp = snap.DaemonPID != 0
	m.Cursor = 0
	return m
}

func idleRunner(id int) state.Runner {
	return state.Runner{ID: id, Name: "runner-1", State: state.RunnerIdle}
}

func busyRunner(id int) state.Runner {
	return state.Runner{ID: id, Name: "runner-1", State: state.RunnerBusy, Job: "build"}
}

// runCmdSync executes a tea.Cmd synchronously and returns its message. Tests
// don't run a real tea.Program (which runs a Cmd's own goroutine and each
// tea.BatchMsg sub-Cmd concurrently), so this flattens any BatchMsg and runs
// its sub-Cmds in order, returning the last non-nil, non-cmdStartedMsg
// message - i.e. the eventual cmdResultMsg an Op's exec produces.
func runCmdSync(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	var last tea.Msg
	for _, sub := range batch {
		if m := runCmdSync(sub); m != nil {
			if _, started := m.(cmdStartedMsg); !started {
				last = m
			}
		}
	}
	return last
}

func TestGuardBusyNonBusyRunsPrimaryImmediately(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{idleRunner(1)}}
	m := newActionsTestModel(f, snap)

	got, cmd := m.execStop(1)
	if got.Confirm != nil {
		t.Fatal("Confirm modal opened for a non-busy runner, want none")
	}
	if cmd == nil {
		t.Fatal("execStop returned a nil Cmd")
	}
	// startOp's Cmd (announce + exec) only lands in Model.Inflight once
	// Update processes the resulting cmdStartedMsg/cmdResultMsg - it isn't
	// synchronous, so drive it through Update like the real program would.
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("execStop's Cmd = %T, want a 2-element tea.BatchMsg (started + exec)", cmd())
	}
	updated, _ := got.Update(batch[0]())
	got = updated.(Model)
	if _, inflight := got.Inflight[runnerKey(1)]; !inflight {
		t.Fatal("stop op not recorded as in-flight after its cmdStartedMsg")
	}
	if len(f.callArgs()) != 0 {
		t.Fatal("the exec ran before its result Cmd was invoked")
	}
	res := batch[1]().(cmdResultMsg)
	updated, _ = got.Update(res)
	got = updated.(Model)
	if _, inflight := got.Inflight[runnerKey(1)]; inflight {
		t.Error("stop op still in-flight after its cmdResultMsg")
	}
	if calls := f.callArgs(); len(calls) != 1 || calls[0][0] != "--stop" {
		t.Fatalf("calls = %v, want a single --stop exec", calls)
	}
}

func TestGuardBusyOpensConfirmForBusyRunner(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{busyRunner(1)}}
	m := newActionsTestModel(f, snap)

	got, _ := m.guardBusy(1,
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
	)
	if got.Confirm == nil {
		t.Fatal("busy runner did not open a confirm modal")
	}
	if len(got.Confirm.Choices) != 3 {
		t.Fatalf("choices = %d, want 3 (drain/kill/cancel)", len(got.Confirm.Choices))
	}
	if len(f.callArgs()) != 0 {
		t.Fatal("exec ran before the modal was confirmed")
	}
}

func TestConfirmDrainChoiceRunsDrain(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{busyRunner(1)}}
	m := newActionsTestModel(f, snap)

	m, _ = m.guardBusy(1,
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
	)

	updated, cmd := m.handleConfirmKey(tea.KeyPressMsg{Text: "d"})
	got := updated.(Model)
	if got.Confirm != nil {
		t.Error("Confirm still open after a choice was made")
	}
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--drain" {
		t.Fatalf("calls = %v, want a single --drain exec", calls)
	}
}

func TestConfirmKillChoiceRunsAggressive(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{busyRunner(1)}}
	m := newActionsTestModel(f, snap)

	m, _ = m.guardBusy(1,
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
	)

	_, cmd := m.handleConfirmKey(tea.KeyPressMsg{Text: "k"})
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--stop" {
		t.Fatalf("calls = %v, want a single --stop exec", calls)
	}
}

func TestConfirmEscCancelsWithoutExec(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{busyRunner(1)}}
	m := newActionsTestModel(f, snap)

	m, _ = m.guardBusy(1,
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
	)

	updated, cmd := m.handleConfirmKey(tea.KeyPressMsg{Text: "esc"})
	got := updated.(Model)
	if got.Confirm != nil {
		t.Error("Confirm still open after Esc")
	}
	if cmd != nil {
		t.Error("Esc produced a Cmd, want none (cancel is a no-op)")
	}
	if len(f.callArgs()) != 0 {
		t.Error("Esc ran an exec, want cancel to run nothing")
	}
}

func TestConfirmEnterPicksDefault(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{busyRunner(1)}}
	m := newActionsTestModel(f, snap)

	m, _ = m.guardBusy(1,
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
		func(mm Model) (Model, tea.Cmd) { return mm.execStop(1) },
	)

	_, cmd := m.handleConfirmKey(tea.KeyPressMsg{Text: "enter"})
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--drain" {
		t.Fatalf("calls = %v, want Enter to pick the default (drain)", calls)
	}
}

func TestRemoveNowAlwaysConfirmsEvenWhenIdle(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{idleRunner(1)}}
	m := newActionsTestModel(f, snap)

	got, _ := m.confirmRemoveNow(1)
	if got.Confirm == nil {
		t.Fatal("D on an idle runner did not confirm, want always-confirm")
	}
}

func TestStopAllSkipsConfirmWhenNoneBusy(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{idleRunner(1), idleRunner(2)}}
	m := newActionsTestModel(f, snap)

	got, cmd := m.confirmAllVariant("stop", func(mm Model) (Model, tea.Cmd) { return mm.execStopAll() })
	if got.Confirm != nil {
		t.Error("confirmAllVariant opened a modal with no busy runners")
	}
	runCmdSync(cmd)
	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--stop-all" {
		t.Fatalf("calls = %v, want a single --stop-all exec", calls)
	}
}

func TestStopAllConfirmsThenRunsStopAll(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{busyRunner(1), idleRunner(2)}}
	m := newActionsTestModel(f, snap)

	m, _ = m.confirmAllVariant("stop", func(mm Model) (Model, tea.Cmd) { return mm.execStopAll() })
	if m.Confirm == nil {
		t.Fatal("confirmAllVariant did not open a modal with a busy runner present")
	}
	if len(f.callArgs()) != 0 {
		t.Fatal("exec ran before the modal was confirmed")
	}

	_, cmd := m.handleConfirmKey(tea.KeyPressMsg{Text: "y"})
	runCmdSync(cmd)

	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--stop-all" {
		t.Fatalf("calls = %v, want a single --stop-all exec after confirming", calls)
	}
}

func TestExecRestartRunsStopThenStart(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Runners: []state.Runner{idleRunner(1)}}
	m := newActionsTestModel(f, snap)

	_, cmd := m.execRestart(1)
	runCmdSync(cmd)

	calls := f.callArgs()
	if len(calls) != 2 || calls[0][0] != "--stop" || calls[1][0] != "--start" {
		t.Fatalf("calls = %v, want --stop then --start", calls)
	}
}

func TestExecRestartSkipsStartWhenStopFails(t *testing.T) {
	f := &fakeRunner{override: map[string]cli.Result{
		"--stop 1 ": {ExitCode: 1, Stderr: "boom"},
	}}
	snap := state.Snapshot{Runners: []state.Runner{idleRunner(1)}}
	m := newActionsTestModel(f, snap)

	_, cmd := m.execRestart(1)
	msg := runCmdSync(cmd)
	res, ok := msg.(cmdResultMsg)
	if !ok {
		t.Fatalf("msg = %T, want cmdResultMsg", msg)
	}
	if res.res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1 (the failed stop's result)", res.res.ExitCode)
	}
	calls := f.callArgs()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want only --stop (start skipped after a failed stop)", calls)
	}
}

func TestExecRestartAllRunsStopAllThenStartAll(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})

	_, cmd := m.execRestartAll()
	runCmdSync(cmd)

	calls := f.callArgs()
	if len(calls) != 2 || calls[0][0] != "--stop-all" || calls[1][0] != "--start-all" {
		t.Fatalf("calls = %v, want --stop-all then --start-all", calls)
	}
}

func TestExecRestartAllSkipsStartAllWhenStopAllFails(t *testing.T) {
	f := &fakeRunner{override: map[string]cli.Result{
		"--stop-all ": {ExitCode: 1, Stderr: "boom"},
	}}
	m := newActionsTestModel(f, state.Snapshot{})

	_, cmd := m.execRestartAll()
	msg := runCmdSync(cmd)
	res, ok := msg.(cmdResultMsg)
	if !ok {
		t.Fatalf("msg = %T, want cmdResultMsg", msg)
	}
	if res.res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1 (the failed stop-all's result)", res.res.ExitCode)
	}
	calls := f.callArgs()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want only --stop-all (start-all skipped after a failure)", calls)
	}
}

func TestExecStartAllRunsStartAll(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})

	_, cmd := m.execStartAll()
	runCmdSync(cmd)

	calls := f.callArgs()
	if len(calls) != 1 || calls[0][0] != "--start-all" {
		t.Fatalf("calls = %v, want a single --start-all exec", calls)
	}
}

func TestCmdResultClearsInflightAndSetsNotice(t *testing.T) {
	m := newTestModel()
	m.Inflight[runnerKey(1)] = Op{Verb: "stop"}

	updated, _ := m.Update(cmdResultMsg{key: runnerKey(1), res: cli.Result{ExitCode: 0, Stdout: "stopped runner-1\n"}})
	got := updated.(Model)

	if _, inflight := got.Inflight[runnerKey(1)]; inflight {
		t.Error("Inflight entry not cleared after cmdResultMsg")
	}
	if len(got.Notices) == 0 || got.Notices[len(got.Notices)-1].Text != "stopped runner-1" {
		t.Errorf("Notices = %v, want a toast with the exec's last stdout line", got.Notices)
	}
}

func TestCmdResultErrorNoticeIsError(t *testing.T) {
	m := newTestModel()
	m.Inflight[globalKey] = Op{Verb: "stop-all"}

	updated, _ := m.Update(cmdResultMsg{key: globalKey, res: cli.Result{ExitCode: 1, Stderr: "denied\n"}})
	got := updated.(Model)

	n := got.Notices[len(got.Notices)-1]
	if n.Text != "denied" || n.Level != LevelError {
		t.Errorf("Notice = %+v, want error toast \"denied\"", n)
	}
}

func TestQuitWithPendingOrInflightConfirms(t *testing.T) {
	m := newTestModel()
	m.Inflight[globalKey] = Op{Verb: "stop-all"}

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "q"})
	got := updated.(Model)
	if got.Confirm == nil {
		t.Fatal("quit with an in-flight op did not confirm")
	}
	if cmd != nil {
		t.Error("quit-confirm should not itself return tea.Quit yet")
	}
}

func TestDrainPickerOnHeaderRowIsANoticeNotAConfirm(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Targets: []state.Target{{URL: urlA, Have: 1}}}
	m := newActionsTestModel(f, snap)
	m.Cursor = 0 // the target's own header row

	updated, _ := m.handleKey(tea.KeyPressMsg{Text: "d"})
	got := updated.(Model)
	if got.Confirm != nil {
		t.Error("'d' on a header row opened a confirm modal, want a notice instead")
	}
	if len(got.Notices) == 0 {
		t.Error("'d' on a header row produced no notice")
	}
}
