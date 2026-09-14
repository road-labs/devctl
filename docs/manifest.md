# devctl.yaml

Every key devctl reads, with what it does and when you need it. The file lives
at the root of your repository; devctl finds it by walking up from the working
directory. Every relative path inside it resolves against the directory holding
it, never against your shell's location.

[`testdata/devctl.yaml`](../testdata/devctl.yaml) is a worked example using all
of this, and the test suite loads it, so it cannot drift from the format.

The file has three top-level keys, all optional except `services`:

```yaml
dependencies: []   # what a run needs from outside the repository
services: []       # what devctl runs and keeps running
tasks: []          # one-shot commands, run on demand
```

Any other top-level key is ignored, so `x-anything:` is a good home for YAML
anchors. Stating a value once and aliasing it is the only way to keep two
places in agreement.

```yaml
x-shared:
  api_key: &api_key dev-key-0000
```

## services

What devctl runs. Each needs at least a `name` and a `cmd`.

| Key | Type | Meaning |
| --- | --- | --- |
| `name` | string | Unique across services and tasks. Used in references and shown in the panel. |
| `description` | string | One line, shown beside the name. |
| `cmd` | string | Run through `sh -c`. May contain references. Required. |
| `dir` | string | Working directory, relative to the manifest. Defaults to the root. |
| `ports` | list | Listeners, see below. A service may have none. |
| `listen` | map | Which environment variable carries each listener's address, see below. |
| `env` | map | Extra environment. Values may contain references. |
| `depends_on` | list | Names of services and dependencies started or checked first. |
| `autostart` | bool | Started by `a`, and on launch. |
| `watch` | list | Directories whose Go source changes restart this service. |
| `fixed_ports` | bool | Pins every port. Prefer `fixed` on the one port that needs it. |

### ports

A service can expose several listeners, so each is named and described
separately.

```yaml
    ports:
      - {name: http, port: 7200, kind: http, fixed: true, description: public API}
      - {name: grpc, port: 7201, kind: grpc}
```

| Key | Type | Meaning |
| --- | --- | --- |
| `name` | string | Unique within the service. The second half of a reference. |
| `port` | int | The default. 1 to 65535. |
| `kind` | string | `http` or `grpc`. Shown in the panel; `http` makes `.url` meaningful. |
| `description` | string | What this listener serves. |
| `fixed` | bool | Refuse to start rather than move. See [Pinning](#pinning). |

### listen

Maps a declared port name to the environment variable the process reads for
that listener. Two forms, because programs disagree about what they want:

```yaml
    listen:
      http: HTTP_ADDR                      # HTTP_ADDR=:7200
      web: {env: PORT, format: number}     # PORT=7300
```

`format` is `addr` (the default) for `:<port>`, or `number` for the bare port.
Next.js and anything else that wants a number rather than an address needs the
second form.

A `listen` entry naming a port the service does not declare is an error at load.

## dependencies

What a run needs that devctl does not start. Checked before anything runs, so a
missing database is a clear message rather than a service crash.

| Key | Type | Meaning |
| --- | --- | --- |
| `name` | string | Unique. Used in `{{ name.address }}` or `{{ name.port }}`. |
| `description` | string | One line, shown in the panel. |
| `kind` | string | How it is checked: `mongo` for a driver ping, `tcp` for a dial. Defaults to `tcp`. |
| `optional` | bool | Warn rather than stop when missing or unreachable. |
| `env` | string | Machine-provided: the variable holding its address. |
| `example` | string | Shown when that variable is missing, so the fix is copy-paste. |
| `port` | int | Forwarded: the local port. |
| `forward` | map | Forwarded: `{cmd, dir}` opening the tunnel. |

### Provided by the machine

`env` names the variable holding the address, read from your environment first,
then the repository's `.env`. Missing, devctl prints the line to add and exits.
Unreachable, it says so and exits.

```yaml
  - name: db
    description: Postgres holding every service's data
    env: DATABASE_URL
    example: postgres://localhost:5432/shop
    kind: tcp
```

Services reach it as `{{ db.address }}`.

### Forwarded by devctl

A local port and the command that makes it reachable. It becomes a row you
start with `s` and stop with `x`. A forwarded dependency is optional by nature:
nothing waits for a tunnel.

```yaml
  - name: payments
    kind: tcp
    port: 7100
    forward: {cmd: "kubectl -n shop port-forward svc/payments {{ payments.port.number }}:8080"}
```

The local port is allocated from the same table as a service's, and the command
refers to it rather than repeating the number. Services reach it as
`{{ payments.port }}`.

A dependency is one or the other. `env` together with `port` and `forward` is an
error, as is a `port` with no `forward`, because neither says clearly where the
address is meant to come from.

## tasks

One-shot commands: seeding a database, running a migration, generating
fixtures. They appear under the services; `s` runs the selected one and its
output goes to the log view, ending in `done` or `failed (n)`.

| Key | Type | Meaning |
| --- | --- | --- |
| `name` | string | Unique across services and tasks. |
| `description` | string | What it does. Say so if it is destructive. |
| `cmd` | string | Run through `sh -c`. Required. |
| `dir` | string | Working directory, relative to the manifest. |
| `env` | map | Extra environment. May contain references. |
| `depends_on` | list | Checked or started first. |

## logs

Without this block devctl keeps the last 2000 lines of each process in memory
and nothing else, which is gone when devctl is. With it, every process also
appends to its own file.

```yaml
logs:
  dir: .devlogs
  max_size: 2MB
```

| Key | Type | Meaning |
| --- | --- | --- |
| `dir` | string | Relative to the repository root, so it can be gitignored. Created if missing. |
| `max_size` | string | Optional. `2MB`, `512KB`, `1GB`, or a plain byte count. |

One `<name>.log` per dependency, service and task, opened for append, so a
restart continues the same file and a crash can be read after the panel has
moved on. `d` and the log view both print the path.

A file already over `max_size` when its process starts is **emptied**, and a new
one begins. One check, at the one moment a development loop reaches often
enough for it to matter: a panel and its services are started many times a day.
The cost of that simplicity is that a single very long run can take a file past
the cap; stopping and starting it brings it back.

A `max_size` that cannot be read is an error at load rather than a silent zero,
because a log that was meant to be capped and is not is a disk that fills up
overnight.

Pick it against the whole directory, not one file: the cap applies per row. A
repository with a dozen services at `2MB` settles around 25MB, which is already
several thousand lines per service and far more than anyone reads to diagnose a
crash.

## References

Any `cmd` or `env` value may refer to a port or a dependency address. The
reference is resolved after allocation, so it carries whatever port the thing
actually got.

| Form | Expands to |
| --- | --- |
| `{{ service.port }}` | `localhost:<port>` |
| `{{ service.port.number }}` | `<port>` |
| `{{ service.port.url }}` | `http://localhost:<port>` |
| `{{ dependency.address }}` | the value of its `env` variable |
| `{{ dependency.port }}` | `localhost:<port>` for a forwarded one |

A reference to something undeclared is an error at load, so a typo fails before
anything starts rather than at the moment a service needs the address.

## Pinning

Ports are allocated once, when devctl starts. A free default is kept. A default
something else holds is replaced by a free port, reported as a warning and
marked `*` in the panel.

`fixed: true` changes that: the run stops instead, naming the process holding
the port. Use it only where the number is written down somewhere devctl cannot
reach, and where moving would therefore break something:

- a URL persisted in a database, such as an OAuth redirect or a SAML ACS
- an address another repository reaches by number
- something a person has bookmarked

A port that other services resolve through a reference is never pinned: they are
handed whatever it was given.

If several repositories reach each other by number, give those ports a range of
their own, away from what a port-forward or another tool is likely to take, and
pin them there. Pinning a common port like 8080 turns a routine collision into a
stopped run.

## devctl.mine.yaml

A file of the same shape beside the manifest, git-ignored, merged over it when
it exists. The committed manifest says what the repository needs; this says how
one machine provides it.

Merge rules:

- mappings merge key by key, recursively;
- a list whose entries are mappings with a `name` merges by that name, so a
  single port or environment variable can be changed without restating the
  service around it, and an entry the manifest does not have is added;
- anything else the overlay replaces outright.

The merge happens on the documents, before either becomes a manifest, so a key
present in the overlay wins even when its value is `false` or `0`. Setting
`autostart: false` therefore turns autostart off, rather than reading as unset.

The result is validated as a whole. When the overlay is what breaks it, the
error names the overlay, since the committed manifest is presumably fine.

```yaml
# devctl.mine.yaml
services:
  - name: catalogue
    ports:
      - {name: grpc, port: 9999}
    autostart: false
```

Add it to `.gitignore`. It is personal by definition, and a committed one is
just a second manifest nobody agreed to.

## Validation

`devctl -check` loads the manifest, resolves and pings the dependencies, prints
the allocation and exits. It is worth running in CI: it catches a reference to a
service that has been renamed, two things declaring one port, and a `listen`
naming a port that no longer exists.

The load itself rejects:

- no services, or two with the same name
- a service or task without a `cmd`
- a task named like a service
- a port outside 1 to 65535, two ports with one name, a `kind` other than
  `http` or `grpc`
- a `listen` entry for an undeclared port
- a dependency with neither `env` nor `forward`, one with both, a `port` with no
  `forward`, or a `kind` other than `mongo` or `tcp`
- `depends_on` naming something that does not exist
- a reference that does not resolve
