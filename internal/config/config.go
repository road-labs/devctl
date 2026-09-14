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

// Dependency is something outside this repository a run needs. It is either
// provided by the machine, in which case Env names the variable holding its
// address (read from the process environment or the repository's .env,
// referenced as {{ name.address }}) and it is checked when devctl starts; or
// forwarded by devctl, in which case Port is the local port and Forward the
// command that opens the tunnel, started from the panel and referenced like
// a service port: {{ name.port }}, {{ name.port.number }}, {{ name.port.url }}.
// A forwarded dependency is optional by nature: nothing waits for the tunnel.
// A machine-provided one is required unless marked optional, in which case
// unconfigured it expands to "" and unreachable it only warns.
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
}

// Forward is the command that brings a forwarded dependency up, typically a
// kubectl port-forward. Its cmd may reference {{ name.port.number }}.
type Forward struct {
	Dir string `yaml:"dir"`
	Cmd string `yaml:"cmd"`
}

// Forwarded reports whether devctl, rather than the machine, provides it.
func (d Dependency) Forwarded() bool { return d.Forward != nil }

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
		if d.Kind == "" {
			d.Kind = "tcp"
		}
		if d.Kind != "mongo" && d.Kind != "tcp" {
			return fmt.Errorf("dependency %q has unknown kind %q (mongo or tcp)", d.Name, d.Kind)
		}
		switch {
		case d.Forwarded() && d.Env != "":
			return fmt.Errorf("dependency %q is either provided by the machine (env) or forwarded (port + forward), not both", d.Name)
		case d.Forwarded():
			d.Optional = true
			if d.Forward.Cmd == "" {
				return fmt.Errorf("dependency %q forward has no cmd", d.Name)
			}
			if d.Port <= 0 || d.Port > 65535 {
				return fmt.Errorf("dependency %q is forwarded and needs a valid local port, got %d", d.Name, d.Port)
			}
		case d.Env == "":
			return fmt.Errorf("dependency %q needs env (the variable holding its address) or port + forward", d.Name)
		case d.Port != 0:
			return fmt.Errorf("dependency %q has a port but no forward", d.Name)
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
