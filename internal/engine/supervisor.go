package engine

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// defaultBackoffBase is the first restart delay: 5s, 10s, 20s, 40s, 80s...
var defaultBackoffBase = 5 * time.Second

func (e *Engine) backoff(fails int) int64 {
	return int64(defaultBackoffBase/time.Second) << fails
}

// tailLines is the last n lines of a file (reading at most 256 KiB).
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const max = 256 * 1024
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := fi.Size() - max
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	b, _ := io.ReadAll(f)
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if off > 0 && len(lines) > 1 {
		lines = lines[1:] // partial first line
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// lastMatch is the newest line matching re.
func lastMatch(lines []string, re *regexp.Regexp) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if re.MatchString(lines[i]) {
			return lines[i]
		}
	}
	return ""
}

// rotateLog copies an oversized log to .log.1 and truncates it in place:
// the runner holds the fd in append mode, so writing continues correctly.
func (e *Engine) rotateLog(id int) {
	f := e.logPath(id)
	fi, err := os.Stat(f)
	if err != nil {
		return
	}
	if fi.Size() <= int64(e.cfg.int("MAX_LOG_SIZE_MB"))*1024*1024 {
		return
	}
	if src, err := os.Open(f); err == nil {
		if dst, err := os.Create(f + ".1"); err == nil {
			io.Copy(dst, src)
			dst.Close()
		}
		src.Close()
	}
	os.Truncate(f, 0)
}

var (
	reExitReason = regexp.MustCompile(`Session Conflict|deprecated version exit code|exit with terminated error|unknown error code|Authentication failed|[Cc]redentials|retryable error|because of updating|Update finished|Listening for Jobs|Starting Runner listener|Running job:|completed with result`)
	reEphemDone  = regexp.MustCompile(`completed with result|\[runnermaxxer\] re-registering`)
	reNonDigit   = regexp.MustCompile(`[^0-9]`)
	reCreds      = regexp.MustCompile(`Authentication failed|[Cc]redentials`)
)

// exitReason classifies why a runner's listener stopped, from its log:
// conflict | deprecated | terminated | unknown (detail = code) | creds, or
// "" when a lifecycle/retry line is newer than any exit reason.
func exitReason(lines []string) (class, detail string) {
	last := lastMatch(lines, reExitReason)
	switch {
	case reCreds.MatchString(last):
		return "creds", ""
	case strings.Contains(last, "Session Conflict"):
		return "conflict", ""
	case strings.Contains(last, "deprecated version exit code"):
		return "deprecated", ""
	case strings.Contains(last, "exit with terminated error"):
		return "terminated", ""
	case strings.Contains(last, "unknown error code"):
		code := last
		if i := strings.LastIndex(last, "code: "); i >= 0 {
			code = last[i+len("code: "):]
		}
		return "unknown", reNonDigit.ReplaceAllString(code, "")
	}
	return "", ""
}

func (e *Engine) runnerExitReason(id int) (string, string) {
	return exitReason(tailLines(e.logPath(id), 40))
}

// exitNote is the lasterr text for an exitReason.
func exitNote(class, detail string) string {
	switch class {
	case "conflict":
		return "session conflict: another process holds this runner's registration (cloned dir? second manager?) - remove and re-add"
	case "deprecated":
		return "runner version too old for GitHub - download the latest runner, then remove and re-add"
	case "terminated":
		return "listener exited (terminated, exit 1) - see log"
	case "unknown":
		return "listener exited with code " + detail + " - see log"
	case "creds":
		return "runner credentials rejected - remove and re-add"
	}
	return ""
}

// ephemeralJobFinished: an ephemeral runner exited after its one job (the
// runner deleted .runner, and the newest job-completed line is newer than
// our last re-registration attempt). That exit is normal, not a crash.
func (e *Engine) ephemeralJobFinished(id int) bool {
	if !e.isEphemeral(id) || e.isConfigured(id) {
		return false
	}
	last := lastMatch(tailLines(e.logPath(id), 200), reEphemDone)
	return strings.Contains(last, "completed with result")
}

// superviseRunners restarts runners that died, with exponential backoff,
// quarantining crash loops; finishes drains.
func (e *Engine) superviseRunners() {
	now := e.now().Unix()
	maxAttempts := e.cfg.int("MAX_RESTART_ATTEMPTS")
	for _, id := range e.runnerIDs() {
		r := e.rt(id)
		e.rotateLog(id)
		if r.draining {
			if !e.isBusy(id) {
				e.dlog(fmt.Sprintf("runner-%d: drained (no job running) - removing", id))
				e.removeRunner(id)
			}
			continue
		}
		if e.isMarkedStopped(id) {
			continue
		}
		if e.isRunning(id) {
			// Healthy for a while: forget past crashes
			if now-r.lastStart >= 60 {
				r.fails = 0
			}
			continue
		}
		if r.quarantined {
			continue
		}
		// Ephemeral runner done with its job: re-register and relaunch now,
		// without counting a crash or backing off
		if e.ephemeralJobFinished(id) {
			r.fails = 0
			e.dlog(fmt.Sprintf("runner-%d: ephemeral job finished - re-registering", id))
			e.startRunner(id)
			continue
		}
		// Conflicts and deprecated versions don't fix themselves
		class, detail := e.runnerExitReason(id)
		if class == "conflict" || class == "deprecated" {
			r.quarantined = true
			e.setLastErr(id, exitNote(class, detail)+" - quarantined")
			continue
		}
		if r.fails >= maxAttempts {
			r.quarantined = true
			e.setLastErr(id, fmt.Sprintf("crash-looped %dx - quarantined (start it to retry)", r.fails))
			continue
		}
		if now-r.lastStart < e.backoff(r.fails) {
			continue
		}
		// Count the attempt regardless of outcome; only sustained uptime
		// (60s above) clears it
		r.fails++
		if class != "" {
			e.setLastErr(id, exitNote(class, detail))
		}
		e.dlog(fmt.Sprintf("runner-%d: not running - restart attempt %d", id, r.fails))
		e.startRunner(id)
		r.lastStart = now
	}
}

// effectiveHealthTicks scales the poll interval with the number of targets
// that have runners: GH_HEALTH_TICKS * max(1, ceil(n/10)).
func effectiveHealthTicks(ticks, n int) int {
	f := (n + 9) / 10
	if f < 1 {
		f = 1
	}
	return ticks * f
}

// ghDataFresh: the last successful poll is younger than two poll intervals
// (min 30s). Polling disabled: never fresh.
func (e *Engine) ghDataFresh() bool {
	ticks := e.cfg.int("GH_HEALTH_TICKS")
	if ticks <= 0 {
		return false
	}
	e.ghs.mu.Lock()
	ts := e.ghs.pollTS
	e.ghs.mu.Unlock()
	if ts == 0 {
		return false
	}
	max := int64(effectiveHealthTicks(ticks, e.healthN) * e.cfg.int("REFRESH_INTERVAL") * 2)
	if max < 30 {
		max = 30
	}
	return e.now().Unix()-ts <= max
}

// ghKnown: fresh GitHub data exists and listed this runner.
func (e *Engine) ghKnown(id int) bool { return e.rt(id).ghSeen && e.ghDataFresh() }

// noteTickTime detects a clock jump (sleep/wake) since the engine was last
// alive and pauses GitHub-health recycles for 2 minutes.
func (e *Engine) noteTickTime() {
	now := e.now().Unix()
	prev := e.lastAlive.Load()
	e.lastAlive.Store(now)
	if prev == 0 {
		return
	}
	if now-prev > int64(3*e.cfg.int("REFRESH_INTERVAL")) {
		e.wakeAt = now
		e.dlog(fmt.Sprintf("clock jumped %ds (sleep/wake?) - GitHub health recycles paused for 2 min", now-prev))
	}
}

func (e *Engine) recentlyWoke() bool {
	return e.wakeAt > 0 && e.now().Unix()-e.wakeAt < 120
}

type ghRow struct {
	status string
	busy   string
}

// checkGithubHealth cross-checks local runners against GitHub: a process
// can be alive while GitHub considers it offline. Two consecutive offline
// sightings recycle it. Skipped when the API is unreachable, so an outage
// doesn't trigger mass restarts. Then autoscales on the fresh data.
func (e *Engine) checkGithubHealth() {
	now := e.now().Unix()
	n := 0
	for _, t := range e.knownTargets() {
		if len(e.runnerIDsForTarget(t)) > 0 {
			n++
		}
	}
	e.healthN = n
	autoscale := 0
	if e.cfg.vals["AUTOSCALE"] == "1" {
		autoscale = 1
	}
	if !e.ghRateOK(n * (1 + autoscale*11)) {
		return
	}
	// A broken token is re-probed once per poll, so the state recovers by
	// itself after `gh auth login`
	if e.ghState() != "ok" {
		e.ghAuthProbe()
	}
	woke := e.recentlyWoke()
	polled := false

	for _, t := range e.knownTargets() {
		ids := e.runnerIDsForTarget(t)
		if len(ids) == 0 {
			continue
		}
		out, r := e.ghAPI("--paginate", targetAPIEndpoint(t), "--jq", `.runners[] | "\(.name)\t\(.status)\t\(.busy)"`)
		if !r.ok() {
			e.targetSetError(t, r.Class, r.Msg)
			// No API view: fall back to the logs
			for _, id := range ids {
				rr := e.rt(id)
				rr.ghSeen, rr.ghBusy = false, false
			}
			continue
		}
		// Listing works; a scope error stays until the probe or a
		// registration clears it
		e.targetClearErrorExcept(t, "scope")
		polled = true
		rows := map[string]ghRow{}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(line, "\t")
			if len(f) < 3 {
				continue
			}
			if _, dup := rows[f[0]]; !dup {
				rows[f[0]] = ghRow{status: f[1], busy: f[2]}
			}
		}

		for _, id := range ids {
			rr := e.rt(id)
			if !e.isRunning(id) {
				rr.ghOffline, rr.ghSeen, rr.ghBusy = 0, false, false
				continue
			}
			// A draining runner is on its way out; never recycle it
			if rr.draining {
				rr.ghOffline = 0
				continue
			}
			name := e.registeredName(id)
			row, seen := rows[name]
			rr.ghSeen = seen
			rr.ghBusy = seen && row.busy == "true"

			// Grace period: a fresh runner may not show online yet
			if now-rr.lastStart < 120 {
				continue
			}
			// Never recycle right after a wake, or a runner running a job by
			// either account
			if woke || rr.ghBusy || strings.HasPrefix(e.logRunnerStatus(id), "running:") {
				continue
			}
			if seen && row.status == "online" {
				rr.ghOffline = 0
				continue
			}
			rr.ghOffline++
			if rr.ghOffline >= 2 {
				e.setLastErr(id, "GitHub reported offline - recycled")
				e.stopProcs([]int{id})
				e.startRunner(id)
				rr.ghOffline = 0
			}
		}
	}
	if polled {
		e.ghs.mu.Lock()
		e.ghs.pollTS = now
		e.ghs.mu.Unlock()
	}
	e.autoscaleTick()
}

// ---- disk ----------------------------------------------------------------------

const diskUsageTicks = 120

// diskUsageTick measures each runner's _work + _diag in the background
// (every diskUsageTicks ticks, and right away for a runner with no figure
// yet) and notes once when free space drops under 15%.
func (e *Engine) diskUsageTick() {
	e.diskTick++
	all := false
	if e.diskTick >= diskUsageTicks {
		all, e.diskTick = true, 0
	}
	for _, id := range e.runnerIDs() {
		e.duMu.Lock()
		_, have := e.workMB[id]
		pending := e.duPending[id]
		if pending || (have && !all) {
			e.duMu.Unlock()
			continue
		}
		e.duPending[id] = true
		e.duMu.Unlock()
		dir := e.runnerDir(id)
		go func(id int) {
			mb := duMB(dir+"/_work", dir+"/_diag")
			e.duMu.Lock()
			e.workMB[id] = mb
			delete(e.duPending, id)
			e.duMu.Unlock()
		}(id)
	}
	if _, pct := diskFree(e.base); pct >= 0 && pct < 15 {
		if !e.diskLowWarned {
			mb, _ := diskFree(e.base)
			e.dlog(fmt.Sprintf("low disk: %d%% free on the runners' filesystem (%d MB)", pct, mb))
			e.diskLowWarned = true
		}
	} else {
		e.diskLowWarned = false
	}
}

// duMB is du -sk of the existing paths, in MB.
func duMB(paths ...string) int {
	var args []string
	for _, p := range paths {
		if exists(p) {
			args = append(args, p)
		}
	}
	if len(args) == 0 {
		return 0
	}
	out, _ := exec.Command("du", append([]string{"-sk"}, args...)...).Output()
	kb := 0
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			n, _ := strconv.Atoi(f[0])
			kb += n
		}
	}
	return kb / 1024
}
