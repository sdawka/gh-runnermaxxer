package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/sdawka/gh-runnermaxxer/tui/internal/state"
)

// detailKeyW is the key column width in the (non-compact) detail block.
const detailKeyW = 9

// detailField is one key/value line of the detail block. wide values
// (paths, error text) always get their own line(s) even in compact mode.
type detailField struct {
	key, value string
	wide       bool
	isErr      bool // render the value in the error style
}

// renderDetail describes the cursor's row in full: everything the table
// row has to leave out or truncate. The side pane shows it one field per
// line; compact (the below-100-columns layout) flows short fields
// together on shared lines to save rows. Every line fits in width columns;
// long values are word-wrapped rather than truncated.
func (m Model) renderDetail(width int, compact bool) []string {
	if width < 1 {
		return nil
	}
	title, fields := m.detailFields()
	lines := []string{m.fit(m.Theme.Bold.Render(title), width)}
	if compact {
		return append(lines, m.layoutFieldsCompact(fields, width)...)
	}
	return append(lines, m.layoutFields(fields, width)...)
}

// detailFields collects the title and fields for the cursor's row.
func (m Model) detailFields() (string, []detailField) {
	row, ok := m.cursorRow()
	if !ok {
		return "Nothing selected", []detailField{{value: "No targets yet: press a to add one.", wide: true}}
	}
	switch row.Kind {
	case RowTarget:
		return m.targetDetail(row.Target)
	case RowRunner:
		return m.runnerDetail(row.Runner)
	default:
		return "Unconfigured", []detailField{{
			value: "Runner directories with no target recorded: their setup never finished (interrupted add, or a failed registration). They aren't supervised or registered with GitHub. Select one and press d to remove it.",
			wide:  true,
		}}
	}
}

func (m Model) runnerDetail(r state.Runner) (string, []detailField) {
	target := "unconfigured"
	if r.Target != "" {
		target = r.Target
		if t, ok := m.Snap.TargetByURL(r.Target); ok && t.Label != "" {
			target = t.Label
		}
	}
	title := r.Name + " · " + target

	stateText := m.runnerStateWord(r)
	if s := strings.TrimRight(r.Status, "."); s != "" && s != stateText && s != string(r.State) {
		stateText += " · " + r.Status
	}
	fields := []detailField{{key: "state", value: stateText}}

	pid := "-"
	if r.PID != 0 {
		pid = fmt.Sprintf("%d", r.PID)
	}
	fields = append(fields,
		detailField{key: "pid", value: pid},
		detailField{key: "version", value: orDefault(r.Version, "?")},
	)

	var flags []string
	if r.Ephemeral {
		flags = append(flags, "ephemeral")
	}
	if r.Draining {
		flags = append(flags, "draining")
	}
	if r.Quarantined {
		flags = append(flags, "quarantined")
	}
	if len(flags) > 0 {
		fields = append(fields, detailField{key: "flags", value: strings.Join(flags, " ")})
	}

	if r.Job != "" {
		fields = append(fields, detailField{key: "job", value: fmt.Sprintf("%s (%s)", r.Job, formatDuration(m.runnerElapsed(r)))})
	}
	if r.Fails > 0 || r.NextRetry > 0 {
		fails := fmt.Sprintf("%d", r.Fails)
		if wait := r.NextRetry - m.Now.Unix(); r.NextRetry > 0 && wait > 0 {
			fails += fmt.Sprintf(" · retry in %ds", wait)
		}
		fields = append(fields, detailField{key: "fails", value: fails})
	}
	if r.WorkMB > 0 {
		fields = append(fields, detailField{key: "work", value: fmt.Sprintf("%d MB", r.WorkMB)})
	}
	fields = append(fields, detailField{key: "log", value: orDefault(r.LogPath, "(none yet)"), wide: true})
	if r.LastErr != "" {
		fields = append(fields, detailField{key: "lasterr", value: r.LastErr, wide: true, isErr: true})
	}
	return title, fields
}

func (m Model) targetDetail(t state.Target) (string, []detailField) {
	title := orDefault(t.Label, t.URL)
	listed := "yes"
	if !t.Listed {
		listed = "no (not in targets file)"
	}
	fields := []detailField{
		{key: "url", value: t.URL, wide: true},
		{key: "type", value: orDefault(t.Type, "?")},
		{key: "listed", value: listed},
		{key: "runners", value: fmt.Sprintf("have %d · want %d · running %d · busy %d", t.Have, t.Want, t.Running, t.Busy)},
	}

	scale := "-"
	if t.Min != 0 || t.Max != 0 {
		scale = fmt.Sprintf("%d-%d", t.Min, t.Max)
	}
	if t.Autoscale {
		scale += " · autoscale on"
	} else {
		scale += " · autoscale off"
	}
	fields = append(fields, detailField{key: "scale", value: scale})

	if t.Queued > 0 || t.Unsatisfiable > 0 {
		fields = append(fields, detailField{key: "queue", value: fmt.Sprintf("queued %d · unsatisfiable %d", t.Queued, t.Unsatisfiable)})
	}
	if t.Draining > 0 {
		fields = append(fields, detailField{key: "draining", value: fmt.Sprintf("%d", t.Draining)})
	}
	if e := t.Error; e != nil {
		text := e.Message
		if e.Class != "" {
			text = e.Class + ": " + text
		}
		if e.Since > 0 {
			text += fmt.Sprintf(" (since %s ago)", formatDuration(int(m.Now.Unix()-e.Since)))
		}
		fields = append(fields, detailField{key: "error", value: text, wide: true, isErr: true})
	}
	return title, fields
}

// layoutFields renders one field per line: a dim key column, then the
// value word-wrapped to the remaining width with a hanging indent. When
// the pane is too narrow for a key column the value goes on the line
// under its key instead.
func (m Model) layoutFields(fields []detailField, width int) []string {
	var out []string
	valueW := width - detailKeyW
	for _, f := range fields {
		if f.key == "" {
			out = append(out, m.styleLines(f, wrapText(f.value, width))...)
			continue
		}
		if valueW < 12 {
			out = append(out, m.fit(m.Theme.Dim.Render(f.key), width))
			out = append(out, m.styleLines(f, wrapText(f.value, width))...)
			continue
		}
		for i, l := range m.styleLines(f, wrapText(f.value, valueW)) {
			prefix := strings.Repeat(" ", detailKeyW)
			if i == 0 {
				prefix = m.Theme.Dim.Render(padRight(f.key, detailKeyW))
			}
			out = append(out, prefix+l)
		}
	}
	return out
}

// layoutFieldsCompact flows short "key value" fields onto shared lines
// separated by two spaces, and gives wide fields their own wrapped lines.
func (m Model) layoutFieldsCompact(fields []detailField, width int) []string {
	var out []string
	var line string
	lineW := 0
	flush := func() {
		if lineW > 0 {
			out = append(out, line)
		}
		line, lineW = "", 0
	}
	for _, f := range fields {
		if f.wide || f.key == "" {
			flush()
			text := f.value
			keyW := 0
			if f.key != "" {
				keyW = ansi.StringWidth(f.key) + 1
			}
			for i, l := range m.styleLines(f, wrapText(text, width-keyW)) {
				prefix := strings.Repeat(" ", keyW)
				if i == 0 && f.key != "" {
					prefix = m.Theme.Dim.Render(f.key) + " "
				}
				out = append(out, m.fit(prefix+l, width))
			}
			continue
		}
		item := m.Theme.Dim.Render(f.key) + " " + f.value
		itemW := ansi.StringWidth(f.key) + 1 + ansi.StringWidth(f.value)
		if lineW > 0 && lineW+2+itemW > width {
			flush()
		}
		if lineW > 0 {
			line += "  "
			lineW += 2
		}
		line += item
		lineW += itemW
	}
	flush()
	for i, l := range out {
		out[i] = m.fit(l, width)
	}
	return out
}

func (m Model) styleLines(f detailField, lines []string) []string {
	if !f.isErr {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = m.Theme.Error.Render(l)
	}
	return out
}
