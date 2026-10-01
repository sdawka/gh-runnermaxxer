// Package engine is the runner manager behind the TUI: config and targets,
// the gh CLI wrapper, runner setup/start/stop/remove, the self-healing
// supervisor, GitHub health checks and autoscaling. It is a port of the
// engine half of runnermaxxer.sh, using the same files on disk, so a
// fleet set up by the script is adopted as is.
//
// One Engine per data directory: Open takes runners/.runnermaxxer.lock for
// the life of the engine. Mutations and supervisor ticks are serialised by
// one mutex; Snapshot reads a cached copy, so long operations never block
// the UI.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// Options configures Open.
type Options struct {
	// Dir holds .runnermaxxer.conf, .runnermaxxer.targets and the runner
	// tarball (default: the working directory).
	Dir string
	// RunnerBase holds runner-N directories, .pids and .logs (default:
	// $RUNNER_BASE_DIR, else Dir/runners).
	RunnerBase string
	// GH is the gh binary (default: $RUNNERMAXXER_GH, else "gh").
	GH string
	// Pending returns the UI's unapplied target counts (URL or owner/repo
	// -> count); autoscale leaves those targets alone. It is called with the
	// engine's lock held and must not call back into the engine.
	Pending func() map[string]int
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// ShutdownMode says what Shutdown does with running runners.
type ShutdownMode int

const (
	// StopNow stops every runner's process tree right away.
	StopNow ShutdownMode = iota
	// DrainThenStop lets runners finish their current job, stopping each
	// as soon as it is idle.
	DrainThenStop
)

var (
	// ErrNeedsSetup: there is no .runnermaxxer.conf yet; call Setup.
	ErrNeedsSetup = errors.New("not set up yet - choose a runner name prefix and maximum first")
	// ErrClosed: the engine has been shut down.
	ErrClosed = errors.New("engine is shut down")
)

const eventRing = 500

// Engine manages one runner fleet.
type Engine struct {
	dir, base, pidDir, logDir string
	confPath                  string
	getenv                    func(string) string
	now                       func() time.Time
	pending                   func() map[string]int

	plat       platform
	labelsOnce sync.Once
	labelsStr  string

	ghExec ghExecFunc
	ghs    ghStatus

	// Everything below is guarded by mu unless noted.
	mu            sync.Mutex
	cfg           *settings
	needsSetup    bool
	confSig       string
	targets       targetsFile
	rts           map[int]*runnerRT
	want          map[string]int       // by targetKey: requested count until reached
	queue         map[string]queueInfo // by targetKey: last queue depth
	idleSince     map[string]int64     // by targetKey
	versionTried  map[int]bool
	tickN         int
	polledOnce    bool
	healthN       int
	diskTick      int
	diskLowWarned bool
	wakeAt        int64
	quitting      bool
	procCache     []proc
	procCached    bool

	lastAlive atomic.Int64 // unix time the engine was last seen running

	// test hooks
	listProcs   func() ([]proc, error)
	startFn     func(id int) error
	stopProcsFn func(ids []int)
	removeFn    func(id int) string

	duMu      sync.Mutex
	workMB    map[int]int
	duPending map[int]bool

	opCtx    context.Context
	opCancel context.CancelFunc

	snapMu     sync.Mutex
	snap       state.Snapshot
	snapCh     chan state.Snapshot
	snapClosed bool
	lastPush   time.Time

	evMu     sync.Mutex
	events   []string
	eventLog string

	lockFile *os.File
	wake     chan struct{}
	loopStop chan struct{}
	loopDone chan struct{}
	closed   atomic.Bool
	shutMu   sync.Mutex
}

// newEngine builds an engine without locking, reconciling or starting the
// supervisor (unit tests use it directly).
func newEngine(opt Options) (*Engine, error) {
	dir := opt.Dir
	if dir == "" {
		dir = "."
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		dir:          dir,
		confPath:     filepath.Join(dir, ".runnermaxxer.conf"),
		getenv:       os.Getenv,
		now:          opt.Now,
		pending:      opt.Pending,
		rts:          map[int]*runnerRT{},
		want:         map[string]int{},
		queue:        map[string]queueInfo{},
		idleSince:    map[string]int64{},
		versionTried: map[int]bool{},
		workMB:       map[int]int{},
		duPending:    map[int]bool{},
		snapCh:       make(chan state.Snapshot, 1),
		wake:         make(chan struct{}, 1),
		loopStop:     make(chan struct{}),
		loopDone:     make(chan struct{}),
		listProcs:    psList,
	}
	if e.now == nil {
		e.now = time.Now
	}
	base := opt.RunnerBase
	if base == "" {
		base = e.getenv("RUNNER_BASE_DIR")
	}
	if base == "" {
		base = filepath.Join(dir, "runners")
	}
	if e.base, err = filepath.Abs(base); err != nil {
		return nil, err
	}
	e.pidDir = filepath.Join(e.base, ".pids")
	e.logDir = filepath.Join(e.base, ".logs")
	e.eventLog = filepath.Join(e.logDir, "runnermaxxer.log")
	gh := opt.GH
	if gh == "" {
		gh = e.getenv("RUNNERMAXXER_GH")
	}
	if gh == "" {
		gh = "gh"
	}
	e.ghExec = execGH(gh)
	e.opCtx, e.opCancel = context.WithCancel(context.Background())
	e.startFn = e.startRunnerReal
	e.stopProcsFn = e.stopProcsReal
	e.removeFn = e.removeRunnerReal
	e.targets = targetsFile{
		path:       filepath.Join(dir, ".runnermaxxer.targets"),
		maxRunners: func() int { return e.cfg.int("MAX_RUNNERS") },
	}
	if e.plat, err = detectPlatform(); err != nil {
		return nil, err
	}
	e.loadConfig()
	return e, nil
}

// Open starts the engine for opt.Dir: takes the lock, adopts runners left
// running by a previous manager and starts the supervisor. It works with no
// configuration yet (NeedsSetup reports true and the supervisor idles).
func Open(ctx context.Context, opt Options) (*Engine, error) {
	e, err := newEngine(opt)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{e.base, e.pidDir, e.logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := e.acquireLock(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		e.releaseLock()
		return nil, err
	}
	go e.labels() // slow probes (system_profiler); setup waits on it if needed

	e.mu.Lock()
	e.reconcile()
	e.dlog(fmt.Sprintf("engine started (pid %d, %d runner(s), tick %ds)", os.Getpid(), len(e.runnerIDs()), e.cfg.int("REFRESH_INTERVAL")))
	e.publishLocked()
	e.mu.Unlock()

	go e.loop()
	return e, nil
}

// labels are the detected runner labels, probed once.
func (e *Engine) labels() string {
	e.labelsOnce.Do(func() { e.labelsStr = detectLabels(e.plat) })
	return e.labelsStr
}

// ---- config ------------------------------------------------------------------

// loadConfig (re)reads the config file (env overrides first, the file
// wins) and moves a v2 REPO_URL/ORG_URL into the targets file.
func (e *Engine) loadConfig() {
	s := newSettings(e.getenv)
	_, statErr := os.Stat(e.confPath)
	e.needsSetup = statErr != nil
	if err := s.parseFile(e.confPath); err != nil {
		s.warn(fmt.Sprintf("cannot read %s: %v", filepath.Base(e.confPath), err))
	}
	if s.vals["RUNNER_NAME_PREFIX"] == "" {
		s.vals["RUNNER_NAME_PREFIX"] = truncRunes(defaultHostname(), 60)
	}
	e.cfg = s
	if legacy := s.vals["REPO_URL"] + s.vals["ORG_URL"]; legacy != "" {
		u := s.vals["REPO_URL"]
		if u == "" {
			u = s.vals["ORG_URL"]
		}
		u = normalizeURL(u)
		if validTargetURL(u) {
			if err := e.targets.add(u); err == nil {
				e.dlog(fmt.Sprintf("moved target %s from %s to %s", u, filepath.Base(e.confPath), filepath.Base(e.targets.path)))
			}
		}
		s.vals["REPO_URL"], s.vals["ORG_URL"] = "", ""
		if statErr == nil {
			s.save(e.confPath)
		}
	}
	s.sanitize()
	e.confSig = e.fileSigs()
}

func (e *Engine) fileSigs() string {
	return fileSig(e.confPath) + " " + fileSig(e.targets.path)
}

// reloadConfigIfChanged picks up edits to the config/targets files made
// outside the engine.
func (e *Engine) reloadConfigIfChanged() {
	if e.fileSigs() == e.confSig {
		return
	}
	e.loadConfig()
	e.dlog("configuration reloaded (config or targets file changed)")
}

// ---- lock ----------------------------------------------------------------------

func pidAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (e *Engine) pidCmd(pid int) string {
	ps, _ := e.listProcs()
	for _, p := range ps {
		if p.pid == pid {
			return p.cmd
		}
	}
	return ""
}

// acquireLock takes an exclusive flock on runners/.runnermaxxer.lock and
// writes our pid into it. It also refuses while runnermaxxer.sh holds the
// same file (it locks by pid, not flock) or runs as a daemon.
func (e *Engine) acquireLock() error {
	path := filepath.Join(e.base, ".runnermaxxer.lock")
	if pid, _ := strconv.Atoi(readTrim(filepath.Join(e.base, ".daemon.pid"))); pid != os.Getpid() && pidAlive(pid) && strings.Contains(e.pidCmd(pid), "--daemon") {
		return fmt.Errorf("the runnermaxxer.sh daemon is running (pid %d) - stop it first (runnermaxxer.sh --stop-daemon)", pid)
	}
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			pid := readTrim(path)
			f.Close()
			return fmt.Errorf("another runnermaxxer is already managing %s (pid %s)", e.base, pid)
		}
		// The holder may have removed the file between our open and flock
		var st1, st2 syscall.Stat_t
		if syscall.Fstat(int(f.Fd()), &st1) != nil || syscall.Stat(path, &st2) != nil || st1.Ino != st2.Ino {
			f.Close()
			continue
		}
		if pid, _ := strconv.Atoi(readTrim(path)); pid != os.Getpid() && pidAlive(pid) && strings.Contains(e.pidCmd(pid), "runnermaxxer.sh") {
			f.Close()
			return fmt.Errorf("runnermaxxer.sh is running (pid %d) - quit it first", pid)
		}
		f.Truncate(0)
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		e.lockFile = f
		return nil
	}
	return errors.New("could not take " + path)
}

func (e *Engine) releaseLock() {
	if e.lockFile == nil {
		return
	}
	os.Remove(e.lockFile.Name())
	syscall.Flock(int(e.lockFile.Fd()), syscall.LOCK_UN)
	e.lockFile.Close()
	e.lockFile = nil
}

// reconcile adopts live runner processes into fresh pid files and drops pid
// files whose process is gone (runners are detached and outlive managers).
func (e *Engine) reconcile() {
	e.invalidateProcs()
	for _, id := range e.runnerIDs() {
		if e.isRunning(id) {
			continue
		}
		if pid := mainPID(e.runnerProcs(id)); pid > 0 {
			writeTrim(e.pidPath(id, "pid"), strconv.Itoa(pid))
			e.dlog(fmt.Sprintf("runner-%d: adopted running process %d", id, pid))
		} else {
			os.Remove(e.pidPath(id, "pid"))
		}
	}
	e.invalidateProcs()
}

// ---- supervisor loop -------------------------------------------------------------

// loop ticks right away, then every REFRESH_INTERVAL. While a long mutation
// holds the lock it keeps the snapshot's tick time fresh, so the UI doesn't
// report the supervisor as stalled.
func (e *Engine) loop() {
	defer close(e.loopDone)
	for {
		for !e.mu.TryLock() {
			e.heartbeat()
			select {
			case <-e.loopStop:
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		select {
		case <-e.loopStop:
			e.mu.Unlock()
			return
		default:
		}
		e.tick()
		interval := time.Duration(e.cfg.int("REFRESH_INTERVAL")) * time.Second
		e.mu.Unlock()
		t := time.NewTimer(interval)
		select {
		case <-e.loopStop:
			t.Stop()
			return
		case <-e.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

func (e *Engine) nudge() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// heartbeat marks the engine alive while the loop waits for the lock.
func (e *Engine) heartbeat() {
	now := e.now()
	e.lastAlive.Store(now.Unix())
	e.snapMu.Lock()
	defer e.snapMu.Unlock()
	if e.snapClosed {
		return
	}
	e.snap.TickTS = now.Unix()
	if now.Sub(e.lastPush) >= time.Second {
		e.pushLocked(e.snap)
		e.lastPush = now
	}
}

// tick is one supervisor pass. Called with e.mu held.
func (e *Engine) tick() {
	e.invalidateProcs()
	e.reloadConfigIfChanged()
	if e.needsSetup {
		e.lastAlive.Store(e.now().Unix())
		e.publishLocked()
		return
	}
	e.noteTickTime()
	if e.quitting {
		e.quitPass()
		e.publishLocked()
		return
	}
	e.superviseRunners()
	e.diskUsageTick()
	if ticks := e.cfg.int("GH_HEALTH_TICKS"); ticks > 0 {
		e.tickN++
		if !e.polledOnce || e.tickN >= effectiveHealthTicks(ticks, e.healthN) {
			e.tickN, e.polledOnce = 0, true
			e.checkGithubHealth()
		}
	}
	e.lastAlive.Store(e.now().Unix())
	e.publishLocked()
}

// ---- snapshots and events ------------------------------------------------------

// publishLocked rebuilds the cached snapshot and pushes it. Called with
// e.mu held.
func (e *Engine) publishLocked() {
	s := e.buildSnapshot()
	e.snapMu.Lock()
	defer e.snapMu.Unlock()
	e.snap = s
	if !e.snapClosed {
		e.pushLocked(s)
		e.lastPush = e.now()
	}
}

// pushLocked replaces whatever unread snapshot is in the channel.
func (e *Engine) pushLocked(s state.Snapshot) {
	select {
	case <-e.snapCh:
	default:
	}
	select {
	case e.snapCh <- s:
	default:
	}
}

// Snapshot is the latest state (cached; never blocks on a running
// operation).
func (e *Engine) Snapshot() state.Snapshot {
	e.snapMu.Lock()
	defer e.snapMu.Unlock()
	return e.snap
}

// Snapshots delivers a snapshot after every tick and mutation. It holds at
// most one (a stale unread one is replaced) and is closed by Shutdown.
func (e *Engine) Snapshots() <-chan state.Snapshot { return e.snapCh }

// dlog records an event: in memory (the last 500) and appended to
// runners/.logs/runnermaxxer.log as "YYYY-mm-dd HH:MM:SS msg".
func (e *Engine) dlog(msg string) {
	line := e.now().Format("2006-01-02 15:04:05") + " " + msg
	e.evMu.Lock()
	defer e.evMu.Unlock()
	e.events = append(e.events, line)
	if len(e.events) > eventRing {
		e.events = append([]string(nil), e.events[len(e.events)-eventRing:]...)
	}
	if e.logDir != "" {
		appendLog(e.eventLog, line)
	}
}

// Events returns the recent event lines, oldest first.
func (e *Engine) Events() []string {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	return append([]string(nil), e.events...)
}

// EventLogPath is the file events are appended to.
func (e *Engine) EventLogPath() string { return e.eventLog }

// ---- operations ------------------------------------------------------------------

// op runs fn as one serialised mutation and publishes the result.
func (e *Engine) op(needConfig bool, fn func() (string, error)) (string, error) {
	if e.closed.Load() {
		return "", ErrClosed
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() {
		return "", ErrClosed
	}
	e.invalidateProcs()
	if needConfig && e.needsSetup {
		return "", ErrNeedsSetup
	}
	msg, err := fn()
	e.invalidateProcs()
	e.publishLocked()
	return msg, err
}

// NeedsSetup reports whether there is no configuration file yet.
func (e *Engine) NeedsSetup() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.needsSetup
}

// Setup writes the configuration (the bash onboarding): the runner name
// prefix ("" = the host name) and the most runners across all targets.
func (e *Engine) Setup(prefix string, maxRunners int) (string, error) {
	return e.op(false, func() (string, error) {
		if prefix == "" {
			prefix = DefaultPrefix()
		}
		if !validPrefix(prefix) {
			return "", errors.New("invalid prefix: use only letters, numbers, underscores, hyphens - at most 60 characters (GitHub caps runner names at 64)")
		}
		if maxRunners < 1 {
			return "", errors.New("invalid maximum: must be 1 or greater")
		}
		e.cfg.vals["RUNNER_NAME_PREFIX"] = prefix
		e.cfg.vals["MAX_RUNNERS"] = strconv.Itoa(maxRunners)
		e.cfg.sanitize()
		if err := e.cfg.save(e.confPath); err != nil {
			return "", err
		}
		e.loadConfig()
		e.dlog(fmt.Sprintf("configuration saved (prefix %s, max %d)", prefix, maxRunners))
		e.nudge()
		return "Configuration saved to " + filepath.Base(e.confPath), nil
	})
}

// normalizeCounts expands target entries to URLs. Of entries naming the
// same target, the one sorting last wins.
func normalizeCounts(counts map[string]int) (map[string]int, error) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]int{}
	for _, k := range keys {
		n := counts[k]
		u := targetEntryToURL(k)
		if !validTargetURL(u) {
			return nil, fmt.Errorf("invalid target: %s (expected owner/repo, org, or URL)", k)
		}
		if n < 0 {
			return nil, fmt.Errorf("invalid count for %s: %d", k, n)
		}
		for have := range out {
			if sameTarget(have, u) {
				delete(out, have)
			}
		}
		out[u] = n
	}
	return out, nil
}

// Scale sets runner counts per target (owner/repo, org or URL -> count).
// Growing checks access, downloads the runner tarball when there is none,
// and registers new runners; shrinking removes idle runners and drains
// busy ones. The fleet total may not exceed MAX_RUNNERS.
func (e *Engine) Scale(counts map[string]int) (string, error) {
	return e.op(true, func() (string, error) {
		want, err := normalizeCounts(counts)
		if err != nil {
			return "", err
		}
		maxr := e.cfg.int("MAX_RUNNERS")
		if total := e.scaleTotalAfter(want); total > maxr {
			return "", fmt.Errorf("that would make %d runners across all projects, over MAX_RUNNERS (%d)", total, maxr)
		}
		urls := make([]string, 0, len(want))
		up := false
		for u, n := range want {
			urls = append(urls, u)
			if n > e.countRunnersForTarget(u) {
				up = true
			}
		}
		sort.Strings(urls)
		var lines []string
		if up {
			if _, err := e.ensureTarball(); err != nil {
				return "", err
			}
			if !e.ghAuthProbe() {
				g := &e.ghs
				g.mu.Lock()
				cls, msg := g.probeState, g.probeMsg
				g.mu.Unlock()
				return "", errors.New(ghErrorNote(cls, msg))
			}
		}
		var failed error
		for _, u := range urls {
			n := want[u]
			if n > e.countRunnersForTarget(u) {
				if r := e.targetAccessible(u); !r.ok() {
					msg := fmt.Sprintf("can't use %s: %s", targetLabel(u), ghErrorNote(r.Class, r.Msg))
					lines = append(lines, msg)
					failed = errors.New(msg)
					continue
				}
			}
			e.targets.add(u)
			if e.countRunnersForTarget(u) == n {
				delete(e.want, targetKey(u))
				lines = append(lines, fmt.Sprintf("%s: already at %d runner(s)", targetLabel(u), n))
				continue
			}
			l, err := e.scaleTarget(u, n)
			lines = append(lines, l...)
			if err != nil {
				failed = err
			}
		}
		e.confSig = e.fileSigs()
		return strings.Join(lines, "\n"), failed
	})
}

func (e *Engine) checkRunner(id int) error {
	if id <= 0 || !e.runnerExists(id) {
		return fmt.Errorf("no such runner: runner-%d", id)
	}
	return nil
}

// Drain removes a runner once its current job finishes (right away when
// idle).
func (e *Engine) Drain(id int) (string, error) {
	return e.op(false, func() (string, error) {
		if err := e.checkRunner(id); err != nil {
			return "", err
		}
		e.rt(id).draining = true
		e.dlog(fmt.Sprintf("runner-%d: draining - removed when its job finishes", id))
		if e.isBusy(id) {
			return fmt.Sprintf("runner-%d is mid-job: draining, it will be removed when the job finishes", id), nil
		}
		msg := fmt.Sprintf("runner-%d was not running a job: removed", id)
		if w := e.removeRunner(id); w != "" {
			msg += " (" + w + ")"
		}
		return msg, nil
	})
}

// Stop stops a runner; it stays down until started.
func (e *Engine) Stop(id int) (string, error) {
	return e.op(false, func() (string, error) {
		if err := e.checkRunner(id); err != nil {
			return "", err
		}
		e.stopRunners([]int{id})
		return fmt.Sprintf("runner-%d stopped (it stays down until started)", id), nil
	})
}

// Start starts a runner with a clean slate (clears quarantine and fails).
func (e *Engine) Start(id int) (string, error) {
	return e.op(true, func() (string, error) {
		if err := e.checkRunner(id); err != nil {
			return "", err
		}
		e.clearFailureState(id)
		if err := e.startRunner(id); err != nil {
			return "", fmt.Errorf("runner-%d failed to start: %v", id, err)
		}
		e.invalidateProcs()
		return fmt.Sprintf("runner-%d running (pid %d)", id, e.readPID(id)), nil
	})
}

// Remove stops, unregisters and deletes a runner, killing a running job.
func (e *Engine) Remove(id int) (string, error) {
	return e.op(false, func() (string, error) {
		if err := e.checkRunner(id); err != nil {
			return "", err
		}
		msg := fmt.Sprintf("runner-%d removed", id)
		if e.isBusy(id) {
			msg += " (it was mid-job; drain lets a job finish)"
		}
		if w := e.removeRunner(id); w != "" {
			msg += " - " + w
		}
		return msg, nil
	})
}

// StopAll stops every runner (they stay down until started).
func (e *Engine) StopAll() (string, error) {
	return e.op(false, func() (string, error) {
		ids := e.runnerIDs()
		if len(ids) == 0 {
			return "no runners", nil
		}
		e.stopRunners(ids)
		var lines []string
		for _, id := range ids {
			lines = append(lines, fmt.Sprintf("runner-%d stopped", id))
		}
		return strings.Join(lines, "\n"), nil
	})
}

// StartAll starts every runner with a clean slate.
func (e *Engine) StartAll() (string, error) {
	return e.op(true, func() (string, error) {
		ids := e.runnerIDs()
		if len(ids) == 0 {
			return "no runners", nil
		}
		var lines []string
		var failed error
		for _, id := range ids {
			e.clearFailureState(id)
			if err := e.startRunner(id); err != nil {
				failed = fmt.Errorf("runner-%d failed to start: %v", id, err)
				lines = append(lines, failed.Error())
			} else {
				lines = append(lines, fmt.Sprintf("runner-%d running", id))
			}
		}
		return strings.Join(lines, "\n"), failed
	})
}

// AddTarget lists a repo/org (owner/repo, org or URL) after checking the
// gh token can see it.
func (e *Engine) AddTarget(entry string) (string, error) {
	return e.op(false, func() (string, error) {
		u := targetEntryToURL(entry)
		if !validTargetURL(u) {
			return "", fmt.Errorf("invalid target: %s (expected owner/repo, org, or URL)", entry)
		}
		if r := e.targetAccessible(u); !r.ok() {
			return "", fmt.Errorf("can't use %s: %s", targetLabel(u), ghErrorNote(r.Class, r.Msg))
		}
		if e.targets.contains(u) {
			return targetLabel(u) + " is already listed", nil
		}
		if err := e.targets.add(u); err != nil {
			return "", err
		}
		e.confSig = e.fileSigs()
		e.dlog("added target " + targetLabel(u))
		return "added " + targetLabel(u), nil
	})
}

// RemoveTarget unlists a target that has no runners left.
func (e *Engine) RemoveTarget(entry string) (string, error) {
	return e.op(false, func() (string, error) {
		u := targetEntryToURL(entry)
		if !validTargetURL(u) {
			return "", fmt.Errorf("invalid target: %s (expected owner/repo, org, or URL)", entry)
		}
		if n := e.countRunnersForTarget(u); n > 0 {
			return "", fmt.Errorf("%s still has %d runner(s) - scale it to 0 first", targetLabel(u), n)
		}
		if err := e.targets.remove(u); err != nil {
			return "", err
		}
		key := targetKey(u)
		delete(e.want, key)
		delete(e.queue, key)
		delete(e.idleSince, key)
		e.targetClearError(u, "")
		e.confSig = e.fileSigs()
		e.dlog("removed target " + targetLabel(u))
		return "removed " + targetLabel(u), nil
	})
}

// SetBounds sets a target's autoscale bounds; 0,0 clears them (fixed count).
func (e *Engine) SetBounds(entry string, mn, mx int) (string, error) {
	return e.op(false, func() (string, error) {
		u := targetEntryToURL(entry)
		if !validTargetURL(u) {
			return "", fmt.Errorf("invalid target: %s (expected owner/repo, org, or URL)", entry)
		}
		clear := mn == 0 && mx == 0
		if !clear {
			if mn < 0 || mx < mn {
				return "", fmt.Errorf("invalid bounds %d-%d (need 0 <= min <= max)", mn, mx)
			}
			if maxr := e.cfg.int("MAX_RUNNERS"); mx > maxr {
				return "", fmt.Errorf("max %d is over MAX_RUNNERS (%d)", mx, maxr)
			}
		}
		if err := e.targets.setBounds(u, !clear, mn, mx); err != nil {
			return "", err
		}
		e.confSig = e.fileSigs()
		if clear {
			return targetLabel(u) + ": bounds cleared (fixed count)", nil
		}
		msg := fmt.Sprintf("%s: autoscale between %d and %d", targetLabel(u), mn, mx)
		if e.cfg.vals["AUTOSCALE"] != "1" {
			msg += "\nNote: AUTOSCALE=0, so the bounds apply once it is on"
		}
		return msg, nil
	})
}

// SetConfig validates and saves one setting.
func (e *Engine) SetConfig(key, val string) (string, error) {
	return e.op(true, func() (string, error) {
		before := len(e.cfg.warnings)
		if err := e.cfg.set(key, val); err != nil {
			return "", err
		}
		e.cfg.sanitize()
		if err := e.cfg.save(e.confPath); err != nil {
			return "", err
		}
		e.confSig = e.fileSigs()
		lines := []string{key + "=" + e.cfg.vals[key]}
		for _, w := range e.cfg.warnings[before:] {
			lines = append(lines, "Note: "+w)
		}
		e.dlog("setting changed: " + lines[0])
		e.nudge()
		return strings.Join(lines, "\n"), nil
	})
}

// Poll checks every target against GitHub now (also when GH_HEALTH_TICKS=0
// turns periodic polling off) and autoscales on the result.
func (e *Engine) Poll() (string, error) {
	return e.op(true, func() (string, error) {
		e.checkGithubHealth()
		e.tickN = 0
		if st := e.ghState(); st != "ok" && st != "unknown" {
			g := &e.ghs
			g.mu.Lock()
			msg := g.errMsg
			g.mu.Unlock()
			return "", errors.New("GitHub poll: " + ghErrorNote(st, msg))
		}
		return "polled GitHub", nil
	})
}

// GHStatus re-probes gh auth and lists what GitHub has registered for
// every target, plus the local orphan list.
func (e *Engine) GHStatus() (string, error) {
	return e.op(false, func() (string, error) {
		var b strings.Builder
		ok := e.ghAuthProbe()
		g := &e.ghs
		g.mu.Lock()
		user, scopes, cls, msg := g.user, g.scopes, g.probeState, g.probeMsg
		g.mu.Unlock()
		if !ok {
			return "", errors.New("gh: " + ghErrorNote(cls, msg))
		}
		if user == "" {
			user = "?"
		}
		if scopes != "" {
			fmt.Fprintf(&b, "✓ gh: %s (%s)\n", user, strings.ReplaceAll(scopes, ",", ", "))
		} else {
			fmt.Fprintf(&b, "✓ gh: %s (no scope list - fine-grained or app token)\n", user)
		}
		for _, t := range e.knownTargets() {
			if te, ok := e.targetError(t); ok && te.class == "scope" {
				fmt.Fprintf(&b, "⚠ %s: %s\n", targetLabel(t), ghErrorNote(te.class, te.msg))
			}
		}
		body, bodyOK := e.githubStatusBody()
		b.WriteString("\n" + body)
		out := strings.TrimRight(b.String(), "\n")
		if !bodyOK {
			return out, errors.New("could not fetch every target from GitHub")
		}
		return out, nil
	})
}

// Download fetches the latest runner tarball for this platform.
func (e *Engine) Download() (string, error) {
	return e.op(false, func() (string, error) { return e.downloadRunnerTarball() })
}

// ---- shutdown ------------------------------------------------------------------

// quitPass (DrainThenStop) stops every runner that isn't running a job,
// without marking it stopped, and finishes pending drain removals. It
// reports how many runners still have processes.
func (e *Engine) quitPass() int {
	e.invalidateProcs()
	left := 0
	for _, id := range e.runnerIDs() {
		if len(e.runnerProcs(id)) == 0 && !e.isRunning(id) {
			continue
		}
		busy := e.isBusy(id)
		switch {
		case e.rt(id).draining && !busy:
			e.dlog(fmt.Sprintf("runner-%d: drained (no job running) - removing", id))
			e.removeRunner(id)
		case !busy:
			e.stopProcs([]int{id})
			e.dlog(fmt.Sprintf("runner-%d: stopped (quitting)", id))
		default:
			left++
		}
	}
	e.invalidateProcs()
	return left
}

// Shutdown stops the supervisor and releases the lock. StopNow stops every
// runner's process tree; DrainThenStop lets current jobs finish first (a
// cancelled ctx escalates to StopNow). Runners are never unregistered or
// deleted, and are not marked stopped, so the next Open restarts them.
// Idempotent.
func (e *Engine) Shutdown(ctx context.Context, mode ShutdownMode) error {
	e.shutMu.Lock()
	defer e.shutMu.Unlock()
	if e.closed.Load() {
		return nil
	}
	var ctxErr error
	if mode == DrainThenStop {
		e.mu.Lock()
		e.quitting = true
		e.dlog("quitting - letting running jobs finish")
		e.mu.Unlock()
		for {
			e.mu.Lock()
			left := e.quitPass()
			e.publishLocked()
			e.mu.Unlock()
			if left == 0 {
				break
			}
			select {
			case <-ctx.Done():
				ctxErr = ctx.Err()
				mode = StopNow
			case <-time.After(time.Second):
			}
			if mode == StopNow {
				break
			}
		}
	}
	if mode == StopNow {
		e.opCancel() // abort gh/config.sh calls of a running operation
		e.mu.Lock()
		e.quitting = true
		e.invalidateProcs()
		var ids []int
		for _, id := range e.runnerIDs() {
			if len(e.runnerProcs(id)) > 0 {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			e.stopProcs(ids)
		}
		e.dlog(fmt.Sprintf("engine stopped (%d runner(s) stopped)", len(ids)))
		e.publishLocked()
		e.mu.Unlock()
	} else {
		e.dlog("engine stopped (all jobs finished)")
	}
	e.closed.Store(true)
	close(e.loopStop)
	<-e.loopDone
	e.opCancel()
	e.releaseLock()
	e.snapMu.Lock()
	e.snapClosed = true
	close(e.snapCh)
	e.snapMu.Unlock()
	return ctxErr
}
