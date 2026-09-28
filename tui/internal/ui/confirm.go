package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Choice is one option a ConfirmModel offers.
type Choice struct {
	Key    string
	Label  string
	Danger bool
}

// ConfirmModel is the generic confirm/choice modal from the Go design doc's
// §3.4: a title, an optional body, a list of choices with a default, and a
// callback that runs once a choice (or Esc, reported as choice "") is made.
// While open it captures every key: Esc always cancels regardless of the
// choice list, Enter picks Default, and any key matching a Choice.Key picks
// that choice.
type ConfirmModel struct {
	Title    string
	Body     string
	Choices  []Choice
	Default  int
	OnChoice func(Model, string) (Model, tea.Cmd)
}

// handleConfirmKey resolves a keypress against the open confirm modal.
func (m Model) handleConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	confirm := m.Confirm
	k := msg.String()

	resolve := func(mm Model, choice string) (tea.Model, tea.Cmd) {
		mm.Confirm = nil
		if confirm.OnChoice == nil {
			return mm, nil
		}
		return confirm.OnChoice(mm, choice)
	}

	switch {
	case k == "esc":
		return resolve(m, "")
	case k == "enter":
		choice := ""
		if confirm.Default >= 0 && confirm.Default < len(confirm.Choices) {
			choice = confirm.Choices[confirm.Default].Key
		}
		return resolve(m, choice)
	}
	for _, ch := range confirm.Choices {
		if ch.Key == k {
			return resolve(m, ch.Key)
		}
	}
	return m, nil // key doesn't match any choice: modal stays open
}

// view renders the modal as plain text. The design doc calls for a centred
// lipgloss.Place overlay; this commit renders it as a trailing block
// instead, which keeps the golden-frame tests simple and deterministic and
// can be swapped for a true overlay later without changing ConfirmModel's
// behaviour.
func (c *ConfirmModel) view(theme Theme) string {
	var b strings.Builder
	b.WriteString(theme.Bold.Render(c.Title))
	if c.Body != "" {
		b.WriteString("\n")
		b.WriteString(c.Body)
	}
	for i, ch := range c.Choices {
		b.WriteString("\n  ")
		marker := "  "
		if i == c.Default {
			marker = "> "
		}
		label := ch.Label
		if ch.Danger {
			label = theme.Error.Render(label)
		}
		b.WriteString(marker)
		b.WriteString("[" + ch.Key + "] ")
		b.WriteString(label)
	}
	return b.String()
}
