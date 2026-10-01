package logtail

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitUpdate(t *testing.T, ch <-chan Update, timeout time.Duration) Update {
	t.Helper()
	select {
	case u := <-ch:
		return u
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a logtail update")
		return Update{}
	}
}

func TestTailInitialLastN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner-1.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial, _, err := Tail(ctx, path, 3)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	want := []string{"c", "d", "e"}
	if len(initial) != len(want) {
		t.Fatalf("initial = %v, want %v", initial, want)
	}
	for i := range want {
		if initial[i] != want[i] {
			t.Errorf("initial[%d] = %q, want %q", i, initial[i], want[i])
		}
	}
}

func TestTailMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.log")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial, _, err := Tail(ctx, path, 10)
	if err != nil {
		t.Fatalf("Tail: %v, want nil for a missing file", err)
	}
	if len(initial) != 0 {
		t.Errorf("initial = %v, want empty", initial)
	}
}

func TestTailAppendDelivers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner-1.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, updates, err := Tail(ctx, path, 10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\nthird\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	upd := waitUpdate(t, updates, 2*time.Second)
	if upd.Err != nil {
		t.Fatalf("upd.Err = %v", upd.Err)
	}
	if upd.Reset {
		t.Errorf("Reset = true, want false for a plain append")
	}
	want := []string{"second", "third"}
	if len(upd.Lines) != len(want) {
		t.Fatalf("Lines = %v, want %v", upd.Lines, want)
	}
	for i := range want {
		if upd.Lines[i] != want[i] {
			t.Errorf("Lines[%d] = %q, want %q", i, upd.Lines[i], want[i])
		}
	}
}

func TestTailPartialLineBuffersAcrossPolls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner-1.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, updates, err := Tail(ctx, path, 10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Write a line with no trailing newline: should not be delivered yet.
	if _, err := f.WriteString("incomplete"); err != nil {
		t.Fatal(err)
	}

	select {
	case upd := <-updates:
		t.Fatalf("got an update before the line completed: %+v", upd)
	case <-time.After(600 * time.Millisecond):
	}

	if _, err := f.WriteString(" line\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	upd := waitUpdate(t, updates, 2*time.Second)
	if len(upd.Lines) != 1 || upd.Lines[0] != "incomplete line" {
		t.Fatalf("Lines = %v, want [\"incomplete line\"]", upd.Lines)
	}
}

func TestTailTruncationResets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner-1.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, updates, err := Tail(ctx, path, 10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}

	// Rotation truncates in place: write a shorter file over the same path.
	if err := os.WriteFile(path, []byte("new-first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	upd := waitUpdate(t, updates, 2*time.Second)
	if !upd.Reset {
		t.Fatalf("Reset = false, want true after truncation")
	}
	if len(upd.Lines) != 1 || upd.Lines[0] != "new-first" {
		t.Fatalf("Lines = %v, want [new-first]", upd.Lines)
	}

	// A further append after the reset should not repeat "new-first".
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new-second\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	upd = waitUpdate(t, updates, 2*time.Second)
	if upd.Reset {
		t.Errorf("Reset = true on the follow-up append, want false")
	}
	if len(upd.Lines) != 1 || upd.Lines[0] != "new-second" {
		t.Fatalf("Lines = %v, want [new-second] (no duplicate of new-first)", upd.Lines)
	}
}

func TestTailStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner-1.log")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, updates, err := Tail(ctx, path, 10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	cancel()

	select {
	case _, ok := <-updates:
		if ok {
			t.Fatal("received an update after cancel, want the channel closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after context cancel")
	}
}
