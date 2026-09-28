package state

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Event is what a Watcher delivers: either a freshly loaded Snapshot, or an
// error (a parse failure, most likely a partial write caught mid-mv - the
// caller should keep showing its last good Snapshot when it sees one of
// these rather than blanking the screen).
type Event struct {
	Snapshot Snapshot
	Err      error
}

// pollInterval is the poll-fallback cadence.
const pollInterval = 1 * time.Second

// fsnotifyGrace is how long a Watcher waits for an fsnotify event on the
// state file before falling back to polling for that tick.
const fsnotifyGrace = 3 * time.Second

// Watcher watches a state.json file for changes: state.json is replaced
// with `mv` (rename over the old file) rather than edited in place, so the
// watch must be on the containing *directory*, not the file itself (a
// rename drops the old inode from an fsnotify watch on the file). It falls
// back to polling mtime+size when fsnotify is unavailable, or when it has
// not delivered an event recently.
type Watcher struct {
	stateFile string
	events    chan Event
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	mu       sync.Mutex
	lastMod  time.Time
	lastSize int64
}

// NewWatcher starts watching stateFile and returns immediately; call
// Events() for the channel of updates and Close() to stop.
func NewWatcher(stateFile string) *Watcher {
	return newWatcher(stateFile, pollInterval, false)
}

// newWatcher is the test seam: it allows a faster poll interval and forcing
// the poll-only path without a real fsnotify backend.
func newWatcher(stateFile string, poll time.Duration, disableFSNotify bool) *Watcher {
	w := &Watcher{
		stateFile: stateFile,
		events:    make(chan Event, 8),
		done:      make(chan struct{}),
	}

	var fsw *fsnotify.Watcher
	if !disableFSNotify {
		if created, err := fsnotify.NewWatcher(); err == nil {
			if err := created.Add(filepath.Dir(stateFile)); err == nil {
				fsw = created
			} else {
				_ = created.Close()
			}
		}
	}

	w.wg.Add(1)
	go w.loop(fsw, poll)
	return w
}

// Events returns the channel of snapshot updates. It is closed after Close().
func (w *Watcher) Events() <-chan Event {
	return w.events
}

// Close stops the watcher and waits for its goroutine to exit.
func (w *Watcher) Close() error {
	w.closeOnce.Do(func() { close(w.done) })
	w.wg.Wait()
	return nil
}

func (w *Watcher) loop(fsw *fsnotify.Watcher, poll time.Duration) {
	defer w.wg.Done()
	defer close(w.events)
	if fsw != nil {
		defer fsw.Close()
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	lastFSEvent := time.Time{}
	target := filepath.Clean(w.stateFile)

	var fsEvents chan fsnotify.Event
	var fsErrors chan error
	if fsw != nil {
		fsEvents = fsw.Events
		fsErrors = fsw.Errors
	}

	// Pick up whatever is already there at startup.
	w.pollAndMaybeEmit()

	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-fsEvents:
			if !ok {
				fsEvents = nil
				continue
			}
			if filepath.Clean(ev.Name) == target {
				lastFSEvent = time.Now()
				w.reloadAndEmit()
			}
		case _, ok := <-fsErrors:
			if !ok {
				fsErrors = nil
			}
			// fsnotify errors are non-fatal: the poller below still covers us.
		case <-ticker.C:
			if fsw == nil || time.Since(lastFSEvent) > fsnotifyGrace {
				w.pollAndMaybeEmit()
			}
		}
	}
}

// pollAndMaybeEmit stats the state file and reloads only if its mtime or
// size changed since the last successful load.
func (w *Watcher) pollAndMaybeEmit() {
	info, err := os.Stat(w.stateFile)
	if err != nil {
		return // no file yet (or removed); nothing to report
	}
	w.mu.Lock()
	changed := !info.ModTime().Equal(w.lastMod) || info.Size() != w.lastSize
	w.mu.Unlock()
	if !changed {
		return
	}
	w.reloadAndEmit()
}

func (w *Watcher) reloadAndEmit() {
	info, statErr := os.Stat(w.stateFile)
	snap, err := Load(w.stateFile)
	// Record what we looked at whether or not it parsed: a file that failed
	// to parse must not be reported again by the poller until it changes.
	if statErr == nil {
		w.mu.Lock()
		w.lastMod = info.ModTime()
		w.lastSize = info.Size()
		w.mu.Unlock()
	}
	if err != nil {
		select {
		case w.events <- Event{Err: err}:
		case <-w.done:
		}
		return
	}
	select {
	case w.events <- Event{Snapshot: snap}:
	case <-w.done:
	}
}
