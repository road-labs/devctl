---
name: devctl-manifest
description: >
  MUST USE when writing or changing a devctl.yaml, the manifest devctl reads to
  run a repository's services locally. Triggers: devctl.yaml, devctl.mine.yaml,
  "set up devctl", "add a service to devctl", a port that moved, a new local
  dependency, "how do I run this locally".
---

# Writing a devctl.yaml

**Everything you need is below.** `devctl -skill` prints this, then the
reference, which is every key and every type, then a worked example of every
feature. Read the reference for the schema; do not go looking in the source for
it, and do not guess. This part is what the reference does not cover, which is
how to find out what belongs in the file.

Almost nobody gets stuck on the YAML. They get stuck because a manifest has to
state things the repository never wrote down in one place, and a few things only
a person knows.

So the job is in four parts, in this order: read, ask, write, and then wire it
into whatever people already type to start things.

---

## What devctl does, so you are not reverse-engineering it

Six facts. Together they are the whole model, and several of the mistakes
further down are someone assuming one of them works another way.

**One file.** `devctl.yaml`, found by walking up from the working directory the
way git finds `.git`. Every relative path in it resolves against the directory
holding it. `devctl.mine.yaml` beside it, when present, overlays it and is never
committed.

**Ports are decided at start, not bound.** devctl allocates a number for every
declared listener before anything runs: a free default is kept, a taken one is
replaced and reported, a `fixed` one that is taken refuses the run. Services
bind them themselves, through the variable `listen` names.

**A process is given the shell's environment, then its own listeners, then its
declared `env`.** Later wins, so a value in the manifest overrides one the
developer happened to have exported. Every `{{ reference }}` is resolved first.

**`.env` is not loaded into the process.** This is the one people get wrong. It
is read only to find the addresses of dependencies the machine provides, and
only under the exact names those dependencies declare in `env:`. Anything else
in `.env` is ignored, so a service that needs `STRIPE_KEY` must say so in the
manifest; putting it in `.env` does nothing.

**The environment beats the file.** For a dependency's address devctl checks the
process environment first, then `<root>/.env`. So exporting a variable
temporarily overrides the file without editing it.

**Nothing else is read.** No implicit config, no conventions, no defaults
discovered from the repository. If the manifest does not say it, it does not
happen. That is the point: the file is the whole answer to "how do I run this".

## 1. Read. Most of it is already written down somewhere

Do this before asking anything. An operator who is asked what they already
committed to the repository loses confidence in the answer, fairly.

**The Makefile and the Taskfile first.** They are the best source in the
repository and usually the only honest one: they are what people actually type,
so they already carry the command, the working directory, the environment and
often the port, all of it kept working because someone would notice the day it
broke. A `make run-identity` or a `task fraud-review` is very nearly a service
entry already. Read every target that runs something, including the ones that
only set variables before calling another.

Then, roughly in order of how much they can be trusted:

| Source | What it gives you |
| --- | --- |
| `Makefile`, `Taskfile.yml`, `justfile`, `Procfile`, `package.json` scripts | The command, the directory, the environment, sometimes the port |
| `docker-compose.yml` | Which databases and brokers a run needs, and their ports |
| `.env.dist`, `.env.example`, `.env.sample` | Every variable a run reads, and which are addresses |
| `cmd/*/main.go`, `main.go`, `src/index.ts` | The entry points, and the flags and env each binds its listeners with |
| The code itself | Literal `:8080`s, `os.Getenv` and `process.env` names |
| The README, "running locally" | What the team believes the procedure is, which may be out of date |
| `deployment/`, `k8s/`, Helm values | Ports and environment as production has them; a starting point, never a copy |
| The devctl panel of a sibling repository | How this organisation writes manifests |

Read the code for the ports rather than trusting a README. A README says what
was true when someone wrote it; `flag.String("addr", ":8080", ...)` says what is
true now.

## 2. Ask. Only what the repository cannot tell you

Gather the questions and ask them **in one round**, not one at a time. Say what
you found first, so the operator is correcting a draft rather than reciting an
inventory.

- **Which of these are worth running locally?** A repository with forty services
  does not need forty rows. Which does someone actually work on, and what has to
  be up for those to work?
- **What does the machine provide, and what does devctl forward?** A database
  might be in Docker, on their own server, or in a cluster behind a port-forward.
  This is the question with the most personal variation, and the answer belongs
  in `devctl.mine.yaml` when it is one developer's arrangement rather than the
  team's.
- **Does another repository dial any of these by number?** Those ports are
  pinned and everything else is free to move. Getting this wrong either way
  hurts: pinning everything means a run refuses to start over a port nothing
  refers to, and pinning nothing breaks the other repository silently.
- **What should be up the moment devctl opens?** That is `autostart`. The rest
  wait for `s`.
- **Which directories, when they change, should restart a service?** Its own
  source, plus any shared library it imports.
- **Anything that has to run by hand?** Seeding, migrations, fixtures. Those are
  tasks, and say so in the description when one is destructive.
- **Which subsets get worked on together?** Those are profiles, and the answer
  is usually a sentence someone already says out loud: "the fraud console and
  what it reads", "just the API". List the roots; devctl pulls in what they
  need. Only worth declaring where a repository is big enough that nobody runs
  all of it.
- **Where should logs go?** Usually a gitignored `.devlogs` with a cap.

## 3. Write it

Dependencies first, then services in dependency order, then tasks: the file then
reads the way a run happens.

The one rule behind all the others: **no address is written twice.** A port is
declared once, on the service that binds it, and everything else refers to it.

```yaml
services:
  - name: identity
    description: Identity API - OIDC issuer, SAML SP, token verification
    dir: applications/identity
    cmd: go run ./cmd/service identity-api
    ports:
      - {name: grpc, number: 24700, kind: grpc, description: management, fixed: true}
    listen: {grpc: GRPC_ADDR}
    env:
      IDENTITY_STORE_DSN: "{{ mongo.address }}"
    depends_on: [mongo]
    watch: [applications/identity, libs]
```

`listen` says which variable the process reads for its own listener; `env`
references carry other people's. Neither contains a number.

## 4. Wire it into how people already start things

A manifest nobody knows how to run is a file nobody runs. If the repository
root has a `Makefile`, a `Taskfile.yml` or a `justfile`, people type that, not
`go tool devctl`, so finish the job there.

**Check whether the name is free first.** Grep for a `dev` target. If there is
one, do not touch it: say what it does today and ask what to call this instead,
because a target somebody else's muscle memory depends on is not yours to
repoint.

**Match how devctl is installed**, which `go.mod` tells you:

| In `go.mod` | The line |
| --- | --- |
| a `tool github.com/road-labs/devctl` directive | `go tool devctl` |
| a `require` only | `go run github.com/road-labs/devctl` |
| nothing | `devctl`, and say it needs installing |

```make
.PHONY: dev
dev:
	go tool devctl
```

```yaml
  dev:
    desc: Run the services locally
    cmds:
      - go tool devctl
```

Then say the sentence the README section this replaces used to say: `make dev`,
or `task dev`. That is the whole point of the exercise.

## The mistakes worth naming

- **A number written twice.** If a port appears in two places, one of them is
  wrong already or will be.
- **Pinning by default.** `fixed: true` belongs only on a port another
  repository dials. A pinned port that something else on the machine holds
  refuses the whole run.
- **`depends_on` naming only services.** It names dependencies too, and that is
  how a machine-provided database gets checked before anything starts.
- **`dir` relative to something other than the repository root.** It is always
  the root, whatever the command would be if you typed it yourself.
- **Copying production environment.** Take the shape from it, never the values.
  Everything in this file is development only and it is committed.
- **A description that repeats the name.** "identity: the identity service"
  helps nobody. Say what it serves and who talks to it; that line is what the
  panel and `d` show.
- **Putting a service's variable in `.env`.** devctl reads `.env` only for
  dependency addresses, under the names dependencies declare. A service's own
  variables belong in its `env`, referencing a dependency where they need an
  address.
- **Secrets.** There are none in this file. Machine-provided addresses come from
  `.env`, which is not committed.

## Verify

Run `devctl -check` after every edit. It validates the manifest, resolves the
dependencies, allocates the ports and prints the plan without starting
anything. Read the plan rather than assuming: it is where a port you thought was
free turns out not to be, and where a `depends_on` typo surfaces.

Then run the panel and press `d` on each new row. Describe shows the command,
the resolved environment and the dependency graph, which is the fastest way to
see that a reference resolved to what you meant.

## Changing one that already exists

Same order, smaller. Read what the repository does now, confirm only what
changed, keep the file's existing conventions even where you would have chosen
differently. A manifest that reads as one person's work is worth more than one
that is half yours.

When the change is one developer's arrangement rather than the team's, it
belongs in `devctl.mine.yaml`, which overlays the committed manifest and is
never committed.
