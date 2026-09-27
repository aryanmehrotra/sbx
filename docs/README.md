# sbx documentation

Every page, grouped by what you came to do. New here? Start with the quickstart. The project's
front page is the [README](../README.md).

## Start here

| | |
|---|---|
| [QUICKSTART.md](QUICKSTART.md) | Ten minutes: install, wake a Postgres with `psql`, snapshot and fork it, hand it to an AI agent |

## How-to guides

| | |
|---|---|
| [AI-AGENTS.md](AI-AGENTS.md) | Use sbx from a coding agent: the block to paste, MCP setup, OpenSandbox SDKs, recipes |
| [USE-CASES.md](USE-CASES.md) | The shapes sbx fits, from branch databases to CI and preview links, with the commands |
| [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | What to do about a symptom you're seeing |
| [examples/](../examples/README.md) | Ready-made `sandbox.json` files, including the built-in templates |
| [console/](../console/README.md) | Metrics, health and a read-only API for a running daemon |

## Reference

| | |
|---|---|
| [CLI.md](CLI.md) | Every command, `sbx serve` flag and `SBX_*` environment variable |
| [SPEC.md](SPEC.md) | Every `sandbox.json` field, per provider, and a docker-compose mapping |
| [BENCHMARKS.md](BENCHMARKS.md) | Every published number, its machine and date, and the script that produced it |
| [test/osb/](../test/osb/README.md) | How OpenSandbox compatibility is checked with upstream's own e2e suite |

## Explanation

| | |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | How it works: the daemon, the wake path, providers, addressing |
| [COMPARISON.md](COMPARISON.md) | How sbx compares with E2B, Daytona, OpenSandbox, Fly, Neon, zeropod and others |
| [DECISIONS.md](DECISIONS.md) | Why things are the way they are, one decision per entry |
| [SECURITY.md](../SECURITY.md) | The threat model, what each isolation level protects, and how to report a vulnerability |

## Project

| | |
|---|---|
| [ROADMAP.md](ROADMAP.md) | What is next, and what is ruled out |
| [release-notes/](release-notes/README.md) | What changed in each release, with upgrade notes |
| [design/](design/README.md) | Design specs and engineering logs, each with its status |
| [CONTRIBUTING.md](../CONTRIBUTING.md) | How to build, test and send a change |
| [AGENTS.md](../AGENTS.md) | Rules for humans and coding agents editing this repo |
| [STYLE.md](STYLE.md) | How these docs are written |
