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
| `provides` | map | Config this service hands its consumers, local and peered, see [provides](#provides-config-that-rides-a-mode). |
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

A dependency has one source, or several it switches between. The source is one
of three kinds: provided by the machine (`env`), forwarded by devctl (`port` +
`forward`), or peered with a sibling devctl (`peer`).

| Key | Type | Meaning |
| --- | --- | --- |
| `name` | string | Unique. Used in `{{ name.address }}` or `{{ name.port }}`. |
| `description` | string | One line, shown in the panel. |
| `kind` | string | Reachability check label. `mongo` and `tcp` both dial the address; `mongo://` defaults the port to 27017. Defaults to `tcp`. |
| `optional` | bool | Warn rather than stop when missing or unreachable. |
| `env` | string | Machine-provided: the variable holding its address. |
| `example` | string | Shown when that variable is missing, so the fix is copy-paste. |
| `port` | int | Forwarded: the local port. |
| `forward` | map | Forwarded: `{cmd, dir}` opening the tunnel. |
| `autostart` | bool | Forwarded: open the tunnel when devctl starts, rather than on `s`. |
| `peer` | map | Peered: `{id, service, port}` naming a sibling devctl's listener. |
| `provides` | map | Env handed to services that depend on this, see below. |
| `modes` | list | Several named sources to switch between, see below. |
| `default` | string | Which mode is live at start. Defaults to the first. |

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

A dependency is one kind at a time. `env` together with `port` and `forward`, or
`env` together with `peer`, is an error, as is a `port` with no `forward`,
because none of them says clearly where the address is meant to come from.

### Peered with a sibling devctl

When you run devctl from several repositories at once and some depend on others,
a `peer` reads another devctl's live ports rather than repeating a number that
moves. It names that devctl by its `id` and the service and port it declares.
The sibling already listens on localhost, so there is no tunnel: devctl reads the
number the sibling actually got and services reach it as `{{ name.port }}`.

```yaml
  - name: platform-api
    description: The platform gateway, read live from the platform repo
    peer: {id: platform, service: gateway, port: http}
```

A peered dependency is optional by nature, like a forward: nothing waits for a
sibling. Until the sibling with that `id` is running the row shows `waiting`, and
it becomes `peered` the moment the sibling comes up. The sibling must set a
top-level [`id`](#id) for this to find it.

A peer also **inherits the config the peered service publishes**. If the sibling's
`identity` service has `provides`, a service depending on this peer is handed
them, resolved by the sibling, so config the sibling owns is stated once and read
live over the socket. A mode's own `provides` override an inherited value.

### Several sources (modes)

A dependency you point at a local service one minute and a port-forwarded
environment the next declares its sources as named `modes` and switches between
them live: press `m` on its row and pick one from the list. `default` names the
one live at start.

```yaml
  - name: platform
    description: The platform API
    default: local
    modes:
      - {name: local,   peer: {id: platform, service: gateway, port: http}}
      - {name: staging, port: 7100, forward: {cmd: "kubectl -n plat port-forward svc/gateway {{ platform.port.number }}:8080"}}
      - {name: shared,  env: PLATFORM_URL, example: https://platform.staging}
```

Each mode is one of the three source kinds, written the way it would be inline.
A dependency with modes carries no inline source of its own, and it is optional
by nature: a mode you can switch away from is not one anything waits for.

**A dependency with modes is referenced only as `{{ name.address }}`**, which
resolves to a dialable address whichever mode is live: the env value, or
`localhost:<port>` for a forward or a peer. The other reference forms
(`{{ name.port }}`, `.number`, `.url`) belong to a single-source forward or peer,
where there is only one shape to resolve.

`devctl.mine.yaml` can set a different `default` per machine, since which source
a developer uses day to day is a personal choice. It overlays the `default`
without restating the modes.

### provides: config that rides a mode

Some config is a function of which mode a dependency is in, not an address. The
identity provider reached locally has one OIDC issuer URL and client id; reached
through staging it has another. That belongs on the mode, as `provides`: env
handed to every service that depends on the dependency, swapped with the address
when the mode switches.

```yaml
  - name: identity
    default: staging
    modes:
      - name: local
        peer: {id: platform, service: identity, port: grpc}
        provides:
          OIDC_PROVIDER_URL: http://localhost:24700/
          OIDC_CLIENT_ID: finance
      - name: staging
        port: 9490
        forward: {cmd: "kubectl ... svc/identity {{ identity.port.number }}:9090"}
        provides:
          OIDC_PROVIDER_URL: https://id.public.road.dev/
          OIDC_CLIENT_ID: d99d247a-...
```

A service that `depends_on: [identity]` is handed `OIDC_PROVIDER_URL` and
`OIDC_CLIENT_ID`, and `m` on the identity row (or a profile choosing its mode)
swaps both. The service's own `env` still wins over a provided value, so it is a
default it can override. Values may contain references. A single-source
dependency can carry `provides` inline, the same as a mode.

**A service can carry `provides` too**, and then it publishes them: the config a
service hands its consumers, resolved and put on this devctl's socket, so a
sibling that peers with the service inherits it. The value is stated once, by the
service that owns it. See [Peered with a sibling devctl](#peered-with-a-sibling-devctl).

Config that belongs to the run rather than to one dependency's mode goes in a
profile's [`env`](#modes-how-a-profile-wires-its-dependencies) instead.

## id

The name this devctl goes by to its siblings. Set it, and devctl publishes its
allocated ports, and the config each service `provides`, on a socket in a shared
temp directory keyed by the id, so another repository's devctl can read them with
a [`peer`](#peered-with-a-sibling-devctl) dependency. Without an id nothing is
published and nothing changes.

```yaml
id: platform
```

The socket is `${TMPDIR}/devctl/<id>.sock`, created when devctl starts and
removed when it quits. Two devctls cannot share one id: the second refuses to
start rather than fight over the socket, which is how a stray copy is caught. A
socket left behind by a devctl that did not clean up is taken over.

Ports are allocated once at start and do not move, so what a sibling reads is
fixed for the run. Start order does not matter: a devctl that peers with a sibling
not yet running shows `waiting` and picks the sibling up when it appears. A
service that had already started against a not-yet-resolved peer is restarted
when it resolves, so it never stays on the empty endpoint it booted with.

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

## profiles

Optional. A repository with thirty services rarely wants all thirty up, and the
subset somebody works on is usually stable enough to name.

```yaml
profiles:
  - name: fraud
    description: The fraud console and what it reads
    include: [fraud-ui, fraud-review]
```

| Key | Type | Meaning |
| --- | --- | --- |
| `name` | string | Unique across services, tasks, dependencies and other profiles. |
| `description` | string | What the subset is for. |
| `include` | list | Services, tasks, dependencies, or other profiles. |
| `modes` | map | Starting mode per multi-mode dependency, see below. |
| `env` | map | Run-level env for this profile, over every service's own, see below. |
| `autostart` | list | The services and forwards that come up for this run, see below. |

```
devctl fraud        # the profile
devctl fraud-ui     # any single name works too, without declaring a profile
devctl              # everything, as before
```

### Modes: how a profile wires its dependencies

A profile can say not just what runs but how a dependency is wired, by naming a
[mode](#several-sources-modes) for it. This is the difference between "run the
UI" and "run the UI against forwarded staging rather than the local services":

```yaml
profiles:
  - name: ui-staging
    description: The UI, with the platform API forwarded from staging
    include: [ui]
    modes:
      platform: staging
```

`devctl ui-staging` runs the UI's closure and starts the `platform` dependency
in its `staging` mode instead of its manifest default. It is a starting
configuration, not a lock: `m` still switches at runtime. A key must name a
dependency that has modes, and the value one of that dependency's mode names, or
the manifest is rejected at load. Two selected profiles that set one dependency
to different modes is an error, since the run cannot honour both.

A profile can also carry `env`: run-level config for that launch, applied to
every service and task in it, over their own env.

```yaml
profiles:
  - name: dev
    include: [ui]
    modes: {identity: staging, billing: local, pricing: local}
    env:   {APP_ENVIRONMENT: local}
```

Use it for config that belongs to the *run* rather than to any one dependency's
mode. Config that follows a dependency's mode, like an auth bundle that changes
with the identity source, belongs in that mode's [`provides`](#provides-config-that-rides-a-mode);
putting it on the profile would repeat it in every profile that shares the mode
and let a profile contradict the mode it chose. The two compose: a profile picks
each dependency's mode (each mode brings its own `provides`) and adds only what
is genuinely run-wide.

A profile can also set `autostart`: the services and forwarded dependencies that
come up on their own for that run. It is the whole set, overriding each thing's
own `autostart`, so a profile both starts what it lists and leaves down what it
does not.

```yaml
profiles:
  - name: local-auth-stack
    include: [console, identity, authorization]
    autostart: [console, identity, authorization]
```

This is how one run brings up a local stack while the default run brings up the
forwards it replaces: give the forwards `autostart: true` in the base, and the
profile's `autostart` list, omitting them, leaves them down for its run.

**What a profile lists are the roots, not the whole set.** Whatever they need
comes with them: `depends_on`, transitively, and anything a `{{ reference }}`
names, because a service that reads another's address needs it up whether or not
it declared so.

Narrowing happens before anything else reads the manifest, which is the point:
a profile does not resolve dependencies it has no use for, and its run cannot be
refused over a port belonging to a service it is not starting. `devctl fraud` on
a machine with no cluster access works even when the wider manifest forwards
three things from staging.

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
| `{{ dependency.address }}` | its `env` value, or a dialable address for a multi-mode one |
| `{{ dependency.port }}` | `localhost:<port>` for a forwarded or peered one |

A multi-mode dependency is referenced only as `{{ name.address }}`: it is the one
form that resolves whichever mode is live. A single-source forwarded or peered
dependency uses `{{ name.port }}` and its `.number` and `.url` forms.

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
- a dependency with none of `env`, `forward` or `peer`, or with more than one; a
  `port` with no `forward`; a `peer` missing `id`, `service` or `port`; or a
  `kind` other than `mongo` or `tcp`
- a dependency with both `modes` and an inline source, a mode without a name, two
  modes with one name, or a `default` naming no mode
- a profile `modes` entry naming something that is not a dependency, a dependency
  with a single source, or a mode that dependency does not have
- a profile `autostart` entry naming something that is not a service or dependency
- `depends_on` naming something that does not exist
- a reference that does not resolve, in a `cmd`, an `env`, a mode's `provides`,
  or a profile's `env`
