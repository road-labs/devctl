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
	"github.com/charmbracelet/x/ansi"

	"github.com/road-labs/devctl/internal/config"
	"github.com/road-labs/devctl/internal/deps"
	"github.com/road-labs/devctl/internal/peer"
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

	// activeModeName is the live mode of a multi-mode dependency, "" for a
	// single-source one. It decides where the dependency's address comes from
	// and is cycled by m.
	activeModeName string

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

	expand ports.Expander
	// peerPort is the sibling port devctl last read for each dependency whose
	// active mode peers to another devctl, 0 while that sibling is not running.
	peerPort map[string]int
	// peerProvides is the config the peered service published, per dependency,
	// inherited by whatever depends on it.
	peerProvides map[string]map[string]string
	rows         []*row
	byName       map[string]*row
	cursor       int
	width        int
	height       int
	message      string

	// The full-screen log view: every process's output merged in the order
	// it arrived, or one process's, scrollable, following the tail until
	// scrolled away from it. Lines wider than the terminal are cut at its
	// edge until wrap folds them; that is kept for the session, not per row.
	logView   bool
	describe  bool
	logFilter string // row name, or "" for every row
	follow    bool
	wrap      bool
	viewport  viewport.Model

	// The mode picker: open on a multi-mode dependency, modeTarget names it and
	// modeCursor is the highlighted mode.
	modePick   bool
	modeTarget string
	modeCursor int
}

// New builds the model from the manifest, the allocated port table and the
// resolved dependency addresses.
func New(root string, file *config.File, table ports.Table, values ports.Values, warnings []string) Model {
	m := Model{root: root, expand: ports.Expander{Ports: table, Values: values}, peerPort: map[string]int{}, peerProvides: map[string]map[string]string{}, byName: map[string]*row{}, follow: true, logs: file.Logs}
	// Load has already refused an unparseable size, so this cannot fail here.
	m.logBytes, _ = file.Logs.Bytes()
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
		r := &row{dep: dep, cfg: config.Service{Name: dep.Name, Description: dep.Description, Autostart: dep.Autostart}, activeModeName: dep.DefaultMode()}
		add(r)
		// A dependency peered with a sibling is read now, so a sibling already up
		// shows as peered from the first frame rather than after the first probe.
		m.readPeer(r)
		m.applyMode(r)
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

// mode returns a dependency row's active source. For a single-source dependency
// it is that one source; for a multi-mode one it is whichever mode is live.
func (r *row) mode() config.Mode { return r.dep.Mode(r.activeModeName) }

// applyMode makes a dependency row reflect its active mode: where its address
// comes from, whether there is a tunnel to run, and, for a multi-mode
// dependency, the {{ name.address }} every consumer resolves through. It is the
// one place a mode's kind is turned into an address, so New, a switch and a peer
// re-read all go through it.
func (m *Model) applyMode(r *row) {
	if r.dep == nil {
		return
	}
	mode := r.mode()
	switch {
	case mode.Forwarded():
		port := m.expand.Ports[r.dep.Name][ports.ForwardPort]
		r.address = "localhost:" + strconv.Itoa(port)
		r.hostPort = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		r.cfg.Cmd, r.cfg.Dir = mode.Forward.Cmd, mode.Forward.Dir
	case mode.Peered():
		r.cfg.Cmd, r.cfg.Dir = "", ""
		if port := m.peerPort[r.dep.Name]; port != 0 {
			r.address = "localhost:" + strconv.Itoa(port)
			r.hostPort = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			// A single-source peer is referenced as {{ name.port }}, resolved from
			// the port table like a forward's.
			if !r.dep.HasModes() {
				m.expand.Ports[r.dep.Name] = map[string]int{ports.ForwardPort: port}
			}
		} else {
			r.address, r.hostPort = "", ""
			if !r.dep.HasModes() {
				delete(m.expand.Ports, r.dep.Name)
			}
		}
	default: // machine-provided
		r.cfg.Cmd, r.cfg.Dir = "", ""
		r.address = m.envModeValue(r.dep, mode)
		r.hostPort, _ = deps.HostPort(r.address)
	}
	if r.dep.HasModes() {
		m.expand.Values[r.dep.Name+".address"] = r.address
	}
}

// envModeValue is the machine-provided address of a mode, resolved earlier from
// the environment or .env.
func (m Model) envModeValue(dep *config.Dependency, mode config.Mode) string {
	if dep.HasModes() {
		return m.expand.Values[deps.ModeKey(dep.Name, mode.Name)]
	}
	return m.expand.Values[deps.Key(*dep)]
}

// readPeer reads the sibling a dependency's active mode peers to, recording the
// port it publishes or 0 when it is not running. A no-op for any other mode.
func (m *Model) readPeer(r *row) {
	if r.dep == nil {
		return
	}
	mode := r.mode()
	if !mode.Peered() {
		return
	}
	port := 0
	var provides map[string]string
	if snap, running, _ := peer.Read(mode.Peer.ID); running {
		if p, ok := snap.Port(mode.Peer.Service, mode.Peer.Port); ok {
			port = p
		}
		provides = snap.ProvidesFor(mode.Peer.Service)
	}
	m.peerPort[r.dep.Name] = port
	m.peerProvides[r.dep.Name] = provides
}

// chooseMode makes name the active mode of a dependency and restarts the running
// services that read it, so they come back pointed at the new source. Choosing
// the mode already live does nothing.
func (m *Model) chooseMode(r *row, name string) tea.Cmd {
	if r.dep == nil || name == "" || name == r.activeModeName {
		return nil
	}
	// A forward tunnel belongs to the mode it opened; leaving that mode stops it.
	if r.proc != nil {
		m.stopProcess(r)
	}
	r.activeModeName = name
	m.readPeer(r)
	m.applyMode(r)
	m.message = fmt.Sprintf("%s → %s, %s", r.dep.Name, r.activeModeName, describeMode(r.mode()))

	cmds, restarted := m.restartConsumers(r.dep.Name)
	if len(restarted) > 0 {
		m.message += "; restarted " + strings.Join(restarted, ", ")
	}
	return tea.Batch(cmds...)
}

// restartConsumers restarts the running services that read a dependency, so they
// come back with its current address and provided config. The reason to do it
// on a mode switch, and when a peer resolves after those services have started.
func (m *Model) restartConsumers(depName string) ([]tea.Cmd, []string) {
	var cmds []tea.Cmd
	var restarted []string
	for _, other := range m.rows {
		if other.dep != nil || other.task || !other.running() {
			continue
		}
		if !readsDependency(other, depName) {
			continue
		}
		m.stopProcess(other)
		cmds = append(cmds, m.start(other, map[string]bool{}))
		restarted = append(restarted, other.cfg.Name)
	}
	return cmds, restarted
}

// sameProvides reports whether two published-config maps are equal, so a peer
// read that changed nothing does not churn the services that read it.
func sameProvides(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// describeMode names a mode's kind for a status line.
func describeMode(mode config.Mode) string {
	switch {
	case mode.Peered():
		return "peered with " + mode.Peer.ID
	case mode.Forwarded():
		return "forwarded"
	default:
		return "provided by the machine"
	}
}

// readsDependency reports whether a service consumes a dependency, by depends_on
// or by naming it in a reference: either way, switching the dependency's source
// means restarting it.
func readsDependency(r *row, name string) bool {
	for _, d := range r.cfg.DependsOn {
		if d == name {
			return true
		}
	}
	values := make([]string, 0, 1+len(r.cfg.Env))
	values = append(values, r.cfg.Cmd)
	for _, v := range r.cfg.Env {
		values = append(values, v)
	}
	for _, v := range values {
		for _, ref := range config.References(v) {
			if ref == name {
				return true
			}
		}
	}
	return false
}

type tickMsg time.Time

type probeMsg struct {
	ports map[string]map[int]bool
	deps  map[string]bool
	peers map[string]peerRead
}

// peerRead is what one turn of the probe learned from a sibling: the port it
// publishes for the peered listener, and the config that service provides.
type peerRead struct {
	port     int
	provides map[string]string
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
	type peerTarget struct{ dep, id, service, port string }
	var peers []peerTarget
	for _, r := range m.rows {
		if r.dep != nil {
			if mode := r.mode(); mode.Peered() {
				peers = append(peers, peerTarget{r.dep.Name, mode.Peer.ID, mode.Peer.Service, mode.Peer.Port})
			}
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
		results := probeMsg{ports: map[string]map[int]bool{}, deps: map[string]bool{}, peers: map[string]peerRead{}}
		for _, lt := range peers {
			var read peerRead
			if snap, running, _ := peer.Read(lt.id); running {
				if p, ok := snap.Port(lt.service, lt.port); ok {
					read.port = p
				}
				read.provides = snap.ProvidesFor(lt.service)
			}
			results.peers[lt.dep] = read
		}
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
		// A peered sibling that has come up, gone away, or moved its port: apply
		// the new number so consumers follow it, exactly as they would a port
		// allocated at start, and its published config alongside. When the address
		// or config actually changed, restart the running services that read it, so
		// a peer that appears after they started is picked up rather than leaving
		// them on the empty endpoint they booted with.
		var cmds []tea.Cmd
		for name, read := range msg.peers {
			changed := m.peerPort[name] != read.port || !sameProvides(m.peerProvides[name], read.provides)
			m.peerPort[name] = read.port
			m.peerProvides[name] = read.provides
			if !changed {
				continue
			}
			if r, ok := m.byName[name]; ok {
				m.applyMode(r)
			}
			// Only when there is an address to pick up, so a peer going away does
			// not restart working services into an empty endpoint.
			if read.port != 0 {
				cs, restarted := m.restartConsumers(name)
				cmds = append(cmds, cs...)
				if len(restarted) > 0 {
					m.message = fmt.Sprintf("%s resolved, restarted %s", name, strings.Join(restarted, ", "))
				}
			}
		}
		return m, tea.Batch(cmds...)
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
		case m.modePick:
			return m.handleModePickKey(msg)
		}
		return m.handleKey(msg)
	}
	return m, nil
}

// handleModePickKey drives the mode picker: move, choose, or leave. Digits pick
// a mode directly.
func (m Model) handleModePickKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	r, ok := m.byName[m.modeTarget]
	if !ok || r.dep == nil || !r.dep.HasModes() {
		m.modePick = false
		return m, nil
	}
	modes := r.dep.Modes
	switch key := msg.String(); key {
	case "q", "ctrl+c":
		m.stopAll()
		return m, tea.Quit
	case "esc", "m":
		m.modePick = false
		return m, nil
	case "up", "k":
		if m.modeCursor > 0 {
			m.modeCursor--
		}
		return m, nil
	case "down", "j":
		if m.modeCursor < len(modes)-1 {
			m.modeCursor++
		}
		return m, nil
	case "enter", " ":
		m.modePick = false
		cmd := m.chooseMode(r, modes[m.modeCursor].Name)
		return m, cmd
	default:
		// A digit jumps straight to that mode and chooses it.
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			if i := int(key[0] - '1'); i < len(modes) {
				m.modeCursor = i
				m.modePick = false
				cmd := m.chooseMode(r, modes[i].Name)
				return m, cmd
			}
		}
		return m, nil
	}
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
	case "w":
		// Fold long lines at the right edge rather than losing what is past it.
		m.wrap = !m.wrap
		m.refreshLogs()
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

	// The merged view prefixes every line with its name; a wrapped line's
	// continuations are indented past that column so the text stays in one.
	indent := 0
	if m.logFilter == "" {
		indent = nameW + 1
	}
	var b strings.Builder
	for _, l := range lines {
		if m.logFilter == "" {
			b.WriteString(styleMuted.Render(fmt.Sprintf("%-*s ", nameW, l.name)))
		}
		text := l.text
		if m.wrap {
			text = wrapLine(text, m.viewport.Width, indent)
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	m.viewport.SetContent(b.String())
	if m.follow {
		m.viewport.GotoBottom()
	}
}

// wrapLine folds text so that, after indent columns of prefix, it fits in
// width columns: at a space where there is one, mid-word where there is not,
// with escape codes kept intact. Each continuation is indented to sit under
// the first line's text. A terminal too narrow to fold into gets the line as
// it came, and the viewport cuts it as before.
func wrapLine(text string, width, indent int) string {
	const minCols = 10
	room := width - indent
	if room < minCols {
		return text
	}
	wrapped := ansi.Wrap(text, room, "")
	if indent == 0 {
		return wrapped
	}
	return strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", indent))
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
		if sel.dep != nil {
			mode := sel.mode()
			switch {
			case mode.Peered():
				// Nothing to start: read the sibling again, in case it has just
				// come up and the next probe has not run yet.
				m.readPeer(sel)
				m.applyMode(sel)
				if sel.address == "" {
					m.message = fmt.Sprintf("%s: no socket for %s yet — start the %s devctl", sel.cfg.Name, mode.Peer.ID, mode.Peer.ID)
				} else {
					m.message = fmt.Sprintf("%s: read %s from the %s devctl's socket", sel.cfg.Name, sel.address, mode.Peer.ID)
				}
				return m, nil
			case !mode.Forwarded(): // machine-provided
				m.message = fmt.Sprintf("%s is provided by the machine: set %s in .env", sel.cfg.Name, mode.Env)
				return m, nil
			}
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
	case "m":
		// Open the mode picker on a multi-mode dependency. A single-source one
		// has nothing to choose.
		if sel.dep == nil || !sel.dep.HasModes() {
			if sel.dep != nil {
				m.message = fmt.Sprintf("%s has a single source, nothing to switch", sel.cfg.Name)
			}
			return m, nil
		}
		m.modePick = true
		m.modeTarget = sel.dep.Name
		m.modeCursor = 0
		for i, md := range sel.dep.Modes {
			if md.Name == sel.activeModeName {
				m.modeCursor = i
				break
			}
		}
		return m, nil
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
	// What each dependency's active mode provides, for the dependencies this row
	// declares. Config that rides a mode: switch the mode and this swaps with it.
	// The row's own env comes after, so a service can still override a value.
	for _, depName := range r.cfg.DependsOn {
		d, ok := m.byName[depName]
		if !ok || d.dep == nil {
			continue
		}
		mode := d.mode()
		// What a peered sibling published for this service, already resolved on its
		// side. The base; the mode's own provides override it below.
		if mode.Peered() {
			for k, v := range m.peerProvides[depName] {
				env[k] = v
			}
		}
		for k, v := range mode.Provides {
			expanded, err := m.expand.Expand(v)
			if err != nil {
				return nil, err
			}
			env[k] = expanded
		}
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

	if r.running() || (r.dep != nil && !r.mode().Forwarded()) {
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

// depStatus reads the active mode: a machine-provided one is configured or not
// and answers or not; a forwarded one also has the tunnel process to account
// for; a peered one is waiting for its sibling or reading its port.
func (r *row) depStatus(up func(string) string) (string, lipgloss.Style) {
	unreachable := styleBad
	if r.dep.Optional {
		unreachable = styleWarn
	}
	mode := r.mode()
	switch {
	case mode.Peered():
		switch {
		case r.address == "":
			return "waiting", styleMuted
		case r.probed && r.reachable:
			return "peered", styleGood
		case !r.probed:
			return "peered", styleMuted
		default:
			return "peered", unreachable
		}
	case mode.Forwarded():
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
	default: // machine-provided
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
