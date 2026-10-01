package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// On-disk runner state is kept to what must survive the TUI exiting while
// runners keep running:
//
//	runners/runner-N/.runner          written by config.sh (gitHubUrl, agentName)
//	runners/.pids/runner-N.pid        pid of the runner's process tree root
//	runners/.pids/runner-N.target     target URL (an ephemeral runner deletes .runner)
//	runners/.pids/runner-N.name       registered name (kept across prefix changes)
//	runners/.pids/runner-N.ephemeral  registered with --ephemeral
//	runners/.pids/runner-N.version    version it was set up with
//	runners/.pids/runner-N.stopped    stopped on purpose: never auto-restarted
//
// Failure counts, quarantine, drains, GitHub sightings and the like live in
// memory (runnerRT) and start fresh with each engine.

var (
	reGitHubURL = regexp.MustCompile(`"gitHubUrl": *"([^"]*)"`)
	reAgentName = regexp.MustCompile(`"agentName": *"([^"]*)"`)
)

// runnerRT is the in-memory per-runner supervisor state.
type runnerRT struct {
	fails       int
	lastStart   int64
	quarantined bool
	lastErr     string
	ghOffline   int  // consecutive polls GitHub reported it offline
	ghSeen      bool // listed in the last poll
	ghBusy      bool // GitHub said busy in the last poll
	draining    bool // remove once its job finishes
}

func (e *Engine) rt(id int) *runnerRT {
	r, ok := e.rts[id]
	if !ok {
		r = &runnerRT{}
		e.rts[id] = r
	}
	return r
}

func (e *Engine) runnerDir(id int) string {
	return filepath.Join(e.base, "runner-"+strconv.Itoa(id))
}

func (e *Engine) pidPath(id int, ext string) string {
	return filepath.Join(e.pidDir, "runner-"+strconv.Itoa(id)+"."+ext)
}

func (e *Engine) logPath(id int) string {
	return filepath.Join(e.logDir, "runner-"+strconv.Itoa(id)+".log")
}

func (e *Engine) orphansPath() string { return filepath.Join(e.base, ".orphaned-registrations") }

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeTrim(path, v string) {
	_ = os.WriteFile(path, []byte(v+"\n"), 0o644)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runnerIDs lists runner-N directories, sorted numerically.
func (e *Engine) runnerIDs() []int {
	ents, err := os.ReadDir(e.base)
	if err != nil {
		return nil
	}
	var ids []int
	for _, d := range ents {
		s, ok := strings.CutPrefix(d.Name(), "runner-")
		if !ok || !reDigits.MatchString(s) {
			continue
		}
		if !d.IsDir() {
			// follow a symlinked runner dir
			if fi, err := os.Stat(filepath.Join(e.base, d.Name())); err != nil || !fi.IsDir() {
				continue
			}
		}
		if n, err := strconv.Atoi(s); err == nil {
			ids = append(ids, n)
		}
	}
	sort.Ints(ids)
	return ids
}

func (e *Engine) runnerExists(id int) bool {
	fi, err := os.Stat(e.runnerDir(id))
	return err == nil && fi.IsDir()
}

func (e *Engine) readPID(id int) int {
	n, _ := strconv.Atoi(readTrim(e.pidPath(id, "pid")))
	return n
}

func (e *Engine) markStopped(id int)          { writeTrim(e.pidPath(id, "stopped"), "") }
func (e *Engine) unmarkStopped(id int)        { os.Remove(e.pidPath(id, "stopped")) }
func (e *Engine) isMarkedStopped(id int) bool { return exists(e.pidPath(id, "stopped")) }

// isEphemeral: the registration mode is fixed at setup time.
func (e *Engine) isEphemeral(id int) bool { return exists(e.pidPath(id, "ephemeral")) }

func (e *Engine) dotRunner(id int) string {
	b, err := os.ReadFile(filepath.Join(e.runnerDir(id), ".runner"))
	if err != nil {
		return ""
	}
	return string(b)
}

func (e *Engine) isConfigured(id int) bool {
	return exists(filepath.Join(e.runnerDir(id), ".runner"))
}

// registeredURL is the target a runner belongs to: gitHubUrl from .runner,
// else the saved .target ("" when neither exists).
func (e *Engine) registeredURL(id int) string {
	u := ""
	if m := reGitHubURL.FindStringSubmatch(e.dotRunner(id)); m != nil {
		u = m[1]
	}
	if u == "" {
		u = readTrim(e.pidPath(id, "target"))
	}
	if u == "" {
		return ""
	}
	return normalizeURL(u)
}

// registeredName is the name the runner is registered under on GitHub:
// agentName from .runner, else the saved name, else PREFIX-N.
func (e *Engine) registeredName(id int) string {
	if m := reAgentName.FindStringSubmatch(e.dotRunner(id)); m != nil && m[1] != "" {
		return m[1]
	}
	if n := readTrim(e.pidPath(id, "name")); n != "" {
		return n
	}
	return e.cfg.vals["RUNNER_NAME_PREFIX"] + "-" + strconv.Itoa(id)
}

func (e *Engine) runnerIDsForTarget(u string) []int {
	var out []int
	for _, id := range e.runnerIDs() {
		if sameTarget(e.registeredURL(id), u) {
			out = append(out, id)
		}
	}
	return out
}

// countRunnersForTarget excludes draining runners: they are on their way out.
func (e *Engine) countRunnersForTarget(u string) int {
	n := 0
	for _, id := range e.runnerIDsForTarget(u) {
		if !e.rt(id).draining {
			n++
		}
	}
	return n
}

func (e *Engine) unassignedRunnerIDs() []int {
	var out []int
	for _, id := range e.runnerIDs() {
		if e.registeredURL(id) == "" {
			out = append(out, id)
		}
	}
	return out
}

// knownTargets is the targets file in order, then any target a runner is
// registered to that isn't listed; deduplicated case-insensitively.
func (e *Engine) knownTargets() []string {
	var out []string
	add := func(t string) {
		if t == "" {
			return
		}
		for _, u := range out {
			if sameTarget(u, t) {
				return
			}
		}
		out = append(out, t)
	}
	for _, t := range e.targets.load() {
		add(t)
	}
	for _, id := range e.runnerIDs() {
		add(e.registeredURL(id))
	}
	return out
}

// clearFailureState gives a runner a clean slate (manual start).
func (e *Engine) clearFailureState(id int) {
	r := e.rt(id)
	draining := r.draining
	*r = runnerRT{lastStart: r.lastStart, draining: draining}
}

// clearRunnerState forgets everything about a removed runner.
func (e *Engine) clearRunnerState(id int) {
	delete(e.rts, id)
	for _, ext := range []string{"pid", "stopped", "target", "name", "ephemeral", "version",
		// legacy bash state files
		"failcount", "quarantined", "lasterr", "ghoffline", "ghbusy", "ghseen", "draining", "laststart", "workmb"} {
		os.Remove(e.pidPath(id, ext))
	}
	e.duMu.Lock()
	delete(e.workMB, id)
	e.duMu.Unlock()
}

func (e *Engine) appendOrphan(name string) {
	f, err := os.OpenFile(e.orphansPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(name + "\n")
}

func (e *Engine) setLastErr(id int, msg string) {
	e.rt(id).lastErr = msg
	e.dlog("runner-" + strconv.Itoa(id) + ": " + msg)
}
