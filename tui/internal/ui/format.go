package ui

import "fmt"

// truncate shortens s to at most n runes, appending an ellipsis when it had
// to cut (matches the dashboard's label truncation, §3.1/§3.2).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// orDefault returns s, or def when s is empty (used for the many
// nullable-in-JSON string fields: gh.user, tarball.version, lasterr, ...).
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// formatDuration renders a runner's elapsed job time the way the bash
// dashboard does: minutes for anything under an hour, "1h5m" above that.
func formatDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	mins := seconds / 60
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	hours := mins / 60
	mins %= 60
	return fmt.Sprintf("%dh%dm", hours, mins)
}
