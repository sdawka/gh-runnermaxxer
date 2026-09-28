package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitEvent(t *testing.T, w *Watcher, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev := <-w.Events():
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a watcher event")
		return Event{}
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeAtomic mirrors write_state_snapshot's tmp+mv so the watcher sees a
// rename over state.json rather than an in-place edit.
func writeAtomic(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherFSNotifyDeliversOnRename(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	w := newWatcher(stateFile, 200*time.Millisecond, false)
	defer w.Close()

	writeAtomic(t, dir, "state.json", readFixture(t, "full.json"))

	ev := waitEvent(t, w, 2*time.Second)
	if ev.Err != nil {
		t.Fatalf("event.Err = %v, want nil", ev.Err)
	}
	if ev.Snapshot.DaemonPID != 41213 {
		t.Errorf("DaemonPID = %d, want 41213", ev.Snapshot.DaemonPID)
	}
}

func TestWatcherReportsParseErrorAndKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	w := newWatcher(stateFile, 200*time.Millisecond, false)
	defer w.Close()

	// A truncated write caught mid-mv: invalid JSON.
	writeAtomic(t, dir, "state.json", []byte(`{"schema":2,"tick_ts":1,"target`))
	ev := waitEvent(t, w, 2*time.Second)
	if ev.Err == nil {
		t.Fatalf("event.Err = nil, want a parse error for truncated JSON")
	}

	// A subsequent good write still comes through.
	writeAtomic(t, dir, "state.json", readFixture(t, "full.json"))
	ev = waitEvent(t, w, 2*time.Second)
	if ev.Err != nil {
		t.Fatalf("event.Err = %v, want nil after a good write", ev.Err)
	}
}

func TestWatcherPollFallbackWithoutFSNotify(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	w := newWatcher(stateFile, 200*time.Millisecond, true /* disableFSNotify */)
	defer w.Close()

	writeAtomic(t, dir, "state.json", readFixture(t, "full.json"))

	ev := waitEvent(t, w, 1500*time.Millisecond)
	if ev.Err != nil {
		t.Fatalf("event.Err = %v, want nil", ev.Err)
	}
	if ev.Snapshot.DaemonPID != 41213 {
		t.Errorf("DaemonPID = %d, want 41213", ev.Snapshot.DaemonPID)
	}
}

func TestWatcherClosesEventsChannel(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	w := newWatcher(stateFile, 50*time.Millisecond, true)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := <-w.Events(); ok {
		t.Fatal("Events() channel still open after Close()")
	}
}
