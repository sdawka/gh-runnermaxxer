//go:build integration

// Package integration exercises internal/cli and internal/state against the
// real runnermaxxer.sh (not a fake Runner), so a schema or argv drift
// between the bash and Go sides is caught here rather than only in
// production. It is gated behind the "integration" build tag (`go test
// -tags integration ./...`) since it shells out to a real script and is
// slower than the rest of the suite; commit 15's CI matrix runs it as its
// own step.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/cli"
	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

// fakeGHScript is a minimal stand-in for `gh` [bash-3's TUI verbs need no
// real GitHub access for the flows this test drives]: it answers exactly
// the calls target_accessible/gh_auth_probe make for a plain --add-target
// and --status --json round trip, and fails loudly (to stderr, non-zero
// exit) on anything else so an unexpected call shows up as a test failure
// instead of a silent hang or a confusing script error.
const fakeGHScript = `#!/bin/sh
case "$1 $2 $3" in
  "api -i user")
    printf 'HTTP/2.0 200 OK\r\nX-OAuth-Scopes: repo, admin:org\r\n\r\n{"login": "test-user"}\n'
    exit 0
    ;;
  "auth status")
    echo "Logged in to github.com account test-user"
    exit 0
    ;;
esac
case "$1 $2" in
  "api repos/"*|"api orgs/"*)
    echo '{}'
    exit 0
    ;;
esac
echo "fake-gh: unsupported call: $*" >&2
exit 1
`

// repoScriptPath locates the real runnermaxxer.sh at the repo root (two
// directories up from this test file: tui/integration/ -> tui/ -> repo
// root), rather than assuming a particular working directory.
func repoScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "runnermaxxer.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("runnermaxxer.sh not found at %s: %v", p, err)
	}
	return p
}

// minimalConfig is a valid .runnermaxxer.conf covering every CONFIG_KEYS
// entry (runnermaxxer.sh:41), so load_config has nothing to complain about.
// AUTOSCALE is on ("1") because state_json's per-target "autoscale" field is
// true only when the global switch is on AND the target has bounds set
// (runnermaxxer.sh:4194) - TestSetBoundsReflectedInStatusJSON needs both.
const minimalConfig = `RUNNER_NAME_PREFIX="ci-test"
MAX_RUNNERS="20"
REFRESH_INTERVAL="5"
MAX_RESTART_ATTEMPTS="5"
MAX_LOG_SIZE_MB="10"
GH_HEALTH_TICKS="12"
SHARED_TOOL_CACHE="0"
EPHEMERAL_RUNNERS="0"
AUTOSCALE="1"
AUTOSCALE_IDLE_MINUTES="30"
`

// newTestClient copies the real runnermaxxer.sh into a fresh temp dir
// (CONFIG_FILE/TARGETS_FILE are always resolved relative to $0's own
// directory - runnermaxxer.sh:20-22 - with no env var override, so running
// it in place would read/write the actual repo's config), drops in a
// minimal config next to it, and points RUNNER_BASE_DIR/RUNNERMAXXER_GH at
// more temp paths so nothing this test does touches the real repo, a real
// gh install, or a real runner.
func newTestClient(t *testing.T) (cli.Client, cli.Paths) {
	t.Helper()
	dir := t.TempDir()

	src, err := os.ReadFile(repoScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "runnermaxxer.sh")
	if err := os.WriteFile(script, src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".runnermaxxer.conf"), []byte(minimalConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	ghPath := filepath.Join(dir, "fake-gh.sh")
	if err := os.WriteFile(ghPath, []byte(fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}

	runnerBase := filepath.Join(dir, "runners")
	t.Setenv("RUNNER_BASE_DIR", runnerBase)
	t.Setenv("RUNNERMAXXER_GH", ghPath)

	client := cli.NewClient(cli.NewExec(script))
	paths := cli.NewPaths(script)
	return client, paths
}

func TestStatusJSONMatchesGoSchemaWithNoTargets(t *testing.T) {
	client, _ := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := client.StatusJSON(ctx)
	if res.Err != nil {
		t.Fatalf("StatusJSON: launch error: %v", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("StatusJSON: exit %d, stderr: %s", res.ExitCode, res.Stderr)
	}

	snap, err := state.Parse([]byte(res.Stdout), state.SourceCLI)
	if err != nil {
		t.Fatalf("Parse real --status --json output: %v\nstdout:\n%s", err, res.Stdout)
	}
	if snap.Schema != 2 {
		t.Errorf("Schema = %d, want 2", snap.Schema)
	}
	if snap.DaemonPID != 0 {
		t.Errorf("DaemonPID = %d, want 0 (no daemon started)", snap.DaemonPID)
	}
	if len(snap.Targets) != 0 {
		t.Errorf("Targets = %v, want none for a fresh install", snap.Targets)
	}
}

func TestAddTargetThenStatusJSONRoundTrips(t *testing.T) {
	client, _ := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	addRes := client.AddTarget(ctx, "octocat/hello-world")
	if addRes.Err != nil || addRes.ExitCode != 0 {
		t.Fatalf("AddTarget: exit %d, err %v, stdout %q, stderr %q",
			addRes.ExitCode, addRes.Err, addRes.Stdout, addRes.Stderr)
	}

	res := client.StatusJSON(ctx)
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("StatusJSON after AddTarget: exit %d, err %v, stderr %q", res.ExitCode, res.Err, res.Stderr)
	}
	snap, err := state.Parse([]byte(res.Stdout), state.SourceCLI)
	if err != nil {
		t.Fatalf("Parse: %v\nstdout:\n%s", err, res.Stdout)
	}
	if len(snap.Targets) != 1 {
		t.Fatalf("Targets = %v, want exactly one after --add-target", snap.Targets)
	}
	target := snap.Targets[0]
	if target.Label != "octocat/hello-world" {
		t.Errorf("Label = %q, want octocat/hello-world", target.Label)
	}
	if !target.Listed {
		t.Error("Listed = false, want true right after --add-target")
	}
	if target.Want != 0 || target.Have != 0 {
		t.Errorf("Want/Have = %d/%d, want 0/0 before any --scale", target.Want, target.Have)
	}

	// --remove-target should be able to undo it, since Have is still 0.
	rmRes := client.RemoveTarget(ctx, target.URL)
	if rmRes.Err != nil || rmRes.ExitCode != 0 {
		t.Fatalf("RemoveTarget: exit %d, err %v, stderr %q", rmRes.ExitCode, rmRes.Err, rmRes.Stderr)
	}
	res2 := client.StatusJSON(ctx)
	snap2, err := state.Parse([]byte(res2.Stdout), state.SourceCLI)
	if err != nil {
		t.Fatalf("Parse after RemoveTarget: %v", err)
	}
	if len(snap2.Targets) != 0 {
		t.Errorf("Targets = %v, want none after --remove-target", snap2.Targets)
	}
}

func TestSetBoundsReflectedInStatusJSON(t *testing.T) {
	client, _ := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if res := client.AddTarget(ctx, "octocat/hello-world"); res.ExitCode != 0 {
		t.Fatalf("AddTarget: exit %d, stderr %q", res.ExitCode, res.Stderr)
	}
	snap, _ := state.Parse([]byte(client.StatusJSON(ctx).Stdout), state.SourceCLI)
	url := snap.Targets[0].URL

	boundsRes := client.SetBounds(ctx, url, 1, 3)
	if boundsRes.Err != nil || boundsRes.ExitCode != 0 {
		t.Fatalf("SetBounds: exit %d, err %v, stdout %q, stderr %q",
			boundsRes.ExitCode, boundsRes.Err, boundsRes.Stdout, boundsRes.Stderr)
	}

	snap2, err := state.Parse([]byte(client.StatusJSON(ctx).Stdout), state.SourceCLI)
	if err != nil {
		t.Fatalf("Parse after SetBounds: %v", err)
	}
	target := snap2.Targets[0]
	if !target.Autoscale {
		t.Error("Autoscale = false, want true after --set-bounds")
	}
	if target.Min != 1 || target.Max != 3 {
		t.Errorf("Min/Max = %d/%d, want 1/3", target.Min, target.Max)
	}
}
