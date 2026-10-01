package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

func runningRunner(id int) state.Runner {
	return state.Runner{ID: id, Name: "runner-1", PID: 1000 + id, State: state.RunnerIdle}
}

func busyRunningRunner(id int) state.Runner {
	r := busyRunner(id)
	r.PID = 1000 + id
	return r
}

// pressQ sends 'q' and returns the model and its Cmd.
func pressQ(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(tea.KeyPressMsg{Text: "q"})
	return updated.(Model), cmd
}

// finishQuit runs the shutdown Cmd and feeds its result back, returning
// the Cmd Update answers with (tea.Quit when quitting completes).
func finishQuit(t *testing.T, m Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	msg := runCmdSync(cmd)
	done, ok := msg.(shutdownDoneMsg)
	if !ok {
		t.Fatalf("shutdown Cmd produced %T, want shutdownDoneMsg", msg)
	}
	_, quit := m.Update(done)
	return quit
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := runCmdSync(cmd).(tea.QuitMsg)
	return ok
}

func TestQuitWithNoRunnersShutsDownAndQuits(t *testing.T) {
	f := &fakeEngine{}
	m := newActionsTestModel(f, state.Snapshot{Runners: []state.Runner{{ID: 1, State: state.RunnerStopped}}})

	m, cmd := pressQ(t, m)
	if m.Confirm != nil {
		t.Fatal("quit with nothing running opened a confirm")
	}
	if !isQuit(finishQuit(t, m, cmd)) {
		t.Fatal("finished shutdown did not quit")
	}
	if calls := f.calls(); len(calls) != 1 || strings.Join(calls[0], " ") != "Shutdown StopNow" {
		t.Fatalf("calls = %v, want Shutdown StopNow", calls)
	}
}

func TestQuitWithIdleRunnersShowsStoppingThenQuits(t *testing.T) {
	f := &fakeEngine{}
	snap := state.Snapshot{Runners: []state.Runner{runningRunner(1), runningRunner(2)}}
	m := newActionsTestModel(f, snap)
	m.Width, m.Height = 100, 30

	m, cmd := pressQ(t, m)
	if m.Confirm != nil {
		t.Fatal("quit with only idle runners opened a confirm")
	}
	if m.Quit != quitStopping {
		t.Fatalf("Quit = %v, want quitStopping", m.Quit)
	}
	if frame := m.renderString(); !strings.Contains(frame, "Stopping 2 runner(s)") {
		t.Errorf("frame lacks the stopping overlay:\n%s", frame)
	}
	if !isQuit(finishQuit(t, m, cmd)) {
		t.Fatal("finished shutdown did not quit")
	}
	if calls := f.calls(); len(calls) != 1 || strings.Join(calls[0], " ") != "Shutdown StopNow" {
		t.Fatalf("calls = %v, want Shutdown StopNow", calls)
	}
}

func busyQuitModel(t *testing.T, f *fakeEngine) Model {
	t.Helper()
	snap := state.Snapshot{Runners: []state.Runner{busyRunningRunner(1), runningRunner(2)}}
	m := newActionsTestModel(f, snap)
	m.Width, m.Height = 100, 30
	m, cmd := pressQ(t, m)
	if cmd != nil {
		t.Fatal("quit with a busy runner ran something before confirming")
	}
	if m.Confirm == nil {
		t.Fatal("quit with a busy runner did not confirm")
	}
	var keys []string
	for _, c := range m.Confirm.Choices {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, "") != "swc" {
		t.Fatalf("choices = %v, want stop now / wait / cancel", keys)
	}
	if !strings.Contains(m.Confirm.Choices[0].Label, "cancels 1 job") {
		t.Errorf("stop-now label = %q, want it to say it cancels 1 job", m.Confirm.Choices[0].Label)
	}
	return m
}

func TestQuitBusyStopNow(t *testing.T) {
	f := &fakeEngine{}
	m := busyQuitModel(t, f)

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "s"})
	m = updated.(Model)
	if m.Quit != quitStopping {
		t.Fatalf("Quit = %v, want quitStopping", m.Quit)
	}
	if !isQuit(finishQuit(t, m, cmd)) {
		t.Fatal("finished shutdown did not quit")
	}
	if calls := f.calls(); len(calls) != 1 || strings.Join(calls[0], " ") != "Shutdown StopNow" {
		t.Fatalf("calls = %v, want Shutdown StopNow", calls)
	}
}

func TestQuitBusyCancel(t *testing.T) {
	for _, key := range []string{"c", "esc"} {
		f := &fakeEngine{}
		m := busyQuitModel(t, f)

		updated, cmd := m.Update(tea.KeyPressMsg{Text: key})
		m = updated.(Model)
		if m.Confirm != nil || m.Quit != quitNone {
			t.Errorf("%s: confirm %v, Quit %v; want the dashboard back", key, m.Confirm, m.Quit)
		}
		runCmdSync(cmd)
		if len(f.calls()) != 0 {
			t.Errorf("%s: calls = %v, want none", key, f.calls())
		}
	}
}

func TestQuitBusyWaitDrainsThenQuits(t *testing.T) {
	f := &fakeEngine{shutdownGate: make(chan struct{})}
	m := busyQuitModel(t, f)

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "w"})
	m = updated.(Model)
	if m.Quit != quitDraining {
		t.Fatalf("Quit = %v, want quitDraining", m.Quit)
	}
	if frame := m.renderString(); !strings.Contains(frame, "finish their jobs") || !strings.Contains(frame, "ctrl+c") {
		t.Errorf("frame lacks the waiting overlay:\n%s", frame)
	}

	// The TUI stays up showing progress: keys other than ctrl+c do nothing.
	updated, _ = m.Update(tea.KeyPressMsg{Text: "q"})
	m = updated.(Model)
	if m.Quit != quitDraining {
		t.Fatal("'q' while waiting changed the quit state")
	}

	close(f.shutdownGate)
	if !isQuit(finishQuit(t, m, cmd)) {
		t.Fatal("finished drain did not quit")
	}
	if calls := f.calls(); len(calls) != 1 || strings.Join(calls[0], " ") != "Shutdown DrainThenStop" {
		t.Fatalf("calls = %v, want Shutdown DrainThenStop", calls)
	}
}

func TestCtrlCWhileWaitingEscalatesToStopNow(t *testing.T) {
	f := &fakeEngine{shutdownGate: make(chan struct{})}
	m := busyQuitModel(t, f)
	updated, cmd := m.Update(tea.KeyPressMsg{Text: "w"})
	m = updated.(Model)

	done := make(chan tea.Msg, 1)
	go func() { done <- runCmdSync(cmd) }()

	updated, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(Model)
	if m.Quit != quitStopping {
		t.Fatalf("Quit = %v after ctrl+c, want quitStopping", m.Quit)
	}

	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctrl+c did not cancel the waiting shutdown")
	}
	if _, quit := m.Update(msg); !isQuit(quit) {
		t.Fatal("escalated shutdown did not quit")
	}
	calls := f.calls()
	if len(calls) != 2 || calls[1][0] != "ShutdownCancelled" {
		t.Fatalf("calls = %v, want the drain shutdown cancelled", calls)
	}
}

func TestQuitWithPendingConfirmsDiscardFirst(t *testing.T) {
	f := &fakeEngine{}
	snap := state.Snapshot{Runners: []state.Runner{busyRunningRunner(1)}}
	m := newActionsTestModel(f, snap)
	m.Pending.values = map[string]int{urlA: 3}

	m, _ = pressQ(t, m)
	if m.Confirm == nil || !strings.Contains(m.Confirm.Title, "Discard") {
		t.Fatalf("confirm = %+v, want the discard confirm first", m.Confirm)
	}
	updated, _ := m.Update(tea.KeyPressMsg{Text: "y"})
	m = updated.(Model)
	if m.Confirm == nil || len(m.Confirm.Choices) != 3 {
		t.Fatalf("confirm = %+v, want the busy-runner quit choices next", m.Confirm)
	}
}
