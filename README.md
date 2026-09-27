# sbx

<img src="docs/hero.svg" width="900" alt="sbx: self-hosted sandboxes for AI agents. A fresh microVM or container for each agent, asleep at 0 B and awake on the first connection. An agent connects, sbx holds the connection while the microVM resumes, and the same socket is served.">

[![CI](https://github.com/aryanmehrotra/sbx/actions/workflows/ci.yaml/badge.svg)](https://github.com/aryanmehrotra/sbx/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/aryanmehrotra/sbx.svg)](https://pkg.go.dev/github.com/aryanmehrotra/sbx)
[![Go Report Card](https://goreportcard.com/badge/github.com/aryanmehrotra/sbx)](https://goreportcard.com/report/github.com/aryanmehrotra/sbx)
[![release](https://img.shields.io/github/v/release/aryanmehrotra/sbx?color=3fb950)](https://github.com/aryanmehrotra/sbx/releases)
[![dependencies](https://img.shields.io/badge/dependencies-0-3fb950)](go.mod)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

Self-hosted sandboxes for AI agents. sbx gives every agent, test run or branch its own sandbox on
your machine or cluster: a fresh Firecracker microVM with its own kernel, or a container. Idle
sandboxes sleep at 0 B of RAM and wake the moment anything connects.

Agents drive it through the [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) API,
an open standard with SDKs in five languages, or as tools over MCP. Anything else only has to
connect: point `psql`, Playwright or a test runner at a sleeping sandbox and sbx holds the
connection while it wakes, then hands it over.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/bench-dark.svg">
  <img src="docs/bench-light.svg" width="900" alt="OpenSandbox API, create a sandbox and run a first command: sbx 307 ms, OpenSandbox server 1,417 ms. Memory held by 20 idle Postgres databases: sbx asleep 17.6 MB, docker compose always on 629 MB. Time until a sleeping Postgres answers psql: sbx 348 ms, first try served 20 of 20; Lazytainer 3,407 ms, first try refused 0 of 5.">
</picture>

- A microVM per sandbox on Linux with KVM, or a container anywhere Docker runs
- Written in Go with only the standard library: one static binary, zero dependencies
- No account and nothing hosted: MIT licensed, on your own hardware

Not to be confused with Docker's own `sbx` CLI (`docker/tap/sbx`).

## Install

```sh
brew install aryanmehrotra/tap/sbx
```

Or `curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh`,
or `go install github.com/aryanmehrotra/sbx@latest`. It needs Docker or a Kubernetes cluster, and
Linux with `/dev/kvm` for microVMs. Run `sbx doctor` to see what your machine supports.

## Sandboxes for AI agents

```sh
# The daemon, serving the OpenSandbox API with four sandboxes started ahead of time
sbx serve --osb-addr 127.0.0.1:8080 --osb-pool python:3.11-slim=4 &
export OPEN_SANDBOX_DOMAIN=127.0.0.1:8080 OPEN_SANDBOX_API_KEY="$(cat ~/.sbx/osb/key)"
pip install opensandbox
```

```python
from opensandbox import SandboxSync

sandbox = SandboxSync.create("python:3.11-slim")
print(sandbox.commands.run("python -c 'print(6 * 7)'").logs.stdout[0].text)  # 42
sandbox.destroy()
```

Add `--provider firecracker` to `sbx serve` and every sandbox is a microVM with its own kernel,
booted fresh from the image. To give Claude Code, Cursor or Codex sandboxes as tools, run
`claude mcp add sbx -- sbx mcp` while the daemon runs. More in [GUIDES.md](docs/GUIDES.md#ai-agents).

## Databases for branches and tests

```sh
sbx serve --idle 5m &                               # once per machine
sbx create my-branch --template postgres            # a Postgres for this branch
eval "$(sbx env my-branch)"
PGPASSWORD=app psql -U app -d app                   # connecting wakes it

sbx snapshot my-branch seeded && sbx fork seeded agent-1         # seed once, copy per agent
sbx with test-db --template postgres -- go test ./...            # removed even if tests fail
```

Templates: `postgres`, `browser`, `nginx`, `web-stack` (Postgres and Redis) and `analytics`. For
your own stack, write a `sandbox.json` ([SPEC.md](docs/SPEC.md)).

<img src="docs/demo.svg" width="900" alt="A terminal recording of sbx: a sandbox is created from the postgres template, sleeps to 0 B when nobody connects, and a plain psql query wakes it. Then an agent reads its addresses as JSON, the sandbox is snapshotted and forked into agent-1, and a redis cache is added.">

## Documentation

- [Quickstart](docs/QUICKSTART.md): a first sandbox in ten minutes
- [Guides](docs/GUIDES.md): branches, CI, previews, AI agents
- [Self-hosting](docs/SELF-HOSTING.md): run it on your own servers for a team
- [CLI reference](docs/CLI.md) and [sandbox.json reference](docs/SPEC.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [How it works](docs/ARCHITECTURE.md), including where it has been tested
- [Benchmarks](docs/BENCHMARKS.md) and [comparison with other tools](docs/COMPARISON.md)
- [Roadmap](docs/ROADMAP.md) and [release notes](docs/release-notes/README.md)
- [Security](SECURITY.md)

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md); coding agents
should read [AGENTS.md](AGENTS.md).

## License

[MIT](LICENSE)
