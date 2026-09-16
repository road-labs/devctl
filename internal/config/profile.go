package config

import (
	"fmt"
	"sort"
	"strings"
)

// Select narrows the manifest to what the named targets need. A target is a
// profile, or the name of any single service, task or dependency, so
// `devctl fraud-ui` works without anyone having declared a profile for it.
//
// No targets means the whole manifest, which is what devctl has always done.
func (f *File) Select(targets []string) (*File, error) {
	if len(targets) == 0 {
		return f, nil
	}

	profiles := map[string]Profile{}
	for _, p := range f.Profiles {
		profiles[p.Name] = p
	}

	// Roots: what was asked for, with profiles flattened. A profile may include
	// another, so this walks. A profile can also set the starting mode of a
	// dependency, gathered here and applied to the selection below.
	roots := map[string]bool{}
	overrides := map[string]string{}
	runEnv := map[string]string{}
	autostart := map[string]bool{}
	setsAutostart := false
	var expand func(name string, seen map[string]bool) error
	expand = func(name string, seen map[string]bool) error {
		if seen[name] {
			return nil
		}
		seen[name] = true
		if p, ok := profiles[name]; ok {
			for dep, mode := range p.Modes {
				if prev, dup := overrides[dep]; dup && prev != mode {
					return fmt.Errorf("selected profiles set %q to both %q and %q", dep, prev, mode)
				}
				overrides[dep] = mode
			}
			for k, v := range p.Env {
				runEnv[k] = v
			}
			if p.Autostart != nil {
				setsAutostart = true
				for _, n := range p.Autostart {
					autostart[n] = true
				}
			}
			for _, inc := range p.Include {
				if err := expand(inc, seen); err != nil {
					return err
				}
			}
			return nil
		}
		if f.find(name) == nil {
			return fmt.Errorf("unknown target %q; %s", name, f.targetHint())
		}
		roots[name] = true
		return nil
	}
	for _, t := range targets {
		if err := expand(t, map[string]bool{}); err != nil {
			return nil, err
		}
	}

	// Closure: what the roots cannot run without. depends_on says most of it,
	// and a {{ reference }} says the rest, because a service that reads another
	// one's address needs it up whether or not it was declared.
	want := map[string]bool{}
	var pull func(name string)
	pull = func(name string) {
		if want[name] {
			return
		}
		entry := f.find(name)
		if entry == nil {
			return
		}
		want[name] = true
		for _, dep := range entry.needs {
			pull(dep)
		}
	}
	for name := range roots {
		pull(name)
	}

	out := *f
	out.Profiles = f.Profiles
	out.Dependencies = nil
	out.Services = nil
	out.Tasks = nil
	for _, d := range f.Dependencies {
		if !want[d.Name] {
			continue
		}
		// A profile's mode override becomes this dependency's default, so the panel
		// and -check start it in the chosen mode. Validation has already checked
		// the mode exists.
		if mode, ok := overrides[d.Name]; ok {
			d.Default = mode
		}
		switch {
		case setsAutostart:
			d.Autostart = autostart[d.Name]
		case roots[d.Name]:
			// You named this forward, so open it at start; a forward pulled in
			// only as something else's closure keeps its own flag.
			d.Autostart = true
		}
		out.Dependencies = append(out.Dependencies, d)
	}
	for _, s := range f.Services {
		if want[s.Name] {
			s.Env = withRunEnv(s.Env, runEnv)
			switch {
			case setsAutostart:
				s.Autostart = autostart[s.Name]
			case roots[s.Name]:
				// A targeted run starts what it names. The closure it pulls in
				// follows through depends_on, so a service named as a root brings
				// its backing services up with it.
				s.Autostart = true
			}
			out.Services = append(out.Services, s)
		}
	}
	for _, t := range f.Tasks {
		if want[t.Name] {
			t.Env = withRunEnv(t.Env, runEnv)
			out.Tasks = append(out.Tasks, t)
		}
	}
	if len(out.Services) == 0 && len(out.Dependencies) == 0 && len(out.Tasks) == 0 {
		return nil, fmt.Errorf("%s selects nothing", strings.Join(targets, ", "))
	}
	return &out, nil
}

// withRunEnv layers a profile's run env over a service or task's own env: the
// run wins, since it is the deliberate choice for this launch. A fresh map, so
// the shared manifest is left alone. When there is no run env the original map
// is kept as is.
func withRunEnv(own, run map[string]string) map[string]string {
	if len(run) == 0 {
		return own
	}
	merged := make(map[string]string, len(own)+len(run))
	for k, v := range own {
		merged[k] = v
	}
	for k, v := range run {
		merged[k] = v
	}
	return merged
}

// Target is one thing `devctl <name>` accepts, with what kind it is so shell
// completion can label it.
type Target struct{ Name, Kind string }

// Targets is everything `devctl <name>` accepts: the profiles, and every single
// service, task and dependency. Shell completion lists these.
func (f *File) Targets() []Target {
	var out []Target
	for _, p := range f.Profiles {
		out = append(out, Target{p.Name, "profile"})
	}
	for _, d := range f.Dependencies {
		out = append(out, Target{d.Name, "dependency"})
	}
	for _, s := range f.Services {
		out = append(out, Target{s.Name, "service"})
	}
	for _, t := range f.Tasks {
		out = append(out, Target{t.Name, "task"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// validateProfiles checks the names and what they point at. A profile shares the
// namespace everything else is in, because `devctl <name>` has to mean one
// thing.
func (f *File) validateProfiles(claim func(name, what string) error) error {
	for _, p := range f.Profiles {
		if err := claim(p.Name, "profile"); err != nil {
			return err
		}
		if len(p.Include) == 0 {
			return fmt.Errorf("profile %q includes nothing", p.Name)
		}
	}
	known := map[string]bool{}
	for _, p := range f.Profiles {
		known[p.Name] = true
	}
	for _, p := range f.Profiles {
		for _, inc := range p.Include {
			if !known[inc] && f.find(inc) == nil {
				return fmt.Errorf("profile %q includes %q, which is not a profile, service, task or dependency", p.Name, inc)
			}
		}
		for depName, modeName := range p.Modes {
			d := f.dependency(depName)
			switch {
			case d == nil:
				return fmt.Errorf("profile %q sets a mode for %q, which is not a dependency", p.Name, depName)
			case !d.HasModes():
				return fmt.Errorf("profile %q sets a mode for %q, which has a single source", p.Name, depName)
			case !d.hasMode(modeName):
				return fmt.Errorf("profile %q sets %q to mode %q, which it does not have", p.Name, depName, modeName)
			}
		}
		for _, n := range p.Autostart {
			if !f.hasService(n) && f.dependency(n) == nil {
				return fmt.Errorf("profile %q autostarts %q, which is not a service or dependency", p.Name, n)
			}
		}
	}
	return nil
}

// hasService reports whether a service of this name is declared.
func (f *File) hasService(name string) bool {
	for _, s := range f.Services {
		if s.Name == name {
			return true
		}
	}
	return false
}

// dependency returns the dependency with this name, or nil.
func (f *File) dependency(name string) *Dependency {
	for i := range f.Dependencies {
		if f.Dependencies[i].Name == name {
			return &f.Dependencies[i]
		}
	}
	return nil
}

// hasMode reports whether the dependency declares a mode of this name.
func (d Dependency) hasMode(name string) bool {
	for _, m := range d.Modes {
		if m.Name == name {
			return true
		}
	}
	return false
}

// entry is one named thing in the manifest and everything it cannot run
// without, whatever kind it is.
type entry struct{ needs []string }

// find returns the entry for a name, or nil.
func (f *File) find(name string) *entry {
	for _, d := range f.Dependencies {
		if d.Name == name {
			// Union across modes: a forward mode's cmd may name a port, and
			// narrowing must keep it whichever mode is live. Peer and env modes
			// need nothing local.
			needs := []string{}
			for _, m := range d.Modeset() {
				if m.Forward != nil {
					needs = append(needs, References(m.Forward.Cmd)...)
				}
			}
			return &entry{needs: needs}
		}
	}
	for _, s := range f.Services {
		if s.Name == name {
			return &entry{needs: append(append([]string{}, s.DependsOn...), referencesIn(s.Cmd, s.Env)...)}
		}
	}
	for _, t := range f.Tasks {
		if t.Name == name {
			return &entry{needs: append(append([]string{}, t.DependsOn...), referencesIn(t.Cmd, t.Env)...)}
		}
	}
	return nil
}

// referencesIn is every name a command and an environment refer to.
func referencesIn(cmd string, env map[string]string) []string {
	out := References(cmd)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	// Sorted so the result does not depend on map order, which matters only for
	// the error messages but matters there.
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, References(env[k])...)
	}
	return out
}

// targetHint lists what a target could have been, for the error.
func (f *File) targetHint() string {
	var profiles, rows []string
	for _, p := range f.Profiles {
		profiles = append(profiles, p.Name)
	}
	for _, d := range f.Dependencies {
		rows = append(rows, d.Name)
	}
	for _, s := range f.Services {
		rows = append(rows, s.Name)
	}
	for _, t := range f.Tasks {
		rows = append(rows, t.Name)
	}
	if len(profiles) > 0 {
		return fmt.Sprintf("profiles: %s; or any of: %s", strings.Join(profiles, ", "), strings.Join(rows, ", "))
	}
	return fmt.Sprintf("no profiles are declared; name any of: %s", strings.Join(rows, ", "))
}
