package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// proc is one row of the process table.
type proc struct {
	pid, ppid int
	cmd       string
}

// psList reads the whole process table with one ps call.
func psList() ([]proc, error) {
	out, err := exec.Command("ps", "-A", "-w", "-w", "-o", "pid=", "-o", "ppid=", "-o", "command=").Output()
	if err != nil {
		return nil, err
	}
	var ps []proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		// keep the command line's own spacing after the two numeric columns
		rest := strings.TrimLeft(line, " ")
		rest = strings.TrimLeft(rest[len(f[0]):], " ")
		rest = strings.TrimLeft(rest[len(f[1]):], " ")
		ps = append(ps, proc{pid: pid, ppid: ppid, cmd: rest})
	}
	return ps, nil
}

// procs is the process table, cached until invalidateProcs.
func (e *Engine) procs() []proc {
	if !e.procCached {
		p, err := e.listProcs()
		if err != nil {
			p = nil
		}
		e.procCache, e.procCached = p, true
	}
	return e.procCache
}

func (e *Engine) invalidateProcs() { e.procCache, e.procCached = nil, false }

func (e *Engine) procPattern(id int) string {
	return e.runnerDir(id) + "/"
}

// matchProcs: every process of runner N has <base>/runner-N/ on its command
// line (run.sh, run-helper.sh, Runner.Listener, Runner.Worker). Our own
// background du is not one of them.
func matchProcs(ps []proc, pattern string) []proc {
	var out []proc
	for _, p := range ps {
		if strings.Contains(p.cmd, pattern) && !strings.HasPrefix(p.cmd, "du ") {
			out = append(out, p)
		}
	}
	return out
}

func (e *Engine) runnerProcs(id int) []proc { return matchProcs(e.procs(), e.procPattern(id)) }

// isRunning: the recorded pid is alive and is this runner (guards against
// pid reuse after a reboot).
func (e *Engine) isRunning(id int) bool {
	pid := e.readPID(id)
	if pid <= 0 {
		return false
	}
	pat := "runner-" + strconv.Itoa(id) + "/"
	for _, p := range e.procs() {
		if p.pid == pid {
			return strings.Contains(p.cmd, pat)
		}
	}
	return false
}

// mainPID is the root of a runner's live process tree (a process whose
// parent is not a runner process), preferring run.sh; 0 when none.
func mainPID(ps []proc) int {
	if len(ps) == 0 {
		return 0
	}
	in := map[int]bool{}
	for _, p := range ps {
		in[p.pid] = true
	}
	best := 0
	for _, p := range ps {
		if in[p.ppid] {
			continue
		}
		if strings.Contains(p.cmd, "run.sh") {
			return p.pid
		}
		if best == 0 {
			best = p.pid
		}
	}
	if best == 0 {
		best = ps[0].pid
	}
	return best
}

// ---- setup -----------------------------------------------------------------

const configTimeout = 5 * time.Minute

func (e *Engine) runIn(dir string, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(e.opCtx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

func lastLine(s string) string {
	lines := nonEmpty(strings.Split(strings.TrimSpace(s), "\n"))
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

func (e *Engine) labelArgs() []string {
	if l := e.labels(); l != "" {
		return []string{"--labels", strings.ReplaceAll(l, " ", ",")}
	}
	return nil
}

// setupRunner extracts the tarball into runner-N and registers it with u.
func (e *Engine) setupRunner(id int, u string) error {
	dir := e.runnerDir(id)
	if e.isConfigured(id) {
		return nil
	}
	if e.runnerExists(id) {
		e.dlog(fmt.Sprintf("runner-%d directory exists but is not configured - rebuilding", id))
		os.RemoveAll(dir)
	}
	if u == "" {
		return fmt.Errorf("no target given for runner-%d", id)
	}
	for _, ext := range []string{"target", "ephemeral", "name", "version"} {
		os.Remove(e.pidPath(id, ext))
	}
	if mb, _ := diskFree(e.base); mb < 500 {
		return fmt.Errorf("refusing to set up runner-%d: only %dMB disk free", id, mb)
	}
	tar := e.detectRunnerTarball()
	if tar == "" {
		return fmt.Errorf("no runner tarball for %s-%s - download it first", e.plat.runnerOS, e.plat.runnerArch)
	}
	// Token first, so a permission problem costs no tar run
	tok, r := e.registrationToken(u)
	if !r.ok() {
		return fmt.Errorf("runner-%d: can't get a registration token for %s: %s", id, targetLabel(u), e.targetErrorNote(u))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("runner-%d: %v", id, err)
	}
	if out, err := e.runIn(dir, configTimeout, "tar", "-xzf", tar, "-C", dir); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("runner-%d: failed to extract the runner tarball: %s", id, lastLine(out))
	}

	prefix := e.cfg.vals["RUNNER_NAME_PREFIX"]
	name := prefix + "-" + strconv.Itoa(id)
	args := []string{"--unattended", "--name", name, "--url", u, "--token", tok, "--replace"}
	args = append(args, e.labelArgs()...)
	ephemeral := e.cfg.vals["EPHEMERAL_RUNNERS"] == "1"
	if ephemeral {
		args = append(args, "--ephemeral")
	}
	if out, err := e.runIn(dir, configTimeout, filepath.Join(dir, "config.sh"), args...); err != nil {
		msg := fmt.Sprintf("runner-%d: failed to configure: %s", id, lastLine(out))
		low := strings.ToLower(out)
		if strings.Contains(low, "libicu") || strings.Contains(low, "libssl") || strings.Contains(low, "dependencies") {
			msg += fmt.Sprintf(" (install runner dependencies: sudo %s/bin/installdependencies.sh)", dir)
		}
		// A half-done registration: unregister so no ghost runner is left
		// on GitHub, or record it for manual cleanup
		if e.isConfigured(id) {
			removed := false
			if rt, r := e.removeToken(u); r.ok() {
				if _, err := e.runIn(dir, configTimeout, filepath.Join(dir, "config.sh"), "remove", "--token", rt); err == nil {
					removed = true
				}
			}
			if !removed {
				e.appendOrphan(name)
			}
		}
		os.RemoveAll(dir)
		return errors.New(msg)
	}

	if v := tarballVersion(tar); v != "" {
		writeTrim(e.pidPath(id, "version"), v)
	}
	// Kept outside .runner, which an ephemeral runner deletes after its job
	writeTrim(e.pidPath(id, "target"), u)
	writeTrim(e.pidPath(id, "name"), name)
	if ephemeral {
		writeTrim(e.pidPath(id, "ephemeral"), "")
	}
	return nil
}

// reconfigureRunner re-registers an ephemeral runner in place after its
// one job (the runner deleted .runner and GitHub dropped the registration).
func (e *Engine) reconfigureRunner(id int) error {
	dir := e.runnerDir(id)
	if e.isConfigured(id) {
		return nil
	}
	u := readTrim(e.pidPath(id, "target"))
	if u == "" {
		e.setLastErr(id, "not configured (no saved target)")
		return errors.New(e.rt(id).lastErr)
	}
	cfgSh := filepath.Join(dir, "config.sh")
	if fi, err := os.Stat(cfgSh); err != nil || fi.Mode()&0o111 == 0 {
		e.setLastErr(id, "config.sh not found/executable")
		return errors.New(e.rt(id).lastErr)
	}
	// Marker line: lets ephemeralJobFinished tell a finished job from a
	// failed re-registration (which still counts as a crash)
	appendLog(e.logPath(id), fmt.Sprintf("[runnermaxxer] re-registering ephemeral runner-%d (%s)\n", id, e.now().Format("2006-01-02 15:04:05")))

	tok, r := e.registrationToken(u)
	if !r.ok() {
		e.setLastErr(id, "re-register failed: "+e.targetErrorNote(u))
		return errors.New(e.rt(id).lastErr)
	}
	name := readTrim(e.pidPath(id, "name"))
	if name == "" {
		name = e.cfg.vals["RUNNER_NAME_PREFIX"] + "-" + strconv.Itoa(id)
	}
	args := []string{"--unattended", "--name", name, "--url", u, "--token", tok, "--replace", "--ephemeral"}
	args = append(args, e.labelArgs()...)
	if out, err := e.runIn(dir, configTimeout, cfgSh, args...); err != nil {
		appendLog(e.logPath(id), out)
		e.setLastErr(id, "re-register failed (see its log)")
		return errors.New(e.rt(id).lastErr)
	}
	return nil
}

func appendLog(path, s string) {
	if s == "" {
		return
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(s)
}

// ---- start / stop ------------------------------------------------------------

// runnerEnv is the environment for a runner launch.
func (e *Engine) runnerEnv() []string {
	env := os.Environ()
	if e.cfg.vals["SHARED_TOOL_CACHE"] == "1" {
		cache := filepath.Join(e.base, ".toolcache")
		os.MkdirAll(cache, 0o755)
		// The runner resolves its tool directory from RUNNER_TOOL_CACHE,
		// then RUNNER_TOOLSDIRECTORY, then AGENT_TOOLSDIRECTORY; the
		// toolkit's tool-cache reads RUNNER_TOOL_CACHE. Set both common ones.
		env = append(env, "RUNNER_TOOL_CACHE="+cache, "AGENT_TOOLSDIRECTORY="+cache)
	}
	// Lets run-helper log "deprecated version exit code" so exitReason can
	// say so
	return append(env, "ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=1")
}

// startGrace is how long a fresh runner must survive to count as started.
var startGrace = time.Second

// startRunner starts (or adopts) a runner. Starting means we want it up:
// the stop marker goes, so the supervisor keeps it alive from now on.
func (e *Engine) startRunner(id int) error {
	e.unmarkStopped(id)
	return e.startFn(id)
}

func (e *Engine) startRunnerReal(id int) error {
	dir := e.runnerDir(id)
	if e.isRunning(id) {
		return nil
	}
	// Adopt a live orphan rather than start a duplicate that would fight
	// over the registration
	if pid := mainPID(e.runnerProcs(id)); pid > 0 {
		writeTrim(e.pidPath(id, "pid"), strconv.Itoa(pid))
		e.dlog(fmt.Sprintf("runner-%d: adopted running process %d", id, pid))
		return nil
	}
	if !e.runnerExists(id) {
		e.setLastErr(id, "runner directory missing")
		return errors.New(e.rt(id).lastErr)
	}
	if !e.isConfigured(id) && e.isEphemeral(id) {
		if err := e.reconfigureRunner(id); err != nil {
			return err
		}
	}
	if !e.isConfigured(id) {
		e.setLastErr(id, "not configured (incomplete setup)")
		return errors.New(e.rt(id).lastErr)
	}
	runSh := filepath.Join(dir, "run.sh")
	if fi, err := os.Stat(runSh); err != nil || fi.Mode()&0o111 == 0 {
		e.setLastErr(id, "run.sh not found/executable")
		return errors.New(e.rt(id).lastErr)
	}

	e.rotateLog(id)
	e.rt(id).lastStart = e.now().Unix()
	os.MkdirAll(e.logDir, 0o755)
	logf, err := os.OpenFile(e.logPath(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		e.setLastErr(id, "cannot open log: "+err.Error())
		return errors.New(e.rt(id).lastErr)
	}
	// The absolute run.sh path on the command line is what identifies the
	// process family later; its own session detaches it from the TUI.
	cmd := exec.Command(runSh)
	cmd.Dir = dir
	cmd.Env = e.runnerEnv()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	logf.Close()
	e.invalidateProcs()
	if err != nil {
		e.setLastErr(id, "failed to launch: "+err.Error())
		return errors.New(e.rt(id).lastErr)
	}
	pid := cmd.Process.Pid
	writeTrim(e.pidPath(id, "pid"), strconv.Itoa(pid))
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	select {
	case <-exited:
		e.setLastErr(id, "process exited immediately (see its log)")
		os.Remove(e.pidPath(id, "pid"))
		return errors.New(e.rt(id).lastErr)
	case <-time.After(startGrace):
	}
	e.rt(id).lastErr = ""
	e.dlog(fmt.Sprintf("runner-%d: started (pid %d)", id, pid))
	return nil
}

// Stop timeouts: SIGINT lets the listener end its session cleanly.
var (
	stopIntWait  = 10 * time.Second
	stopTermWait = 5 * time.Second
)

// stopProcs kills every process of the given runners, all at once: INT,
// then TERM, then KILL. Killing only run.sh would orphan Runner.Listener,
// which stays attached to GitHub and keeps taking jobs.
func (e *Engine) stopProcs(ids []int) { e.stopProcsFn(ids) }

func (e *Engine) stopProcsReal(ids []int) {
	defer e.invalidateProcs()
	remaining := func() []proc {
		ps, _ := e.listProcs()
		var out []proc
		for _, id := range ids {
			out = append(out, matchProcs(ps, e.procPattern(id))...)
		}
		return out
	}
	signal := func(ps []proc, sig syscall.Signal) {
		for _, p := range ps {
			syscall.Kill(p.pid, sig)
		}
	}
	wait := func(d time.Duration) []proc {
		deadline := time.Now().Add(d)
		for {
			left := remaining()
			if len(left) == 0 || time.Now().After(deadline) {
				return left
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if left := remaining(); len(left) > 0 {
		signal(left, syscall.SIGINT)
		if left = wait(stopIntWait); len(left) > 0 {
			signal(left, syscall.SIGTERM)
			if left = wait(stopTermWait); len(left) > 0 {
				signal(left, syscall.SIGKILL)
				wait(time.Second)
			}
		}
	}
	for _, id := range ids {
		os.Remove(e.pidPath(id, "pid"))
	}
}

// stopRunners stops on purpose: the supervisor leaves them down.
func (e *Engine) stopRunners(ids []int) {
	for _, id := range ids {
		e.markStopped(id)
	}
	e.stopProcs(ids)
	for _, id := range ids {
		e.dlog(fmt.Sprintf("runner-%d: stopped", id))
	}
}

func (e *Engine) removeRunner(id int) string { return e.removeFn(id) }

// removeRunnerReal stops, unregisters and deletes a runner. Returns a
// warning when the GitHub registration could not be removed.
func (e *Engine) removeRunnerReal(id int) string {
	dir := e.runnerDir(id)
	e.stopRunners([]int{id})
	warning := ""
	if e.runnerExists(id) {
		if e.isConfigured(id) {
			regName, regURL := e.registeredName(id), e.registeredURL(id)
			unregistered := false
			if tok, r := e.removeToken(regURL); r.ok() {
				if _, err := e.runIn(dir, configTimeout, filepath.Join(dir, "config.sh"), "remove", "--token", tok); err == nil {
					unregistered = true
				}
			} else if r.Class == "notfound" {
				// The repo/org itself is gone, and its registrations with it
				e.dlog(fmt.Sprintf("runner-%d: %s returns 404 - treating %s as unregistered", id, targetLabel(regURL), regName))
				unregistered = true
			}
			// GitHub deletes an ephemeral runner's registration after its
			// job; only record an orphan if it is still listed
			if !unregistered && e.isEphemeral(id) && e.runnerGoneFromGithub(regURL, regName) {
				unregistered = true
			}
			if !unregistered {
				e.appendOrphan(regName)
				warning = fmt.Sprintf("failed to unregister %s from GitHub - recorded in %s; remove it in GitHub Settings → Actions → Runners", regName, filepath.Base(e.orphansPath()))
				e.dlog("runner-" + strconv.Itoa(id) + ": " + warning)
			}
		}
		os.RemoveAll(dir)
	}
	os.Remove(e.logPath(id))
	os.Remove(e.logPath(id) + ".1")
	e.clearRunnerState(id)
	e.dlog(fmt.Sprintf("runner-%d: removed", id))
	return warning
}
