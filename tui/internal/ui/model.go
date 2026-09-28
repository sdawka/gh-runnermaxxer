// Package ui is the Bubble Tea model/update/view for runnermaxxer-tui.
package ui

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runner-swarm/tui/internal/cli"
	"github.com/sdawka/gh-runner-swarm/tui/internal/logtail"
	"github.com/sdawka/gh-runner-swarm/tui/internal/state"
)

// noLogOpen and daemonLogID are the two Model.Log sentinel ids; any other
// value is a runner id.
const (
	noLogOpen   = -1
	daemonLogID = -2
)

// Screen identifies which top-level view is active. Only ScreenDashboard
// exists so far; the others are added as their commits land (Go plan §8,
// commits 11-13).
type Screen int

const (
	ScreenDashboard Screen = iota
	ScreenProjects
)

// Level is a Notice's severity, used to pick its style.
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

// Notice is a bottom-line toast (§2.4: "toast with the script's stdout last
// line" / sticky error toasts).
type Notice struct {
	Text  string
	Level Level
	Until time.Time // zero means sticky: cleared by a keypress, not by time
}

// Op describes a running (or just-finished) exec, keyed in Model.Inflight
// by "target:<url>", "runner:<id>", or "global" (§2.4).
type Op struct {
	Verb    string
	Args    []string
	Started time.Time
}

// Model is the top-level Bubble Tea model. Sub-screen state (projects,
// add-target, bounds, config, logs, gh-status, confirm) is added to this
// struct in the commits that build those screens; this commit only lays
// down the wiring, messages, keys, theme and pending-count logic that
// every later screen shares.
type Model struct {
	ctx    context.Context
	Client cli.Client
	Paths  cli.Paths
	Keys   KeyMap
	Theme  Theme
	Glyphs Glyphs

	watcher       *state.Watcher
	watcherEvents <-chan state.Event

	Snap     state.Snapshot
	SnapErr  error
	DaemonUp bool
	Now      time.Time

	Cursor    int
	Pending   *PendingCounts
	Filter    string
	Filtering bool

	Inflight map[string]Op
	Spinner  spinner.Model
	Notices  []Notice

	Screen  Screen
	Help    bool
	Confirm *ConfirmModel

	Width, Height int
	ShowLog       bool // wide enough for a permanent side log pane, if one is open
	Compact       bool

	// Log is the currently open log view (a runner's, or the daemon's when
	// its id is daemonLogID), rendered as a side pane when ShowLog is true
	// or full-screen otherwise (§3.4). nil means no log view is open.
	Log        *LogPane
	logCancel  context.CancelFunc
	logUpdates <-chan logtail.Update
}

// New builds the initial Model. w may be nil when there is no state.json to
// watch yet; Init falls back to polling --status --json.
func New(ctx context.Context, client cli.Client, paths cli.Paths, w *state.Watcher) Model {
	var events <-chan state.Event
	if w != nil {
		events = w.Events()
	}
	return Model{
		ctx:           ctx,
		Client:        client,
		Paths:         paths,
		Keys:          DefaultKeyMap(),
		Theme:         NewTheme(),
		Glyphs:        NewGlyphs(false),
		watcher:       w,
		watcherEvents: events,
		Pending:       NewPendingCounts(),
		Inflight:      map[string]Op{},
		Spinner:       spinner.New(),
		Now:           time.Now(),
		Screen:        ScreenDashboard,
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tickEvery(time.Second), m.Spinner.Tick}
	if m.watcherEvents != nil {
		cmds = append(cmds, watchSnapshot(m.watcherEvents))
	} else {
		cmds = append(cmds, pollCLI(m.ctx, m.Client))
	}
	return tea.Batch(cmds...)
}
