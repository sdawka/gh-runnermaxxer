// Package ui is the Bubble Tea model/update/view for runnermaxxer.
package ui

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/logtail"
	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// noLogOpen and eventLogID are the two Model.Log sentinel ids; any other
// value is a runner id.
const (
	noLogOpen  = -1
	eventLogID = -2
)

// Screen identifies which top-level view is active. The full-screen log
// viewer is handled separately via Model.Log rather than as a Screen.
type Screen int

const (
	ScreenDashboard Screen = iota
	ScreenProjects
	ScreenAddTarget
	ScreenBounds
	ScreenConfig
	ScreenGHStatus
	ScreenSetup
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
	Engine Engine
	Keys   KeyMap
	Theme  Theme
	Glyphs Glyphs

	// EventLog is the engine's event log file, shown by 'L' and by the
	// side pane's live tail on a header row.
	EventLog string

	// SharedPending, when set, receives a copy of the pending edits after
	// every update, for the engine's autoscaler.
	SharedPending *SharedPending

	Snap state.Snapshot
	Now  time.Time

	Cursor int
	// TableOffset is the first table row on screen when the table has more
	// rows than fit; Update keeps it following the cursor (scrollTable).
	TableOffset int
	Pending     *PendingCounts
	Filter      string
	Filtering   bool

	Inflight map[string]Op
	Spinner  spinner.Model
	Notices  []Notice

	Screen  Screen
	Help    bool
	Confirm *ConfirmModel

	// formReturn is the screen to restore once the currently open form
	// (AddTarget or Bounds) closes, since 'a' opens the same form from
	// either the dashboard or the projects screen (§3.4).
	formReturn Screen
	AddTarget  *AddTargetForm
	Bounds     *BoundsForm
	Config     *ConfigModel

	// GHStatusText holds the GitHub status report while ScreenGHStatus is
	// open; nil means the modal isn't open.
	GHStatusText *string

	// Setup is the first-run form, open (as ScreenSetup) until the engine
	// has a configuration.
	Setup *SetupForm

	// Quit is the quit flow's progress once the engine is shutting down;
	// quitCancel escalates a drain-then-stop shutdown to stop-now.
	Quit       quitState
	quitCancel context.CancelFunc

	Width, Height int
	ShowLog       bool // wide enough for a permanent side log pane, if one is open
	Compact       bool

	// Log is the currently open log view (a runner's, or the event log when
	// its id is eventLogID), rendered as a side pane when ShowLog is true
	// or full-screen otherwise (§3.4). nil means no log view is open.
	Log        *LogPane
	logCancel  context.CancelFunc
	logUpdates <-chan logtail.Update

	// Tail is the dashboard's always-on live tail of the cursor's runner
	// log (the event log on a header row), shown under the detail block.
	// Update re-points it when the cursor moves (syncTail); it is stopped
	// while an explicit Log view is open, since that shows the same thing
	// in more room.
	Tail        *LogPane
	tailCancel  context.CancelFunc
	tailUpdates <-chan logtail.Update

	// logSeq numbers every tail started, for LogPane.stream.
	logSeq int
}

// New builds the initial Model around eng, starting on the first-run setup
// form when the engine has no configuration yet.
func New(ctx context.Context, eng Engine) Model {
	m := Model{
		ctx:      ctx,
		Engine:   eng,
		Keys:     DefaultKeyMap(),
		Theme:    NewTheme(),
		Glyphs:   NewGlyphs(false),
		EventLog: eng.EventLogPath(),
		Snap:     eng.Snapshot(),
		Pending:  NewPendingCounts(),
		Inflight: map[string]Op{},
		Spinner:  spinner.New(),
		Now:      time.Now(),
		Screen:   ScreenDashboard,
	}
	if eng.NeedsSetup() {
		m.Screen = ScreenSetup
		m.Setup = newSetupForm()
	}
	return m
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tickEvery(time.Second), m.Spinner.Tick, waitForSnapshot(m.Engine.Snapshots())}
	if m.Setup != nil {
		cmds = append(cmds, m.Setup.focus())
	}
	return tea.Batch(cmds...)
}
