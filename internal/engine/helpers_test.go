package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGH answers gh calls from a handler and records them.
type fakeGH struct {
	mu      sync.Mutex
	calls   []string
	handler func(args []string) (stdout, stderr string, rc int)
}

func (f *fakeGH) exec(_ context.Context, args []string) (string, string, int) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(args, " "))
	h := f.handler
	f.mu.Unlock()
	if h == nil {
		return "", "", 0
	}
	return h(args)
}

func (f *fakeGH) set(h func(args []string) (string, string, int)) {
	f.mu.Lock()
	f.handler = h
	f.calls = nil
	f.mu.Unlock()
}

func (f *fakeGH) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// testEnv is an engine on temp dirs with gh, the process table, start,
// stop and remove all stubbed.
type testEnv struct {
	t       *testing.T
	e       *Engine
	gh      *fakeGH
	procs   []proc
	now     time.Time
	started []int
	stopped []int
	removed []int
	env     map[string]string
}

func newTestEnv(t *testing.T, conf string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	if conf != "" {
		if err := os.WriteFile(filepath.Join(dir, ".runnermaxxer.conf"), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	te := &testEnv{t: t, gh: &fakeGH{}, now: time.Unix(1_800_000_000, 0), env: map[string]string{}}
	e, err := newEngine(Options{Dir: dir, RunnerBase: filepath.Join(dir, "runners"), Now: func() time.Time { return te.now }})
	if err != nil {
		t.Fatal(err)
	}
	te.e = e
	e.getenv = func(k string) string { return te.env[k] }
	e.loadConfig()
	e.ghExec = te.gh.exec
	e.labelsOnce.Do(func() { e.labelsStr = "macos arm64" })
	e.listProcs = func() ([]proc, error) { return append([]proc(nil), te.procs...), nil }
	e.startFn = func(id int) error {
		te.started = append(te.started, id)
		return nil
	}
	e.stopProcsFn = func(ids []int) {
		te.stopped = append(te.stopped, ids...)
		for _, id := range ids {
			te.setRunning(id, false)
		}
	}
	e.removeFn = func(id int) string {
		te.removed = append(te.removed, id)
		te.setRunning(id, false)
		os.RemoveAll(e.runnerDir(id))
		e.clearRunnerState(id)
		return ""
	}
	for _, d := range []string{e.base, e.pidDir, e.logDir} {
		os.MkdirAll(d, 0o755)
	}
	return te
}

// runner creates runner-N registered to url ("" = no .runner).
func (te *testEnv) runner(id int, url string) {
	te.t.Helper()
	d := te.e.runnerDir(id)
	os.MkdirAll(d, 0o755)
	if url != "" {
		body := "{\n  \"agentName\": \"runner-" + strconv.Itoa(id) + "\",\n  \"gitHubUrl\": \"" + url + "\"\n}\n"
		os.WriteFile(filepath.Join(d, ".runner"), []byte(body), 0o644)
	}
}

// setRunning adds/removes a live process for runner-N.
func (te *testEnv) setRunning(id int, on bool) {
	pid := 10000 + id
	var keep []proc
	for _, p := range te.procs {
		if p.pid != pid {
			keep = append(keep, p)
		}
	}
	te.procs = keep
	if on {
		te.procs = append(te.procs, proc{pid: pid, ppid: 1, cmd: "/bin/bash " + te.e.runnerDir(id) + "/run.sh"})
		writeTrim(te.e.pidPath(id, "pid"), strconv.Itoa(pid))
	} else {
		os.Remove(te.e.pidPath(id, "pid"))
	}
	te.e.invalidateProcs()
}

func (te *testEnv) log(id int, lines ...string) {
	os.WriteFile(te.e.logPath(id), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func (te *testEnv) targets(content string) {
	os.WriteFile(te.e.targets.path, []byte(content), 0o644)
}

func eq[T comparable](t *testing.T, want, got T, name string) {
	t.Helper()
	if want != got {
		t.Errorf("%s: want %v, got %v", name, want, got)
	}
}

func contains(t *testing.T, s, sub, name string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%s: %q does not contain %q", name, s, sub)
	}
}

func joinInts(ids []int) string {
	var s []string
	for _, id := range ids {
		s = append(s, strconv.Itoa(id))
	}
	return strings.Join(s, " ")
}

func itoa(n int) string { return strconv.Itoa(n) }

// fakeConfigSh records its argv (one line per call) in <base>/config.argv
// and writes .runner like the real one; <base>/config.rc sets its exit
// status for registrations (not for "remove").
const fakeConfigSh = `#!/bin/sh
base=$(cd "$(dirname "$0")/.." && pwd)
echo "$*" >> "$base/config.argv"
if [ "$1" = remove ]; then rm -f .runner; exit 0; fi
name=""; url=""
while [ $# -gt 0 ]; do
    case "$1" in
        --name) name=$2; shift ;;
        --url)  url=$2; shift ;;
    esac
    shift
done
printf '{\n  "agentName": "%s",\n  "gitHubUrl": "%s"\n}\n' "$name" "$url" > .runner
rc=0
[ -f "$base/config.rc" ] && rc=$(cat "$base/config.rc")
exit "$rc"
`

// fakeRunSh logs like a runner and idles until signalled; it exits 1 at
// once while <dir>/crash or <base>/crash-all exists.
const fakeRunSh = `#!/bin/sh
dir=$(cd "$(dirname "$0")" && pwd)
if [ -f "$dir/crash" ] || [ -f "$dir/../crash-all" ]; then
    echo "Runner listener exit with terminated error, stop the service, no retry needed."
    exit 1
fi
trap 'echo "$(date -u "+%Y-%m-%d %H:%M:%S")Z: Exiting runner"; exit 0' INT TERM
echo "$(date -u "+%Y-%m-%d %H:%M:%S")Z: Starting Runner listener"
echo "$(date -u "+%Y-%m-%d %H:%M:%S")Z: Listening for Jobs"
while :; do sleep 1; done
`

// makeFakeTarball writes actions-runner-OS-ARCH-ver.tar.gz into the
// engine's Dir.
func makeFakeTarball(t *testing.T, e *Engine, ver string) string {
	t.Helper()
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "bin"), 0o755)
	os.WriteFile(filepath.Join(src, "config.sh"), []byte(fakeConfigSh), 0o755)
	os.WriteFile(filepath.Join(src, "run.sh"), []byte(fakeRunSh), 0o755)
	out := filepath.Join(e.dir, "actions-runner-"+e.plat.runnerOS+"-"+e.plat.runnerArch+"-"+ver+".tar.gz")
	if b, err := exec.Command("tar", "-czf", out, "-C", src, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, b)
	}
	return out
}
