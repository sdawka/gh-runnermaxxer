//go:build integration

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// fakeGHScript stands in for the gh CLI: tokens for any token request, a
// logged-in user with full scopes, no runners listed, and {} for any other
// API call (so every repo/org is accessible).
const fakeGHScript = `#!/bin/sh
echo "$*" >> "$(dirname "$0")/gh.calls"
case "$*" in
    *registration-token*|*remove-token*) echo tok ;;
    "api -i user") printf 'HTTP/2.0 200 OK\nX-Oauth-Scopes: repo, admin:org\n\n{"login":"tester"}\n' ;;
    *rate_limit*) printf '5000\t0\n' ;;
    *--paginate*) ;;
    *) echo '{}' ;;
esac
`

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func snapRunner(e *Engine, id int) (state.Runner, bool) {
	for _, r := range e.Snapshot().Runners {
		if r.ID == id {
			return r, true
		}
	}
	return state.Runner{}, false
}

func alive(r state.Runner) bool {
	return r.PID > 0 && (r.State == state.RunnerIdle || r.State == state.RunnerRunning || r.State == state.RunnerBusy)
}

// runnerProcCount counts live processes running from under base.
func runnerProcCount(t *testing.T, base string) int {
	t.Helper()
	ps, err := psList()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range ps {
		if strings.Contains(p.cmd, base+"/runner-") && !strings.HasPrefix(p.cmd, "du ") {
			n++
		}
	}
	return n
}

func TestIntegration(t *testing.T) {
	oldGrace, oldBackoff := startGrace, defaultBackoffBase
	startGrace, defaultBackoffBase = 300*time.Millisecond, 100*time.Millisecond
	defer func() { startGrace, defaultBackoffBase = oldGrace, oldBackoff }()

	dir := t.TempDir()
	gh := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(gh, []byte(fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "runners")
	opt := Options{Dir: dir, GH: gh}
	ctx := context.Background()

	e, err := Open(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e != nil {
			e.Shutdown(ctx, StopNow)
		}
	}()

	// setup
	if !e.NeedsSetup() {
		t.Fatal("fresh dir does not need setup")
	}
	if _, err := e.Scale(map[string]int{"o/r": 1}); err != ErrNeedsSetup {
		t.Fatalf("Scale before setup: %v", err)
	}
	if _, err := e.Setup("itest", 4); err != nil {
		t.Fatal(err)
	}
	if e.NeedsSetup() {
		t.Fatal("still needs setup")
	}
	for k, v := range map[string]string{"REFRESH_INTERVAL": "1", "MAX_RESTART_ATTEMPTS": "2", "GH_HEALTH_TICKS": "0"} {
		if _, err := e.SetConfig(k, v); err != nil {
			t.Fatalf("SetConfig %s: %v", k, err)
		}
	}
	makeFakeTarball(t, e, "2.334.0")

	// add target
	if _, err := e.AddTarget("o/r"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "target listed", func() bool {
		ts := e.Snapshot().Targets
		return len(ts) == 1 && ts[0].Listed
	})

	// scale to 2
	if _, err := e.Scale(map[string]int{"o/r": 2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "2 runners alive", func() bool {
		r1, ok1 := snapRunner(e, 1)
		r2, ok2 := snapRunner(e, 2)
		return ok1 && ok2 && alive(r1) && alive(r2)
	})
	if r, _ := snapRunner(e, 1); r.Name != "itest-1" || r.Version != "2.334.0" {
		t.Errorf("runner-1: %+v", r)
	}

	// kill one: restarted
	r1, _ := snapRunner(e, 1)
	syscall.Kill(-r1.PID, syscall.SIGKILL)
	waitFor(t, "runner-1 restarted", func() bool {
		r, _ := snapRunner(e, 1)
		return alive(r) && r.PID != r1.PID
	})

	// crash loop: quarantined, Start retries with a clean slate
	os.WriteFile(filepath.Join(base, "crash-all"), nil, 0o644)
	r2, _ := snapRunner(e, 2)
	syscall.Kill(-r2.PID, syscall.SIGKILL)
	waitFor(t, "runner-2 quarantined", func() bool {
		r, _ := snapRunner(e, 2)
		return r.State == state.RunnerQuarantined
	})
	if r, _ := snapRunner(e, 2); !strings.Contains(r.LastErr, "quarantined") {
		t.Errorf("lasterr: %q", r.LastErr)
	}
	if _, err := e.Start(2); err == nil {
		t.Error("Start succeeded while run.sh crashes")
	}
	os.Remove(filepath.Join(base, "crash-all"))
	if _, err := e.Start(2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "runner-2 back", func() bool {
		r, _ := snapRunner(e, 2)
		return alive(r) && !r.Quarantined && r.Fails == 0
	})

	// drain (idle -> removed at once) and remove
	if _, err := e.Drain(1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Remove(2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "runners gone", func() bool { return len(e.Snapshot().Runners) == 0 })
	if n := runnerProcCount(t, base); n != 0 {
		t.Errorf("%d runner processes left after drain/remove", n)
	}
	calls, _ := os.ReadFile(filepath.Join(filepath.Dir(gh), "gh.calls"))
	if c := strings.Count(string(calls), "remove-token"); c != 2 {
		t.Errorf("remove tokens minted: %d, want 2", c)
	}

	// Shutdown(StopNow) leaves no runner processes and keeps registrations
	if _, err := e.Scale(map[string]int{"o/r": 2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "2 runners alive again", func() bool {
		s := e.Snapshot()
		return len(s.Runners) == 2 && alive(s.Runners[0]) && alive(s.Runners[1])
	})
	if err := e.Shutdown(ctx, StopNow); err != nil {
		t.Fatal(err)
	}
	if n := runnerProcCount(t, base); n != 0 {
		t.Errorf("%d runner processes left after Shutdown(StopNow)", n)
	}
	for _, id := range []int{1, 2} {
		if !e.isConfigured(id) {
			t.Errorf("runner-%d unregistered by Shutdown", id)
		}
	}

	// reopen: runners restart (Shutdown does not mark them stopped)
	e, err = Open(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "runners restarted on reopen", func() bool {
		s := e.Snapshot()
		return len(s.Runners) == 2 && alive(s.Runners[0]) && alive(s.Runners[1])
	})
	pids := map[int]int{}
	for _, r := range e.Snapshot().Runners {
		pids[r.ID] = r.PID
	}

	// the TUI dies without Shutdown: stop the loop and drop the lock only
	e.closed.Store(true)
	close(e.loopStop)
	<-e.loopDone
	e.releaseLock()
	os.Remove(e.pidPath(1, "pid")) // a lost pid file is found by command line

	e, err = Open(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "orphans adopted", func() bool {
		s := e.Snapshot()
		if len(s.Runners) != 2 {
			return false
		}
		for _, r := range s.Runners {
			if !alive(r) || r.PID != pids[r.ID] {
				return false
			}
		}
		return true
	})
	if !strings.Contains(strings.Join(e.Events(), "\n"), "adopted running process") {
		t.Error("no adopt event")
	}
	if err := e.Shutdown(ctx, StopNow); err != nil {
		t.Fatal(err)
	}
	e = nil
	if n := runnerProcCount(t, base); n != 0 {
		t.Errorf("%d runner processes left at the end", n)
	}
}
