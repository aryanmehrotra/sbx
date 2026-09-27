# Quickstart

In ten minutes you'll wake a sleeping Postgres with plain `psql`, fork it, and let an AI agent
create its own sandboxes. A sandbox is one named, isolated copy of a project's services. You need
a running Docker engine (Docker Desktop, colima, or Docker on Linux; WSL2 on Windows).

## 1. Install

```sh
brew install aryanmehrotra/tap/sbx
# or: curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh
# or: go install github.com/aryanmehrotra/sbx@latest

sbx doctor       # what this machine can do; if Docker is not reachable, start it and rerun
```

## 2. Start the daemon

```sh
sbx serve --idle 1m --osb-addr 127.0.0.1:8080 & DAEMON_PID=$!
```

The daemon owns every sandbox's ports, wakes services on connect, and sleeps them after `--idle`.
Run one per machine. `--osb-addr` turns on the API that step 5 uses. For a quiet terminal, run it
in a second one without `& DAEMON_PID=$!`.

## 3. Create a Postgres and wake it with `psql`

```sh
sbx create demo --template postgres
eval "$(sbx env demo)"                                      # sets PGHOST, PGPORT, DATABASE_HOST, DATABASE_PORT
sbx list                                                    # demo  postgres  asleep
PGPASSWORD=app psql -U app -d app -c 'select count(*) from todo'
sbx list                                                    # demo  postgres  awake
```

A new sandbox starts asleep at 0 B of RAM. `psql` did not fail or retry: sbx held its connection
while Postgres started, then handed it over. After a minute idle it sleeps again, or run
`sbx sleep demo`.

No `psql`? `sbx wake demo` wakes it, and `sbx exec -t demo postgres psql -U app -d app` opens
`psql` inside the container.

## 4. Seed once, fork many

```sh
sbx exec demo postgres psql -U app -d app -c "insert into todo (title) values ('seeded')"
sbx snapshot demo seeded
sbx fork seeded demo-fork
sbx exec demo-fork postgres psql -U app -d app -tAc 'select title from todo'    # seeded
```

`demo-fork` is a full copy with its own ports. Its writes do not reach `demo`:

```sh
sbx exec demo-fork postgres psql -U app -d app -c "insert into todo (title) values ('fork only')"
sbx exec demo postgres psql -U app -d app -tAc 'select count(*) from todo'    # still 1
```

For a database that lives for one command, `sbx with` creates it, runs the command, and removes
it even if the command fails:

```sh
sbx with ci-db --template postgres -- sh -c 'PGPASSWORD=app psql -U app -d app -c "select 1"'
```

## 5. Hand sandboxes to an AI agent

Either paste the block from [GUIDES.md](GUIDES.md#ai-agents) into your repo's `AGENTS.md` or
`CLAUDE.md`, or give the agent MCP tools. The daemon from step 2 already serves the API they use:

```sh
claude mcp add sbx -- sbx mcp                    # reads ~/.sbx/osb/key itself
```

In a new Claude Code session, ask: *"Create a sandbox from `node:22-slim` and run `node -v` in it."*
It gets the same 19 tools as OpenSandbox's own MCP server. Cursor, Codex and the SDKs are in
[GUIDES.md](GUIDES.md#ai-agents).

## 6. Look around, then clean up

```sh
sbx ui                        # every sandbox live; q to quit
sbx rm demo                   # deletes the sandbox and its data
sbx rm demo-fork
sbx gc --snapshots            # lists what is left, including "seeded"
sbx gc --snapshots --force    # deletes it
kill $DAEMON_PID
```

To keep the daemon running across reboots, use the launchd plist or systemd unit in
[`deploy/`](../deploy/).

## Next

Describe your own services with `sbx init` and [SPEC.md](SPEC.md). Then see [GUIDES.md](GUIDES.md)
for branches, CI and agents, [CLI.md](CLI.md) for every command, and
[TROUBLESHOOTING.md](TROUBLESHOOTING.md) when something goes wrong.
