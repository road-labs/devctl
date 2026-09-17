package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/road-labs/devctl/internal/config"
	"github.com/road-labs/devctl/internal/deps"
)

// The panel's colours. Adaptive, so it reads on a light terminal as well as the
// dark one most of us run; everything else is faint, or one of the three state
// tones the rows already use.
var (
	colAccent = lipgloss.AdaptiveColor{Light: "25", Dark: "111"}
	colSelBg  = lipgloss.AdaptiveColor{Light: "153", Dark: "24"}
	colSelFg  = lipgloss.AdaptiveColor{Light: "232", Dark: "231"}
	colRule   = lipgloss.AdaptiveColor{Light: "250", Dark: "238"}
)

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styleMuted    = lipgloss.NewStyle().Faint(true)
	styleGood     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleBad      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleCursor   = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styleHeader   = lipgloss.NewStyle().Faint(true).Underline(true)
	styleKey      = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styleRule     = lipgloss.NewStyle().Foreground(colRule)
	styleSelected = lipgloss.NewStyle().Background(colSelBg).Foreground(colSelFg)
	styleColumn   = lipgloss.NewStyle().Bold(true)
)

// pad extends s to n visible columns. Visible: the string carries escape
// sequences, and len() counts those.
func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// clip truncates s to n visible columns, marking the cut.
func clip(s string, n int) string {
	if n <= 1 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// gutter is the space between two columns.
const gutter = 2

// col is one column of a panel's table. Exactly one column in a table is flex;
// it takes whatever the fixed ones leave.
type col struct {
	title string
	flex  bool
	right bool
}

// fit gives every column its width. A fixed column is as wide as the widest
// thing in it, its title included; the flex column takes the rest. `seed` sets a
// floor per column, which is how the three panels keep one left edge: the name
// and status columns are measured across all of them, not one table at a time.
func fit(cols []col, rows [][]string, total int, seed []int) []int {
	widths := make([]int, len(cols))
	used := gutter * (len(cols) - 1)
	for i, c := range cols {
		if c.flex {
			continue
		}
		w := lipgloss.Width(c.title)
		if i < len(seed) {
			w = max(w, seed[i])
		}
		for _, r := range rows {
			if i < len(r) {
				w = max(w, lipgloss.Width(r[i]))
			}
		}
		// No fixed column may take more than a third of the panel. One long
		// address would otherwise leave the description nothing to sit in.
		widths[i] = min(w, max(total/3, 12))
		used += widths[i]
	}
	for i, c := range cols {
		if c.flex {
			widths[i] = max(total-used, 8)
		}
	}
	return widths
}

// cells lays one row into the computed columns.
func cells(cols []col, widths []int, row []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		text := ""
		if i < len(row) {
			text = clip(row[i], widths[i])
		}
		if c.right {
			text = strings.Repeat(" ", max(0, widths[i]-lipgloss.Width(text))) + text
		} else {
			text = pad(text, widths[i])
		}
		parts[i] = text
	}
	return strings.TrimRight(strings.Join(parts, strings.Repeat(" ", gutter)), " ")
}

// table renders a header row and its rows into one panel body.
func table(cols []col, rows [][]string, selected []bool, inner int, seed []int) []boxRow {
	widths := fit(cols, rows, inner, seed)
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
	}
	out := make([]boxRow, 0, len(rows)+1)
	out = append(out, boxRow{styleColumn.Render(cells(cols, widths, titles)), false})
	for i, r := range rows {
		out = append(out, boxRow{cells(cols, widths, r), i < len(selected) && selected[i]})
	}
	return out
}

// boxRow is one line inside a panel. A selected line is drawn as a bar across
// the whole panel, which means dropping its own colours: a background under
// text that keeps resetting it comes out striped.
type boxRow struct {
	text     string
	selected bool
}

// box draws a titled panel. The title sits in the top rule, the way a k8s
// console titles a table, so a panel says what it holds and how much.
func box(title string, inner int, rows []boxRow) string {
	total := inner + 4
	head := styleTitle.Render(title)
	fill := total - 3 - lipgloss.Width(head) - 2
	if fill < 0 {
		fill = 0
	}
	var b strings.Builder
	b.WriteString(styleRule.Render("╭─ ") + head + styleRule.Render(" "+strings.Repeat("─", fill)+"╮") + "\n")
	for _, r := range rows {
		text := r.text
		if r.selected {
			text = ansi.Strip(text)
		}
		text = " " + pad(clip(text, inner), inner) + " "
		if r.selected {
			text = styleSelected.Render(text)
		}
		b.WriteString(styleRule.Render("│") + text + styleRule.Render("│") + "\n")
	}
	b.WriteString(styleRule.Render("╰"+strings.Repeat("─", total-2)+"╯") + "\n")
	return b.String()
}

// tilde shortens a path under the home directory, which is where a checkout
// almost always is and which otherwise eats the header's first line.
func tilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, home) {
		return path
	}
	return "~" + path[len(home):]
}

// project names the repository devctl is running for. The manifest carries no
// name, so the checkout's own directory is it.
func (m Model) project() string {
	return filepath.Base(m.root)
}

// counts summarises each kind for the header: how many there are and how many
// are up.
func (m Model) counts() (depsUp, depsAll, svcUp, svcAll, tasks int) {
	for _, r := range m.rows {
		switch {
		case r.dep != nil:
			depsAll++
			if r.reachable {
				depsUp++
			}
		case r.task:
			tasks++
		default:
			svcAll++
			if r.running() {
				svcUp++
			}
		}
	}
	return
}

// field is one "label  value" line of the header's identity block.
func field(label, value string) string {
	return styleMuted.Render(pad(label, 9)) + value
}

// header is the three-column block above the panels: what this is running for
// on the left, the keys in the middle, what the markers mean on the right. The
// keys used to be one dense line at the bottom, where a reader had to scan a
// sentence to find a letter.
func (m Model) header(width int) string {
	depsUp, depsAll, svcUp, svcAll, tasks := m.counts()
	identity := []string{
		styleTitle.Render("devctl"),
		field("project", m.project()),
		field("root", tilde(m.root)),
		field("services", fmt.Sprintf("%d of %d running", svcUp, svcAll)),
	}
	if depsAll > 0 {
		identity = append(identity, field("deps", fmt.Sprintf("%d of %d reachable", depsUp, depsAll)))
	}
	if tasks > 0 {
		identity = append(identity, field("tasks", fmt.Sprintf("%d", tasks)))
	}

	// A column of keys. The chip is padded to the widest key in ITS OWN column,
	// so a long one like <pgup/dn> widens that column rather than running into
	// its own label.
	keyCol := func(items ...[2]string) []string {
		chip := 0
		for _, it := range items {
			chip = max(chip, len("<"+it[0]+">"))
		}
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = styleKey.Render(pad("<"+it[0]+">", chip+1)) + " " + styleMuted.Render(it[1])
		}
		return out
	}
	keysLeft := keyCol(
		[2]string{"s", "start / run / forward"},
		[2]string{"x", "stop"},
		[2]string{"r", "restart"},
		[2]string{"w", "auto-restart on/off"},
		[2]string{"m", "pick a dependency mode"},
	)
	keysRight := keyCol(
		[2]string{"d", "describe"},
		[2]string{"l", "logs, this row"},
		[2]string{"L", "logs, everything"},
		[2]string{"j/k", "move"},
		[2]string{"pgup/dn", "band above / below"},
		[2]string{"q", "quit"},
	)
	legend := []string{
		styleGood.Render("●") + styleMuted.Render(" listening"),
		styleMuted.Render("○ closed"),
		styleWarn.Render("*") + styleMuted.Render(" moved from its declared port"),
		styleMuted.Render("↻ watching for changes"),
		styleKey.Render("⇄") + styleMuted.Render(" has modes, m to switch"),
	}

	col := func(lines []string, w int) string {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = pad(clip(l, w), w)
		}
		return strings.Join(out, "\n")
	}
	// Every column is as wide as its own content plus a gutter, rather than a
	// number picked in advance: a label that grows widens its column instead of
	// being cut, which is what happened to "band above / below". The legend then
	// takes whatever is left, and drops entirely on a terminal too narrow to
	// hold it.
	const gap, legendMin = 3, 34
	widest := func(lines []string) int {
		w := 0
		for _, l := range lines {
			w = max(w, lipgloss.Width(l))
		}
		return w + gap
	}
	idW, leftW, rightW := widest(identity), widest(keysLeft), widest(keysRight)
	blocks := []string{col(identity, idW), col(keysLeft, leftW), col(keysRight, rightW)}
	if rest := width - idW - leftW - rightW; rest >= legendMin {
		blocks = append(blocks, col(legend, rest))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, blocks...) + "\n\n"
}

// View renders the header, one panel per kind, the log pane and the status bar,
// or the full-screen log view.
func (m Model) View() string {
	switch {
	case m.logView:
		return m.logViewString()
	case m.describe:
		return m.describeString()
	case m.modePick:
		return m.modePickString()
	}
	width := m.width
	if width <= 0 {
		width = 120
	}
	inner := width - 4
	if inner < 40 {
		inner = 40
	}

	var b strings.Builder
	b.WriteString(m.header(width))

	// One table, not three. A dependency, a service and a task are all things
	// that have to be up before the next one works, and splitting them into
	// panels made a reader carry the shape of three grids instead of one; the
	// TYPE column says which is which in less space than a section header.
	var (
		rows     [][]string
		selected []bool
		rules    = map[int]bool{}
		things   int
	)
	last := -1
	for i, r := range m.rows {
		// A rule where one band ends: the list is one table, but dependencies,
		// services and tasks are still three answers to three questions.
		if g := r.group(); last >= 0 && g != last {
			rules[len(rows)] = true
		} else if last < 0 {
			last = r.group()
		}
		last = r.group()
		label, style := r.status()
		status := style.Render(label)
		switch {
		case r.dep != nil:
			things++
			address := deps.Redact(r.address)
			if address == "" {
				address = "-"
			}
			// A multi-mode dependency wears a ⇄ and the mode that is live, so the
			// panel says both that it can switch and what it is pointed at now. The
			// glyph is in the key colour, tying it to the m that acts on it.
			if r.dep.HasModes() {
				address = styleKey.Render("⇄ ") + styleMuted.Render(r.activeModeName+" · ") + address
			}
			rows = append(rows, []string{r.cfg.Name, styleMuted.Render(m.kindOf(r)), status, address, r.restartCell(), "", styleMuted.Render(r.cfg.Description)})
		case r.task:
			things++
			rows = append(rows, []string{r.cfg.Name, styleMuted.Render("task"), status, "", r.restartCell(), "", styleMuted.Render(r.cfg.Description)})
		default:
			things++
			rows = append(rows, []string{r.cfg.Name, styleMuted.Render("service"), status, "", r.restartCell(), r.watchCell(), styleMuted.Render(r.cfg.Description)})
		}
		selected = append(selected, i == m.cursor)

		// A listener is a row of the same table rather than a line in its own
		// format: a service can expose several servers with different jobs, and
		// they are easier to compare down a column than along a line.
		for _, declared := range r.cfg.Ports {
			number := m.expand.Ports[r.cfg.Name][declared.Name]
			dot := styleMuted.Render("○")
			if r.portsOpen[number] {
				dot = styleGood.Render("●")
			}
			port := strconv.Itoa(number)
			if number != declared.Number {
				port += styleWarn.Render("*")
			}
			rows = append(rows, []string{
				"  " + dot + " " + styleMuted.Render(declared.Name),
				styleMuted.Render(declared.Kind),
				"",
				styleMuted.Render(port),
				"",
				"",
				styleMuted.Render(declared.Description),
			})
			selected = append(selected, false)
		}
	}

	cols := []col{
		{title: "NAME"},
		{title: "TYPE"},
		{title: "STATUS"},
		{title: "ENDPOINT"},
		{title: "RESTARTS", right: true},
		{title: "WATCH"},
		{title: "DESCRIPTION", flex: true},
	}
	body := table(cols, rows, selected, inner, nil)
	// table returns the header first, so a rule before row n lands at n+1.
	withRules := make([]boxRow, 0, len(body)+len(rules))
	for i, r := range body {
		if rules[i-1] {
			withRules = append(withRules, boxRow{styleRule.Render(strings.Repeat("─", inner)), false})
		}
		withRules = append(withRules, r)
	}
	b.WriteString(box(fmt.Sprintf("%s[%d]", m.project(), things), inner, withRules))

	b.WriteString(m.statusBar(width))
	return b.String()
}

// restartCell reads as nothing until something has come back, and then as a
// count worth noticing: a service restarting on its own is the first sign that
// something is wrong with it.
func (r *row) restartCell() string {
	n := r.restarts()
	if n == 0 {
		if r.starts == 0 {
			return ""
		}
		return styleMuted.Render("0")
	}
	return styleWarn.Render(strconv.Itoa(n))
}

// statusBar is the last line: which row the keys act on, then whatever the
// panel has to say. It is one line and stays one line; the warnings used to
// run off the right edge and get cut mid-word.
func (m Model) statusBar(width int) string {
	sel := m.rows[m.cursor]
	kind := "service"
	switch {
	case sel.dep != nil:
		kind = "dependency"
	case sel.task:
		kind = "task"
	}
	crumb := styleSelected.Render(" " + kind + " · " + sel.cfg.Name + " ")
	msg := m.message
	if msg == "" {
		msg = sel.cfg.Description
	}
	room := width - lipgloss.Width(crumb) - 2
	return crumb + " " + styleMuted.Render(clip(msg, room))
}

// modePickString is the mode picker: the dependency's modes with the one live
// marked and the source each points at. Choosing one switches the dependency and
// restarts what reads it.
func (m Model) modePickString() string {
	r, ok := m.byName[m.modeTarget]
	if !ok || r.dep == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(styleTitle.Render("switch mode") + styleMuted.Render("  ·  "+r.dep.Name) + "\n")
	if r.dep.Description != "" {
		b.WriteString(styleMuted.Render(r.dep.Description) + "\n")
	}
	b.WriteString("\n")

	nameW := 0
	for _, md := range r.dep.Modes {
		nameW = max(nameW, len(md.Name))
	}
	for i, md := range r.dep.Modes {
		cursor := "  "
		if i == m.modeCursor {
			cursor = styleCursor.Render("❯ ")
		}
		live := "    "
		if md.Name == r.activeModeName {
			live = styleGood.Render("live")
		}
		kind, detail := m.modeDetail(md)
		line := cursor +
			styleColumn.Render(pad(md.Name, nameW+2)) +
			pad(live, 6) +
			styleMuted.Render(pad(kind, 9)) +
			styleMuted.Render(detail)
		b.WriteString(clip(line, max(m.width-2, 40)) + "\n")
	}
	b.WriteString("\n" + styleMuted.Render("↑↓ move   1-9 jump   enter switch   esc cancel"))
	return b.String()
}

// modeDetail names a mode's source kind and what it points at, for the picker.
func (m Model) modeDetail(md config.Mode) (kind, detail string) {
	switch {
	case md.Peered():
		return "peer", md.Peer.ID + " · " + md.Peer.Service + "." + md.Peer.Port
	case md.Forwarded():
		cmd, err := m.expand.Expand(md.Forward.Cmd)
		if err != nil {
			cmd = md.Forward.Cmd
		}
		return "forward", cmd
	default:
		return "env", md.Env
	}
}

// logViewString is the full-screen log view: a title with whether the tail is
// being followed and whether long lines wrap, a tab bar of sources with the
// current one highlighted, the scrollable content, and the keys.
func (m Model) logViewString() string {
	state := "following"
	if !m.follow {
		state = fmt.Sprintf("scrolled, %d%%", int(m.viewport.ScrollPercent()*100))
	}
	tabs := make([]string, 0, len(m.rows)+1)
	for _, name := range m.logSources() {
		label := name
		if label == "" {
			label = "all"
		}
		if name == m.logFilter {
			tabs = append(tabs, styleSelected.Render(" "+label+" "))
		} else {
			tabs = append(tabs, styleMuted.Render(" "+label+" "))
		}
	}
	var b strings.Builder
	head := styleTitle.Render("logs") + styleMuted.Render("  ·  "+state)
	if m.wrap {
		head += styleMuted.Render("  ·  wrapped")
	}
	// The file behind the tail, when there is one: what is on screen is the last
	// 2000 lines, and the answer to "where is the rest" should not need asking.
	if r, ok := m.byName[m.logFilter]; ok {
		if path := m.logPath(r); path != "" {
			head += styleMuted.Render("  ·  " + tilde(path))
		}
	}
	b.WriteString(head + "\n")
	b.WriteString(strings.Join(tabs, " ") + "\n")
	b.WriteString(m.viewport.View() + "\n")
	b.WriteString(styleMuted.Render("←/→ or tab service  f follow  w wrap  ↑↓ pgup pgdn scroll  g/G top/bottom  esc or L back  q quit"))
	return b.String()
}
