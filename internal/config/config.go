// Package config loads development/devctl/services.yaml, the single source of
// truth for what can run locally, on which ports, in which modes, what it
// needs from the machine, and which one-shot tasks the panel offers.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Reference matches {{ service.port }}, {{ service.port.number }},
// {{ service.port.url }} and {{ dependency.address }}. It lives here because it
// is the manifest's own grammar; ports resolves it and profiles read it to work
// out what a subset cannot run without.
var Reference = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)(?:\.(number|url))?\s*\}\}`)

// References names everything s refers to, by the first segment: the service or
// dependency, not the port. Referring to something is depending on it, whether
// or not depends_on says so.
func References(s string) []string {
	var out []string
	for _, m := range Reference.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// Listen is how one listener's port reaches the process.
type Listen struct {
	Env    string `yaml:"env"`
	Format string `yaml:"format"` // addr (default): ":<port>"; number: "<port>"
}

// UnmarshalYAML accepts the short scalar form as well as the mapping.
func (l *Listen) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*l = Listen{Env: node.Value, Format: "addr"}
		return nil
	}
	type plain Listen
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*l = Listen(p)
	if l.Format == "" {
		l.Format = "addr"
	}
	return nil
}

// Port is one listener of a service. A service can expose several, for
// example a management gRPC server and a separate authentication gRPC server,
// so each carries what kind it is and what it serves.
type Port struct {
	Name        string `yaml:"name"`
	Number      int    `yaml:"port"`
	Kind        string `yaml:"kind"` // http or grpc
	Description string `yaml:"description"`
	// Fixed refuses to start rather than move this one listener to a free
	// port. Use it only where something outside this process has the number
	// written down: a URL persisted in a store, a redirect URI, IdP metadata.
	// A port other services find through {{ svc.name }} is never fixed, since
	// they are handed whatever it was given.
	Fixed bool `yaml:"fixed"`
}

// Service is one row in the tool: one command on declared ports.
type Service struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Ports are the service's listeners in declaration order, with their
	// default numbers.
	Ports []Port `yaml:"ports"`
	// FixedPorts pins every one of the service's ports. Prefer `fixed` on the
	// single port that needs it: pinning all of them refuses to start over a
	// clash on a port nothing outside the process refers to.
	FixedPorts bool `yaml:"fixed_ports"`
	// DependsOn names services started first and dependencies that must be
	// configured.
	DependsOn []string `yaml:"depends_on"`
	Autostart bool     `yaml:"autostart"`
	// Dir is relative to the repository root; Cmd runs through sh -c.
	Dir string `yaml:"dir"`
	Cmd string `yaml:"cmd"`
	// Listen maps a port name declared on the service to the environment
	// variable this process reads for that listener. The plain form
	// `{http: HTTP_ADDR}` passes ":<port>"; `{web: {env: PORT, format: number}}`
	// passes the bare number, for programs such as Next.js that want only that.
	Listen map[string]Listen `yaml:"listen"`
	// Env is extra environment; values may reference ports as {{ svc.port }}
	// and dependencies as {{ dep.address }} or {{ dep.port }}.
	Env map[string]string `yaml:"env"`
	// Watch lists directories, relative to the repository root, whose Go
	// source changes restart the service. Empty means no auto-restart.
	Watch []string `yaml:"watch"`
}

// Dependency is something outside this repository a run needs. It has one or
// more sources, which is where its address comes from. A single-source
// dependency carries the source inline; a dependency with several carries them
// as named Modes and switches between them live.
//
// A source is one of three kinds:
//
//   - provided by the machine: Env names the variable holding its address (read
//     from the process environment or the repository's .env, referenced as
//     {{ name.address }}) and it is checked when devctl starts;
//   - forwarded by devctl: Port is the local port and Forward the command that
//     opens the tunnel, started from the panel and referenced like a service
//     port: {{ name.port }}, {{ name.port.number }}, {{ name.port.url }};
//   - peered to a sibling devctl: Peer names another devctl by id and reads one
//     of its listeners' live ports, referenced the same way as a forward.
//
// A forwarded, peered or multi-mode dependency is optional by nature: nothing
// waits for a tunnel, a sibling, or a mode you can switch away from. A
// single-source machine-provided one is required unless marked optional, in
// which case unconfigured it expands to "" and unreachable it only warns.
type Dependency struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Env         string `yaml:"env"`
	Example     string `yaml:"example"`
	Kind        string `yaml:"kind"` // mongo (ping) or tcp (dial)
	Optional    bool   `yaml:"optional"`
	// Port and Forward describe a dependency devctl tunnels to.
	Port    int      `yaml:"port"`
	Forward *Forward `yaml:"forward"`
	// Peer describes a dependency read from a sibling devctl.
	Peer *Peer `yaml:"peer"`
	// Modes are the several sources a dependency can be satisfied by, one active
	// at a time. Default names the one active at start. A dependency with modes
	// is referenced only as {{ name.address }}, which resolves to a dialable
	// address whichever mode is live.
	Modes   []Mode `yaml:"modes"`
	Default string `yaml:"default"`
}

// Mode is one named source of a multi-source dependency: the same three kinds a
// single-source dependency has, given a name so it can be chosen and switched.
type Mode struct {
	Name    string   `yaml:"name"`
	Env     string   `yaml:"env"`
	Example string   `yaml:"example"`
	Kind    string   `yaml:"kind"`
	Port    int      `yaml:"port"`
	Forward *Forward `yaml:"forward"`
	Peer    *Peer    `yaml:"peer"`
}

// Peer points at one listener of another devctl on this machine, by that
// devctl's id and the service and port name it declares. devctl reads the
// sibling's live allocation over its socket, so the number follows whatever the
// sibling actually got.
type Peer struct {
	ID      string `yaml:"id"`
	Service string `yaml:"service"`
	Port    string `yaml:"port"`
}

// Forward is the command that brings a forwarded dependency up, typically a
// kubectl port-forward. Its cmd may reference {{ name.port.number }}.
type Forward struct {
	Dir string `yaml:"dir"`
	Cmd string `yaml:"cmd"`
}

// Forwarded reports whether devctl, rather than the machine, provides the
// single inline source. It is the pre-modes check; multi-mode dependencies ask
// their active Mode instead.
func (d Dependency) Forwarded() bool { return d.Forward != nil }

// Peered reports whether the single inline source is read from a sibling devctl.
func (d Dependency) Peered() bool { return d.Peer != nil }

// HasModes reports whether the dependency carries several named sources rather
// than one inline. A multi-mode dependency is referenced only as
// {{ name.address }} and switched with m in the panel.
func (d Dependency) HasModes() bool { return len(d.Modes) > 0 }

// Modeset is the dependency's sources as a uniform list, so code that does not
// care whether it was written inline or as modes has one thing to range over. A
// single-source dependency becomes a list of one, its name empty.
func (d Dependency) Modeset() []Mode {
	if len(d.Modes) > 0 {
		return d.Modes
	}
	return []Mode{{Env: d.Env, Example: d.Example, Kind: d.Kind, Port: d.Port, Forward: d.Forward, Peer: d.Peer}}
}

// DefaultMode names the mode live at start: Default when set, else the first
// mode, else "" for a single-source dependency.
func (d Dependency) DefaultMode() string {
	if d.Default != "" {
		return d.Default
	}
	if len(d.Modes) > 0 {
		return d.Modes[0].Name
	}
	return ""
}

// Mode returns the named source, falling back to the default and then the
// first, so a stale selection never leaves a dependency with no source.
func (d Dependency) Mode(name string) Mode {
	set := d.Modeset()
	for _, m := range set {
		if m.Name == name {
			return m
		}
	}
	if def := d.Default; def != "" {
		for _, m := range set {
			if m.Name == def {
				return m
			}
		}
	}
	return set[0]
}

// Forwarded reports whether this source is a devctl tunnel.
func (m Mode) Forwarded() bool { return m.Forward != nil }

// Peered reports whether this source is read from a sibling devctl.
func (m Mode) Peered() bool { return m.Peer != nil }

// PortShaped reports whether the source is referenced by port ({{ name.port }}),
// which forwards and peers are. An env source is address-shaped instead.
func (m Mode) PortShaped() bool { return m.Forward != nil || m.Peer != nil }

// Task is a one-shot command offered by the panel, such as seeding the
// database. It runs to completion and shows its exit status.
type Task struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Dir         string            `yaml:"dir"`
	Cmd         string            `yaml:"cmd"`
	Env         map[string]string `yaml:"env"`
	DependsOn   []string          `yaml:"depends_on"`
}

// Port returns the declared port with this name.
func (s Service) Port(name string) (Port, bool) {
	for _, p := range s.Ports {
		if p.Name == name {
			return p, true
		}
	}
	return Port{}, false
}

// File is the parsed services.yaml.
type File struct {
	// ID names this devctl to its siblings. When set, devctl publishes its
	// allocated ports on a socket keyed by it, so another repository's devctl can
	// read them with a peer dependency. Optional: without it nothing is
	// published and nothing changes.
	ID string `yaml:"id"`
	// Logs, when set, keeps a file per service so a crash can be read after the
	// panel has moved on.
	Logs *Logs `yaml:"logs"`
	// Profiles name subsets worth running on their own. Optional: without any,
	// devctl runs everything.
	Profiles     []Profile    `yaml:"profiles"`
	Dependencies []Dependency `yaml:"dependencies"`
	Services     []Service    `yaml:"services"`
	Tasks        []Task       `yaml:"tasks"`
}

// Profile is a named subset of the manifest: the things someone actually works
// on, without the rest of the repository coming up around them. What it lists
// are the roots; what those need comes with them.
type Profile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Include names services, tasks, dependencies, or other profiles.
	Include []string `yaml:"include"`
	// Modes sets the starting mode of multi-mode dependencies, by name, when this
	// profile is selected: {platform: staging} runs the platform dependency
	// forwarded rather than in whatever it defaults to. A named starting
	// configuration, not a lock: m still switches at runtime.
	Modes map[string]string `yaml:"modes"`
}

// Logs configures on-disk output. Without it devctl keeps the last 2000 lines
// of each process in memory and nothing else, which is gone when devctl is.
type Logs struct {
	// Dir is relative to the repository root, so it can be gitignored. One
	// <service>.log per row, appended to across restarts.
	Dir string `yaml:"dir"`
	// MaxSize is a size like "10MB" or "512KB", or a plain byte count. A file
	// already over it at start is rotated to <service>.log.1 and begun again.
	// Empty never rotates.
	MaxSize string `yaml:"max_size"`
}

// sizeUnits are the suffixes MaxSize accepts, longest first so "MB" is matched
// before "B".
var sizeUnits = []struct {
	suffix string
	scale  int64
}{
	{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	{"B", 1},
}

// Bytes is MaxSize as a number. An unparseable value is an error rather than a
// silent zero: a log that was meant to be capped and is not is a disk that
// fills up overnight.
func (l *Logs) Bytes() (int64, error) {
	if l == nil || strings.TrimSpace(l.MaxSize) == "" {
		return 0, nil
	}
	text := strings.ToUpper(strings.TrimSpace(l.MaxSize))
	for _, unit := range sizeUnits {
		if strings.HasSuffix(text, unit.suffix) {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(text, unit.suffix)), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("logs.max_size %q: %w", l.MaxSize, err)
			}
			return n * unit.scale, nil
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("logs.max_size %q: want a byte count or a size like 10MB", l.MaxSize)
	}
	return n, nil
}

// Load reads and validates the manifest.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

func (f *File) validate() error {
	if len(f.Services) == 0 {
		return fmt.Errorf("no services defined")
	}
	// Checked here so `-check` catches it, rather than at the first start, where
	// a cap that does not parse would quietly become no cap at all.
	if _, err := f.Logs.Bytes(); err != nil {
		return err
	}
	// One namespace for everything the panel shows and depends_on can name.
	seen := map[string]string{}
	claim := func(name, what string) error {
		if name == "" {
			return fmt.Errorf("a %s has no name", what)
		}
		if prev, dup := seen[name]; dup {
			return fmt.Errorf("%s %q is already the name of a %s", what, name, prev)
		}
		seen[name] = what
		return nil
	}

	for i := range f.Dependencies {
		d := &f.Dependencies[i]
		if err := claim(d.Name, "dependency"); err != nil {
			return err
		}
		if err := f.validateDependency(d); err != nil {
			return err
		}
	}

	for i := range f.Services {
		s := &f.Services[i]
		if err := claim(s.Name, "service"); err != nil {
			return err
		}
		if s.Cmd == "" {
			return fmt.Errorf("service %q has no cmd", s.Name)
		}
		portNames := map[string]bool{}
		for _, port := range s.Ports {
			if port.Name == "" {
				return fmt.Errorf("service %q has a port without a name", s.Name)
			}
			if portNames[port.Name] {
				return fmt.Errorf("service %q declares port %q twice", s.Name, port.Name)
			}
			portNames[port.Name] = true
			if port.Number <= 0 || port.Number > 65535 {
				return fmt.Errorf("service %q port %q has an invalid number %d", s.Name, port.Name, port.Number)
			}
			if port.Kind != "" && port.Kind != "http" && port.Kind != "grpc" {
				return fmt.Errorf("service %q port %q has unknown kind %q (http or grpc)", s.Name, port.Name, port.Kind)
			}
		}
		for name, listen := range s.Listen {
			if !portNames[name] {
				return fmt.Errorf("service %q listens on undeclared port %q", s.Name, name)
			}
			if listen.Env == "" {
				return fmt.Errorf("service %q listen %q names no env variable", s.Name, name)
			}
			if listen.Format != "addr" && listen.Format != "number" {
				return fmt.Errorf("service %q listen %q has unknown format %q (addr or number)", s.Name, name, listen.Format)
			}
		}
	}

	for i := range f.Tasks {
		t := &f.Tasks[i]
		if err := claim(t.Name, "task"); err != nil {
			return err
		}
		if t.Cmd == "" {
			return fmt.Errorf("task %q has no cmd", t.Name)
		}
	}

	for _, s := range f.Services {
		for _, dep := range s.DependsOn {
			if what := seen[dep]; what != "service" && what != "dependency" {
				return fmt.Errorf("service %q depends on unknown service or dependency %q", s.Name, dep)
			}
		}
	}
	for _, t := range f.Tasks {
		for _, dep := range t.DependsOn {
			if what := seen[dep]; what != "service" && what != "dependency" {
				return fmt.Errorf("task %q depends on unknown service or dependency %q", t.Name, dep)
			}
		}
	}
	// Last, because it claims names in the same namespace and needs everything
	// else already claimed.
	return f.validateProfiles(claim)
}

// validateDependency checks a dependency's sources, whether written inline or as
// modes, and fills the defaults each kind carries. A dependency is one inline
// source or a list of named modes, never both.
func (f *File) validateDependency(d *Dependency) error {
	if d.HasModes() {
		if d.Env != "" || d.Forward != nil || d.Peer != nil || d.Port != 0 {
			return fmt.Errorf("dependency %q has modes, so its source belongs in a mode, not inline", d.Name)
		}
		// A dependency you can switch is optional by nature: a mode you can leave
		// is not one anything waits for.
		d.Optional = true
		names := map[string]bool{}
		for i := range d.Modes {
			m := &d.Modes[i]
			if m.Name == "" {
				return fmt.Errorf("dependency %q has a mode without a name", d.Name)
			}
			if names[m.Name] {
				return fmt.Errorf("dependency %q declares mode %q twice", d.Name, m.Name)
			}
			names[m.Name] = true
			if err := validateMode(d.Name, m); err != nil {
				return err
			}
		}
		if d.Default != "" && !names[d.Default] {
			return fmt.Errorf("dependency %q default %q is not one of its modes", d.Name, d.Default)
		}
		return nil
	}

	inline := &Mode{Env: d.Env, Example: d.Example, Kind: d.Kind, Port: d.Port, Forward: d.Forward, Peer: d.Peer}
	if err := validateMode(d.Name, inline); err != nil {
		return err
	}
	d.Kind = inline.Kind
	if inline.PortShaped() {
		d.Optional = true
	}
	return nil
}

// validateMode is the single definition of what one source is, shared by inline
// dependencies and modes: exactly one of env, forward or peer, each with what it
// needs. It fills the kind default of a machine-provided source.
func validateMode(dep string, m *Mode) error {
	sources := 0
	for _, has := range []bool{m.Env != "", m.Forward != nil, m.Peer != nil} {
		if has {
			sources++
		}
	}
	where := "dependency " + strconv.Quote(dep)
	if m.Name != "" {
		where += " mode " + strconv.Quote(m.Name)
	}
	switch {
	case sources == 0:
		return fmt.Errorf("%s needs one of env, forward or peer", where)
	case sources > 1:
		return fmt.Errorf("%s is provided one way: env, forward or peer, not several", where)
	}
	switch {
	case m.Forward != nil:
		if m.Forward.Cmd == "" {
			return fmt.Errorf("%s forward has no cmd", where)
		}
		if m.Port <= 0 || m.Port > 65535 {
			return fmt.Errorf("%s is forwarded and needs a valid local port, got %d", where, m.Port)
		}
	case m.Peer != nil:
		if m.Peer.ID == "" || m.Peer.Service == "" || m.Peer.Port == "" {
			return fmt.Errorf("%s peer needs id, service and port", where)
		}
		if m.Port != 0 {
			return fmt.Errorf("%s is peered, so its port comes from the sibling, not a local one", where)
		}
	default: // machine-provided
		if m.Kind == "" {
			m.Kind = "tcp"
		}
		if m.Kind != "mongo" && m.Kind != "tcp" {
			return fmt.Errorf("%s has unknown kind %q (mongo or tcp)", where, m.Kind)
		}
		if m.Port != 0 {
			return fmt.Errorf("%s has a port but no forward", where)
		}
	}
	return nil
}
