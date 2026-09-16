# A worked example you can run

Two small projects that depend on each other, so you can see the pieces that are
hard to picture from the reference: a devctl reading a **sibling devctl's** live
ports, and a dependency you **switch between modes** while it runs.

Everything here is standard-library Go in its own module, so nothing to install.
You need devctl on your path (or run it with `go run github.com/road-labs/devctl`).

```
example/
  platform/   an API on a dynamic port, published under id: platform
  billing/    needs the platform API; gets it by peering, or from a local mock
```

## Peering: billing reads platform's live port

Run each project in its own terminal.

```
cd example/platform && devctl
```

The `api` service comes up on a port devctl picked. Because the manifest sets
`id: platform`, that port is now published for siblings.

```
cd example/billing && devctl
```

The `platform` dependency row starts on its `local` mode and shows **peered**,
having read the api's real port from the platform devctl. The `billing` service
logs `platform says: hello from the platform api ...` every few seconds. Press
`l` on the billing row to watch it.

Start order does not matter. Start billing first and its `platform` row shows
**waiting**; start the platform devctl and it flips to **peered** on its own,
and billing starts talking to it.

## Modes: switch platform to a local mock

You do not even need the platform devctl. In the billing panel, put the cursor
on the `platform` row and press **m**. A picker opens listing the modes; choose
`mock`. Press **s** to start the mock (a tiny local server on the forwarded
port), and billing restarts pointed at it: now it logs `platform says: hello from
the MOCK platform`. Press **m** again and pick `local` to go back.

That is the whole idea: one dependency, several sources, switched live, with
everything that reads it following along.

A profile can bake that choice in. `billing` declares a `mock` profile that
starts `platform` in mock mode:

```
cd example/billing && devctl mock
```

Now the `platform` row comes up on `mock` from the start; press `s` to run the
mock server. A profile sets not just what runs but how it is wired.

## What each feature looks like here

- **id + peer** — `platform/devctl.yaml` sets `id: platform`;
  `billing/devctl.yaml` reads it with `peer: {id: platform, service: api, port: http}`.
- **modes + switching** — billing's `platform` dependency lists a `local` (peer)
  and a `mock` (forward) mode with `default: local`, switched with `m`.
- **dynamic ports** — nothing is pinned; billing reads whatever port the api got.
- **a worker with no ports, autostart, watch** — `platform/devctl.yaml`.

To make one machine's arrangement stick, drop a git-ignored `devctl.mine.yaml`
beside a manifest. For example, `billing/devctl.mine.yaml` with
`dependencies: [{name: platform, default: mock}]` would start billing on the mock
every time, without changing the committed manifest.
