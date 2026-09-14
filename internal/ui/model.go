// Package ui is the devctl terminal interface: the dependencies a run needs
// from outside the repository, one row per service with its status and
// ports, the one-shot tasks, and keys to start, stop, restart and read logs.
// Services with a watch list restart themselves when their Go source changes.
package ui

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/road-labs/devctl/internal/config"
	"github.com/road-labs/devctl/internal/deps"
	"github.com/road-labs/devctl/internal/ports"
	"github.com/road-labs/devctl/internal/proc"
	"github.com/road-labs/devctl/internal/watch"
)

const (
	tickEvery     = 500 * time.Millisecond
	probeTimeout  = 150 * time.Millisecond
	stopTimeout   = 5 * time.Second
	watchDebounce = 400 * time.Millisecond
	// logLines is how much of each process's output the full log view merges.
	logLines = 2000
)

// row is a dependency, a service or a task. All three are started the same
// way when they have a command; they differ in what their status means.
type row struct {
	cfg  config.Service // synthesised for tasks and forwarded dependencies
	task bool
	dep  *config.Dependency

	// Dependency state: the address in use, what to dial to see if it
	// answers, and the last probe result.
	address   string
	hostPort  string
	reachable bool
	probed    bool

	proc      *proc.Process
	portsOpen map[int]bool

	// Auto-restart: on for services with a watch list until toggled off.
	autoRestart bool
	watcher     *watch.Watcher
}

// Model is the Bubble Tea model.
type Model struct {
	root     string
	expand   ports.Expander
	rows     []*row
	byName   map[string]*row
	cursor   int
	showLogs bool
	width    int
	height   int
	message  string

	// The full-screen log view: every process's output merged in the order
	// it arrived, or one process's, scrollable, following the tail until
	// scrolled away from it.
	logView   bool
	logFilter string // row name, or "" for every row
	follow    bool
	viewport  viewport.Model
}

// New builds the model from the manifest, the allocated port table and the
// resolved dependency addresses.
func New(root string, file *config.File, table ports.Table, values ports.Values, warnings []string) Model {
	m := Model{root: root, expand: ports.Expander{Ports: table, Values: values}, byName: map[string]*row{}, showLogs: true, follow: true}
	m.viewport = viewport.New(80, 20)
	// Scrolling keys only; f, b and space are ours.
	m.viewport.KeyMap = viewport.KeyMap{
		Up:           key.NewBinding(key.WithKeys("up", "k")),
		Down:         key.NewBinding(key.WithKeys("down", "j")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
	}
	if len(warnings) > 0 {
		m.message = strings.Join(warnings, "; ")
	}
	add := func(r *row) {
		r.portsOpen = map[int]bool{}
		m.rows = append(m.rows, r)
		m.byName[r.cfg.Name] = r
	}
	for i := range file.Dependencies {
		dep := &file.Dependencies[i]
		r := &row{dep: dep, cfg: config.Service{Name: dep.Name, Description: dep.Description}}
		if dep.Forwarded() {
			port := table[dep.Name][ports.ForwardPort]
			r.address = "localhost:" + strconv.Itoa(port)
			r.hostPort = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			r.cfg.Cmd, r.cfg.Dir = dep.Forward.Cmd, dep.Forward.Dir
		} else {
			r.address = values[deps.Key(*dep)]
			r.hostPort, _ = deps.HostPort(r.address)
		}
		add(r)
	}
	for _, svc := range file.Services {
		add(&row{cfg: svc, autoRestart: len(svc.Watch) > 0})
	}
	for _, task := range file.Tasks {
		add(&row{task: true, cfg: config.Service{
			Name: task.Name, Description: task.Description, DependsOn: task.DependsOn,
			Dir: task.Dir, Cmd: task.Cmd, Env: task.Env,
		}})
	}
	return m
}

type tickMsg time.Time

type probeMsg struct {
	ports map[string]map[int]bool
	deps  map[string]bool
}

// changedMsg says a watched service's source changed.
type changedMsg struct {
	name string
	path string
}

// Shutdown asks the panel to stop everything it started and quit. main sends
// it on SIGHUP, SIGINT and SIGTERM, so a closed terminal does not leave the
// children running in their own process groups.
type Shutdown struct{}

func tick() tea.Cmd {
	return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// awaitChange waits for the row's watcher to report a change.
func awaitChange(r *row) tea.Cmd {
	w := r.watcher
	if w == nil {
		return nil
	}
	return func() tea.Msg {
		path, ok := <-w.C
		if !ok {
			return nil
		}
		return changedMsg{name: r.cfg.Name, path: path}
	}
}

// probe dials every declared port and every dependency concurrently.
func (m Model) probe() tea.Cmd {
	type target struct {
		name string
		addr string
		port int
	}
	var targets []target
	for _, r := range m.rows {
		if r.dep != nil {
			if r.hostPort != "" {
				targets = append(targets, target{name: r.cfg.Name, addr: r.hostPort})
			}
			continue
		}
		for _, p := range m.expand.Ports[r.cfg.Name] {
			targets = append(targets, target{name: r.cfg.Name, addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(p)), port: p})
		}
	}
	return func() tea.Msg {
		results := probeMsg{ports: map[string]map[int]bool{}, deps: map[string]bool{}}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, t := range targets {
			wg.Add(1)
			go func(t target) {
				defer wg.Done()
				conn, err := net.DialTimeout("tcp", t.addr, probeTimeout)
				open := err == nil
				if conn != nil {
					_ = conn.Close()
				}
				mu.Lock()
				defer mu.Unlock()
				if t.port == 0 {
					results.deps[t.name] = open
					return
				}
				if results.ports[t.name] == nil {
					results.ports[t.name] = map[int]bool{}
				}
				results.ports[t.name][t.port] = open
			}(t)
		}
		wg.Wait()
		return results
	}
}

// Init starts the clock, the first probe, and the autostart set.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tick(), m.probe()}
	for _, r := range m.rows {
		if r.cfg.Autostart {
			cmds = append(cmds, m.start(r, map[string]bool{}))
		}
	}
	return tea.Batch(cmds...)
}

// Update handles keys, ticks, probe results and source changes.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = max(msg.Height-5, 3)
		if m.logView {
			m.refreshLogs()
		}
		return m, nil
	case tickMsg:
		if m.logView {
			m.refreshLogs()
		}
		return m, tea.Batch(tick(), m.probe())
	case probeMsg:
		for name, open := range msg.ports {
			if r, ok := m.byName[name]; ok {
				r.portsOpen = open
			}
		}
		for name, reachable := range msg.deps {
			if r, ok := m.byName[name]; ok {
				r.reachable, r.probed = reachable, true
			}
		}
		return m, nil
	case Shutdown:
		m.stopAll()
		return m, tea.Quit
	case changedMsg:
		r, ok := m.byName[msg.name]
		if !ok || r.watcher == nil {
			return m, nil
		}
		m.stopProcess(r)
		cmd := m.start(r, map[string]bool{})
		m.message = fmt.Sprintf("%s: %s changed, restarted", r.cfg.Name, relPath(m.root, msg.path))
		return m, tea.Batch(cmd, awaitChange(r))
	case tea.KeyMsg:
		if m.logView {
			return m.handleLogKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

// handleLogKey drives the full-screen log view.
func (m Model) handleLogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.stopAll()
		return m, tea.Quit
	case "esc", "L":
		m.logView = false
		return m, nil
	case "tab", "right", "l":
		m.moveLogFilter(1)
		return m, nil
	case "shift+tab", "left", "h":
		m.moveLogFilter(-1)
		return m, nil
	case "f":
		m.follow = !m.follow
		if m.follow {
			m.viewport.GotoBottom()
		}
		return m, nil
	case "g", "home":
		m.follow = false
		m.viewport.GotoTop()
		return m, nil
	case "G", "end":
		m.follow = true
		m.viewport.GotoBottom()
		return m, nil
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	// Scrolling away from the tail stops following; reaching it resumes.
	m.follow = m.viewport.AtBottom()
	return m, cmd
}

// logSources is the tab bar of the log view: every service first, then each
// row that has produced output, in panel order.
func (m *Model) logSources() []string {
	names := []string{""}
	for _, r := range m.rows {
		if r.proc != nil {
			names = append(names, r.cfg.Name)
		}
	}
	return names
}

// moveLogFilter steps the tab bar left or right, wrapping round, and
// follows the tail of whatever it lands on.
func (m *Model) moveLogFilter(step int) {
	names := m.logSources()
	current := 0
	for i, name := range names {
		if name == m.logFilter {
			current = i
			break
		}
	}
	m.logFilter = names[(current+step+len(names))%len(names)]
	m.follow = true
	m.refreshLogs()
}

// refreshLogs rebuilds the viewport's content from the processes' rings.
func (m *Model) refreshLogs() {
	type tagged struct {
		seq  uint64
		name string
		text string
	}
	var lines []tagged
	nameW := 0
	for _, r := range m.rows {
		if r.proc == nil || (m.logFilter != "" && r.cfg.Name != m.logFilter) {
			continue
		}
		nameW = max(nameW, len(r.cfg.Name))
		for _, l := range r.proc.TailLines(logLines) {
			lines = append(lines, tagged{seq: l.Seq, name: r.cfg.Name, text: l.Text})
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].seq < lines[j].seq })

	var b strings.Builder
	for _, l := range lines {
		if m.logFilter == "" {
			b.WriteString(styleMuted.Render(fmt.Sprintf("%-*s ", nameW, l.name)))
		}
		b.WriteString(l.text)
		b.WriteString("\n")
	}
	m.viewport.SetContent(b.String())
	if m.follow {
		m.viewport.GotoBottom()
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sel := m.rows[m.cursor]
	switch msg.String() {
	case "q", "ctrl+c":
		m.stopAll()
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "s", "enter":
		if sel.dep != nil && !sel.dep.Forwarded() {
			m.message = fmt.Sprintf("%s is provided by the machine: set %s in .env", sel.cfg.Name, sel.dep.Env)
			return m, nil
		}
		if sel.task && sel.proc != nil {
			// A finished task can be run again; a running one is left alone.
			if state, _ := sel.proc.State(); state == proc.StateExited {
				sel.proc = nil
			}
		}
		return m, m.start(sel, map[string]bool{})
	case "x":
		m.stop(sel)
	case "r":
		m.stopProcess(sel)
		return m, m.start(sel, map[string]bool{})
	case "w":
		return m, m.toggleWatch(sel)
	case "a":
		var cmds []tea.Cmd
		for _, r := range m.rows {
			if r.cfg.Autostart {
				cmds = append(cmds, m.start(r, map[string]bool{}))
			}
		}
		return m, tea.Batch(cmds...)
	case "l":
		m.showLogs = !m.showLogs
	case "L":
		// Full-screen logs, opened on the selected row when it has output.
		m.logFilter = ""
		if sel.proc != nil {
			m.logFilter = sel.cfg.Name
		}
		m.follow = true
		m.logView = true
		m.viewport.Width = max(m.width, 40)
		m.viewport.Height = max(m.height-5, 3)
		m.refreshLogs()
	}
	return m, nil
}

// start launches a row, starting the services it depends on first. It refuses
// to start over ports something else already holds. For a watched service it
// also opens the watcher and returns the command that waits for a change.
func (m *Model) start(r *row, visiting map[string]bool) tea.Cmd {
	if visiting[r.cfg.Name] {
		return nil
	}
	visiting[r.cfg.Name] = true

	if r.running() || (r.dep != nil && !r.dep.Forwarded()) {
		return nil
	}
	// Preflight: every port the service will bind must be free right now, not
	// just at the last probe, or the process would die on EADDRINUSE.
	if r.dep == nil {
		for name, port := range m.expand.Ports[r.cfg.Name] {
			if r.portsOpen[port] || !ports.IsFree(port) {
				m.message = fmt.Sprintf("%s: port %s (%d) is already in use%s, start refused", r.cfg.Name, name, port, ports.Holder(port))
				return nil
			}
		}
	}
	var cmds []tea.Cmd
	for _, dep := range r.cfg.DependsOn {
		if d, ok := m.byName[dep]; ok && !d.running() && !d.task {
			cmds = append(cmds, m.start(d, visiting))
		}
	}

	// Listeners get ":<port>" or the bare number; references in env and cmd
	// resolve through the port table and dependency addresses, so no address
	// is written anywhere but the manifest and .env.
	env := map[string]string{}
	for name, listen := range r.cfg.Listen {
		env[listen.Env] = ports.Listen(m.expand.Ports[r.cfg.Name][name], listen.Format)
	}
	for k, v := range r.cfg.Env {
		expanded, err := m.expand.Expand(v)
		if err != nil {
			m.message = fmt.Sprintf("%s: %v", r.cfg.Name, err)
			return tea.Batch(cmds...)
		}
		env[k] = expanded
	}
	command, err := m.expand.Expand(r.cfg.Cmd)
	if err != nil {
		m.message = fmt.Sprintf("%s: %v", r.cfg.Name, err)
		return tea.Batch(cmds...)
	}

	p, err := proc.Start(proc.Spec{
		Command: command,
		Dir:     filepath.Join(m.root, r.cfg.Dir),
		Env:     env,
	})
	if err != nil {
		m.message = fmt.Sprintf("%s: %v", r.cfg.Name, err)
		return tea.Batch(cmds...)
	}
	r.proc = p
	switch {
	case r.task:
		m.message = fmt.Sprintf("%s: running, pid %d", r.cfg.Name, p.PID())
	case r.dep != nil:
		m.message = fmt.Sprintf("%s: forwarding to %s, pid %d", r.cfg.Name, r.address, p.PID())
	default:
		m.message = fmt.Sprintf("%s: started, pid %d", r.cfg.Name, p.PID())
	}

	if r.autoRestart && r.watcher == nil && len(r.cfg.Watch) > 0 {
		w, err := watch.New(m.root, r.cfg.Watch, watchDebounce)
		if err != nil {
			m.message = fmt.Sprintf("%s: started, but not watching: %v", r.cfg.Name, err)
		} else {
			r.watcher = w
			cmds = append(cmds, awaitChange(r))
		}
	}
	return tea.Batch(cmds...)
}

// stopProcess ends the process but keeps watching, for restarts.
func (m *Model) stopProcess(r *row) {
	if r.proc == nil {
		return
	}
	if err := r.proc.Stop(stopTimeout); err != nil {
		m.message = fmt.Sprintf("%s: %v", r.cfg.Name, err)
	}
	r.proc = nil
}

// stop ends the process and the watcher: the user asked for it to be down.
func (m *Model) stop(r *row) {
	had := r.proc != nil
	m.stopProcess(r)
	m.closeWatcher(r)
	if had {
		m.message = fmt.Sprintf("%s: stopped", r.cfg.Name)
	}
}

func (m *Model) closeWatcher(r *row) {
	if r.watcher != nil {
		_ = r.watcher.Close()
		r.watcher = nil
	}
}

// toggleWatch turns auto-restart on or off for a watched service.
func (m *Model) toggleWatch(r *row) tea.Cmd {
	if len(r.cfg.Watch) == 0 {
		m.message = fmt.Sprintf("%s has no watch list", r.cfg.Name)
		return nil
	}
	r.autoRestart = !r.autoRestart
	if !r.autoRestart {
		m.closeWatcher(r)
		m.message = fmt.Sprintf("%s: auto-restart off", r.cfg.Name)
		return nil
	}
	m.message = fmt.Sprintf("%s: auto-restart on", r.cfg.Name)
	if r.running() && r.watcher == nil {
		w, err := watch.New(m.root, r.cfg.Watch, watchDebounce)
		if err != nil {
			m.message = fmt.Sprintf("%s: not watching: %v", r.cfg.Name, err)
			return nil
		}
		r.watcher = w
		return awaitChange(r)
	}
	return nil
}

func (m *Model) stopAll() {
	var wg sync.WaitGroup
	for _, r := range m.rows {
		m.closeWatcher(r)
		if r.proc == nil {
			continue
		}
		wg.Add(1)
		go func(p *proc.Process) {
			defer wg.Done()
			_ = p.Stop(stopTimeout)
		}(r.proc)
		r.proc = nil
	}
	wg.Wait()
}

func (r *row) running() bool {
	if r.proc == nil {
		return false
	}
	state, _ := r.proc.State()
	return state == proc.StateRunning
}

// status is the state shown in the table, with the uptime while something
// is running.
func (r *row) status() (label string, style lipgloss.Style) {
	up := func(label string) string {
		if r.proc == nil {
			return label
		}
		return label + " " + shortDuration(r.proc.Uptime())
	}
	switch {
	case r.dep != nil:
		return r.depStatus(up)
	case r.task:
		if r.proc == nil {
			return "idle", styleMuted
		}
		state, code := r.proc.State()
		switch {
		case state == proc.StateRunning:
			return up("running"), styleWarn
		case code == 0:
			return "done", styleGood
		default:
			return fmt.Sprintf("failed (%d)", code), styleBad
		}
	}
	if r.proc == nil {
		if r.anyPortOpen() {
			return "external", styleWarn
		}
		return "stopped", styleMuted
	}
	state, code := r.proc.State()
	if state == proc.StateExited {
		if code == 0 {
			return "exited", styleMuted
		}
		return fmt.Sprintf("failed (%d)", code), styleBad
	}
	if r.allPortsOpen() {
		return up("running"), styleGood
	}
	return up("starting"), styleWarn
}

// depStatus: a machine-provided dependency is configured or not and answers
// or not; a forwarded one also has the tunnel process to account for.
func (r *row) depStatus(up func(string) string) (string, lipgloss.Style) {
	unreachable := styleBad
	if r.dep.Optional {
		unreachable = styleWarn
	}
	if !r.dep.Forwarded() {
		switch {
		case r.address == "":
			return "not configured", styleMuted
		case !r.probed:
			return "configured", styleMuted
		case r.reachable:
			return "reachable", styleGood
		default:
			return "unreachable", unreachable
		}
	}
	if r.proc != nil {
		state, code := r.proc.State()
		switch {
		case state == proc.StateRunning && r.reachable:
			return up("forwarded"), styleGood
		case state == proc.StateRunning:
			return up("connecting"), styleWarn
		case code == 0:
			return "exited", styleMuted
		default:
			return fmt.Sprintf("failed (%d)", code), styleBad
		}
	}
	if r.probed && r.reachable {
		return "reachable", styleGood
	}
	return "down", styleMuted
}

func (r *row) allPortsOpen() bool {
	if len(r.portsOpen) == 0 {
		return false
	}
	for _, open := range r.portsOpen {
		if !open {
			return false
		}
	}
	return true
}

func (r *row) anyPortOpen() bool {
	for _, open := range r.portsOpen {
		if open {
			return true
		}
	}
	return false
}

// shortDuration renders an uptime compactly: 12s, 4m12s, 1h02m.
func shortDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func relPath(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleMuted  = lipgloss.NewStyle().Faint(true)
	styleGood   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleCursor = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleHeader = lipgloss.NewStyle().Faint(true).Underline(true)
)

const statusW = 20

// View renders the dependencies, the service table, the tasks, the log pane
// and the key help, or the full-screen log view.
func (m Model) View() string {
	if m.logView {
		return m.logViewString()
	}
	width := m.width
	if width <= 0 {
		width = 120
	}
	clip := func(s string, room int) string {
		if room > 10 && len(s) > room {
			return s[:room-1] + "…"
		}
		return s
	}
	var b strings.Builder
	b.WriteString(styleTitle.Render("devctl") + styleMuted.Render("  platform local running  ·  "+m.root) + "\n\n")

	nameW := 8
	for _, r := range m.rows {
		nameW = max(nameW, len(r.cfg.Name))
	}
	line := func(cursor, name, status, desc string) string {
		return fmt.Sprintf("%s %-*s  %-*s  %s", cursor, nameW, name, statusW, status, desc)
	}
	room := width - (nameW + statusW + 6)
	lines := 0
	section := ""
	for i, r := range m.rows {
		// Section headers as the row kind changes: dependencies, services, tasks.
		want := "service"
		switch {
		case r.dep != nil:
			want = "dependency"
		case r.task:
			want = "task"
		}
		if want != section {
			if section != "" {
				b.WriteString("\n")
				lines++
			}
			section = want
			switch section {
			case "dependency":
				b.WriteString(styleHeader.Render(line(" ", "DEPENDENCY", "STATUS", "ADDRESS")) + "\n")
			case "service":
				b.WriteString(styleHeader.Render(line(" ", "SERVICE", "STATUS", "DESCRIPTION")) + "\n")
			default:
				b.WriteString(styleHeader.Render(line(" ", "TASK", "STATUS", "DESCRIPTION")) + "\n")
			}
			lines++
		}

		cursor := " "
		if i == m.cursor {
			cursor = styleCursor.Render("▶")
		}
		label, style := r.status()
		status := style.Render(fmt.Sprintf("%-*s", statusW, label))
		switch {
		case r.dep != nil:
			kind := r.dep.Kind
			if r.dep.Forwarded() {
				kind = "forward"
			}
			address := deps.Redact(r.address)
			if address == "" {
				address = "-"
			}
			head := address + "  " + kind
			desc := head + "  " + styleMuted.Render(clip(r.cfg.Description, room-len(head)-2))
			b.WriteString(line(cursor, r.cfg.Name, status, desc) + "\n")
		default:
			desc := clip(r.cfg.Description, room)
			if len(r.cfg.Watch) > 0 {
				marker := "↻ "
				if !r.autoRestart {
					marker = "↻ off "
				}
				desc = styleMuted.Render(marker) + styleMuted.Render(clip(r.cfg.Description, room-len(marker)))
			} else {
				desc = styleMuted.Render(desc)
			}
			b.WriteString(line(cursor, r.cfg.Name, status, desc) + "\n")
		}
		lines++

		// One line per listener: a service can expose several servers with
		// different jobs, so each shows its own kind, number and state.
		for _, declared := range r.cfg.Ports {
			number := m.expand.Ports[r.cfg.Name][declared.Name]
			dot := styleMuted.Render("○")
			if r.portsOpen[number] {
				dot = styleGood.Render("●")
			}
			moved := " "
			if number != declared.Number {
				moved = styleWarn.Render("*")
			}
			fmt.Fprintf(&b, "      %s %-8s %-4s %5d%s  %s\n", dot, declared.Name, styleMuted.Render(declared.Kind), number, moved, styleMuted.Render(clip(declared.Description, width-34)))
			lines++
		}
	}

	if m.showLogs {
		sel := m.rows[m.cursor]
		b.WriteString("\n")
		title := "logs · " + sel.cfg.Name
		switch {
		case sel.task:
			title = "output · " + sel.cfg.Name
		case sel.dep != nil:
			title = sel.cfg.Name + " · " + sel.cfg.Description
		}
		if sel.proc != nil {
			title += fmt.Sprintf(" · pid %d", sel.proc.PID())
		}
		b.WriteString(styleHeader.Render(title) + "\n")
		avail := m.height - lines - 9
		if avail < 5 {
			avail = 5
		}
		switch {
		case sel.proc != nil:
			for _, l := range sel.proc.Tail(avail) {
				if len(l) > width-1 {
					l = l[:width-2] + "…"
				}
				b.WriteString(l + "\n")
			}
		case sel.dep != nil && !sel.dep.Forwarded():
			b.WriteString(styleMuted.Render(fmt.Sprintf("provided by the machine; %s from .env or the environment", sel.dep.Env)) + "\n")
		case sel.dep != nil:
			forward, err := m.expand.Expand(sel.dep.Forward.Cmd)
			if err != nil {
				forward = sel.dep.Forward.Cmd
			}
			b.WriteString(styleMuted.Render("not forwarded; s runs: "+forward) + "\n")
		default:
			b.WriteString(styleMuted.Render("not started") + "\n")
		}
	}

	b.WriteString("\n" + styleMuted.Render("s start/run/forward  x stop  r restart  w auto-restart on/off  a autostart set  l log pane  L full logs  j/k move  q quit    ● listening  ○ closed  * moved  ↻ watching"))
	if m.message != "" {
		b.WriteString("\n" + m.message)
	}
	return b.String()
}

// logViewString is the full-screen log view: a title with whether the tail is
// being followed, a tab bar of sources with the current one highlighted, the
// scrollable content, and the keys.
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
			tabs = append(tabs, styleCursor.Render("["+label+"]"))
		} else {
			tabs = append(tabs, styleMuted.Render(" "+label+" "))
		}
	}
	var b strings.Builder
	b.WriteString(styleTitle.Render("logs") + styleMuted.Render("  ·  "+state) + "\n")
	b.WriteString(strings.Join(tabs, " ") + "\n")
	b.WriteString(m.viewport.View() + "\n")
	b.WriteString(styleMuted.Render("←/→ or tab service  f follow  ↑↓ pgup pgdn scroll  g/G top/bottom  esc or L back  q quit"))
	return b.String()
}
