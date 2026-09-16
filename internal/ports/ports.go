// Package ports allocates every declared port once at start and expands the
// {{ service.port }} references that let services find each other without
// anyone typing an address twice.
package ports

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/road-labs/devctl/internal/config"
)

// Table is the allocated port for every service and port name.
type Table map[string]map[string]int

// Values are the addresses of dependencies the machine provides, keyed
// "<name>.address", so manifest values can say {{ mongo.address }} next to
// {{ identity.grpc }}.
type Values map[string]string

// Expander resolves both kinds of reference, which together are how a manifest
// avoids stating an address twice: a service says where it listens once, and
// everything that has to reach it says {{ service.port }} instead of a number.
// See Model.envFor for what that buys.
type Expander struct {
	Ports  Table
	Values Values
}

// ForwardPort is the port name a forwarded dependency's local port goes by.
const ForwardPort = "port"

// forwardPort is the local port a dependency needs reserved for a forward
// source, and whether it has one. A single forward uses its own port; a
// multi-mode dependency uses its default forward mode, else its first, since
// its forward modes share the one local port. A slot is reserved even when a
// forward is not the mode live at start, so switching to it later has a port
// ready. Peer and machine-provided sources reserve nothing.
func forwardPort(dep config.Dependency) (int, bool) {
	if !dep.HasModes() {
		if dep.Forwarded() {
			return dep.Port, true
		}
		return 0, false
	}
	def := dep.DefaultMode()
	var first *config.Mode
	for i := range dep.Modes {
		m := &dep.Modes[i]
		if m.Forward == nil {
			continue
		}
		if first == nil {
			first = m
		}
		if m.Name == def {
			return m.Port, true
		}
	}
	if first != nil {
		return first.Port, true
	}
	return 0, false
}

// Allocate decides the port for every declared name, services' listeners and
// forwarded dependencies' local ports alike. A free default is kept; a default
// something else holds is replaced by a free port unless that port is fixed,
// on the port itself or on the whole service, in which case that is an error.
// Warnings describe every replacement.
func Allocate(file *config.File, free func(port int) bool, pick func() (int, error)) (Table, []string, error) {
	table := Table{}
	var warnings []string
	taken := map[int]string{}

	for _, dep := range file.Dependencies {
		declared, ok := forwardPort(dep)
		if !ok {
			continue
		}
		if owner, dup := taken[declared]; dup {
			return nil, nil, fmt.Errorf("%s.%s and %s both declare port %d", dep.Name, ForwardPort, owner, declared)
		}
		port := declared
		if !free(port) {
			alt, err := pick()
			if err != nil {
				return nil, nil, fmt.Errorf("%s.%s: default %d in use and no free port found: %w", dep.Name, ForwardPort, port, err)
			}
			warnings = append(warnings, fmt.Sprintf("%s.%s: default %d in use, using %d", dep.Name, ForwardPort, port, alt))
			port = alt
		}
		taken[port] = dep.Name + "." + ForwardPort
		table[dep.Name] = map[string]int{ForwardPort: port}
	}

	for _, svc := range file.Services {
		table[svc.Name] = map[string]int{}
		for _, declared := range svc.Ports {
			name, port := declared.Name, declared.Number
			if owner, dup := taken[port]; dup {
				return nil, nil, fmt.Errorf("%s.%s and %s both declare port %d", svc.Name, name, owner, port)
			}
			if !free(port) {
				if svc.FixedPorts || declared.Fixed {
					return nil, nil, fmt.Errorf("%s.%s needs port %d but it is in use%s, and the service's ports are fixed; stop that process and start again", svc.Name, name, port, Holder(port))
				}
				alt, err := pick()
				if err != nil {
					return nil, nil, fmt.Errorf("%s.%s: default %d in use and no free port found: %w", svc.Name, name, port, err)
				}
				warnings = append(warnings, fmt.Sprintf("%s.%s: default %d in use%s, using %d", svc.Name, name, port, Holder(port), alt))
				port = alt
			}
			taken[port] = svc.Name + "." + name
			table[svc.Name][name] = port
		}
	}
	return table, warnings, nil
}

// Holder names the process listening on the port, as " by <command> (pid N)",
// or "" when it cannot be told.
func Holder(port int) string {
	pid, command, ok := HolderPID(port)
	switch {
	case !ok:
		return ""
	case command == "":
		return fmt.Sprintf(" by pid %d", pid)
	default:
		return fmt.Sprintf(" by %s (pid %d)", command, pid)
	}
}

// HolderPID finds the process listening on the port. It asks lsof, which
// macOS and Linux both have; a leftover `go run` child from an earlier devctl
// is the usual answer.
func HolderPID(port int) (pid int, command string, ok bool) {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		return 0, "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p") && pid == 0:
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "c") && command == "":
			command = line[1:]
		}
	}
	return pid, command, pid != 0
}

// HolderCWD is the working directory of the process listening on the port.
// It is what separates a leftover of ours from an unrelated program that
// happens to want the same port: a second Next dev server on 3000 looks
// exactly like the console's by command name, and only its directory says
// which repository it came from.
func HolderCWD(pid int) (string, bool) {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:], true
		}
	}
	return "", false
}

// IsFree reports whether nothing on this machine holds the port. Two checks,
// because one is not enough: on macOS a bind to 127.0.0.1:<port> succeeds
// while another process listens on the wildcard [::]:<port> (SO_REUSEADDR),
// which is how a leftover Next dev server sits on 3000. So anything that
// accepts a connection on the port counts as holding it, and then the
// wildcard and loopback binds must both succeed.
func IsFree(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 150*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return false
		}
	}
	for _, addr := range []string{":", "127.0.0.1:"} {
		l, err := net.Listen("tcp", addr+strconv.Itoa(port))
		if err != nil {
			return false
		}
		_ = l.Close()
	}
	return true
}

// PickFree asks the OS for an unused port.
func PickFree() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Expand replaces every reference in s. A dependency value is substituted as
// is. For a port, the bare form is a dial address, localhost:<port>; ".number"
// is the bare port; ".url" is http://localhost:<port>.
func (e Expander) Expand(s string) (string, error) {
	var firstErr error
	fail := func(err error) string {
		if firstErr == nil {
			firstErr = err
		}
		return ""
	}
	out := config.Reference.ReplaceAllStringFunc(s, func(match string) string {
		m := config.Reference.FindStringSubmatch(match)
		if value, ok := e.Values[m[1]+"."+m[2]]; ok {
			if m[3] != "" {
				return fail(fmt.Errorf("%s.%s has no .%s form, it is not a port", m[1], m[2], m[3]))
			}
			return value
		}
		port, ok := e.Ports[m[1]][m[2]]
		if !ok {
			return fail(fmt.Errorf("unknown reference %s.%s", m[1], m[2]))
		}
		switch m[3] {
		case "number":
			return strconv.Itoa(port)
		case "url":
			return "http://localhost:" + strconv.Itoa(port)
		default:
			return "localhost:" + strconv.Itoa(port)
		}
	})
	return out, firstErr
}

// Expand resolves port references only; see Expander for dependencies too.
func (t Table) Expand(s string) (string, error) {
	return Expander{Ports: t}.Expand(s)
}

// Validate checks that every reference in a manifest resolves to a declared
// port or dependency, so a typo fails at load rather than at start.
func Validate(file *config.File) error {
	declared := Expander{Ports: Table{}, Values: Values{}}
	for _, svc := range file.Services {
		declared.Ports[svc.Name] = map[string]int{}
		for _, port := range svc.Ports {
			declared.Ports[svc.Name][port.Name] = 1
		}
	}
	for _, dep := range file.Dependencies {
		switch {
		case dep.HasModes():
			// A multi-mode dependency is referenced only as {{ name.address }},
			// which resolves to a dialable address whichever mode is live.
			declared.Values[dep.Name+".address"] = "placeholder"
		case dep.Forwarded() || dep.Peered():
			declared.Ports[dep.Name] = map[string]int{ForwardPort: 1}
		default:
			declared.Values[dep.Name+".address"] = "placeholder"
		}
	}
	check := func(where string, values ...string) error {
		for _, v := range values {
			if _, err := declared.Expand(v); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		return nil
	}
	for _, svc := range file.Services {
		values := make([]string, 0, 1+len(svc.Env))
		values = append(values, svc.Cmd)
		for _, v := range svc.Env {
			values = append(values, v)
		}
		if err := check("service "+svc.Name, values...); err != nil {
			return err
		}
	}
	// Forward commands are checked against a table that also carries each
	// dependency's own local port, so a forward may state its port by referring
	// to itself even on a multi-mode dependency, where consumers may not.
	withForwards := Expander{Ports: Table{}, Values: declared.Values}
	for name, p := range declared.Ports {
		withForwards.Ports[name] = p
	}
	for _, dep := range file.Dependencies {
		withForwards.Ports[dep.Name] = map[string]int{ForwardPort: 1}
	}
	for _, dep := range file.Dependencies {
		for _, m := range dep.Modeset() {
			if m.Forward == nil {
				continue
			}
			if _, err := withForwards.Expand(m.Forward.Cmd); err != nil {
				return fmt.Errorf("dependency %s forward: %w", dep.Name, err)
			}
		}
	}
	for _, task := range file.Tasks {
		values := make([]string, 0, 1+len(task.Env))
		values = append(values, task.Cmd)
		for _, v := range task.Env {
			values = append(values, v)
		}
		if err := check("task "+task.Name, values...); err != nil {
			return err
		}
	}
	return nil
}

// Listen renders a port the way the process wants it: ":<port>" to bind to
// (format addr), or the bare number (format number).
func Listen(port int, format string) string {
	if format == "number" {
		return strconv.Itoa(port)
	}
	return ":" + strconv.Itoa(port)
}

// Describe renders the allocated ports of a service in declaration order,
// marking any that differ from the declared default.
func (t Table) Describe(svc config.Service) string {
	parts := make([]string, 0, len(svc.Ports))
	for _, declared := range svc.Ports {
		port := t[svc.Name][declared.Name]
		label := fmt.Sprintf("%s:%d", declared.Name, port)
		if port != declared.Number {
			label += "*"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " ")
}
