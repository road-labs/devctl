# devctl

Run a repository's services locally from one panel: each on its own ports, with
its status, its logs and a key to restart it. Everything devctl knows comes from
one file at the root of your repository, `devctl.yaml`.

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

Requires Go 1.24 or newer for `go tool`; the program itself builds with older
versions.

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

## Keys

| | |
| --- | --- |
| `s` | start a service, run a task, open a forward |
| `x` | stop |
| `r` | restart |
| `a` | start everything marked `autostart` |
| `w` | auto-restart on or off for this service |
| `l` | show or hide the log pane |
| `L` | full-screen logs |
| `j` `k` | move |
| `q` | quit, stopping everything devctl started |

## Logs

The pane under the table tails the selected row. `L` opens the full-screen view:
every process merged in arrival order with each line prefixed by its service, or
one service on its own. `←` `→` or `tab` move between them, `g` and `G` jump to
the ends, `f` follows the tail, which also stops when you scroll up and resumes
when you reach the bottom. `esc` returns.

## Auto-restart

A service with a `watch` list restarts when a Go file, `go.mod` or `go.sum`
under those directories changes, after a short quiet period so a multi-file save
restarts once. `↻` marks it; `w` turns it off per service. Leave the list empty
for anything that reloads itself, such as a Next.js dev server.

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
