package ui

import (
	"context"
	"sync"

	"github.com/sdawka/gh-runnermaxxer/internal/engine"
	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// Engine is the part of *engine.Engine the UI drives: the snapshot stream,
// first-run setup, every mutation, and shutdown. An interface so tests can
// substitute a fake that records calls instead of managing real runners.
// Every mutation returns the message to toast on success, or the error to
// toast on failure; each one publishes a fresh snapshot when it finishes.
type Engine interface {
	Snapshot() state.Snapshot
	Snapshots() <-chan state.Snapshot
	EventLogPath() string

	NeedsSetup() bool
	Setup(prefix string, maxRunners int) (string, error)

	Scale(counts map[string]int) (string, error)
	Drain(id int) (string, error)
	Stop(id int) (string, error)
	Start(id int) (string, error)
	Remove(id int) (string, error)
	StopAll() (string, error)
	StartAll() (string, error)
	AddTarget(entry string) (string, error)
	RemoveTarget(entry string) (string, error)
	SetBounds(entry string, min, max int) (string, error)
	SetConfig(key, val string) (string, error)
	Poll() (string, error)
	GHStatus() (string, error)
	Download() (string, error)

	Shutdown(ctx context.Context, mode engine.ShutdownMode) error
}

// SharedPending hands the model's unapplied scale edits to the engine's
// autoscaler (engine.Options.Pending), which reads them from its own
// goroutine with its lock held: the model publishes a copy after every
// update, and Get returns that copy without ever touching the model.
type SharedPending struct {
	mu     sync.Mutex
	counts map[string]int
}

// Get returns the last published pending counts (URL -> count).
func (s *SharedPending) Get() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts
}

func (s *SharedPending) set(counts map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = counts
}
