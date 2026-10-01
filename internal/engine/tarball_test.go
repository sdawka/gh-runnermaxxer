package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTarballDetectAndVersion(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	eq(t, "", e.detectRunnerTarball(), "none")
	old := makeFakeTarball(t, e, "2.330.0")
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	newer := makeFakeTarball(t, e, "2.334.0")
	eq(t, newer, e.detectRunnerTarball(), "newest by mtime")
	os.WriteFile(filepath.Join(e.dir, "actions-runner-other-x64-9.tar.gz"), nil, 0o644)
	eq(t, newer, e.detectRunnerTarball(), "other platforms ignored")
	eq(t, "2.334.0", tarballVersion("/x/actions-runner-osx-arm64-2.334.0.tar.gz"), "version")
	eq(t, "", tarballVersion(""), "empty")
}

func TestLatestRunnerTag(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	te.gh.set(func([]string) (string, string, int) { return "v2.337.0\n", "", 0 })
	tag, _, ok := e.latestRunnerTag(false)
	eq(t, true, ok, "ok")
	eq(t, "v2.337.0", tag, "tag")
	e.latestRunnerTag(false)
	eq(t, 1, te.gh.count("releases/latest"), "24h cache")
	e.latestRunnerTag(true)
	eq(t, 2, te.gh.count("releases/latest"), "refresh skips the cache")
	eq(t, "v2.337.0", e.cachedLatestRunnerTag(), "cached tag")
	te.now = te.now.Add(25 * time.Hour)
	e.latestRunnerTag(false)
	eq(t, 3, te.gh.count("releases/latest"), "cache expires after 24h")

	// gh fails with no cache: no tag, no cache file
	os.Remove(e.tagCachePath())
	te.gh.set(func([]string) (string, string, int) { return "", "gh: dial tcp: timeout", 1 })
	_, _, ok = e.latestRunnerTag(false)
	eq(t, false, ok, "failure")
	eq(t, false, exists(e.tagCachePath()), "no cache file left behind")
}

func TestTarballFreshnessInSnapshot(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	makeFakeTarball(t, e, "2.334.0")
	s := e.buildSnapshot()
	eq(t, "2.334.0", s.Tarball.Version, "version")
	eq(t, false, s.Tarball.Stale, "unknown latest -> not stale")
	te.gh.set(func([]string) (string, string, int) { return "v2.337.0\n", "", 0 })
	e.latestRunnerTag(false)
	s = e.buildSnapshot()
	eq(t, "2.337.0", s.Tarball.Latest, "latest from the cache")
	eq(t, true, s.Tarball.Stale, "stale")
}

func TestDownloadRunnerTarball(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	content := []byte("fake tarball bytes")
	sum := sha256.Sum256(content)
	hexsum := hex.EncodeToString(sum[:])
	asset := "actions-runner-" + e.plat.runnerOS + "-" + e.plat.runnerArch + "-2.337.0.tar.gz"
	body := "notes <!-- BEGIN SHA " + e.plat.runnerOS + "-" + e.plat.runnerArch + " -->" + hexsum + "<!-- END SHA -->"
	te.gh.set(func(args []string) (string, string, int) {
		a := strings.Join(args, " ")
		switch {
		case strings.Contains(a, "releases/latest"):
			return "v2.337.0\n", "", 0
		case strings.HasPrefix(a, "release download"):
			os.WriteFile(filepath.Join(e.dir, asset), content, 0o644)
			return "", "", 0
		case strings.Contains(a, "releases/tags/v2.337.0"):
			return body, "", 0
		}
		return "", "", 0
	})
	msg, err := e.Download()
	if err != nil {
		t.Fatal(err)
	}
	contains(t, msg, "checksum verified", "verified")
	eq(t, filepath.Join(e.dir, asset), e.detectRunnerTarball(), "installed")

	// mismatch: deleted
	body = strings.Replace(body, hexsum, strings.Repeat("0", 64), 1)
	_, err = e.Download()
	if err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	contains(t, err.Error(), "checksum mismatch", "error")
	eq(t, "", e.detectRunnerTarball(), "deleted")

	// ensureTarball downloads when none is present
	body = "no sha here"
	p, err := e.ensureTarball()
	eq(t, nil, err, "ensure downloads")
	eq(t, filepath.Join(e.dir, asset), p, "path")
}

func TestShutdownIdempotentAndLock(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".runnermaxxer.conf"), []byte("GH_HEALTH_TICKS=\"0\"\n"), 0o644)
	ctx := context.Background()
	e, err := Open(ctx, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, Options{Dir: dir}); err == nil {
		t.Fatal("second engine on the same dir opened")
	}
	select {
	case s := <-e.Snapshots():
		eq(t, "tui", s.Writer, "snapshot pushed")
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot")
	}
	if err := e.Shutdown(ctx, StopNow); err != nil {
		t.Fatal(err)
	}
	eq(t, nil, e.Shutdown(ctx, StopNow), "idempotent")
	closed := make(chan struct{})
	go func() {
		for range e.Snapshots() {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("snapshot channel not closed")
	}
	if _, err := e.Poll(); err != ErrClosed {
		t.Errorf("Poll after shutdown: %v", err)
	}
	e2, err := Open(ctx, Options{Dir: dir})
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	e2.Shutdown(ctx, DrainThenStop)
}

func TestValidPrefix(t *testing.T) {
	p60 := strings.Repeat("a", 60)
	eq(t, true, validPrefix(p60), "60 characters")
	eq(t, false, validPrefix(p60+"a"), "61 characters")
	eq(t, false, validPrefix(""), "empty")
	eq(t, false, validPrefix("a.b"), "dot")
}
