package state

import (
	"errors"
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) Snapshot {
	t.Helper()
	snap, err := Load("testdata/" + name)
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	return snap
}

func TestLoadFull(t *testing.T) {
	snap := mustLoad(t, "full.json")

	if snap.Schema != 2 {
		t.Errorf("Schema = %d, want 2", snap.Schema)
	}
	if snap.Writer != "daemon" {
		t.Errorf("Writer = %q, want daemon", snap.Writer)
	}
	if snap.DaemonPID != 41213 {
		t.Errorf("DaemonPID = %d, want 41213", snap.DaemonPID)
	}
	if snap.RefreshInterval != 5 {
		t.Errorf("RefreshInterval = %d, want 5", snap.RefreshInterval)
	}
	if len(snap.GH.Scopes) != 2 || snap.GH.Scopes[0] != "repo" || snap.GH.Scopes[1] != "admin:org" {
		t.Errorf("GH.Scopes = %v, want [repo admin:org]", snap.GH.Scopes)
	}
	if !snap.Tarball.Stale {
		t.Errorf("Tarball.Stale = false, want true")
	}
	if snap.Config.MaxRunners != 20 || !snap.Config.Autoscale {
		t.Errorf("Config = %+v, unexpected", snap.Config)
	}
	if len(snap.Warnings) != 1 {
		t.Errorf("Warnings = %v, want 1 entry", snap.Warnings)
	}
	if len(snap.Targets) != 3 {
		t.Fatalf("len(Targets) = %d, want 3", len(snap.Targets))
	}
	if snap.Targets[2].Error == nil || snap.Targets[2].Error.Class != "scope" {
		t.Errorf("Targets[2].Error = %+v, want class=scope", snap.Targets[2].Error)
	}
	if snap.Targets[0].Label != "myorg/myrepo" {
		t.Errorf("Targets[0].Label = %q, want myorg/myrepo", snap.Targets[0].Label)
	}
	if snap.Targets[2].Label != "myorg (org)" {
		t.Errorf("Targets[2].Label = %q, want myorg (org)", snap.Targets[2].Label)
	}

	if len(snap.Runners) != 6 {
		t.Fatalf("len(Runners) = %d, want 6", len(snap.Runners))
	}
	if snap.Runners[1].Job != "build" || snap.Runners[1].Elapsed != 748 {
		t.Errorf("Runners[1] = %+v, unexpected", snap.Runners[1])
	}
	if !snap.Runners[4].Quarantined {
		t.Errorf("Runners[4].Quarantined = false, want true")
	}
	if snap.Runners[5].Target != "" {
		t.Errorf("Runners[5].Target = %q, want empty (unassigned)", snap.Runners[5].Target)
	}

	if snap.Source != SourceDaemonFile {
		t.Errorf("Source = %v, want SourceDaemonFile", snap.Source)
	}
}

func TestSnapshotHelpers(t *testing.T) {
	snap := mustLoad(t, "full.json")

	myrepo := "https://github.com/myorg/myrepo"
	if got := snap.RunnersFor(myrepo); len(got) != 3 {
		t.Errorf("RunnersFor(myrepo) = %d runners, want 3", len(got))
	}
	if got := snap.Unassigned(); len(got) != 1 || got[0].ID != 9 {
		t.Errorf("Unassigned() = %+v, want [runner id 9]", got)
	}
	if got := snap.TotalWant(); got != 5 { // 2 + 2 + 1
		t.Errorf("TotalWant() = %d, want 5", got)
	}
	if !snap.IsBusy(2) {
		t.Errorf("IsBusy(2) = false, want true")
	}
	if snap.IsBusy(1) {
		t.Errorf("IsBusy(1) = true, want false")
	}
	if _, ok := snap.TargetByURL(myrepo); !ok {
		t.Errorf("TargetByURL(myrepo) not found")
	}
	if _, ok := snap.RunnerByID(999); ok {
		t.Errorf("RunnerByID(999) unexpectedly found")
	}
}

func TestStale(t *testing.T) {
	snap := mustLoad(t, "stale.json")
	now := time.Unix(snap.TickTS+100, 0) // 100s later, interval=5 -> stale threshold 15s
	stale, age := snap.Stale(now)
	if !stale {
		t.Errorf("Stale() = false, want true (age %s)", age)
	}

	fresh := mustLoad(t, "full.json")
	now2 := time.Unix(fresh.TickTS+2, 0)
	stale2, _ := fresh.Stale(now2)
	if stale2 {
		t.Errorf("Stale() = true for a 2s-old tick, want false")
	}
}

func TestGHAuthState(t *testing.T) {
	snap := mustLoad(t, "gh_auth.json")
	if snap.GH.State != GHStateAuth {
		t.Errorf("GH.State = %q, want auth", snap.GH.State)
	}
	if snap.GH.User != "" {
		t.Errorf("GH.User = %q, want empty", snap.GH.User)
	}
}

func TestNoDaemon(t *testing.T) {
	snap := mustLoad(t, "no_daemon.json")
	if snap.DaemonPID != 0 {
		t.Errorf("DaemonPID = %d, want 0 (no daemon)", snap.DaemonPID)
	}
	if snap.Writer != "cli" {
		t.Errorf("Writer = %q, want cli", snap.Writer)
	}
}

func TestMinimalDefaults(t *testing.T) {
	snap := mustLoad(t, "minimal.json")
	if snap.RefreshInterval != 5 {
		t.Errorf("RefreshInterval = %d, want default 5", snap.RefreshInterval)
	}
	if snap.Targets[0].Label != "myorg/myrepo" {
		t.Errorf("Targets[0].Label = %q, want derived myorg/myrepo", snap.Targets[0].Label)
	}
}

func TestFutureSchemaRejected(t *testing.T) {
	_, err := Load("testdata/v_future.json")
	if err == nil {
		t.Fatal("Load(v_future.json) succeeded, want ErrSchemaTooNew")
	}
	var tooNew ErrSchemaTooNew
	if !errors.As(err, &tooNew) {
		t.Fatalf("err = %v (%T), want ErrSchemaTooNew", err, err)
	}
	if tooNew.Got != 3 {
		t.Errorf("tooNew.Got = %d, want 3", tooNew.Got)
	}
}

func TestMissingSchemaNeedsUpgrade(t *testing.T) {
	_, err := Load("testdata/empty.json")
	if err == nil {
		t.Fatal("Load(empty.json) succeeded, want ErrNeedsUpgrade")
	}
	var needsUpgrade ErrNeedsUpgrade
	if !errors.As(err, &needsUpgrade) {
		t.Fatalf("err = %v (%T), want ErrNeedsUpgrade", err, err)
	}
}

func TestParseUnknownFieldsIgnored(t *testing.T) {
	data := []byte(`{"schema":2,"version":"3.1.0","tick_ts":1,"targets":[],"runners":[],"totally_unknown_field":{"a":1}}`)
	snap, err := Parse(data, SourceCLI)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if snap.Source != SourceCLI {
		t.Errorf("Source = %v, want SourceCLI", snap.Source)
	}
}
