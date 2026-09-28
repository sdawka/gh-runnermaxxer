package ui

import "charm.land/bubbles/v2/key"

// KeyMap is the set of key bindings shown in the help overlay/footer. The
// actual dispatch happens through keyToAction (a pure function so it can be
// table-tested without a running tea.Program); KeyMap exists to drive
// help.Model and to document, in one place, what the plan's README key
// table maps to on the dashboard.
type KeyMap struct {
	Up, Down       key.Binding
	Left, Right    key.Binding
	Inc, Dec       key.Binding
	Apply, Discard key.Binding
	Drain, Remove  key.Binding
	Stop, Start    key.Binding
	Restart        key.Binding
	StopAll        key.Binding
	StartAll       key.Binding
	RestartAll     key.Binding
	Projects       key.Binding
	AddTarget      key.Binding
	Logs           key.Binding
	DaemonLog      key.Binding
	CheckGH        key.Binding
	Config         key.Binding
	Download       key.Binding
	InstallService key.Binding
	Filter         key.Binding
	Help           key.Binding
	Quit           key.Binding
}

// DefaultKeyMap matches the README usage table plus the hjkl/vi aliases and
// shift-for-all variants from the Go design doc's §3.4.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Up:             key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "move up")),
		Down:           key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "move down")),
		Left:           key.NewBinding(key.WithKeys("left"), key.WithHelp("←", "count -1")),
		Right:          key.NewBinding(key.WithKeys("right"), key.WithHelp("→", "count +1")),
		Inc:            key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "count +1")),
		Dec:            key.NewBinding(key.WithKeys("-", "_"), key.WithHelp("-", "count -1")),
		Apply:          key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply pending")),
		Discard:        key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "discard pending")),
		Drain:          key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "drain runner")),
		Remove:         key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "remove now")),
		Stop:           key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop runner")),
		Start:          key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start runner")),
		Restart:        key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restart runner")),
		StopAll:        key.NewBinding(key.WithKeys("X"), key.WithHelp("X", "stop all")),
		StartAll:       key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "start all")),
		RestartAll:     key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "restart all")),
		Projects:       key.NewBinding(key.WithKeys("t", "n"), key.WithHelp("t", "projects")),
		AddTarget:      key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add target")),
		Logs:           key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "logs")),
		DaemonLog:      key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "daemon log")),
		CheckGH:        key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "check GitHub")),
		Config:         key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "config")),
		Download:       key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "download tarball")),
		InstallService: key.NewBinding(key.WithKeys("I"), key.WithHelp("I", "install service (no daemon)")),
		Filter:         key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Help:           key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:           key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp implements help.KeyMap.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Left, k.Right, k.Apply, k.Discard, k.Drain, k.Stop, k.Start, k.Restart, k.Projects, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Left, k.Right, k.Inc, k.Dec},
		{k.Apply, k.Discard, k.Drain, k.Remove},
		{k.Stop, k.Start, k.Restart, k.StopAll, k.StartAll, k.RestartAll},
		{k.Projects, k.AddTarget, k.Logs, k.DaemonLog, k.CheckGH, k.Config, k.Download, k.InstallService},
		{k.Filter, k.Help, k.Quit},
	}
}

// ActionKind is the semantic effect of a keypress, independent of which
// physical key produced it and independent of Bubble Tea entirely - this is
// what keys_test.go exercises without a running program.
type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionUp
	ActionDown
	ActionInc
	ActionDec
	ActionSetCount
	ActionApply
	ActionDiscardOrBack
	ActionDrain
	ActionDrainPicker // 'd' on a target header row: pick which runner
	ActionRemoveNow
	ActionStop
	ActionStopAll
	ActionStart
	ActionStartAll
	ActionRestart
	ActionRestartAll
	ActionProjects
	ActionAddTarget
	ActionRemoveFromList // 'x' in the projects screen
	ActionBounds
	ActionLogs
	ActionDaemonLog
	ActionCheckGH
	ActionConfig
	ActionDownload
	ActionInstallService // 'I': only meaningful in the no-daemon banner (§4.1)
	ActionFilter
	ActionHelp
	ActionQuit
)

// Action is the decoded effect of a keypress: a kind plus, for
// ActionSetCount, the digit that was pressed.
type Action struct {
	Kind ActionKind
	N    int
}

// keyToAction maps a key string (tea.KeyPressMsg.String()) to an Action for
// the given screen. onHeaderRow distinguishes a target header row from a
// runner row on the dashboard, since 'd' behaves differently on each (§3.1,
// §3.4 of the Go design doc). It has no dependency on Bubble Tea or the
// Model, so it can be tested as a pure table.
func keyToAction(screen Screen, key string, onHeaderRow bool) Action {
	switch screen {
	case ScreenProjects:
		return projectsKeyToAction(key)
	default:
		return dashboardKeyToAction(key, onHeaderRow)
	}
}

func dashboardKeyToAction(k string, onHeaderRow bool) Action {
	switch k {
	case "up", "k":
		return Action{Kind: ActionUp}
	case "down", "j":
		return Action{Kind: ActionDown}
	case "left":
		return Action{Kind: ActionDec}
	case "right":
		return Action{Kind: ActionInc}
	case "+", "=":
		return Action{Kind: ActionInc}
	case "-", "_":
		return Action{Kind: ActionDec}
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return Action{Kind: ActionSetCount, N: int(k[0] - '0')}
	case "enter":
		return Action{Kind: ActionApply}
	case "esc":
		return Action{Kind: ActionDiscardOrBack}
	case "d":
		if onHeaderRow {
			return Action{Kind: ActionDrainPicker}
		}
		return Action{Kind: ActionDrain}
	case "D":
		return Action{Kind: ActionRemoveNow}
	case "x":
		return Action{Kind: ActionStop}
	case "X":
		return Action{Kind: ActionStopAll}
	case "s":
		return Action{Kind: ActionStart}
	case "S":
		return Action{Kind: ActionStartAll}
	case "r":
		return Action{Kind: ActionRestart}
	case "R":
		return Action{Kind: ActionRestartAll}
	case "t", "n":
		return Action{Kind: ActionProjects}
	case "a":
		return Action{Kind: ActionAddTarget}
	case "l":
		return Action{Kind: ActionLogs}
	case "L":
		return Action{Kind: ActionDaemonLog}
	case "c":
		return Action{Kind: ActionCheckGH}
	case "e":
		return Action{Kind: ActionConfig}
	case "g":
		return Action{Kind: ActionDownload}
	case "I":
		return Action{Kind: ActionInstallService}
	case "/":
		return Action{Kind: ActionFilter}
	case "?":
		return Action{Kind: ActionHelp}
	case "q", "ctrl+c":
		return Action{Kind: ActionQuit}
	default:
		return Action{Kind: ActionNone}
	}
}

// projectsKeyToAction implements the projects screen's key table, where
// (unlike the dashboard) h/l are count keys and there is no logs key.
func projectsKeyToAction(k string) Action {
	switch k {
	case "up", "k":
		return Action{Kind: ActionUp}
	case "down", "j":
		return Action{Kind: ActionDown}
	case "left", "h":
		return Action{Kind: ActionDec}
	case "right", "l":
		return Action{Kind: ActionInc}
	case "+", "=":
		return Action{Kind: ActionInc}
	case "-", "_":
		return Action{Kind: ActionDec}
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return Action{Kind: ActionSetCount, N: int(k[0] - '0')}
	case "a":
		return Action{Kind: ActionAddTarget}
	case "x":
		return Action{Kind: ActionRemoveFromList}
	case "b":
		return Action{Kind: ActionBounds}
	case "enter", "s":
		return Action{Kind: ActionApply}
	case "q", "esc":
		return Action{Kind: ActionDiscardOrBack}
	default:
		return Action{Kind: ActionNone}
	}
}
