package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// View implements tea.Model: status bar, banners, the grouped runner table,
// the detail/log area, and a footer that is either the short help line or
// the latest toast. Below 12 rows (§3.2) the footer help line is dropped to
// leave room for the table; '?' still opens the full help overlay.
func (m Model) View() tea.View {
	return tea.NewView(m.renderString())
}

// logPaneWidth is how much of the frame's width the side pane takes when
// the terminal is wide enough for one (ShowLog).
func (m Model) logPaneWidth() int {
	w := m.Width / 3
	if w < 30 {
		w = 30
	}
	if w > m.Width-20 {
		w = m.Width - 20
	}
	if w < 1 {
		w = 1
	}
	return w
}

// renderString builds the full frame as plain text; split out from View so
// tests can assert on it directly without unwrapping a tea.View. When a log
// view is open and the terminal isn't wide enough for a side pane (§3.4),
// the log view takes over the whole frame instead. Whatever is drawn, the
// frame never exceeds Height lines or Width columns (once sized).
func (m Model) renderString() string {
	return clampFrame(m.renderFrame(), m.Width, m.Height, m)
}

func (m Model) renderFrame() string {
	if m.Log != nil && !m.ShowLog {
		return m.renderLogFullScreen()
	}
	if m.Screen == ScreenAddTarget && m.AddTarget != nil {
		return m.AddTarget.view()
	}
	if m.Screen == ScreenBounds && m.Bounds != nil {
		return m.Bounds.view()
	}
	if m.Screen == ScreenConfig && m.Config != nil {
		return m.Config.view(m.Snap.Config)
	}
	if m.Screen == ScreenGHStatus && m.GHStatusText != nil {
		return "GitHub status\n\n" + *m.GHStatusText + "\n\n" + m.Theme.Dim.Render("any key closes")
	}

	f := m.layout()
	lines := append([]string(nil), f.top...)

	table := m.renderTable(m.tableColumns(f.tableW, !f.detailShown), f.tableH)
	if f.wide {
		pane := m.renderSidePane(f.paneW-1, f.body)
		lines = append(lines, joinColumns(table, f.tableW+1, pane, m.Theme.Dim.Render(m.Glyphs.RuleV), f.body)...)
	} else {
		lines = append(lines, table...)
		if f.auxH > 0 {
			lines = append(lines, m.renderBelowTable(f.tableW, f.auxH)...)
		}
	}

	if f.footer != "" {
		lines = append(lines, f.footer)
	}
	return strings.Join(lines, "\n")
}

// frameLayout is how the dashboard/projects frame divides its rows and
// columns. Update uses it too, to keep the table's scroll window in sync
// with what the view will actually show.
type frameLayout struct {
	top    []string // header, banners and (projects screen) its title line
	footer string   // "" when there is no footer line
	body   int      // lines between top and footer; 0 = unbounded (unsized)

	wide   bool // side pane layout (>= 100 columns)
	tableW int  // table width
	paneW  int  // side pane width, separator column included (wide only)
	tableH int  // lines the table may use; 0 = unbounded
	auxH   int  // narrow only: lines under the table for detail + log tail

	// detailShown is true when a detail block is on screen, so rows can
	// drop their inline error text.
	detailShown bool
}

// minBodyForDetail: below this many body rows, the narrow layout only
// shows a detail block when the whole table fits above it too.
const minBodyForDetail = 12

func (m Model) layout() frameLayout {
	var f frameLayout
	f.top = append(f.top, m.renderHeader()...)
	f.top = append(f.top, m.renderBanners()...)
	if m.Screen == ScreenProjects {
		f.top = append(f.top, m.Theme.Dim.Render("-- Projects --"))
	}
	f.footer = m.renderFooter()

	f.wide = m.ShowLog
	f.tableW = m.Width
	if f.wide {
		f.paneW = m.logPaneWidth()
		f.tableW = m.Width - f.paneW - 2 // a space and the separator column
	}

	if m.Height <= 0 {
		// Not sized yet: draw the whole table and nothing else.
		f.wide = false
		f.tableW = m.Width
		return f
	}

	f.body = m.Height - len(f.top)
	if f.footer != "" {
		f.body--
	}
	if f.body < 1 {
		f.body = 1
	}

	if f.wide {
		f.tableH = f.body
		f.detailShown = m.Log == nil && m.overlayLines(f.paneW-1) == nil
		return f
	}

	// Narrow: the table takes what it needs, and the detail block and log
	// tail fill the rest. When everything doesn't fit, the table keeps at
	// least half the body (scrolling), a confirm/help overlay wins over
	// the table (it has to be readable to be answered), and a detail area
	// too small to say anything is dropped.
	need := m.tableLineCount()
	overlay := m.overlayLines(f.tableW)
	want := 1 + len(overlay) // the rule above the aux area
	if overlay == nil {
		want = 1 + len(m.renderDetail(f.tableW, true))
	}
	switch {
	case need+want <= f.body:
		f.tableH = need
	case overlay != nil:
		keep := need
		if keep > 3 {
			keep = 3
		}
		f.tableH = f.body - want
		if f.tableH < keep {
			f.tableH = keep
		}
	default:
		f.tableH = f.body - want
		if half := (f.body + 1) / 2; f.tableH < half {
			f.tableH = half
		}
		if f.tableH > need {
			f.tableH = need
		}
	}
	if f.tableH > f.body {
		f.tableH = f.body
	}
	f.auxH = f.body - f.tableH
	if overlay == nil && (f.auxH < 3 || (f.tableH < need && f.body < minBodyForDetail)) {
		// Too little room: on a short terminal the table matters more
		// than the detail block.
		f.tableH, f.auxH = f.body, 0
	}
	f.detailShown = f.auxH > 0 && overlay == nil
	return f
}

// overlayLines renders whatever modal content replaces the detail block:
// the confirm modal, else the '?' help. nil when neither is open.
func (m Model) overlayLines(width int) []string {
	switch {
	case m.Confirm != nil:
		var out []string
		for _, l := range strings.Split(m.Confirm.view(m.Theme), "\n") {
			for _, w := range strings.Split(ansi.Wrap(l, width, ""), "\n") {
				out = append(out, m.fit(w, width))
			}
		}
		return out
	case m.Help:
		return m.renderHelp(width)
	}
	return nil
}

// renderHelp lays the full key map out in as many columns as fit width.
func (m Model) renderHelp(width int) []string {
	var entries []string
	for _, group := range m.Keys.FullHelp() {
		for _, b := range group {
			h := b.Help()
			entries = append(entries, padRight(h.Key, 6)+" "+h.Desc)
		}
	}
	colW := 0
	for _, e := range entries {
		if w := ansi.StringWidth(e); w > colW {
			colW = w
		}
	}
	colW += 2
	cols := 1
	if width > 0 && colW > 0 {
		cols = width / colW
	}
	if cols < 1 {
		cols = 1
	}
	rows := (len(entries) + cols - 1) / cols
	out := []string{m.fit(m.Theme.Bold.Render("Keys")+m.Theme.Dim.Render("  (? closes)"), width)}
	for r := 0; r < rows; r++ {
		var b strings.Builder
		for c := 0; c < cols; c++ {
			i := c*rows + r
			if i >= len(entries) {
				break
			}
			if c < cols-1 {
				b.WriteString(padRight(entries[i], colW))
			} else {
				b.WriteString(entries[i])
			}
		}
		out = append(out, m.fit(strings.TrimRight(b.String(), " "), width))
	}
	return out
}

// renderSidePane fills the right-hand pane (width columns, height lines):
// the detail block (or a confirm/help overlay in its place), a rule, then
// the live tail of the selected runner's log. With an explicit 'l'/'L' log
// view open the detail block collapses and that view takes the space.
func (m Model) renderSidePane(width, height int) []string {
	var top []string
	if overlay := m.overlayLines(width); overlay != nil {
		top = overlay
	} else if m.Log == nil {
		top = m.renderDetail(width, false)
	}
	return m.stackLogBelow(top, width, height)
}

// renderBelowTable fills the narrow layout's rows under the table: a rule,
// then the compact detail block (or overlay), then the log tail.
func (m Model) renderBelowTable(width, height int) []string {
	top := m.overlayLines(width)
	if top == nil {
		top = m.renderDetail(width, true)
	}
	top = append([]string{m.rule("", width)}, top...)
	return m.stackLogBelow(top, width, height)
}

// stackLogBelow puts top in the first lines of a width x height block and
// fills the rest with a rule and the log: the explicit log view if one is
// open, else the live tail. Always returns exactly height lines.
func (m Model) stackLogBelow(top []string, width, height int) []string {
	if len(top) > height {
		top = top[:height]
	}
	lines := append([]string(nil), top...)
	rest := height - len(lines)

	switch {
	case m.Log != nil:
		if len(lines) > 0 && rest >= 3 {
			lines = append(lines, m.rule("", width))
			rest--
		}
		if rest >= 2 {
			m.Log.resize(width, rest-1)
			for _, l := range strings.Split(m.Log.view(), "\n") {
				lines = append(lines, m.fit(l, width))
			}
		}
	case rest >= 2:
		lines = append(lines, m.rule(m.tailTitle(), width))
		lines = append(lines, m.tailLines(width, rest-1)...)
	}

	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines
}

func (m Model) tailTitle() string {
	if m.Tail == nil {
		return "log"
	}
	return m.Tail.Title
}

// tailLines is the newest n lines of the live tail, one screen line each
// (truncated, never wrapped), oldest first so the newest sits at the
// bottom.
func (m Model) tailLines(width, n int) []string {
	if n <= 0 {
		return nil
	}
	lp := m.Tail
	switch {
	case lp == nil:
		return []string{m.Theme.Dim.Render(m.fit("(no log selected)", width))}
	case lp.Err != nil:
		return []string{m.Theme.Error.Render(m.fit("error: "+lp.Err.Error(), width))}
	case lp.path == "":
		return []string{m.Theme.Dim.Render(m.fit("(no log file yet)", width))}
	case len(lp.Lines) == 0:
		return []string{m.Theme.Dim.Render(m.fit("(no output yet)", width))}
	}
	src := lp.Lines
	if len(src) > n {
		src = src[len(src)-n:]
	}
	out := make([]string, len(src))
	for i, l := range src {
		out[i] = m.fit(sanitizeLogLine(l), width)
	}
	return out
}

// sanitizeLogLine makes a raw log line safe to place in a fixed-width
// cell: escape sequences stripped (the runner's own colours would bleed
// into the frame), tabs expanded, carriage returns dropped.
func sanitizeLogLine(s string) string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\t", "    ")
}

// rule is a dim horizontal rule width columns wide, optionally carrying a
// title ("── log: mac-2 ─────").
func (m Model) rule(title string, width int) string {
	if width <= 0 {
		return ""
	}
	h := m.Glyphs.RuleH
	if title == "" {
		return m.Theme.Dim.Render(strings.Repeat(h, width))
	}
	head := strings.Repeat(h, 2) + " " + title + " "
	if w := ansi.StringWidth(head); w < width {
		head += strings.Repeat(h, width-w)
	}
	return m.Theme.Dim.Render(m.fit(head, width))
}

// joinColumns lays left (padded to leftW) and right side by side with sep
// between them, for height lines.
func joinColumns(left []string, leftW int, right []string, sep string, height int) []string {
	n := height
	if len(left) > n {
		n = len(left)
	}
	if len(right) > n {
		n = len(right)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		line := padRight(l, leftW) + sep
		if r != "" {
			line += " " + r
		}
		out[i] = line
	}
	return out
}

// clampFrame is the last line of defence for the frame invariant: at most
// height lines, each at most width columns. The layout already sizes
// everything to fit; this only matters for screens with unbounded content
// (config, gh status) or terminals too small for the fixed chrome.
func clampFrame(s string, width, height int, m Model) string {
	if width <= 0 && height <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	if width > 0 {
		for i, l := range lines {
			lines[i] = m.fit(l, width)
		}
	}
	return strings.Join(lines, "\n")
}

// renderLogFullScreen replaces the whole frame with the open log view, for
// terminals too narrow to also show the side pane (§3.4).
func (m Model) renderLogFullScreen() string {
	height := m.Height - 2
	if height < 1 {
		height = 1
	}
	m.Log.resize(m.Width, height)
	footer := "Esc/q close  f follow  n/p next/prev runner  ↑↓ scroll"
	if m.Log.RunnerID == daemonLogID {
		footer = "Esc/q close  f follow  ↑↓ scroll"
	}
	return m.Log.view() + "\n" + m.Theme.Dim.Render(footer)
}

// renderFooter shows the most recent toast if one is active, else the
// short help line (suppressed below 12 rows so the table gets the space).
func (m Model) renderFooter() string {
	if len(m.Notices) > 0 {
		n := m.Notices[len(m.Notices)-1]
		style := m.Theme.Dim
		switch n.Level {
		case LevelWarn:
			style = m.Theme.Warn
		case LevelError:
			style = m.Theme.Error
		}
		return style.Render(n.Text)
	}
	if m.Height > 0 && m.Height < 12 {
		return ""
	}
	if m.Screen == ScreenProjects {
		return m.Theme.Dim.Render("↑↓ move  ←→/hl count  Enter apply & back  a add  x remove  b bounds  q/Esc back  ? help")
	}
	return m.Theme.Dim.Render("↑↓ move  ←→ count  Enter apply  Esc discard  d drain  x stop  s start  r restart  t projects  ? help")
}
