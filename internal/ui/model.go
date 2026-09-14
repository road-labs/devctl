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
	// starts counts how many times this row's process has been launched, so
	// the table can say how many of those were restarts.
	starts int

	// Auto-restart: on for services with a watch list until toggled off.
	autoRestart bool
	watcher     *watch.Watcher
}

// Model is the Bubble Tea model.
type Model struct {
	root string
	// logs is the manifest's log directory, and logBytes its size cap already
	// parsed, so a start does not re-read a string every time.
	logs     *config.Logs
	logBytes int64

	expand  ports.Expander
	rows    []*row
	byName  map[string]*row
	cursor  int
	width   int
	height  int
	message string

	// The full-screen log view: every process's output merged in the order
	// it arrived, or one process's, scrollable, following the tail until
	// scrolled away from it.
	logView   bool
	describe  bool
	logFilter string // row name, or "" for every row
	follow    bool
	viewport  viewport.Model
}

// New builds the model from the manifest, the allocated port table and the
// resolved dependency addresses.
func New(root string, file *config.File, table ports.Table, values ports.Values, warnings []string) Model {
	m := Model{root: root, expand: ports.Expander{Ports: table, Values: values}, byName: map[string]*row{}, follow: true, logs: file.Logs}
	// An unparseable size is reported once, here, rather than on every start.
	size, err := file.Logs.Bytes()
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	m.logBytes = size
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
		switch {
		case m.logView:
			m.refreshLogs()
		case m.describe:
			m.viewport.SetContent(m.describeBody())
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
		// Only claim a restart if one happened. Where start refused, its own
		// message says why, and overwriting it here would leave a watched service
		// down with the panel insisting it had just come back.
		if r.proc != nil {
			m.message = fmt.Sprintf("%s: %s changed, restarted", r.cfg.Name, relPath(m.root, msg.path))
		}
		return m, tea.Batch(cmd, awaitChange(r))
	case tea.KeyMsg:
		switch {
		case m.logView:
			return m.handleLogKey(msg)
		case m.describe:
			return m.handleDescribeKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

// handleDescribeKey drives the describe view: scrolling, and the ways out.
func (m Model) handleDescribeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "d":
		m.describe = false
		return m, nil
	case "ctrl+c":
		m.stopAll()
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
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
	case "pgdown", "ctrl+d":
		m.cursor = m.nextGroup(1)
	case "pgup", "ctrl+u":
		m.cursor = m.nextGroup(-1)
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
		// The command is taken first, on purpose. start has a pointer receiver and
		// writes m.message; `return m, m.start(...)` lets the compiler copy m for
		// the return before the call runs, and a refusal would never reach the
		// status bar. Every mutating call on this path is written this way.
		cmd := m.start(sel, map[string]bool{})
		return m, cmd
	case "x":
		m.stop(sel)
	case "r":
		m.stopProcess(sel)
		cmd := m.start(sel, map[string]bool{})
		return m, cmd
	case "w":
		cmd := m.toggleWatch(sel)
		return m, cmd
	case "d":
		// Everything devctl knows about this row, and what it is wired to.
		m.describe = true
		m.viewport.Width = max(m.width, 40)
		m.viewport.Height = max(m.height-4, 3)
		m.viewport.SetContent(m.describeBody())
		m.viewport.GotoTop()
	case "l":
		// This row's own output, whether or not it has any yet: opening an empty
		// log is an answer too.
		m.openLogs(sel.cfg.Name)
	case "L":
		// Everything, merged in the order it arrived.
		m.openLogs("")
	}
	return m, nil
}

// group is which of the three bands a row sits in. The single table keeps them
// in this order, with a rule drawn where one ends.
func (r *row) group() int {
	switch {
	case r.dep != nil:
		return 0
	case !r.task:
		return 1
	}
	return 2
}

// nextGroup is the first row of the band above or below the cursor's, which is
// what page up and page down do here: a list this short does not need paging,
// but jumping between dependencies, services and tasks is worth a key.
func (m Model) nextGroup(step int) int {
	here := m.rows[m.cursor].group()
	if step > 0 {
		for i := m.cursor + 1; i < len(m.rows); i++ {
			if m.rows[i].group() != here {
				return i
			}
		}
		return len(m.rows) - 1
	}
	// Up: the first row of the band before this one, which means finding its
	// start rather than its end.
	first := m.cursor
	for first > 0 && m.rows[first-1].group() == here {
		first--
	}
	if first == 0 {
		return 0
	}
	prev := m.rows[first-1].group()
	for first > 0 && m.rows[first-1].group() == prev {
		first--
	}
	return first
}

// logPath is where a row's output is appended, or "" when the manifest declares
// no log directory. One file per row, named for it, so a reader can open the
// one they want without knowing what devctl called the run.
func (m Model) logPath(r *row) string {
	if m.logs == nil || m.logs.Dir == "" {
		return ""
	}
	return filepath.Join(m.root, m.logs.Dir, r.cfg.Name+".log")
}

// envFor is the environment a row's process is given, and it is where devctl's
// whole wiring model lands. Two sources, in this order:
//
//  1. The service's own listeners. `listen` maps a port NAME to the variable
//     the process reads for that listener, so the process is told where to bind
//     rather than choosing. devctl has already allocated the number by now.
//  2. The service's declared `env`, with every {{ reference }} resolved: another
//     service's port becomes localhost:<port>, a machine-provided dependency
//     becomes its address from .env, a forwarded one becomes its local tunnel.
//
// The point of doing it this way is that a port is written down once, in the
// manifest, and everything else is derived. devctl allocates at start, so a
// default that something else on the machine is holding is quietly replaced by
// a free one; every consumer of that port reads the new number through the same
// table, in the same pass, without anyone editing anything. That is why a
// developer's .env holds only what the machine provides and no ports at all,
// and why two services can never disagree about where a third one is.
//
// Describe renders exactly this map, which is why start no longer builds it
// inline: a panel that showed its own guess at the environment would be worth
// less than nothing on the day the two drifted apart.
func (m Model) envFor(r *row) (map[string]string, error) {
	env := map[string]string{}
	for name, listen := range r.cfg.Listen {
		env[listen.Env] = ports.Listen(m.expand.Ports[r.cfg.Name][name], listen.Format)
	}
	for k, v := range r.cfg.Env {
		expanded, err := m.expand.Expand(v)
		if err != nil {
			return nil, err
		}
		env[k] = expanded
	}
	return env, nil
}

// openLogs switches to the full-screen log view, showing one row's output or
// every row's.
func (m *Model) openLogs(filter string) {
	m.logFilter = filter
	m.follow = true
	m.logView = true
	m.viewport.Width = max(m.width, 40)
	m.viewport.Height = max(m.height-5, 3)
	m.refreshLogs()
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

	env, err := m.envFor(r)
	if err != nil {
		m.message = fmt.Sprintf("%s: %v", r.cfg.Name, err)
		return tea.Batch(cmds...)
	}
	command, err := m.expand.Expand(r.cfg.Cmd)
	if err != nil {
		m.message = fmt.Sprintf("%s: %v", r.cfg.Name, err)
		return tea.Batch(cmds...)
	}

	p, err := proc.Start(proc.Spec{
		Command:    command,
		Dir:        filepath.Join(m.root, r.cfg.Dir),
		Env:        env,
		LogFile:    m.logPath(r),
		LogMaxSize: m.logBytes,
	})
	if err != nil {
		m.message = fmt.Sprintf("%s: %v", r.cfg.Name, err)
		return tea.Batch(cmds...)
	}
	r.starts++
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
	if r.proc != nil {
		if err := r.proc.Stop(stopTimeout); err != nil {
			m.message = fmt.Sprintf("%s: %v", r.cfg.Name, err)
		}
		r.proc = nil
	}
	// The probe runs twice a second, so whatever it last saw is from before the
	// process exited. start's preflight reads exactly this map, and a restart
	// would refuse over the row's own port, which it had just released. Clearing
	// leaves the preflight to ask the operating system, and the next probe
	// refills it.
	clear(r.portsOpen)
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

// restarts is how many times the process came back after its first start,
// whether by hand, by a watched file changing, or after a crash.
func (r *row) restarts() int {
	if r.starts == 0 {
		return 0
	}
	return r.starts - 1
}

// watchCell says whether the service restarts itself when a watched file
// changes. Blank where nothing is watched: a column of "off" on rows that were
// never going to restart says nothing. Autostart is not here, because it is a
// manifest fact that has already happened by the time anyone reads the table.
func (r *row) watchCell() string {
	switch {
	case len(r.cfg.Watch) == 0:
		return ""
	case r.autoRestart:
		return styleGood.Render("on")
	default:
		return styleMuted.Render("off")
	}
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
