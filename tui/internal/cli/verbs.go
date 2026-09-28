package cli

import (
	"context"
	"sort"
	"strconv"
)

// Client wraps a Runner with typed verbs matching runnermaxxer.sh's
// scripting CLI (README "Headless / scripting" section, plus the new verbs
// the bash plan's PR3 adds: AddTarget, RemoveTarget, SetBounds, SetConfig,
// Poll, GHStatus, StopAll, StartAll). Every method is one exec; none of
// them touch runner state directly.
//
// The [bash-3] verbs (AddTarget, RemoveTarget, SetBounds, SetConfig, Poll,
// GHStatus, StopAll, StartAll) depend on CLI flags the bash side has not
// landed yet; calling them against an older runnermaxxer.sh will simply
// fail with that script's own "unknown flag" error, which callers should
// surface like any other non-zero exit.
type Client struct {
	Runner Runner
}

// NewClient wraps r in a Client.
func NewClient(r Runner) Client {
	return Client{Runner: r}
}

func (c Client) exec(ctx context.Context, args ...string) Result {
	return c.Runner.Run(ctx, DefaultTimeout, args...)
}

func (c Client) execLong(ctx context.Context, args ...string) Result {
	return c.Runner.Run(ctx, LongTimeout, args...)
}

// Scale sets runner counts for one or more targets in a single exec, one
// "--scale url=N" flag per entry, sorted by URL for deterministic argv
// (and therefore deterministic tests).
func (c Client) Scale(ctx context.Context, wanted map[string]int) Result {
	urls := make([]string, 0, len(wanted))
	for u := range wanted {
		urls = append(urls, u)
	}
	sort.Strings(urls)

	args := make([]string, 0, len(urls)*2)
	for _, u := range urls {
		args = append(args, "--scale", u+"="+strconv.Itoa(wanted[u]))
	}
	return c.execLong(ctx, args...)
}

// Drain removes runner id once its current job finishes (or immediately if idle).
func (c Client) Drain(ctx context.Context, id int) Result {
	return c.exec(ctx, "--drain", strconv.Itoa(id))
}

// Stop stops runner id; it stays down until Start.
func (c Client) Stop(ctx context.Context, id int) Result {
	return c.exec(ctx, "--stop", strconv.Itoa(id))
}

// Start starts runner id, clearing any quarantine/backoff state.
func (c Client) Start(ctx context.Context, id int) Result {
	return c.exec(ctx, "--start", strconv.Itoa(id))
}

// Remove unregisters and deletes runner id immediately (no drain).
func (c Client) Remove(ctx context.Context, id int) Result {
	return c.exec(ctx, "--remove", strconv.Itoa(id))
}

// AddTarget adds a target (owner/repo, org name, or a full URL) to the
// targets file. [bash-3].
func (c Client) AddTarget(ctx context.Context, entry string) Result {
	return c.exec(ctx, "--add-target", entry)
}

// RemoveTarget removes a target from the targets file; the script refuses
// while runners exist for it. [bash-3].
func (c Client) RemoveTarget(ctx context.Context, url string) Result {
	return c.exec(ctx, "--remove-target", url)
}

// SetBounds sets (or, with min==0 && max==0, clears) autoscale bounds for a
// target. [bash-3].
func (c Client) SetBounds(ctx context.Context, url string, min, max int) Result {
	if min == 0 && max == 0 {
		return c.exec(ctx, "--set-bounds", url+"=none")
	}
	return c.exec(ctx, "--set-bounds", url+"="+strconv.Itoa(min)+"-"+strconv.Itoa(max))
}

// SetConfig sets a single config key. Repeat the call for multiple keys;
// the script's --set-config flag is itself repeatable but the TUI's config
// screen only ever changes one field at a time. [bash-3].
func (c Client) SetConfig(ctx context.Context, key, value string) Result {
	return c.exec(ctx, "--set-config", key+"="+value)
}

// Poll forces a GitHub health poll on the daemon's next tick. [bash-3].
func (c Client) Poll(ctx context.Context) Result {
	return c.exec(ctx, "--poll")
}

// GHStatus returns the text body of check_github_status (per-target
// registered runners, orphans note). [bash-3].
func (c Client) GHStatus(ctx context.Context) Result {
	return c.exec(ctx, "--gh-status")
}

// StopAll stops every runner. Falls back to looping Stop per id is the
// caller's job if the script predates --stop-all. [bash-3].
func (c Client) StopAll(ctx context.Context) Result {
	return c.exec(ctx, "--stop-all")
}

// StartAll starts every runner. [bash-3].
func (c Client) StartAll(ctx context.Context) Result {
	return c.exec(ctx, "--start-all")
}

// Download fetches (or re-fetches) the latest runner tarball for this
// platform.
func (c Client) Download(ctx context.Context) Result {
	return c.execLong(ctx, "--download")
}

// InstallService installs the login service (launchd/systemd --user) that
// runs the daemon.
func (c Client) InstallService(ctx context.Context) Result {
	return c.exec(ctx, "--install-service")
}

// StopDaemon stops a running daemon; runners keep running unsupervised.
func (c Client) StopDaemon(ctx context.Context) Result {
	return c.exec(ctx, "--stop-daemon")
}

// StatusJSON runs `--status --json`, the fallback loader when there is no
// state.json to watch.
func (c Client) StatusJSON(ctx context.Context) Result {
	return c.exec(ctx, "--status", "--json")
}

// Version runs `--version` (used at startup to check the script is new
// enough for the features the TUI needs).
func (c Client) Version(ctx context.Context) Result {
	return c.exec(ctx, "--version")
}
