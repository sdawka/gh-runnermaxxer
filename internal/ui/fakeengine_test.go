package ui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/sdawka/gh-runnermaxxer/internal/engine"
	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// fakeEngine records every call, in order, as its method name followed by
// its arguments (Scale as one "url=N" per entry, sorted; SetConfig as
// "KEY=val"; SetBounds as entry, min, max). Calls succeed with an empty
// message unless results has an entry for the call's space-joined form,
// e.g. "Stop 1".
type fakeEngine struct {
	mu         sync.Mutex
	log        [][]string
	results    map[string]opResult
	needsSetup bool
	eventLog   string
	snap       state.Snapshot
	snaps      chan state.Snapshot
	// shutdownGate, when set, blocks Shutdown until it is closed or the
	// call's ctx is cancelled (recorded as "ShutdownCancelled").
	shutdownGate chan struct{}
}

func (f *fakeEngine) record(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, args)
	res := f.results[strings.Join(args, " ")]
	return res.msg, res.err
}

func (f *fakeEngine) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.log...)
}

func (f *fakeEngine) Snapshot() state.Snapshot { return f.snap }

func (f *fakeEngine) Snapshots() <-chan state.Snapshot { return f.snaps }

func (f *fakeEngine) EventLogPath() string { return f.eventLog }

func (f *fakeEngine) NeedsSetup() bool { return f.needsSetup }

func (f *fakeEngine) Setup(prefix string, maxRunners int) (string, error) {
	return f.record("Setup", prefix, strconv.Itoa(maxRunners))
}

func (f *fakeEngine) Scale(counts map[string]int) (string, error) {
	args := []string{"Scale"}
	var entries []string
	for k, v := range counts {
		entries = append(entries, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(entries)
	return f.record(append(args, entries...)...)
}

func (f *fakeEngine) Drain(id int) (string, error)  { return f.record("Drain", strconv.Itoa(id)) }
func (f *fakeEngine) Stop(id int) (string, error)   { return f.record("Stop", strconv.Itoa(id)) }
func (f *fakeEngine) Start(id int) (string, error)  { return f.record("Start", strconv.Itoa(id)) }
func (f *fakeEngine) Remove(id int) (string, error) { return f.record("Remove", strconv.Itoa(id)) }
func (f *fakeEngine) StopAll() (string, error)      { return f.record("StopAll") }
func (f *fakeEngine) StartAll() (string, error)     { return f.record("StartAll") }

func (f *fakeEngine) AddTarget(entry string) (string, error) {
	return f.record("AddTarget", entry)
}

func (f *fakeEngine) RemoveTarget(entry string) (string, error) {
	return f.record("RemoveTarget", entry)
}

func (f *fakeEngine) SetBounds(entry string, min, max int) (string, error) {
	return f.record("SetBounds", entry, strconv.Itoa(min), strconv.Itoa(max))
}

func (f *fakeEngine) SetConfig(key, val string) (string, error) {
	return f.record("SetConfig", key+"="+val)
}

func (f *fakeEngine) Poll() (string, error)     { return f.record("Poll") }
func (f *fakeEngine) GHStatus() (string, error) { return f.record("GHStatus") }
func (f *fakeEngine) Download() (string, error) { return f.record("Download") }

func (f *fakeEngine) Shutdown(ctx context.Context, mode engine.ShutdownMode) error {
	name := "StopNow"
	if mode == engine.DrainThenStop {
		name = "DrainThenStop"
	}
	f.record("Shutdown", name)
	if f.shutdownGate != nil {
		select {
		case <-f.shutdownGate:
		case <-ctx.Done():
			f.record("ShutdownCancelled")
			return ctx.Err()
		}
	}
	return nil
}
