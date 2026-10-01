package ui

import (
	"strings"
	"testing"
)

func TestConfirmModelViewShowsTitleBodyAndChoices(t *testing.T) {
	c := &ConfirmModel{
		Title: "Remove runner-1 now (no drain)?",
		Choices: []Choice{
			{Key: "y", Label: "Remove now", Danger: true},
			{Key: "n", Label: "Cancel"},
		},
		Default: 1,
	}
	got := c.view(NewTheme())
	for _, want := range []string{"Remove runner-1 now (no drain)?", "[y]", "Remove now", "[n]", "Cancel"} {
		if !strings.Contains(got, want) {
			t.Errorf("view() missing %q:\n%s", want, got)
		}
	}
}
