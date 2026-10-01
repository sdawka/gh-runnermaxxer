package ui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/sdawka/gh-runnermaxxer/internal/engine"
	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// AddTargetForm is the single-field 'a' form (§3.4): "owner/repo,
// org-name, or https://github.com/...", submitted as one AddTarget
// call. Deliberately plain-text rendering rather than a centred overlay,
// the same documented deviation as ConfirmModel.
type AddTargetForm struct {
	Input textinput.Model
}

func newAddTargetForm() *AddTargetForm {
	ti := textinput.New()
	ti.Placeholder = "owner/repo, org-name, or https://github.com/..."
	ti.CharLimit = 200
	ti.SetWidth(60)
	return &AddTargetForm{Input: ti}
}

func (f *AddTargetForm) view() string {
	return fmt.Sprintf("Add project\n\n  %s\n\n  Enter add   Esc cancel", f.Input.View())
}

// BoundsForm is the two-field 'b' form: min/max autoscale bounds for the
// target under the cursor when it was opened, prefilled from its current
// bounds (empty when the target has none). Submitting both fields empty
// clears the bounds, mirroring menu_set_bounds.
type BoundsForm struct {
	URL     string
	Label   string
	Min     textinput.Model
	Max     textinput.Model
	Focused int // 0 = Min, 1 = Max
}

func newBoundsForm(t state.Target) *BoundsForm {
	min := textinput.New()
	min.Placeholder = "min"
	min.CharLimit = 6
	min.SetWidth(8)

	max := textinput.New()
	max.Placeholder = "max"
	max.CharLimit = 6
	max.SetWidth(8)

	if t.Autoscale {
		min.SetValue(strconv.Itoa(t.Min))
		max.SetValue(strconv.Itoa(t.Max))
	}

	return &BoundsForm{URL: t.URL, Label: t.Label, Min: min, Max: max}
}

func (f *BoundsForm) view() string {
	return fmt.Sprintf(
		"Autoscale bounds for %s\n\n  min %s   max %s\n\n  Tab switch field   Enter save (both empty clears)   Esc cancel",
		f.Label, f.Min.View(), f.Max.View(),
	)
}

// defaultMaxRunners is the setup form's suggested fleet-wide maximum.
const defaultMaxRunners = 20

// SetupForm is the first-run form shown while the engine has no
// configuration: the runner name prefix (empty means the host name, shown
// as the placeholder) and the most runners across all projects. There is no
// cancel; the only way past it without submitting is quitting.
type SetupForm struct {
	Prefix  textinput.Model
	Max     textinput.Model
	Focused int // 0 = Prefix, 1 = Max
	// Err is the last validation or Setup failure, shown under the fields.
	Err string
}

func newSetupForm() *SetupForm {
	prefix := textinput.New()
	prefix.Placeholder = engine.DefaultPrefix()
	prefix.CharLimit = 60
	prefix.SetWidth(30)

	max := textinput.New()
	max.CharLimit = 4
	max.SetWidth(6)
	max.SetValue(strconv.Itoa(defaultMaxRunners))

	f := &SetupForm{Prefix: prefix, Max: max}
	f.focus() // Init asks again, for the cursor blink Cmd
	return f
}

func (f *SetupForm) focus() tea.Cmd {
	if f.Focused == 0 {
		f.Max.Blur()
		return f.Prefix.Focus()
	}
	f.Prefix.Blur()
	return f.Max.Focus()
}

func (f *SetupForm) view(theme Theme, submitting bool) string {
	var b strings.Builder
	b.WriteString(theme.Bold.Render("Welcome to gh-runnermaxxer"))
	b.WriteString("\n\nFirst, a couple of settings (change them later with 'e').\n\n")
	fmt.Fprintf(&b, "  Runner name prefix  %s\n", f.Prefix.View())
	b.WriteString(theme.Dim.Render("                      runners are named <prefix>-1, <prefix>-2, ... (empty = host name)"))
	fmt.Fprintf(&b, "\n\n  Max runners         %s\n", f.Max.View())
	b.WriteString(theme.Dim.Render("                      the most runners across all projects"))
	b.WriteString("\n\n")
	switch {
	case submitting:
		b.WriteString("  Saving...\n\n")
	case f.Err != "":
		b.WriteString("  " + theme.Error.Render(f.Err) + "\n\n")
	}
	b.WriteString(theme.Dim.Render("  Tab switch field   Enter save   ctrl+c quit"))
	return b.String()
}
