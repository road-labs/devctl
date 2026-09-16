# devctl

Run a repository's services locally from one panel: each on its own ports, with
its status, its logs, its dependencies and a key to restart it. Everything
devctl knows comes from one file at the root of your repository, `devctl.yaml`.

It is a terminal panel, not a daemon. No state is kept between runs, and quitting
stops everything it started.

```
go install github.com/road-labs/devctl@latest
devctl
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

Install it once and it is on your `PATH`, which is what makes `devctl` a plain
command in every repository, and what shell completion needs:

```
go install github.com/road-labs/devctl@latest
devctl
```

`go.mod` declares Go 1.27.1, so that is what building it needs.

You can pin it as a tool of a single repository instead, with
`go get -tool github.com/road-labs/devctl@latest` and `go tool devctl`, but then
it is not on your `PATH` and completion cannot hook it, so the plain install is
the recommendation.

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
suite loads, so it cannot drift from the format. [`example/`](example) is two
small projects you can actually run, to see one devctl peer with another and a
dependency switch between modes.

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

**Peered with a sibling devctl.** When you run devctl from several repositories at
once and some depend on others, a `peer` reads another devctl's live ports rather
than repeating a number that moves. It names that devctl by its `id` and the
service and port it declares. The sibling already listens on localhost, so there
is no tunnel; services reach it as `{{ name.port }}`.

```yaml
  - name: platform-api
    description: The platform gateway, read live from the platform repo
    peer: {id: platform, service: gateway, port: http}
```

See [Peering devctls together](#peering-devctls-together) for the `id` that makes
a devctl readable.

Mark a dependency `optional: true` and an unconfigured or unreachable one warns
rather than stops the run. Forwarded and peered ones are optional already.

## One dependency, several sources

Running a UI, you often want to point a dependency at a local service one minute
and a port-forwarded environment the next. A dependency declares those sources as
named `modes` and switches between them live: select its row, press `m`, and
pick one. The services that read it restart, so they come back pointed at the new source.

```yaml
  - name: platform
    description: The platform API
    default: local
    modes:
      - {name: local,   peer: {id: platform, service: gateway, port: http}}
      - {name: staging, port: 7100, forward: {cmd: "kubectl -n plat port-forward svc/gateway {{ platform.port.number }}:8080"}}
      - {name: shared,  env: PLATFORM_URL, example: https://platform.staging}
```

Each mode is one of the three source kinds. `default` names the one live at
start; `devctl.mine.yaml` can set a different one per machine, since which source
a developer uses day to day is their own business. Whatever reads the dependency
refers to it as `{{ platform.address }}`, which follows whichever mode is live.

Some config comes *with* a mode rather than being an address, an auth bundle that
differs local vs staging, say. Put it on the mode as `provides`, and every
service that depends on the dependency is handed it, swapped when the mode
switches:

```yaml
  - name: identity
    default: staging
    modes:
      - name: local
        peer: {id: platform, service: identity, port: grpc}
        provides: {OIDC_PROVIDER_URL: http://localhost:24700/, OIDC_CLIENT_ID: finance}
      - name: staging
        port: 9490
        forward: {cmd: "kubectl ... svc/identity {{ identity.port.number }}:9090"}
        provides: {OIDC_PROVIDER_URL: https://id.public.road.dev/, OIDC_CLIENT_ID: d99d247a-...}
```

One `m` on the identity row flips the address and the bundle together. A service
can still override a provided value in its own `env`.

## Peering devctls together

Give a devctl an `id` and it publishes its allocated ports for its siblings:

```yaml
id: platform
```

Another repository's devctl then reads them by that id with a `peer` dependency,
so `billing` finds `platform` on whatever port it actually got, with nobody
writing a number down. The socket lives under the system temp directory and is
removed when devctl quits. Start order does not matter: a devctl that peers with a
sibling not yet running shows `waiting`, and picks it up the moment it comes up,
restarting the running services that read it so they never stay on an empty
endpoint.
Two devctls cannot share one id; the second refuses to start, which catches a
stray copy.

A published devctl also carries the config its services `provides`, so a peer
inherits it. If `platform`'s identity service publishes the OIDC issuer and
client id, `billing` peering that service gets them without restating anything.
The service that owns a value states it once; everyone downstream reads it live.

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

Naming a target runs it. `devctl fraud-ui` starts fraud-ui, and `devctl fraud`
starts the profile's roots, without anything needing `autostart: true` in the
manifest; the backing services follow through `depends_on`. A bare `devctl` with
no target is the exception: there the manifest's own `autostart` flags decide
what comes up, so a repository can leave everything down on a bare run and still
have `devctl fraud` bring the fraud console up. A profile's `autostart` list, when
it sets one, stays definitive and overrides both.

The narrowing happens before anything else reads the manifest, so a profile
neither resolves dependencies it has no use for nor has its run refused over a
port belonging to a service it is not starting.

A profile can also say how its dependencies are wired, not just which things run.
Give it `modes` to start a multi-mode dependency in a chosen source:

```yaml
profiles:
  - name: ui-staging
    description: The UI, with the platform API forwarded from staging
    include: [ui]
    modes:
      platform: staging
```

`devctl ui-staging` runs the UI and starts `platform` forwarded rather than in
its default mode. `m` still switches at runtime; this only sets where it starts.

A profile picks each dependency's mode independently and can add `env` for the
run, so "auth against staging, data local" is one profile:

```yaml
profiles:
  - name: dev
    include: [ui]
    modes: {identity: staging, billing: local, pricing: local}
    env:   {APP_ENVIRONMENT: local}
```

The auth bundle comes with identity's `staging` mode (its `provides`), so it is
written once and every run that chooses that mode inherits it. The profile's
`env` is for config that belongs to the run rather than to one dependency.

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
| `m` | pick a dependency mode (opens a picker) |
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

## Completion

`devctl <TAB>` completes the run targets, the profiles and every single service,
task and dependency, read from the manifest in the current directory. It follows
you between repositories, since it reads whichever manifest you are standing in.

Load it for your shell (needs `devctl` on your `PATH`, so `go install` it):

```
# bash, in ~/.bashrc
source <(devctl -completion bash)

# zsh, in ~/.zshrc (after compinit)
source <(devctl -completion zsh)

# fish
devctl -completion fish > ~/.config/fish/completions/devctl.fish
```

Each script asks `devctl -complete` for the target list, so a new profile shows
up the moment it is in the manifest, with nothing to regenerate.

## Licence

MIT. See [LICENSE](LICENSE).
