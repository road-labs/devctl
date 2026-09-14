// Command devctl runs a repository's services locally from one panel: every
// service on its ports with its status and logs, the dependencies a run needs
// from outside the repository, and the one-shot tasks. Everything it knows
// comes from devctl.yaml at the root of the repository it is run in.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/road-labs/devctl/internal/config"
	"github.com/road-labs/devctl/internal/deps"
	"github.com/road-labs/devctl/internal/ports"
	"github.com/road-labs/devctl/internal/proc"
	"github.com/road-labs/devctl/internal/ui"
)

// ManifestName is the file devctl looks for. It is found by walking up from the
// working directory, the way git finds .git, so devctl runs from anywhere in a
// repository. The directory holding it is the root every relative path in it
// resolves against.
const ManifestName = "devctl.yaml"

// exampleManifest is a worked example of every feature, printed by -example. It
// is embedded rather than fetched: a starting point has to work on a plane, and
// it is the same file the tests run against, so it cannot rot.
//
//go:embed testdata/devctl.yaml
var exampleManifest string

// skillURL is the agent skill for writing a manifest, fetched rather than
// embedded so it is whatever the project says today and not whatever the
// binary was built with. `go install` leaves a user the binary and nothing
// else, so printing is the only way either of these reaches them.
const skillURL = "https://raw.githubusercontent.com/road-labs/devctl/main/skills/devctl-manifest/SKILL.md"

// fetchSkill prints the skill. A failure says where to read it instead, since
// a machine without network access is a fair reason to be reading a URL out of
// an error message.
func fetchSkill() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, skillURL, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w\n\nread it at %s", err, skillURL)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s says %s\n\nread it at %s", skillURL, res.Status, skillURL)
	}
	_, err = io.Copy(os.Stdout, res.Body)
	return err
}

func main() {
	manifest := flag.String("manifest", "", "path to "+ManifestName+" (default: found by walking up from the working directory)")
	check := flag.Bool("check", false, "validate the manifest, resolve and check dependencies, print the plan and exit")
	example := flag.Bool("example", false, "print a worked example manifest and exit")
	skill := flag.Bool("skill", false, "print the agent skill for writing a "+ManifestName+" and exit")
	flag.Parse()

	// Both print rather than write: what to do with them is the reader's
	// business, and a tool that drops files into a repository uninvited is a
	// tool people stop running.
	switch {
	case *example:
		fmt.Print(exampleManifest)
		return
	case *skill:
		if err := fetchSkill(); err != nil {
			fail(err)
		}
		return
	}

	// Anything left after the flags names what to run: a profile, or any single
	// service, task or dependency.
	targets := flag.Args()

	manifestPath, err := findManifest(*manifest)
	if err != nil {
		fail(err)
	}
	// Everything in the manifest is relative to the directory holding it.
	repoRoot := filepath.Dir(manifestPath)

	// A personal overlay beside it, if there is one, says how this machine
	// provides what the committed manifest asks for.
	overlayPath := filepath.Join(repoRoot, config.OverlayName)
	file, err := config.LoadWithOverlay(manifestPath, overlayPath)
	if err != nil {
		fail(err)
	}
	if err := ports.Validate(file); err != nil {
		fail(err)
	}

	// Narrowing happens here, before anything else looks at the manifest, so a
	// profile neither resolves dependencies it does not need nor has its run
	// refused over a port belonging to a service it is not starting.
	file, err = file.Select(targets)
	if err != nil {
		fail(err)
	}

	// Dependencies come from the machine: resolved from the environment or
	// .env, then checked, before anything starts against them.
	values, err := deps.Resolve(repoRoot, file.Dependencies)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	warnings, err := deps.Check(ctx, file.Dependencies, values)
	cancel()
	if err != nil {
		fail(err)
	}

	// A fixed port held by a leftover of an earlier devctl is offered for
	// reclaiming before allocation refuses it.
	if !*check {
		if err := reclaimLeftovers(repoRoot, file, os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
	}

	// Every port is decided once, here, before anything starts: services'
	// listeners and forwarded dependencies' local ports alike.
	table, portWarnings, err := ports.Allocate(file, ports.IsFree, ports.PickFree)
	if err != nil {
		fail(err)
	}
	warnings = append(warnings, portWarnings...)

	if *check {
		printSummary(file, table, values, warnings)
		return
	}

	program := tea.NewProgram(ui.New(repoRoot, file, table, values, warnings), tea.WithAltScreen())

	// The children live in their own process groups so devctl can stop them
	// together, which also means a closed terminal does not reach them. Turn
	// the signals that end devctl into an orderly shutdown instead.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		program.Send(ui.Shutdown{})
	}()

	if _, err := program.Run(); err != nil {
		fail(err)
	}
}

// reclaimLeftovers looks at every fixed port that is busy. When the holder is
// one of devctl's own children from an earlier run, judged by its command name
// and by running from inside this repository, it offers to kill it; anything
// else is left alone and allocation will refuse the port with the holder named.
// Only fixed ports are considered: a busy port that can move simply moves.
func reclaimLeftovers(repoRoot string, file *config.File, in *os.File, out *os.File) error {
	reader := bufio.NewReader(in)
	for _, svc := range file.Services {
		for _, declared := range svc.Ports {
			if !svc.FixedPorts && !declared.Fixed {
				continue
			}
			if ports.IsFree(declared.Number) {
				continue
			}
			pid, command, ok := ports.HolderPID(declared.Number)
			if !ok || !looksLikeOurs(svc, command) || !runsFromRepo(repoRoot, pid) {
				continue
			}
			fmt.Fprintf(out, "%s.%s: port %d is held by %s (pid %d), which looks like a leftover from an earlier devctl. Kill it? [y/N] ", svc.Name, declared.Name, declared.Number, command, pid)
			answer, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(answer)) != "y" {
				continue
			}
			if err := proc.Kill(pid, 3*time.Second); err != nil {
				return fmt.Errorf("%s.%s: %w", svc.Name, declared.Name, err)
			}
			fmt.Fprintf(out, "%s.%s: pid %d stopped\n", svc.Name, declared.Name, pid)
		}
	}
	return nil
}

// looksLikeOurs reports whether a process name matches what the service's
// command would run: the last path element of a `go run ./cmd/x` is the
// binary x; `npm run dev` for the console ends up as node or next-server.
func looksLikeOurs(svc config.Service, command string) bool {
	command = strings.ToLower(command)
	if command == "" {
		return false
	}
	fields := strings.Fields(svc.Cmd)
	for _, f := range fields {
		if base := strings.ToLower(filepath.Base(f)); base == command || strings.HasPrefix(command, base) {
			return true
		}
	}
	if strings.HasPrefix(svc.Cmd, "npm ") {
		return command == "node" || strings.HasPrefix(command, "next-server")
	}
	return false
}

// runsFromRepo reports whether the process was started from inside this
// repository. Without it, any Next dev server on 3000 reads as the console's
// leftover, and devctl offers to kill another project's UI. When the working
// directory cannot be read, the answer is no: the prompt kills something, so
// an unknown is not our business.
func runsFromRepo(repoRoot string, pid int) bool {
	cwd, ok := ports.HolderCWD(pid)
	if !ok {
		return false
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, cwd)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "devctl:", err)
	os.Exit(1)
}

// findManifest returns the absolute path of the manifest: the one given, or the
// nearest ManifestName at or above the working directory.
func findManifest(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, ManifestName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no " + ManifestName + " in this directory or any parent; pass -manifest")
		}
		dir = parent
	}
}

func printSummary(file *config.File, table ports.Table, values ports.Values, warnings []string) {
	expand := ports.Expander{Ports: table, Values: values}
	for _, d := range file.Dependencies {
		kind := "dependency"
		if d.Optional {
			kind = "optional"
		}
		switch {
		case d.Forwarded():
			// The command as it will actually run: it refers to its own allocated port.
			cmd, err := expand.Expand(d.Forward.Cmd)
			if err != nil {
				cmd = d.Forward.Cmd
			}
			fmt.Printf("%-14s %-11s localhost:%d via: %s\n", d.Name, kind, table[d.Name][ports.ForwardPort], cmd)
		case values[deps.Key(d)] == "":
			fmt.Printf("%-14s %-11s not configured (%s)\n", d.Name, kind, d.Env)
		default:
			fmt.Printf("%-14s %-11s %s\n", d.Name, kind, deps.Redact(values[deps.Key(d)]))
		}
	}
	for _, s := range file.Services {
		watch := ""
		if len(s.Watch) > 0 {
			watch = "  watch " + strings.Join(s.Watch, ",")
		}
		fmt.Printf("%-14s %-11s %-44s %s%s\n", s.Name, "service", table.Describe(s), s.Cmd, watch)
	}
	for _, t := range file.Tasks {
		fmt.Printf("%-14s %-11s %s\n", t.Name, "task", t.Description)
	}
	for _, w := range warnings {
		fmt.Println("warning:", w)
	}
}
