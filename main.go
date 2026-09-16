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
	"github.com/road-labs/devctl/internal/peer"
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

// The skill and the schema reference are fetched, so they are whatever the
// project says today rather than whatever the binary was built with. Both go
// out under -skill, joined, because an agent handed a document with a link in
// it does not follow the link: it goes and greps the module cache instead, and
// then guesses. One command, one document, nothing left to find.
const (
	skillURL     = "https://raw.githubusercontent.com/road-labs/devctl/main/skills/devctl-manifest/SKILL.md"
	referenceURL = "https://raw.githubusercontent.com/road-labs/devctl/main/docs/manifest.md"
)

// fetchSkill prints the skill, the reference and a worked example as one
// document. A failure says where to read it instead, since a machine without
// network access is a fair reason to be reading a URL out of an error message.
func fetchSkill(out io.Writer) error {
	skill, err := fetchText(skillURL)
	if err != nil {
		return err
	}
	reference, err := fetchText(referenceURL)
	if err != nil {
		return err
	}
	fmt.Fprint(out, skill)
	fmt.Fprint(out, "\n---\n\n# Reference\n\n")
	fmt.Fprint(out, strings.TrimPrefix(reference, "# devctl.yaml\n"))
	// The example comes from the binary rather than the network: it is the file
	// this build's tests run against, so it describes what this devctl does.
	fmt.Fprint(out, "\n---\n\n# A worked example\n\n```yaml\n")
	fmt.Fprint(out, exampleManifest)
	fmt.Fprint(out, "```\n")
	return nil
}

func fetchText(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w\n\nread it at %s", err, url)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s says %s", url, res.Status)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func main() {
	manifest := flag.String("manifest", "", "path to "+ManifestName+" (default: found by walking up from the working directory)")
	check := flag.Bool("check", false, "validate the manifest, resolve and check dependencies, print the plan and exit")
	example := flag.Bool("example", false, "print a worked example manifest and exit")
	skill := flag.Bool("skill", false, "print the agent skill for writing a "+ManifestName+" and exit")
	completion := flag.String("completion", "", "print a shell completion script (bash, zsh or fish) and exit")
	complete := flag.Bool("complete", false, "list the run targets for shell completion and exit")
	flag.Parse()

	// Both print rather than write: what to do with them is the reader's
	// business, and a tool that drops files into a repository uninvited is a
	// tool people stop running.
	switch {
	case *example:
		fmt.Print(exampleManifest)
		return
	case *skill:
		if err := fetchSkill(os.Stdout); err != nil {
			fail(err)
		}
		return
	case *completion != "":
		if err := printCompletion(os.Stdout, *completion); err != nil {
			fail(err)
		}
		return
	case *complete:
		// The candidates a shell offers for `devctl <TAB>`. Silent on any error:
		// completion runs on every keypress and must never print a diagnostic.
		printTargets(*manifest)
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

	// Publish this devctl's ports, and the config each service provides, so a
	// sibling that peers with it by id reads the numbers it actually got and
	// inherits that service's config. Nothing is published without an id.
	if file.ID != "" {
		expand := ports.Expander{Ports: table, Values: values}
		provided := map[string]map[string]string{}
		for _, svc := range file.Services {
			if len(svc.Provides) == 0 {
				continue
			}
			resolved := map[string]string{}
			for k, v := range svc.Provides {
				// Publish the concrete value; a sibling cannot resolve our references.
				out, err := expand.Expand(v)
				if err != nil {
					fail(fmt.Errorf("service %s provides %s: %w", svc.Name, k, err))
				}
				resolved[k] = out
			}
			provided[svc.Name] = resolved
		}
		closer, err := peer.Serve(peer.Snapshot{ID: file.ID, Ports: table, Provides: provided})
		if err != nil {
			fail(err)
		}
		defer closer.Close()
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
		fmt.Printf("%-14s %-11s %s\n", d.Name, kind, depSummary(d, table, values, expand))
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

// depSummary is the one-line plan a dependency shows under -check, read through
// its mode live at start. A peered sibling is read now, so the plan says whether
// it is up; a multi-mode dependency is prefixed with the mode it will use.
func depSummary(d config.Dependency, table ports.Table, values ports.Values, expand ports.Expander) string {
	mode := d.Mode(d.DefaultMode())
	body := ""
	switch {
	case mode.Peered():
		switch snap, running, _ := peer.Read(mode.Peer.ID); {
		case !running:
			body = fmt.Sprintf("waiting for %s", mode.Peer.ID)
		default:
			if p, ok := snap.Port(mode.Peer.Service, mode.Peer.Port); ok {
				body = fmt.Sprintf("peered localhost:%d (from %s)", p, mode.Peer.ID)
			} else {
				body = fmt.Sprintf("waiting: %s has no %s.%s", mode.Peer.ID, mode.Peer.Service, mode.Peer.Port)
			}
		}
	case mode.Forwarded():
		// The command as it will actually run: it refers to its own local port.
		cmd, err := expand.Expand(mode.Forward.Cmd)
		if err != nil {
			cmd = mode.Forward.Cmd
		}
		body = fmt.Sprintf("forward localhost:%d via: %s", table[d.Name][ports.ForwardPort], cmd)
	default: // machine-provided
		value := values[deps.Key(d)]
		if d.HasModes() {
			value = values[deps.ModeKey(d.Name, mode.Name)]
		}
		if value == "" {
			body = fmt.Sprintf("not configured (%s)", mode.Env)
		} else {
			body = deps.Redact(value)
		}
	}
	if d.HasModes() {
		return mode.Name + ": " + body
	}
	return body
}

// printTargets lists what `devctl <name>` accepts for the manifest nearest the
// working directory: profiles, and every single service, task and dependency. It
// is what the completion scripts call. Every error is swallowed, since completion
// runs on a keystroke and a stray line would land in the user's prompt.
func printTargets(manifest string) {
	path, err := findManifest(manifest)
	if err != nil {
		return
	}
	file, err := config.LoadWithOverlay(path, filepath.Join(filepath.Dir(path), config.OverlayName))
	if err != nil {
		return
	}
	// One target per line as "name<TAB>kind": the shells split on the tab, using
	// the name to match and the kind as the description shown beside it.
	for _, t := range file.Targets() {
		fmt.Printf("%s\t%s\n", t.Name, t.Kind)
	}
}

// printCompletion writes the completion script for one shell. Each script calls
// `devctl -complete` for the target list, so completion follows the manifest in
// whatever directory the shell sits in, profiles and all.
func printCompletion(out io.Writer, shell string) error {
	switch shell {
	case "bash":
		fmt.Fprint(out, bashCompletion)
	case "zsh":
		fmt.Fprint(out, zshCompletion)
	case "fish":
		fmt.Fprint(out, fishCompletion)
	default:
		return fmt.Errorf("unknown shell %q; one of bash, zsh, fish", shell)
	}
	return nil
}

const bashCompletion = `# devctl bash completion. Load it with:  source <(devctl -completion bash)
_devctl() {
	local cur="${COMP_WORDS[COMP_CWORD]}"
	if [[ "$cur" == -* ]]; then
		COMPREPLY=($(compgen -W "-manifest -check -example -skill -completion -complete" -- "$cur"))
		return
	fi
	# bash shows no per-item description, so just the names (first tab field).
	COMPREPLY=($(compgen -W "$(devctl -complete 2>/dev/null | cut -f1)" -- "$cur"))
}
complete -F _devctl devctl
`

const zshCompletion = `#compdef devctl
# devctl zsh completion. Load it with:  source <(devctl -completion zsh)
_devctl() {
	# "name<TAB>kind" becomes "name:kind", which _describe shows as name -- kind.
	local -a targets
	targets=(${(f)"$(devctl -complete 2>/dev/null | sed 's/\t/:/')"})
	_describe -t targets 'run target' targets
}
compdef _devctl devctl
`

const fishCompletion = `# devctl fish completion. Install it with:
#   devctl -completion fish > ~/.config/fish/completions/devctl.fish
# devctl -complete prints "name<TAB>kind"; fish reads the tab as the description.
complete -c devctl -f -a "(devctl -complete 2>/dev/null)"
complete -c devctl -f -l manifest -d "path to devctl.yaml"
complete -c devctl -f -l check -d "validate and print the plan"
complete -c devctl -f -l example -d "print a worked example manifest"
complete -c devctl -f -l skill -d "print the agent skill"
complete -c devctl -f -l completion -d "print a completion script"
`
