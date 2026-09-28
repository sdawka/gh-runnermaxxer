// Package state defines the runnermaxxer.sh state.json snapshot (schema 2)
// and the loader that turns it into typed Go values for the TUI.
//
// The TUI never writes this file. It is produced by the bash daemon (or the
// CLI, when no daemon runs) as documented in the bash-side plan, section 2.1
// ("state.json schema (version 2)"). Field names below are kept identical to
// the JSON the bash writer emits so this file can be diffed against that
// schema directly.
package state

import "time"

// SupportedSchema is the highest state.json "schema" value this client
// understands. A snapshot with a higher schema number is rejected rather
// than partially trusted.
const SupportedSchema = 2

// Source records where a Snapshot came from.
type Source int

const (
	// SourceUnknown is the zero value; never set on a successfully loaded Snapshot.
	SourceUnknown Source = iota
	// SourceDaemonFile means the Snapshot was read from state.json on disk.
	SourceDaemonFile
	// SourceCLI means the Snapshot came from `runnermaxxer.sh --status --json`.
	SourceCLI
)

// GHState mirrors the bash gh.state enum: ok|auth|sso|ratelimit|network|error|unknown.
type GHState string

const (
	GHStateOK        GHState = "ok"
	GHStateAuth      GHState = "auth"
	GHStateSSO       GHState = "sso"
	GHStateRateLimit GHState = "ratelimit"
	GHStateNetwork   GHState = "network"
	GHStateError     GHState = "error"
	GHStateUnknown   GHState = "unknown"
)

// RunnerState mirrors the bash runner_state enum plus the client-derived
// "backoff" state (restarting + a future next_retry).
type RunnerState string

const (
	RunnerIdle        RunnerState = "idle"
	RunnerBusy        RunnerState = "busy"
	RunnerRunning     RunnerState = "running"
	RunnerDraining    RunnerState = "draining"
	RunnerStopped     RunnerState = "stopped"
	RunnerQuarantined RunnerState = "quarantined"
	RunnerRestarting  RunnerState = "restarting"
)

// Snapshot is the top-level shape of state.json (schema 2).
type Snapshot struct {
	Schema          int      `json:"schema"`
	ScriptVersion   string   `json:"version"`
	Writer          string   `json:"writer"` // daemon|tui|cli
	DaemonPID       int      `json:"daemon_pid"`
	TickTS          int64    `json:"tick_ts"`
	RefreshInterval int      `json:"refresh_interval"`
	MaxRunners      int      `json:"max_runners"`
	GH              GH       `json:"gh"`
	Tarball         Tarball  `json:"tarball"`
	Disk            Disk     `json:"disk"`
	Config          Config   `json:"config"`
	Warnings        []string `json:"warnings"`
	Targets         []Target `json:"targets"`
	Runners         []Runner `json:"runners"`

	// Derived, not present in the JSON.
	LoadedAt time.Time `json:"-"`
	Source   Source    `json:"-"`
}

// GH is the gh{} object: auth identity, scopes and API health.
type GH struct {
	User          string   `json:"user"`
	Host          string   `json:"host"`
	Scopes        []string `json:"scopes"`
	State         GHState  `json:"state"`
	Message       string   `json:"message"`
	Checked       int64    `json:"checked"`
	RateRemaining int      `json:"rate_remaining"`
	RateReset     int64    `json:"rate_reset"`
	PolledAt      int64    `json:"polled_at"`
}

// Tarball is the tarball{} object: on-disk runner tarball freshness.
type Tarball struct {
	Version string `json:"version"`
	Latest  string `json:"latest"`
	Stale   bool   `json:"stale"`
}

// Disk is the disk{} object.
type Disk struct {
	FreeMB  int `json:"free_mb"`
	PctFree int `json:"pct_free"`
}

// Config mirrors the 10 CONFIG_KEYS the bash config parser tracks
// (runnermaxxer-agent-aplan-bash §3.1), at their effective (post-clamp)
// values, so the TUI's config screen ('e') shows what is actually in
// effect rather than the raw file contents.
type Config struct {
	RunnerNamePrefix     string `json:"RUNNER_NAME_PREFIX"`
	MaxRunners           int    `json:"MAX_RUNNERS"`
	RefreshInterval      int    `json:"REFRESH_INTERVAL"`
	MaxRestartAttempts   int    `json:"MAX_RESTART_ATTEMPTS"`
	MaxLogSizeMB         int    `json:"MAX_LOG_SIZE_MB"`
	GHHealthTicks        int    `json:"GH_HEALTH_TICKS"`
	SharedToolCache      bool   `json:"SHARED_TOOL_CACHE"`
	EphemeralRunners     bool   `json:"EPHEMERAL_RUNNERS"`
	Autoscale            bool   `json:"AUTOSCALE"`
	AutoscaleIdleMinutes int    `json:"AUTOSCALE_IDLE_MINUTES"`
}

// TargetError is a target-level error (scope, 404, etc.), set to nil in the
// JSON ("error": null) when the target is healthy.
type TargetError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
	Since   int64  `json:"since"`
}

// Target is one entry of targets[]: a repo or org this fleet manages.
type Target struct {
	URL           string       `json:"url"`
	Label         string       `json:"label"`
	Type          string       `json:"type"` // repo|org
	Listed        bool         `json:"listed"`
	Want          int          `json:"want"`
	Have          int          `json:"have"`
	Running       int          `json:"running"`
	Busy          int          `json:"busy"`
	Draining      int          `json:"draining"`
	Min           int          `json:"min"`
	Max           int          `json:"max"`
	Autoscale     bool         `json:"autoscale"`
	Queued        int          `json:"queued"`
	Unsatisfiable int          `json:"unsatisfiable"`
	Error         *TargetError `json:"error"`
}

// Runner is one entry of runners[].
type Runner struct {
	ID          int         `json:"id"`
	Name        string      `json:"name"`
	Target      string      `json:"target"` // "" (null) -> unassigned/unconfigured
	PID         int         `json:"pid"`
	State       RunnerState `json:"state"`
	Status      string      `json:"status"`
	Job         string      `json:"job"`
	JobStarted  int64       `json:"job_started"`
	Elapsed     int         `json:"elapsed"`
	Version     string      `json:"version"`
	Ephemeral   bool        `json:"ephemeral"`
	Draining    bool        `json:"draining"`
	Quarantined bool        `json:"quarantined"`
	Fails       int         `json:"fails"`
	NextRetry   int64       `json:"next_retry"`
	LastErr     string      `json:"lasterr"`
	LogPath     string      `json:"log_path"`
	WorkMB      int         `json:"work_mb"`
}

// Backoff reports whether this runner is in the client-derived "backoff"
// state: restarting with a next_retry still in the future.
func (r Runner) Backoff(now time.Time) bool {
	return r.State == RunnerRestarting && r.NextRetry > now.Unix()
}

// Stale reports whether the snapshot's tick is old enough that the daemon
// (or CLI writer) is presumed stuck or dead: more than 3x the refresh
// interval since tick_ts (interval defaults to 5s when missing/zero, per
// the loader rules).
func (s Snapshot) Stale(now time.Time) (bool, time.Duration) {
	interval := s.RefreshInterval
	if interval <= 0 {
		interval = 5
	}
	age := now.Sub(time.Unix(s.TickTS, 0))
	return age > time.Duration(3*interval)*time.Second, age
}

// RunnersFor returns the runners currently assigned to the given target URL.
func (s Snapshot) RunnersFor(url string) []Runner {
	var out []Runner
	for _, r := range s.Runners {
		if r.Target == url {
			out = append(out, r)
		}
	}
	return out
}

// Unassigned returns runners with no target (the "Unconfigured" pseudo-group).
func (s Snapshot) Unassigned() []Runner {
	var out []Runner
	for _, r := range s.Runners {
		if r.Target == "" {
			out = append(out, r)
		}
	}
	return out
}

// TotalWant sums the pending "want" counts across all targets, i.e. what the
// fleet will converge to (MAX_RUNNERS is checked against this).
func (s Snapshot) TotalWant() int {
	total := 0
	for _, t := range s.Targets {
		total += t.Want
	}
	return total
}

// IsBusy reports whether the runner with the given id is currently running a job.
func (s Snapshot) IsBusy(id int) bool {
	for _, r := range s.Runners {
		if r.ID == id {
			return r.State == RunnerBusy
		}
	}
	return false
}

// TargetByURL finds a target by URL.
func (s Snapshot) TargetByURL(url string) (Target, bool) {
	for _, t := range s.Targets {
		if t.URL == url {
			return t, true
		}
	}
	return Target{}, false
}

// RunnerByID finds a runner by id.
func (s Snapshot) RunnerByID(id int) (Runner, bool) {
	for _, r := range s.Runners {
		if r.ID == id {
			return r, true
		}
	}
	return Runner{}, false
}
