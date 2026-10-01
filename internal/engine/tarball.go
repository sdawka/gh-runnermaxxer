package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const tagCacheTTL = 24 * time.Hour

var reTarballVer = regexp.MustCompile(`^actions-runner-[a-z]*-[a-z0-9]*-(.*)\.tar\.gz$`)

// detectRunnerTarball is the newest actions-runner-OS-ARCH-*.tar.gz in Dir
// ("" when none).
func (e *Engine) detectRunnerTarball() string {
	matches, _ := filepath.Glob(filepath.Join(e.dir, fmt.Sprintf("actions-runner-%s-%s-*.tar.gz", e.plat.runnerOS, e.plat.runnerArch)))
	newest := ""
	var newestT time.Time
	for _, f := range matches {
		fi, err := os.Stat(f)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if newest == "" || fi.ModTime().After(newestT) {
			newest, newestT = f, fi.ModTime()
		}
	}
	return newest
}

// tarballVersion is the version in a tarball's file name.
func tarballVersion(path string) string {
	if path == "" {
		return ""
	}
	if m := reTarballVer.FindStringSubmatch(filepath.Base(path)); m != nil {
		return m[1]
	}
	return ""
}

func (e *Engine) tagCachePath() string { return filepath.Join(e.base, ".latest-runner-tag") }

// latestRunnerTag is the latest actions/runner release tag (e.g. v2.337.0),
// cached in $RUNNER_BASE_DIR/.latest-runner-tag (epoch + tag) for 24h;
// refresh skips the cache.
func (e *Engine) latestRunnerTag(refresh bool) (string, ghResult, bool) {
	now := e.now()
	if !refresh {
		if lines := readLines(e.tagCachePath()); len(lines) >= 2 {
			if ep, err := strconv.ParseInt(lines[0], 10, 64); err == nil && lines[1] != "" && now.Sub(time.Unix(ep, 0)) < tagCacheTTL {
				return lines[1], ghResult{Class: "ok"}, true
			}
		}
	}
	out, r := e.ghAPI("repos/actions/runner/releases/latest", "--jq", ".tag_name")
	tag := strings.TrimSpace(out)
	if !r.ok() || tag == "" {
		return "", r, false
	}
	_ = os.WriteFile(e.tagCachePath(), []byte(fmt.Sprintf("%d\n%s\n", now.Unix(), tag)), 0o644)
	return tag, r, true
}

// cachedLatestRunnerTag is the cached tag only, never calling gh.
func (e *Engine) cachedLatestRunnerTag() string {
	if lines := readLines(e.tagCachePath()); len(lines) >= 2 {
		return lines[1]
	}
	return ""
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const downloadTimeout = 30 * time.Minute

// downloadRunnerTarball fetches the latest runner release for this platform
// into Dir and verifies its SHA-256 against the release notes.
func (e *Engine) downloadRunnerTarball() (string, error) {
	tag, r, ok := e.latestRunnerTag(true)
	if !ok {
		return "", fmt.Errorf("could not query the latest runner release: %s", ghErrorNote(r.Class, r.Msg))
	}
	ver := strings.TrimPrefix(tag, "v")
	asset := fmt.Sprintf("actions-runner-%s-%s-%s.tar.gz", e.plat.runnerOS, e.plat.runnerArch, ver)
	e.dlog("downloading " + asset)
	if _, r := e.ghRunTimeout(downloadTimeout, "release", "download", tag, "-R", "actions/runner", "--pattern", asset, "--dir", e.dir, "--clobber"); !r.ok() {
		return "", fmt.Errorf("download failed (%s) - get it manually from https://github.com/actions/runner/releases", ghErrorNote(r.Class, r.Msg))
	}
	path := filepath.Join(e.dir, asset)

	// Verify against the SHA published in the release notes
	var notes []string
	body, _ := e.ghAPI("repos/actions/runner/releases/tags/"+tag, "--jq", ".body")
	re := regexp.MustCompile(`BEGIN SHA ` + regexp.QuoteMeta(e.plat.runnerOS+"-"+e.plat.runnerArch) + ` -->([a-f0-9]{64})<`)
	if m := re.FindStringSubmatch(body); m != nil {
		actual, err := sha256File(path)
		if err == nil && actual != m[1] {
			os.Remove(path)
			return "", fmt.Errorf("checksum mismatch for %s - deleted. Try again or download manually", asset)
		}
		notes = append(notes, "checksum verified")
	} else {
		notes = append(notes, "no checksum in the release notes for "+tag+" - installed without verification")
	}

	// Browsers and some download paths tag files with com.apple.quarantine,
	// which Gatekeeper then applies to the extracted runner binaries
	if e.plat.osFamily == "macos" && haveCmd("xattr") {
		_ = exec.Command("xattr", "-d", "com.apple.quarantine", path).Run()
	}
	msg := fmt.Sprintf("Downloaded %s (%s)", asset, strings.Join(notes, ", "))
	e.dlog(msg)
	return msg, nil
}

// ensureTarball returns the runner tarball, downloading it when missing.
func (e *Engine) ensureTarball() (string, error) {
	if t := e.detectRunnerTarball(); t != "" {
		return t, nil
	}
	if _, err := e.downloadRunnerTarball(); err != nil {
		return "", err
	}
	if t := e.detectRunnerTarball(); t != "" {
		return t, nil
	}
	return "", errors.New("no runner tarball for " + e.plat.runnerOS + "-" + e.plat.runnerArch)
}
