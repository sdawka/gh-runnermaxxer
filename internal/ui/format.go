package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// truncate shortens s to at most n runes, appending an ellipsis when it had
// to cut (matches the dashboard's label truncation, §3.1/§3.2).
func truncate(s string, n int) string {
	return truncateWith(s, n, "…")
}

func truncateWith(s string, n int, ellipsis string) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	e := []rune(ellipsis)
	if n <= len(e) {
		return string(r[:n])
	}
	return string(r[:n-len(e)]) + ellipsis
}

// truncate is truncate using the model's glyph set's ellipsis, so
// -no-unicode frames stay ASCII.
func (m Model) truncate(s string, n int) string {
	return truncateWith(s, n, m.ellipsis())
}

func (m Model) ellipsis() string {
	if m.Glyphs.Ellipsis == "" {
		return "…"
	}
	return m.Glyphs.Ellipsis
}

// fit truncates a (possibly styled) line to width display columns,
// appending the ellipsis when it had to cut. width <= 0 leaves s alone.
func (m Model) fit(s string, width int) string {
	if width <= 0 || ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, m.ellipsis())
}

// padRight pads a (possibly styled) string with spaces to width display
// columns; strings already that wide are returned unchanged.
func padRight(s string, width int) string {
	if w := ansi.StringWidth(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// wrapText word-wraps plain text to width columns, hard-breaking words
// (paths, URLs) longer than a line. Returns at least one line.
func wrapText(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var lines []string
	var cur []rune
	for _, word := range strings.Fields(s) {
		w := []rune(word)
		switch {
		case len(cur) == 0:
		case len(cur)+1+len(w) <= width:
			cur = append(cur, ' ')
		default:
			lines = append(lines, string(cur))
			cur = nil
		}
		for len(cur)+len(w) > width {
			// A word longer than the line (a path, a URL): hard-break it.
			n := width - len(cur)
			if len(cur) == 0 {
				// Prefer breaking a path/URL just after a '/' when one
				// falls in the back half of the line.
				for i := n - 1; i >= n/2; i-- {
					if w[i] == '/' {
						n = i + 1
						break
					}
				}
			}
			lines = append(lines, string(append(cur, w[:n]...)))
			cur, w = nil, w[n:]
		}
		cur = append(cur, w...)
	}
	if len(cur) > 0 || len(lines) == 0 {
		lines = append(lines, string(cur))
	}
	return lines
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
