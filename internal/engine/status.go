package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

var (
	reLifecycle = regexp.MustCompile(`Running job:|Job .* completed with result|Listening for Jobs|Could not connect|Authentication failed|Starting Runner listener|Exiting runner`)
	reJobTS     = regexp.MustCompile(`^([0-9][0-9-]* [0-9:]*Z):`)
)

// logTSToEpoch converts a runner log timestamp ("2026-09-27 14:44:46Z").
func logTSToEpoch(ts string) (int64, bool) {
	t, err := time.Parse("2006-01-02 15:04:05", strings.TrimSuffix(ts, "Z"))
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// formatDuration renders seconds as "1h05m" / "12m" / "45s".
func formatDuration(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	h, m, s := secs/3600, (secs%3600)/60, secs%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", s)
}

// parseJobLine extracts the job name and start (0 when unparseable) from a
// "Running job:" line.
func parseJobLine(line string) (name string, start int64) {
	name = line
	if i := strings.Index(line, "Running job: "); i >= 0 {
		name = line[i+len("Running job: "):]
	}
	if m := reJobTS.FindStringSubmatch(line); m != nil {
		start, _ = logTSToEpoch(m[1])
	}
	return name, start
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// logStatus is the status derived from log lines alone: only the newest
// lifecycle line counts.
func logStatus(lines []string, now int64) string {
	if len(lines) == 0 {
		return "no logs"
	}
	last := lastMatch(lines, reLifecycle)
	switch {
	case strings.Contains(last, "Running job:"):
		name, start := parseJobLine(last)
		suffix := ""
		if start > 0 {
			suffix = " (" + formatDuration(now-start) + ")"
		}
		return "running: " + truncRunes(name, 25) + suffix
	case strings.Contains(last, "Listening for Jobs"):
		return "idle"
	case strings.Contains(last, "completed with result: "):
		return "idle (last: " + last[strings.LastIndex(last, "completed with result: ")+len("completed with result: "):] + ")"
	case strings.Contains(last, "Could not connect"):
		return "connection error"
	case strings.Contains(last, "Authentication failed"):
		return "auth error"
	case strings.Contains(last, "Starting Runner listener"):
		return "starting..."
	case strings.Contains(last, "Exiting runner"):
		return "exiting"
	}
	return "unknown"
}

func (e *Engine) logRunnerStatus(id int) string {
	if _, err := os.Stat(e.logPath(id)); err != nil {
		return "no logs"
	}
	return logStatus(tailLines(e.logPath(id), 50), e.now().Unix())
}

// logLastJob is the job a runner is on per its log ("" when none).
func (e *Engine) logLastJob(id int) (string, int64) {
	last := lastMatch(tailLines(e.logPath(id), 50), reLifecycle)
	if !strings.Contains(last, "Running job:") {
		return "", 0
	}
	return parseJobLine(last)
}

// getRunnerStatus is the display status: GitHub only ever upgrades it
// toward busy.
func (e *Engine) getRunnerStatus(id int) string {
	st := e.logRunnerStatus(id)
	if e.ghKnown(id) && e.rt(id).ghBusy && !strings.HasPrefix(st, "running:") {
		st = "busy (per GitHub)"
	}
	return st
}

// isBusy: either source saying busy counts. The GitHub flag can be a poll
// interval stale, so a job started since is only visible in the log.
func (e *Engine) isBusy(id int) bool {
	if !e.isRunning(id) {
		return false
	}
	if e.ghKnown(id) && e.rt(id).ghBusy {
		return true
	}
	return strings.HasPrefix(e.logRunnerStatus(id), "running:")
}

func (e *Engine) runnerState(id int) state.RunnerState {
	r := e.rt(id)
	running := e.isRunning(id)
	if e.quitting {
		if running {
			return state.RunnerDraining
		}
		return state.RunnerStopped
	}
	switch {
	case running && r.draining:
		return state.RunnerDraining
	case running && e.isBusy(id):
		return state.RunnerBusy
	case running && strings.HasPrefix(e.getRunnerStatus(id), "idle"):
		return state.RunnerIdle
	case running:
		return state.RunnerRunning
	case r.draining:
		return state.RunnerDraining
	case e.isMarkedStopped(id):
		return state.RunnerStopped
	case r.quarantined:
		return state.RunnerQuarantined
	}
	return state.RunnerRestarting
}

var reVersion = regexp.MustCompile(`^[0-9][0-9.]*$`)

// runnerVersion: from the bin -> bin.X.Y.Z symlink a self-updated runner
// leaves, else the version saved at setup, else Runner.Listener --version
// (cached). "" when unknown.
func (e *Engine) runnerVersion(id int) string {
	dir := e.runnerDir(id)
	if t, err := os.Readlink(filepath.Join(dir, "bin")); err == nil {
		t = filepath.Base(strings.TrimSuffix(t, "/"))
		if v, ok := strings.CutPrefix(t, "bin."); ok && v != "" {
			return v
		}
	}
	if v := readTrim(e.pidPath(id, "version")); v != "" {
		return v
	}
	if e.versionTried[id] {
		return ""
	}
	e.versionTried[id] = true
	listener := filepath.Join(dir, "bin", "Runner.Listener")
	if fi, err := os.Stat(listener); err != nil || fi.Mode()&0o111 == 0 {
		return ""
	}
	out, err := e.runIn(dir, 10*time.Second, listener, "--version")
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if !reVersion.MatchString(v) {
		return ""
	}
	writeTrim(e.pidPath(id, "version"), v)
	return v
}

// stateWarnings are host notes recomputed per snapshot.
func (e *Engine) stateWarnings() []string {
	var w []string
	if fi, err := os.Stat(e.orphansPath()); err == nil && fi.Size() > 0 {
		w = append(w, "orphaned GitHub registrations need manual cleanup (see "+filepath.Base(e.orphansPath())+")")
	}
	if _, pct := diskFree(e.base); pct >= 0 && pct < 15 {
		w = append(w, fmt.Sprintf("low disk: %d%% free on the runners' filesystem", pct))
	}
	return w
}

// buildSnapshot collects everything into a state.Snapshot. Never calls gh.
// Called with e.mu held.
func (e *Engine) buildSnapshot() state.Snapshot {
	now := e.now()
	nowU := now.Unix()
	cfg := e.cfg.typed()
	s := state.Snapshot{
		Schema:        2,
		ScriptVersion: Version,
		Writer:        "tui",
		// The supervisor runs inside this process, so the UI's "daemon
		// alive" checks treat the TUI itself as the daemon.
		DaemonPID:       os.Getpid(),
		TickTS:          nowU,
		RefreshInterval: cfg.RefreshInterval,
		MaxRunners:      cfg.MaxRunners,
		Config:          cfg,
		LoadedAt:        now,
		Source:          state.SourceUnknown,
	}

	g := &e.ghs
	g.mu.Lock()
	s.GH = state.GH{
		User:      g.user,
		Host:      g.host,
		State:     state.GHState(g.stateLocked()),
		Checked:   g.checked,
		RateReset: g.rateReset,
		PolledAt:  g.pollTS,
	}
	if g.rateKnown {
		s.GH.RateRemaining = int(g.rateRem)
	}
	if g.errClass != "" {
		s.GH.Message = g.errMsg
	} else {
		s.GH.Message = g.probeMsg
	}
	if g.scopes != "" {
		for _, sc := range strings.Split(g.scopes, ",") {
			if sc != "" {
				s.GH.Scopes = append(s.GH.Scopes, sc)
			}
		}
	}
	g.mu.Unlock()
	if s.GH.Host == "" {
		s.GH.Host = e.getenv("GH_HOST")
		if s.GH.Host == "" {
			s.GH.Host = "github.com"
		}
	}

	tarVer := tarballVersion(e.detectRunnerTarball())
	latest := strings.TrimPrefix(e.cachedLatestRunnerTag(), "v")
	s.Tarball = state.Tarball{Version: tarVer, Latest: latest, Stale: tarVer != "" && latest != "" && tarVer != latest}
	free, pct := diskFree(e.base)
	s.Disk = state.Disk{FreeMB: free, PctFree: pct}

	s.Warnings = append(s.Warnings, e.plat.notes...)
	s.Warnings = append(s.Warnings, e.cfg.warnings...)
	s.Warnings = append(s.Warnings, e.cfg.validationWarnings()...)
	for _, l := range e.targets.invalid() {
		s.Warnings = append(s.Warnings, fmt.Sprintf("ignored invalid line in %s: %s", filepath.Base(e.targets.path), l))
	}
	s.Warnings = append(s.Warnings, e.stateWarnings()...)

	e.duMu.Lock()
	work := make(map[int]int, len(e.workMB))
	for k, v := range e.workMB {
		work[k] = v
	}
	e.duMu.Unlock()

	collect := func(t string, ids []int) (conf, run, busy, drain int) {
		for _, id := range ids {
			r := e.rt(id)
			st := e.runnerState(id)
			rn := state.Runner{
				ID:          id,
				Name:        e.registeredName(id),
				Target:      t,
				State:       st,
				Version:     e.runnerVersion(id),
				Ephemeral:   e.isEphemeral(id),
				Draining:    r.draining,
				Quarantined: r.quarantined,
				Fails:       r.fails,
				LastErr:     r.lastErr,
				LogPath:     e.logPath(id),
				WorkMB:      work[id],
			}
			if e.isRunning(id) {
				rn.PID = e.readPID(id)
				rn.Status = e.getRunnerStatus(id)
				if job, start := e.logLastJob(id); job != "" {
					rn.Job, rn.JobStarted = job, start
					if start > 0 {
						rn.Elapsed = int(max(nowU-start, 0))
					}
				}
			}
			if st == state.RunnerRestarting && r.lastStart > 0 {
				rn.NextRetry = r.lastStart + e.backoff(r.fails)
			}
			s.Runners = append(s.Runners, rn)
			switch st {
			case state.RunnerDraining:
				drain++
			case state.RunnerBusy:
				busy++
				run++
				conf++
			case state.RunnerRunning, state.RunnerIdle:
				run++
				conf++
			default:
				conf++
			}
		}
		return
	}

	for _, t := range e.knownTargets() {
		conf, run, busy, drain := collect(t, e.runnerIDsForTarget(t))
		key := targetKey(t)
		tg := state.Target{
			URL:      t,
			Label:    targetLabel(t),
			Type:     targetType(t),
			Listed:   e.targets.contains(t),
			Want:     conf,
			Have:     conf,
			Running:  run,
			Busy:     busy,
			Draining: drain,
		}
		if w, ok := e.want[key]; ok {
			tg.Want = w
		}
		if mn, mx, ok := e.targets.bounds(t); ok {
			tg.Min, tg.Max = mn, mx
			tg.Autoscale = cfg.Autoscale
		}
		if q, ok := e.queue[key]; ok {
			tg.Queued = q.sat
			tg.Unsatisfiable = q.unsat
		}
		if te, ok := e.targetError(t); ok {
			tg.Error = &state.TargetError{Class: te.class, Message: ghErrorNote(te.class, te.msg), Since: te.since}
		}
		s.Targets = append(s.Targets, tg)
	}
	collect("", e.unassignedRunnerIDs())
	return s
}

// Version is the engine's version, reported as the snapshot's version.
const Version = "4.0.0"
