package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/sdawka/gh-runner-swarm/tui/internal/state"
)

// renderHeader is the status bar (Go design doc §3.1): script version,
// daemon pid, tick age, gh identity/scopes, rate limit. Below 30 rows
// (m.Compact) it collapses to the one line; otherwise a second line adds
// the tarball/fleet/disk summary.
func (m Model) renderHeader() []string {
	version := orDefault(m.Snap.ScriptVersion, "?")
	daemon := "none"
	if m.Snap.DaemonPID != 0 {
		daemon = fmt.Sprintf("%d", m.Snap.DaemonPID)
	}

	tick := "n/a"
	if m.Snap.TickTS > 0 {
		age := m.Now.Sub(time.Unix(m.Snap.TickTS, 0))
		if age < 0 {
			age = 0
		}
		tick = fmt.Sprintf("%ds ago", int(age.Seconds()))
	}

	line1 := fmt.Sprintf("gh-runnermaxxer %s · daemon %s · tick %s · %s", version, daemon, tick, m.renderGHSummary())

	if m.Compact {
		return []string{line1}
	}
	return []string{line1, m.renderFleetSummary()}
}

func (m Model) renderGHSummary() string {
	gh := m.Snap.GH
	switch gh.State {
	case state.GHStateOK:
		s := fmt.Sprintf("gh: %s %s", orDefault(gh.User, "?"), m.Glyphs.Selected)
		if len(gh.Scopes) > 0 {
			s += fmt.Sprintf(" (%s)", strings.Join(gh.Scopes, ", "))
		}
		if gh.RateRemaining > 0 {
			s += fmt.Sprintf(" · rate %d/5000", gh.RateRemaining)
		}
		return s
	case "":
		return "gh: unknown"
	default:
		return fmt.Sprintf("gh: %s", gh.State)
	}
}

func (m Model) renderFleetSummary() string {
	tb := m.Snap.Tarball
	tbLine := fmt.Sprintf("tarball %s", orDefault(tb.Version, "?"))
	if tb.Stale {
		tbLine += fmt.Sprintf(" (latest %s) %s", orDefault(tb.Latest, "?"), m.Glyphs.Warn)
	}
	return fmt.Sprintf("%s · disk %d%% free", tbLine, m.Snap.Disk.PctFree)
}

// renderBanners stacks the sticky warning banners: daemon down/stalled, gh
// auth/rate/network problems, and the snapshot's own warnings[] (config
// values that got clamped, per the bash plan's C2).
func (m Model) renderBanners() []string {
	var out []string

	if m.Snap.DaemonPID == 0 {
		out = append(out, m.Theme.Warn.Render(fmt.Sprintf("%s no daemon running: runners are not supervised (no auto-restart, drains never finish)", m.Glyphs.Warn)))
	} else if stale, age := m.Snap.Stale(m.Now); stale {
		out = append(out, m.Theme.Warn.Render(fmt.Sprintf("%s daemon %d stalled: last tick %s ago", m.Glyphs.Warn, m.Snap.DaemonPID, age.Round(time.Second))))
	}

	switch m.Snap.GH.State {
	case state.GHStateAuth, state.GHStateSSO, state.GHStateError:
		out = append(out, m.Theme.Error.Render(fmt.Sprintf("%s gh: %s", m.Glyphs.Warn, orDefault(m.Snap.GH.Message, string(m.Snap.GH.State)))))
	case state.GHStateRateLimit:
		out = append(out, m.Theme.Warn.Render(fmt.Sprintf("%s gh: rate limited (polling paused, runners unaffected)", m.Glyphs.Warn)))
	case state.GHStateNetwork:
		out = append(out, m.Theme.Warn.Render(fmt.Sprintf("%s gh: unreachable (%s)", m.Glyphs.Warn, orDefault(m.Snap.GH.Message, "network error"))))
	}

	for _, w := range m.Snap.Warnings {
		out = append(out, m.Theme.Dim.Render(fmt.Sprintf("%s %s", m.Glyphs.Warn, w)))
	}
	return out
}
