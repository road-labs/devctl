package ui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/road-labs/devctl/internal/deps"
	"github.com/road-labs/devctl/internal/peer"
	"github.com/road-labs/devctl/internal/ports"
)

// describeString is the full-screen describe view: everything devctl knows
// about one row, and how it is wired to the rest.
func (m Model) describeString() string {
	sel := m.rows[m.cursor]
	var b strings.Builder
	b.WriteString(styleTitle.Render("describe") + styleMuted.Render("  ·  "+sel.cfg.Name) + "\n")
	b.WriteString(m.viewport.View() + "\n")
	b.WriteString(styleMuted.Render("↑↓ pgup pgdn scroll  g/G top/bottom  esc or d back  q quit"))
	return b.String()
}

// describeBody is the scrollable part: what the row is, what it would run, what
// it listens on, what it is handed, and the graph either side of it.
func (m Model) describeBody() string {
	r := m.rows[m.cursor]
	width := max(m.width-2, 60)
	var b strings.Builder

	kind := "service"
	switch {
	case r.dep != nil:
		kind = "dependency"
		switch mode := r.mode(); {
		case mode.Peered():
			kind = "dependency, peered"
		case mode.Forwarded():
			kind = "dependency, forwarded"
		}
	case r.task:
		kind = "task"
	}
	status, style := r.status()
	b.WriteString(styleColumn.Render(r.cfg.Name) + styleMuted.Render("  ·  "+kind) + "\n")
	if r.cfg.Description != "" {
		b.WriteString(styleMuted.Render(r.cfg.Description) + "\n")
	}
	b.WriteString("\n")

	field := func(label, value string) {
		if value == "" {
			return
		}
		b.WriteString(styleMuted.Render(pad("  "+label, 14)) + value + "\n")
	}
	field("status", style.Render(status))
	if r.proc != nil {
		field("pid", strconv.Itoa(r.proc.PID()))
		field("uptime", shortDuration(r.proc.Uptime()))
	}
	if r.starts > 0 {
		field("starts", fmt.Sprintf("%d, %d of them restarts", r.starts, r.restarts()))
	}
	if r.dep != nil {
		mode := r.mode()
		if r.dep.HasModes() {
			labels := make([]string, 0, len(r.dep.Modes))
			for _, md := range r.dep.Modes {
				label := md.Name
				if md.Name == r.activeModeName {
					label = "[" + label + "]"
				}
				labels = append(labels, label)
			}
			field("mode", r.activeModeName+styleMuted.Render("  (m picks)"))
			field("modes", strings.Join(labels, "  "))
		}
		field("env", mode.Env)
		field("address", deps.Redact(r.address))
		if mode.Peered() {
			// Say where this port comes from: not a tunnel or a variable, but read
			// live from a sibling devctl over its socket. When the sibling is not
			// running there is no address yet, so the reader knows to start it.
			field("peer", mode.Peer.ID+"  "+mode.Peer.Service+"."+mode.Peer.Port)
			read := "read over its socket"
			if r.address == "" {
				read = "waiting: run the " + mode.Peer.ID + " devctl"
			}
			field("socket", tilde(peer.SocketPath(mode.Peer.ID))+styleMuted.Render("  ("+read+")"))
		}
		if mode.Env != "" {
			probe := mode.Kind
			if probe == "" {
				probe = "tcp"
			}
			field("probe", probe)
		}
		// What this mode hands to the services that depend on it. It rides the
		// mode, so switching swaps the whole set.
		if len(mode.Provides) > 0 {
			keys := make([]string, 0, len(mode.Provides))
			for k := range mode.Provides {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			field("provides", strings.Join(keys, "  "))
		}
		if r.dep.Optional {
			field("optional", "yes, an unreachable one only warns")
		}
	}
	field("directory", r.cfg.Dir)
	if r.cfg.Cmd != "" {
		cmd, err := m.expand.Expand(r.cfg.Cmd)
		if err != nil {
			cmd = r.cfg.Cmd
		}
		field("command", cmd)
	}
	if r.dep != nil {
		if mode := r.mode(); mode.Forwarded() {
			forward, err := m.expand.Expand(mode.Forward.Cmd)
			if err != nil {
				forward = mode.Forward.Cmd
			}
			field("forward", forward)
		}
	}
	if len(r.cfg.Watch) > 0 {
		state := "on"
		if !r.autoRestart {
			state = "off"
		}
		field("watching", strings.Join(r.cfg.Watch, ", ")+styleMuted.Render("  (auto-restart "+state+")"))
	}
	if r.cfg.Autostart {
		field("autostart", "yes, started when devctl starts")
	}
	// What this service hands its consumers, local or peered from another devctl.
	if len(r.cfg.Provides) > 0 {
		keys := make([]string, 0, len(r.cfg.Provides))
		for k := range r.cfg.Provides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		field("publishes", strings.Join(keys, "  "))
	}
	// Where to look once devctl is gone, which is the whole point of writing it
	// to disk: the ring in memory dies with the panel.
	if path := m.logPath(r); path != "" {
		field("log file", tilde(path))
	}

	if len(r.cfg.Ports) > 0 {
		b.WriteString("\n" + styleColumn.Render("listeners") + "\n")
		cols := []col{
			{title: "NAME"}, {title: "KIND"},
			{title: "DECLARED", right: true}, {title: "ACTUAL", right: true},
			{title: "STATE"}, {title: "ENV"}, {title: "DESCRIPTION", flex: true},
		}
		var rows [][]string
		for _, declared := range r.cfg.Ports {
			number := m.expand.Ports[r.cfg.Name][declared.Name]
			state := styleMuted.Render("closed")
			if r.portsOpen[number] {
				state = styleGood.Render("listening")
			}
			actual := strconv.Itoa(number)
			if number != declared.Number {
				actual = styleWarn.Render(actual)
			}
			env := ""
			if listen, ok := r.cfg.Listen[declared.Name]; ok {
				env = listen.Env + "=" + ports.Listen(number, listen.Format)
			}
			rows = append(rows, []string{
				declared.Name, styleMuted.Render(declared.Kind),
				styleMuted.Render(strconv.Itoa(declared.Number)), actual,
				state, styleMuted.Render(env), styleMuted.Render(declared.Description),
			})
		}
		for _, line := range table(cols, rows, nil, width-2, nil) {
			b.WriteString("  " + line.text + "\n")
		}
	}

	// The wired environment, which is the thing a manifest is really for: every
	// address here was derived from the port table rather than typed, so this
	// section is where a reader finds out what a service was actually told. See
	// Model.envFor. Shown even when it cannot be built, because a reference that
	// does not resolve is exactly why this row will not start.
	env, err := m.envFor(r)
	switch {
	case err != nil:
		b.WriteString("\n" + styleColumn.Render("environment") + "\n")
		b.WriteString("  " + styleBad.Render(err.Error()) + "\n")
	case len(env) > 0:
		b.WriteString("\n" + styleColumn.Render("environment") + styleMuted.Render("  ·  what the process is handed") + "\n")
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		keyW := 0
		for _, k := range keys {
			keyW = max(keyW, len(k))
		}
		for _, k := range keys {
			b.WriteString("  " + pad(k, keyW+2) + styleMuted.Render(deps.Redact(env[k])) + "\n")
		}
	}

	b.WriteString("\n" + styleColumn.Render("depends on") + styleMuted.Render("  ·  started first, and what they need in turn") + "\n")
	b.WriteString(m.tree(r, map[string]bool{}, "  ", true))
	b.WriteString("\n" + styleColumn.Render("needed by") + styleMuted.Render("  ·  what starting this brings up on its own") + "\n")
	b.WriteString(m.dependents(r))
	return b.String()
}

// tree draws what a row depends on, and what those depend on, down to the
// leaves. A name already on the branch is marked rather than followed, so a
// cycle in the manifest shows up here instead of hanging.
func (m Model) tree(r *row, seen map[string]bool, prefix string, root bool) string {
	var b strings.Builder
	names := r.cfg.DependsOn
	for i, name := range names {
		last := i == len(names)-1
		branch, next := "├ ", prefix+"│ "
		if last {
			branch, next = "└ ", prefix+"  "
		}
		dep, ok := m.byName[name]
		if !ok {
			b.WriteString(prefix + styleMuted.Render(branch) + name + styleBad.Render("  not in the manifest") + "\n")
			continue
		}
		label, style := dep.status()
		b.WriteString(prefix + styleMuted.Render(branch) + pad(dep.cfg.Name, 16) + styleMuted.Render(pad(m.kindOf(dep), 12)) + style.Render(label) + "\n")
		if seen[name] {
			b.WriteString(next + styleMuted.Render("↑ already above") + "\n")
			continue
		}
		seen[name] = true
		b.WriteString(m.tree(dep, seen, next, false))
		delete(seen, name)
	}
	if len(names) == 0 && root {
		b.WriteString(prefix + styleMuted.Render("nothing") + "\n")
	}
	return b.String()
}

// dependents lists the rows that name this one, which is the question a
// dependency's owner actually has: who breaks if this is down.
func (m Model) dependents(r *row) string {
	var out []*row
	for _, other := range m.rows {
		for _, name := range other.cfg.DependsOn {
			if name == r.cfg.Name {
				out = append(out, other)
				break
			}
		}
	}
	if len(out) == 0 {
		return "  " + styleMuted.Render("nothing") + "\n"
	}
	var b strings.Builder
	for i, other := range out {
		branch := "├ "
		if i == len(out)-1 {
			branch = "└ "
		}
		label, style := other.status()
		b.WriteString("  " + styleMuted.Render(branch) + pad(other.cfg.Name, 16) + styleMuted.Render(pad(m.kindOf(other), 12)) + style.Render(label) + "\n")
	}
	return b.String()
}

// kindOf names a row the way the table's TYPE column does. A dependency reads
// as its active mode, so a switch shows here too.
func (m Model) kindOf(r *row) string {
	switch {
	case r.dep != nil:
		mode := r.mode()
		switch {
		case mode.Peered():
			return "peer"
		case mode.Forwarded():
			return "forward"
		case mode.Kind != "":
			return mode.Kind
		default:
			return "tcp"
		}
	case r.task:
		return "task"
	}
	return "service"
}
