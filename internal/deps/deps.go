// Package deps resolves and checks what a run needs from outside this
// repository. A dependency the machine provides names the environment variable
// holding its address; the value comes from the process environment or, failing
// that, the repository's .env, and a required one is checked to be reachable
// before anything starts. A dependency devctl forwards is brought up from the
// panel and is not checked here.
package deps

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/road-labs/devctl/internal/config"
	"github.com/road-labs/devctl/internal/ports"
)

// EnvFile is the machine-specific, git-ignored file dependencies are read from.
const EnvFile = ".env"

// Key is how a machine-provided dependency's address is referenced.
func Key(dep config.Dependency) string { return dep.Name + ".address" }

// ModeKey is where a multi-mode dependency's machine-provided address is kept,
// one per env mode. It is not a reference: a mode is chosen at runtime, not
// named in a manifest, so the space keeps it out of the {{ name.address }}
// grammar entirely.
func ModeKey(dep, mode string) string { return dep + " " + mode + ".address" }

// Resolve finds the address of every machine-provided dependency, process
// environment first, then <root>/.env, keyed for the manifest
// ({{ mongo.address }}). A missing required one is an error that says exactly
// what line to add where; a missing optional one resolves to "" so the
// services it feeds see the feature as unconfigured.
func Resolve(root string, deps []config.Dependency) (ports.Values, error) {
	fromFile, err := readEnvFile(filepath.Join(root, EnvFile))
	if err != nil {
		return nil, err
	}
	get := func(env string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return fromFile[env]
	}
	values := ports.Values{}
	var missing []string
	for _, dep := range deps {
		// A multi-mode dependency resolves every machine-provided mode it has, so
		// switching to one finds its address ready. None of them is required: a
		// mode you can switch away from is not one anything waits for.
		if dep.HasModes() {
			for _, mode := range dep.Modes {
				if mode.Env != "" {
					values[ModeKey(dep.Name, mode.Name)] = get(mode.Env)
				}
			}
			continue
		}
		inline := dep.Modeset()[0]
		if inline.Env == "" {
			continue // forwarded or peered: nothing to read from the environment
		}
		value := get(inline.Env)
		values[Key(dep)] = value
		if value == "" && !dep.Optional {
			example := inline.Example
			if example == "" {
				example = "<address>"
			}
			missing = append(missing, fmt.Sprintf("  %s=%s   # %s", inline.Env, example, dep.Description))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("not configured: add to %s (or export)\n%s", filepath.Join(root, EnvFile), strings.Join(missing, "\n"))
	}
	return values, nil
}

// readEnvFile parses KEY=VALUE lines; comments, blanks, an `export ` prefix
// and surrounding quotes are tolerated. A missing file is not an error.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, scanner.Err()
}

// Check verifies each configured machine-provided dependency answers, by dialing
// the host and port its address names. A required one that does not is an error;
// an optional one is reported as a warning. Forwarded dependencies are not
// checked, they are started from the panel.
func Check(ctx context.Context, deps []config.Dependency, values ports.Values) ([]string, error) {
	var warnings []string
	for _, dep := range deps {
		if dep.Forwarded() {
			continue
		}
		value := values[Key(dep)]
		if value == "" {
			continue // optional and unconfigured; Resolve refused a required one
		}
		if err := dial(ctx, value); err != nil {
			problem := fmt.Sprintf("%s (%s=%s) is not reachable: %v", dep.Name, dep.Env, Redact(value), err)
			if dep.Optional {
				warnings = append(warnings, problem)
				continue
			}
			return warnings, errors.New(problem)
		}
	}
	return warnings, nil
}

func dial(ctx context.Context, address string) error {
	hostPort, ok := HostPort(address)
	if !ok {
		return fmt.Errorf("cannot tell host and port from %q", address)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return err
	}
	return conn.Close()
}

// HostPort extracts a dialable host:port from an address or URL, for the
// panel's reachability dot. mongodb:// defaults to port 27017. SRV records
// and multi-host URIs are not probed.
func HostPort(address string) (string, bool) {
	if !strings.Contains(address, "://") {
		if _, _, err := net.SplitHostPort(address); err == nil {
			return address, true
		}
		return "", false
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || strings.Contains(u.Host, ",") || strings.HasSuffix(u.Scheme, "+srv") {
		return "", false
	}
	if u.Port() != "" {
		return u.Host, true
	}
	switch u.Scheme {
	case "mongodb":
		return net.JoinHostPort(u.Hostname(), "27017"), true
	case "http":
		return net.JoinHostPort(u.Hostname(), "80"), true
	case "https":
		return net.JoinHostPort(u.Hostname(), "443"), true
	}
	return "", false
}

// credentials matches user:password@ in a URL.
var credentials = regexp.MustCompile(`://([^:/@]+):[^@]*@`)

// Redact hides the password in a URL so addresses can be shown in the panel.
func Redact(address string) string {
	return credentials.ReplaceAllString(address, "://$1:***@")
}
