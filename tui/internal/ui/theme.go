package ui

import (
	"os"

	"charm.land/lipgloss/v2"
)

// Theme is every style the views use, built once in main from the
// terminal's actual colour profile (Go design doc §3.3) so NO_COLOR and
// non-truecolor terminals degrade to bold/dim/reverse rather than being
// handed colours they can't show.
type Theme struct {
	NoColor bool // true under NO_COLOR or TERM=dumb: styles below still work, they just carry no ANSI colour codes

	Running  lipgloss.Style // green
	Busy     lipgloss.Style // cyan
	Selected lipgloss.Style // cyan/reverse
	Pending  lipgloss.Style // yellow
	Draining lipgloss.Style // yellow
	Warn     lipgloss.Style // yellow
	Error    lipgloss.Style // red
	Stopped  lipgloss.Style // red
	Dim      lipgloss.Style // secondary text
	Bold     lipgloss.Style
	Reverse  lipgloss.Style
}

// NewTheme builds a Theme honouring NO_COLOR (any value at all, per
// no-color.org) and TERM=dumb.
func NewTheme() Theme {
	noColor := os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
	if noColor {
		return Theme{
			NoColor:  true,
			Running:  lipgloss.NewStyle(),
			Busy:     lipgloss.NewStyle().Bold(true),
			Selected: lipgloss.NewStyle().Reverse(true),
			Pending:  lipgloss.NewStyle().Bold(true),
			Draining: lipgloss.NewStyle().Bold(true),
			Warn:     lipgloss.NewStyle().Bold(true),
			Error:    lipgloss.NewStyle().Reverse(true),
			Stopped:  lipgloss.NewStyle().Faint(true),
			Dim:      lipgloss.NewStyle().Faint(true),
			Bold:     lipgloss.NewStyle().Bold(true),
			Reverse:  lipgloss.NewStyle().Reverse(true),
		}
	}
	return Theme{
		Running:  lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		Busy:     lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		Selected: lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true),
		Pending:  lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		Draining: lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		Warn:     lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		Error:    lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		Stopped:  lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		Dim:      lipgloss.NewStyle().Faint(true),
		Bold:     lipgloss.NewStyle().Bold(true),
		Reverse:  lipgloss.NewStyle().Reverse(true),
	}
}

// Glyphs is the set of state symbols views use; NewGlyphs swaps in ASCII
// when --no-unicode is passed or NO_COLOR-style plainness is wanted for
// glyphs too (the design doc treats glyph choice and colour as separate
// switches: glyphs are UTF-8 either way unless --no-unicode says otherwise).
type Glyphs struct {
	Running, Idle, Stopped, Backoff, Quarantined string
	HeaderOpen, Selected                         string
	Warn                                         string
}

// NewGlyphs returns the Unicode glyph set, or the ASCII fallback when
// noUnicode is true.
func NewGlyphs(noUnicode bool) Glyphs {
	if noUnicode {
		return Glyphs{
			Running: "*", Idle: "*", Stopped: "o", Backoff: ".", Quarantined: "X",
			HeaderOpen: "v", Selected: "<", Warn: "!",
		}
	}
	return Glyphs{
		Running: "●", Idle: "●", Stopped: "○", Backoff: "◌", Quarantined: "✖",
		HeaderOpen: "▾", Selected: "◂", Warn: "⚠",
	}
}
