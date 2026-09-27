# Quickstart

A ten-minute tutorial for a first-time user. Every command here is copy-pasteable.

You'll finish with: a Postgres that woke on a plain `psql`, a forked copy of it, and an AI agent
creating its own sandboxes. A *sandbox* is one named, isolated copy of a project's services, such
as a Postgres for one branch.

## What you need

- A running Docker engine: Docker Desktop, colima, or Docker on Linux. On Windows, use WSL2.
- `psql` on your PATH is nice to have. Step 3 shows a way round it.
- Disk space for the `postgres:16-alpine` image, which the first create pulls.

## 1. Install and check the machine

```sh
brew install aryanmehrotra/tap/sbx
# or: curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh
# or: go install github.com/aryanmehrotra/sbx@latest

sbx doctor       # what this machine can do: engine, providers, isolation
```

If `doctor` says Docker is not reachable, start your engine and run it again.

## 2. Start the daemon

```sh
sbx serve --idle 1m --osb-addr 127.0.0.1:8080 & SBX_PID=$!
```

`sbx serve` is the daemon, the one long-running process: it owns every sandbox's ports, wakes
services when something connects, and sleeps them after `--idle` with no traffic. Run one per
machine, not one per sandbox. A one-minute idle timer makes this tutorial quicker; the default is
five minutes. `--osb-addr` turns on the API that step 5 uses. `SBX_PID` remembers the daemon so
step 6 can stop it.

Its log lines appear in this terminal. If you prefer a quiet one, run the command in a second
terminal instead (without `& SBX_PID=$!`) and press Ctrl-C there in step 6.

## 3. Create a Postgres and wake it with `psql`

```sh
sbx create demo --template postgres
eval "$(sbx env demo)"
env | grep -E '^(PG|DATABASE_)'
```

`sbx env` prints the addresses this sandbox got: `PGHOST` and `PGPORT` for libpq, and
`DATABASE_HOST` and `DATABASE_PORT` for your app. Never hard-code the port; read it from `sbx env`.

A new sandbox starts asleep: nothing runs until something connects. Check, then connect:

```sh
sbx list
PGPASSWORD=app psql -U app -d app -c 'select count(*) from todo'
sbx list
```

The first `sbx list` shows `demo  postgres  asleep` with an address such as `127.0.0.1:20000`
(your port may differ): no container is running, so it uses 0 B of RAM. `psql` prints a count of
`0`, and the second `sbx list` shows `awake`. After a minute with no traffic it sleeps again;
`sbx sleep demo` puts it to sleep at once.

The `psql` call did not fail and did not retry. sbx held its connection open while Postgres
started, then handed it over. That is the whole trick: any client that opens a TCP socket wakes
the sandbox, with no SDK and no `sbx start`. The `todo` table exists because the template's
`init` step created it.

No `psql` here? `sbx wake demo` wakes it and waits until it serves, and
`sbx exec -t demo postgres psql -U app -d app` opens `psql` inside the container.

## 4. Seed once, fork many

Put a row in the database, save every service's data under a name, and make a new sandbox from it:

```sh
sbx exec demo postgres psql -U app -d app -c "insert into todo (title) values ('seeded')"
sbx snapshot demo seeded
sbx fork seeded demo-fork
sbx exec demo-fork postgres psql -U app -d app -tAc 'select title from todo'
```

The last command prints `seeded`. `demo-fork` is a full copy with its own ports. Prove the two
are independent:

```sh
sbx exec demo-fork postgres psql -U app -d app -c "insert into todo (title) values ('fork only')"
sbx exec demo postgres psql -U app -d app -tAc 'select count(*) from todo'    # still 1
```

This is how you give each branch, test run or agent its own database: pay for the seeding once,
then fork as many copies as you need. `sbx exec` wakes a sleeping sandbox too.

For a database that should exist only for one command, use `sbx with`. It creates the sandbox,
runs the command with its variables set, and removes it afterwards, even if the command fails:

```sh
sbx with ci-db --template postgres -- sh -c 'PGPASSWORD=app psql -U app -d app -c "select 1"'
```

## 5. Hand sandboxes to an AI agent

There are two ways. Use one or both.

**As a CLI the agent shells out to.** Copy the block under "Paste this into your agent's
instructions" in [GUIDES.md](GUIDES.md#ai-agents) into your repo's `AGENTS.md` or `CLAUDE.md`. It
tells the agent to create one sandbox per task, read addresses from `sbx env`, and remove only
what it created.

**As MCP tools.** MCP (Model Context Protocol) is the standard way AI assistants such as Claude
Code or Cursor call outside tools. The daemon from step 2 already serves the OpenSandbox API (an
open-source API standard for AI-agent sandboxes) and wrote its key to `~/.sbx/osb/key`. Register
`sbx mcp` with your agent; the example uses Claude Code:

```sh
claude mcp add sbx -- sbx mcp                    # reads that key itself
```

In a new Claude Code session, ask: *"Create a sandbox from `node:22-slim` and run `node -v` in it."*
The agent gets the same 19 tools as OpenSandbox's own MCP server. Cursor, Codex and the
OpenSandbox SDKs are in [GUIDES.md](GUIDES.md#ai-agents).

## 6. Look around, then clean up

```sh
sbx ui           # every sandbox live: state, cpu and memory against its limit, recent wakes
```

Press `q` to leave the dashboard. When you are done:

```sh
sbx rm demo
sbx rm demo-fork
sbx gc --snapshots            # lists what is left, including the "seeded" snapshot
sbx gc --snapshots --force    # deletes it
kill $SBX_PID                 # stop the daemon (or Ctrl-C in its own terminal)
```

`sbx rm` deletes the sandbox and its data. To keep the daemon running across reboots, use the
launchd plist or systemd unit in [`deploy/`](../deploy/), both of which run as you, not root.

## Next

- Describe your own services in a `sandbox.json`: [SPEC.md](SPEC.md), or run `sbx init`.
- Every task, from branch previews to CI and AI agents: [GUIDES.md](GUIDES.md).
- Look up any command or flag: [CLI.md](CLI.md).
- Something went wrong: [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
