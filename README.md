# devctl

Run a repository's services locally from one panel: each on its own ports, with
its status, its logs, its dependencies and a key to restart it. Everything
devctl knows comes from one file at the root of your repository, `devctl.yaml`.

It is a terminal panel, not a daemon. Nothing is installed, no state is kept
between runs, and quitting stops everything it started.

```
go tool devctl
```

## Why

A repository with more than a couple of services accumulates a README section
that says: export these variables, start this, then this, in that order, and if
port 8080 is taken, good luck. That text goes stale, and every developer keeps a
slightly different version of it in their shell history.

devctl replaces it with a file. The file is checked at start, so a mistake in it
is an error message rather than a service that half-runs, and the ports are
allocated once so two things cannot silently fight over one number.

## Install

devctl is a Go program, so the usual ways all work. As a pinned tool of the
repository it serves, which is what most projects want:

```
go get -tool github.com/road-labs/devctl@latest
go tool devctl
```

Or standalone:

```
go install github.com/road-labs/devctl@latest
```

`go.mod` declares Go 1.27.1, so that is what building it needs. `go tool`
itself arrived in 1.24.

## The manifest

`devctl.yaml` sits at the root of your repository. devctl finds it by walking up
from the working directory, the way git finds `.git`, so it runs from anywhere
in the tree. Every relative path inside it resolves against the directory that
holds it.

```yaml
dependencies:
  - name: db
    description: Postgres holding every service's data
    env: DATABASE_URL
    example: postgres://localhost:5432/shop
    kind: tcp

services:
  - name: catalogue
    description: Product catalogue, the API everything else reads
    depends_on: [db]
    ports:
      - {name: http, port: 7200, kind: http, fixed: true}
      - {name: grpc, port: 7201, kind: grpc}
    autostart: true
    dir: services/catalogue
    watch: [services/catalogue, libs]
    cmd: go run ./cmd/catalogue
    listen: {http: HTTP_ADDR, grpc: GRPC_ADDR}
    env:
      DATABASE_URL: "{{ db.address }}"

  - name: storefront
    depends_on: [catalogue]
    ports:
      - {name: web, port: 7300, kind: http, fixed: true}
    autostart: true
    dir: services/storefront
    cmd: npm run dev
    listen: {web: {env: PORT, format: number}}
    env:
      CATALOGUE_URL: "{{ catalogue.http.url }}"

tasks:
  - name: seed
    description: Wipe the database and seed a shop to click around in
    depends_on: [db]
    dir: services/catalogue
    cmd: go run ./cmd/seed
```

[`docs/manifest.md`](docs/manifest.md) documents every key.
[`testdata/devctl.yaml`](testdata/devctl.yaml) is a worked example that the test
suite loads, so it cannot drift from the format.

## Nobody types an address twice

Ports are declared once and allocated when devctl starts. A free default is
kept. A default something else already holds is replaced by a free port and
reported, marked `*` in the panel. A port marked `fixed: true` refuses to start
instead, which is what you want for anything written down outside the process:
a URL persisted in a database, an OAuth redirect, a browser bookmark.

Anything that needs an address refers to it, and follows it if it moves:

| Reference | Expands to |
| --- | --- |
| `{{ catalogue.grpc }}` | `localhost:7201` |
| `{{ catalogue.grpc.number }}` | `7201` |
| `{{ catalogue.http.url }}` | `http://localhost:7200` |
| `{{ db.address }}` | whatever `DATABASE_URL` holds |

A reference that names something undeclared is an error at load, so a typo
fails before anything starts.

## Dependencies

What a run needs from outside the repository is a dependency, and there are two
kinds.

**Provided by the machine.** A database, usually. `env` names the variable
holding its address, read from your environment or the repository's git-ignored
`.env`. If it is missing, devctl prints the line to add and exits. If it does
not answer, devctl says so and exits, so nothing starts against a database that
is not there.

```
# .env at the repository root
DATABASE_URL=postgres://localhost:5432/shop
```

`.env` is not loaded into your services. devctl reads it only for the addresses
of dependencies, under the exact names they declare in `env`, and your own
environment beats the file. A service's own variables belong in its `env` in the
manifest; putting them in `.env` does nothing.

What a service is given is your shell's environment, then its listeners, then
its declared `env` with every reference resolved. Later wins, so the manifest
overrides anything you happened to have exported.

**Forwarded by devctl.** Something in a remote cluster, reached through a
tunnel. It is a row with a local port and the command that opens it; select it
and press `s`. Nothing waits for a tunnel, so without one only the screens that
need it fail.

```yaml
  - name: payments
    kind: tcp
    port: 7100
    forward: {cmd: "kubectl -n shop port-forward svc/payments {{ payments.port.number }}:8080"}
```

The command states the local port once, by referring to the port devctl gave it.

Mark a dependency `optional: true` and an unconfigured or unreachable one warns
rather than stops the run.

## Running part of it

A repository with thirty services rarely wants all thirty up. Name the subsets
people actually work on:

```yaml
profiles:
  - name: fraud
    description: The fraud console and what it reads
    include: [fraud-ui, fraud-review]
```

```
devctl fraud        # the profile
devctl fraud-ui     # any single name works too
devctl              # everything
```

What a profile lists are the roots. What they need comes with them, following
`depends_on` and any `{{ reference }}`, because a service that reads another's
address needs it up whether or not it declared so.

The narrowing happens before anything else reads the manifest, so a profile
neither resolves dependencies it has no use for nor has its run refused over a
port belonging to a service it is not starting.

## Your machine is not everyone's

`devctl.mine.yaml`, beside the manifest and git-ignored, is merged over the top
when it exists. The committed manifest says what the repository needs; this says
how your machine provides it.

It exists because those are different questions. Everyone on a project needs a
database. Whether it arrives from Docker, a container runtime or a box under
someone's desk is nobody else's business, and a committed manifest that picks
one forces it on everybody.

```yaml
# devctl.mine.yaml
services:
  - name: catalogue
    ports:
      - {name: grpc, port: 9999}   # 7201 is taken on this machine, permanently
    autostart: false               # I start this one by hand
    env:
      LOG_LEVEL: debug
```

Merging is by name and by key, so you change one thing without restating what
surrounds it: the other ports stay, the other environment variables stay, the
command stays. An entry the manifest does not have is added, so you can keep a
scratch service or a personal task without committing it.

Because the merge happens on the file rather than on parsed values, `false` and
`0` in the overlay mean what they say rather than reading as "not set".

## The panel

One table. Dependencies, then services with a line per listener under each, then
tasks, separated by a rule. `TYPE` says which is which, `ENDPOINT` carries a
dependency's address or a listener's port, `RESTARTS` counts how many times a
process has come back, and `WATCH` says whether it restarts itself. The header
above it carries what devctl is running for, the keys, and what the markers
mean.

| | |
| --- | --- |
| `s` | start a service, run a task, open a forward |
| `x` | stop |
| `r` | restart |
| `w` | auto-restart on or off for this service |
| `d` | describe the selected row |
| `l` | logs for this row |
| `L` | logs for everything |
| `j` `k` | move |
| `pgup` `pgdn` | to the band above or below |
| `q` | quit, stopping everything devctl started |

## Describe

`d` opens everything devctl knows about a row: its status and uptime, the
command it runs and the directory it runs in, its listeners with declared
against actual ports, the environment it is handed with every reference
resolved, and the dependency graph both ways.

Both directions matter. What a row depends on is in the manifest; what depends
on *it* is not, and that is the question a dependency's owner has, which is who
breaks if this is down.

The environment is shown even when it cannot be resolved, because a reference
that does not resolve is exactly why the row will not start.

## Logs

`l` opens the selected row's output, `L` every process merged in arrival order
with each line prefixed by its service. `←` `→` or `tab` move between them, `g`
and `G` jump to the ends, `f` follows the tail, which also stops when you scroll
up and resumes when you reach the bottom. `esc` returns.

That is the last 2000 lines of each process, kept in memory and gone when devctl
is. To keep them, give the manifest a log directory:

```yaml
logs:
  dir: .devlogs
  max_size: 2MB
```

One `<name>.log` per row, appended to across restarts, so a service that died
while you were reading something else can still be read afterwards. A file
already over the cap when its process starts is emptied and begun again. Both
`d` and the log view print the path. Gitignore the directory.

## Auto-restart

A service with a `watch` list restarts when a Go file, `go.mod` or `go.sum`
under those directories changes, after a short quiet period so a multi-file save
restarts once. The `WATCH` column says `on` or `off`; `w` toggles it. Leave the
list empty for anything that reloads itself, such as a Next.js dev server.

Services marked `autostart` in the manifest come up when devctl does, so there
is no key for it.

## Starting a manifest

`go install` leaves you a binary and nothing else, so the two things that help
you write a manifest are printed by the binary rather than shipped as files.

```
devctl -example
```

A worked example of every feature, which is also the manifest devctl's own
tests run against, so it cannot drift from what the code does. Redirect it into
`devctl.yaml` and cut it down, or read it beside your own.

```
devctl -skill
```

An agent skill for writing one, with the full schema reference and a worked
example appended, so it is one document with nothing left to go and find. The
skill itself covers where in a repository the answers already are, starting
with the Makefile and the Taskfile, then the handful of questions only a person
can answer, then the mistakes worth naming.

The skill and the reference are fetched, so they are whatever the project says
today rather than whatever your binary was built with. Save it where your agent
looks:

```
mkdir -p .claude/skills/devctl-manifest
devctl -skill > .claude/skills/devctl-manifest/SKILL.md
```

## Checking

```
devctl -check
```

Validates the manifest, resolves and pings the dependencies, prints the
allocation and exits without opening the panel. Useful in CI to keep the
manifest honest.

```
devctl -manifest path/to/devctl.yaml
```

Points at a manifest explicitly, rather than walking up to find one.

## Licence

MIT. See [LICENSE](LICENSE).
