package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

func TestApplyPendingNoopWhenEmpty(t *testing.T) {
	f := &fakeRunner{}
	m := newActionsTestModel(f, state.Snapshot{})

	_, cmd := m.applyPending()
	if cmd != nil {
		t.Error("applyPending with nothing pending returned a Cmd, want nil")
	}
}

func TestApplyPendingRunsScaleAndClearsInflightOnResult(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Targets: []state.Target{{URL: urlA, Want: 2, Have: 2}}}
	m := newActionsTestModel(f, snap)
	m.Pending.values = map[string]int{urlA: 5}

	m, cmd := m.applyPending()
	if cmd == nil {
		t.Fatal("applyPending returned a nil Cmd")
	}

	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("applyPending's Cmd = %T, want a 2-element BatchMsg (started + exec)", cmd())
	}

	updated, _ := m.Update(batch[0]())
	m = updated.(Model)
	if _, inflight := m.Inflight[targetKey(urlA)]; !inflight {
		t.Fatal("target not marked in-flight after the started message")
	}

	resMsg := batch[1]()
	applyRes, ok := resMsg.(applyResultMsg)
	if !ok {
		t.Fatalf("exec Cmd produced %T, want applyResultMsg", resMsg)
	}
	if len(f.callArgs()) != 1 || f.callArgs()[0][0] != "--scale" || f.callArgs()[0][1] != urlA+"=5" {
		t.Fatalf("calls = %v, want a single --scale exec for %s=5", f.callArgs(), urlA)
	}

	updated, reload := m.Update(applyRes)
	m = updated.(Model)
	if _, inflight := m.Inflight[targetKey(urlA)]; inflight {
		t.Error("target still in-flight after applyResultMsg")
	}
	if reload == nil {
		t.Error("applyResultMsg produced no reload Cmd, want an immediate reloadNow poll")
	}
}

func TestApplyPendingRefusesWhileAlreadyInflight(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Targets: []state.Target{{URL: urlA, Want: 2, Have: 2}}}
	m := newActionsTestModel(f, snap)
	m.Pending.values = map[string]int{urlA: 5}
	m.Inflight[targetKey(urlA)] = Op{Verb: "scale"}

	_, cmd := m.applyPending()
	if cmd != nil {
		t.Error("applyPending started a second exec for an already in-flight target")
	}
}

func TestCmdResultTriggersReload(t *testing.T) {
	m := newTestModel()
	m.Inflight[runnerKey(1)] = Op{Verb: "stop"}

	_, cmd := m.Update(cmdResultMsg{key: runnerKey(1), res: cli.Result{ExitCode: 0}})
	if cmd == nil {
		t.Error("cmdResultMsg produced no reload Cmd, want an immediate reloadNow poll")
	}
}

func TestRenderTargetRowShowsSpinnerWhenInflight(t *testing.T) {
	f := &fakeRunner{}
	snap := state.Snapshot{Targets: []state.Target{{URL: urlA, Want: 2, Have: 2, Label: "a/a"}}}
	m := newActionsTestModel(f, snap)
	m.Inflight[targetKey(urlA)] = Op{Verb: "scale"}

	line := m.renderTargetRow(snap.Targets[0], false, 40)
	if !strings.Contains(line, m.Spinner.View()) {
		t.Errorf("target row missing spinner while in-flight:\n%s", line)
	}
}

func TestRenderRunnerRowShowsSpinnerWhenInflight(t *testing.T) {
	f := &fakeRunner{}
	r := idleRunner(1)
	m := newActionsTestModel(f, state.Snapshot{Runners: []state.Runner{r}})
	m.Inflight[runnerKey(1)] = Op{Verb: "stop"}

	line := m.renderRunnerRow(r, false, false)
	if !strings.Contains(line, m.Spinner.View()) {
		t.Errorf("runner row missing spinner while in-flight:\n%s", line)
	}
}
