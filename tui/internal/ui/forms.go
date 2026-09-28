package ui

import (
	"fmt"
	"strconv"

	"charm.land/bubbles/v2/textinput"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

// AddTargetForm is the single-field 'a' form (§3.4): "owner/repo,
// org-name, or https://github.com/...", submitted as one --add-target
// exec. Deliberately plain-text rendering rather than a centred overlay,
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
