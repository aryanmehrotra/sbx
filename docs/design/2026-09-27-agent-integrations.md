# Agent integrations: Claude Code, Codex and friends inside a sandbox, off by default

**Status:** proposed - research and plan, nothing built
**Date:** 2026-09-27

> **Short version:** `SBX_FEATURES=agents sbx agent my-task claude` runs Claude Code, and
> `sbx agent my-task codex` runs Codex, each inside its own sandbox. These two are the v1
> integrations; others come later. The repository is mounted at `/work`. The agent reaches
> only its own model API plus the hosts you add. Every call it makes keeps the box awake, and it
> sleeps to 0 B when the agent stops. **Nothing about it is on by default.** It needs a gate to
> exist, an install to be present, and a credential you hand over on purpose, every time. Most
> of it is assembly of parts sbx already has. The new code is a small registry of what each
> agent needs, and one command.

---

## Why, and why now

Two directions, and sbx has one of them.

| direction | today |
|---|---|
| **An agent on the host drives sandboxes** (creates a Postgres, runs tests in a box) | shipped: `sbx mcp`, `--shell json`, [GUIDES.md › AI agents](../GUIDES.md#ai-agents) |
| **An agent runs inside a sandbox** (its shell, its writes and its network are the box's, not your laptop's) | possible by hand. The pieces all shipped ([Work inside a sandbox](../GUIDES.md#work-inside-a-sandbox), [Keep a sandbox awake, or limit where it can connect](../GUIDES.md#keep-a-sandbox-awake-or-limit-where-it-can-connect)), but nobody has assembled them. The only record is one line in the 2026-08-30 tunnels spec: "a `node:22-alpine` image with the Claude Code CLI baked in builds and runs" |

The second direction is the one people ask for, because of what coding agents are run with.
`claude --dangerously-skip-permissions` and `codex --dangerously-bypass-approvals-and-sandbox` are
how agents get unattended work done, and both vendors' docs say to use them **only inside a
sandbox**. sbx is that sandbox, and it has two things a plain `docker run` does not:

- **An egress allow-list that is enforced, not advisory.** An agent that has been talked into
  exfiltrating the repo cannot reach anything but its model API.
- **A box that sleeps when the agent stops, and not before.** Every model call goes through
  sbx's filtering proxy, so it counts as activity. A fleet of parked agents costs
  0 B.

Blocked items from the 2026-08-30 spec, now unblocked:

1. `egress_allow` did not work off native Linux (D3). **Fixed**: the filter runs as a container
   on the sandbox network when the gateway cannot be bound (`internal/provider/egress_container.go`).
2. `idle` had no signal for work inside the box. **Fixed**: egress bytes stamp activity
   (`internal/daemon/egressproxy.go`, `egressscrape.go`).

So the parts exist. What is missing is packaging, and the one thing that must not be packaged
carelessly: **credentials**.

## What "not by default" means - four separate locks

"Integration" has to mean opt-in at every layer. A `sandbox.json` that never mentions an agent
must behave exactly as it does today. A user who has never heard of the feature must never have
a model key copied anywhere.

| lock | what it means | where it lives |
|---|---|---|
| **1. The gate** | `agents` is a **preview** feature. Without `SBX_FEATURES=agents`, `sbx agent` refuses with the one line that turns it on, and the `agent` spec field (phase 2) is refused by name | `internal/features`, same as `ssh` / `devcontainer` |
| **2. No agent in any default image** | no template, no base image and no activator ships an agent CLI. The agent image is built only when `sbx agent` is run naming that agent, from a pinned version | a generated Dockerfile through the existing `build:` path |
| **3. No implicit credentials** | sbx never reads `~/.claude`, `~/.codex` or a key variable unless the command names the agent. The credential goes in **per exec**, never into the container's config (see [Credentials](#credentials)) | `sbx agent` only |
| **4. No implicit reach** | the box's egress is the agent's API hosts plus what you pass with `--allow`. There is no "allow everything" shorthand. Unrestricted egress needs `--egress open` spelled out, and it prints a line saying the box is unbounded | the registry + `egress_allow` |

Lock 1 is temporary: the gate is deleted when the feature graduates, as for every gate. Locks 2-4
are permanent. They are the design, not its maturity.

## The shape

```sh
export SBX_FEATURES=agents

sbx agent fix-flaky claude                        # interactive Claude Code, repo at /work
sbx agent fix-flaky claude -- -p "fix the flaky test in ./internal/cli"   # headless, prints the result
sbx agent review-7 codex --allow github.com -- exec "review this diff"
sbx agent list                                     # the integrations this build knows, and what each needs
```

`sbx agent <sandbox> <integration> [flags] [-- agent args...]`

1. **Resolve the integration** from the registry (below). An unknown name lists the known ones.
2. **Resolve the credential** from the host environment or `--auth`. If none is found, refuse
   *before* creating anything, and name the variable or command that provides one. This is the
   `egress_allow`-on-colima lesson: never report success on a box that cannot work.
3. **Create or reuse** sandbox `<sandbox>` with one service, `agent`:
   - `build:` a generated context (base image + pinned CLI + non-root user), cached by content
     hash like every other build, so the second run does no build work
   - `mounts: {"<cwd>": "/work"}`: the repo, read-write
   - `egress_allow`: integration hosts ∪ `--allow`
   - `idle`: `10m` (overridable), which is safe because the model calls are the activity signal
   - `args: ["sleep", "infinity"]`, `ports: [7777]`, the "Work inside a sandbox" shape, unchanged
4. **Exec with a TTY** into `agent`. The agent's command, the credential as exec-time env, and the
   integration's "the sandbox is the boundary" flags go in with it. `sbx agent` only does this;
   the rest is the ordinary `sbx exec -t` path (`internal/cli/cli.go`, `ExecTTY`), which already
   wakes the service first.
5. **Return the agent's exit code.** The sandbox stays and sleeps. `--rm` removes it on exit,
   which is `sbx with` semantics.

Everything after step 2 is existing machinery. Because the sandbox is an ordinary sandbox,
`sbx ui`, `sbx history`, `sbx egress`, `sbx checkpoint` and `sbx fork` all work on it without
change.

### Composes with what exists

- **N agents, each on its own copy of a seeded DB**: `sbx snapshot` once, then per agent
  `sbx fork seed task-N && sbx agent task-N claude -- -p "..."`. The agent service joins the
  forked sandbox, and `sbx env` gives it `DATABASE_HOST/PORT`. "Seed once, fork many" in GUIDES.md, with the agent inside.
- **Parking an agent**: `sbx checkpoint task-N` on podman and firecracker keeps memory, so a
  headless run can be parked mid-thought. This is untested for agents and is marked so.
- **A git worktree per agent**: recommended in the docs, not built in v1. Two agents mounting the
  same checkout will edit the same files. `--worktree` (a `git worktree add` under
  `~/.sbx/worktrees/<sandbox>`) is phase 2.

## The integration registry

One Go table, `internal/agents`, and no plug-in mechanism. An integration is data, and adding
one is a reviewed PR with a test that builds its image, which is the same bar as a template. The
entries below are a **research starting point**. Every host, variable and flag is listed under
[Verify before building](#verify-before-building), because vendors move these.

**v1 ships exactly two: Claude Code and Codex.** Both are first-class, with the same flags, the
same tests and the same docs. For both, **the subscription login (Claude Pro/Max, ChatGPT
Plus/Pro) is the default credential**. An API key is the second option, for people without a
subscription or for billing that has to be per-token.

| | Claude Code | Codex CLI |
|---|---|---|
| install | npm `@anthropic-ai/claude-code@<pin>` | npm `@openai/codex@<pin>` (Rust binary) |
| base | `node:22-slim` + git, ripgrep | same |
| **subscription (default, `--auth login`)** | `CLAUDE_CODE_OAUTH_TOKEN`, made once on the host with `claude setup-token` (a long-lived token, no refresh) | `~/.codex/auth.json` from `codex login` on the host (a file with a refresh token, see [Credentials](#credentials)) |
| API key (`--auth key`) | `ANTHROPIC_API_KEY`, read from the host env when `sbx agent … claude` runs | `OPENAI_API_KEY`, read the same way |
| egress, minimum | `api.anthropic.com` (same host for both kinds of credential) | subscription: `chatgpt.com`, `auth.openai.com` · API key: `api.openai.com`. Only the set for the credential in use is allowed |
| "sbx is the boundary" | `--dangerously-skip-permissions` (refuses as root → the image runs as uid 1000) | `--sandbox danger-full-access --ask-for-approval never` (its own landlock/seccomp sandbox may not work nested in a container) |
| headless | `claude -p "<task>"` | `codex exec "<task>"` |
| proxy | honours `HTTPS_PROXY` (documented) | Rust/reqwest honours `HTTPS_PROXY` |
| telemetry to quiet | `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` | - |

**The resolution order is subscription first, then API key.** `sbx agent … claude` uses
`CLAUDE_CODE_OAUTH_TOKEN` when it is set. If it is not set, it uses `ANTHROPIC_API_KEY`. If
neither is set, it refuses before creating anything:

    no Claude subscription token: run `claude setup-token` once and export CLAUDE_CODE_OAUTH_TOKEN
    (or pass --auth key with ANTHROPIC_API_KEY set)

Codex follows the same order: `~/.codex/auth.json` from `codex login`, then `OPENAI_API_KEY`,
then the refusal naming `codex login`. `--auth login` or `--auth key` pins one source and turns
off the fallback. Every run prints which one it used, for example `claude: subscription
(CLAUDE_CODE_OAUTH_TOKEN)` or `codex: API key (OPENAI_API_KEY)`. That way a per-token bill never
arrives unannounced because a subscription token had quietly gone missing.

Subscription-first has two consequences the docs must state:
- **Usage is shared.** An agent in a box draws on the same plan limits as the person's own
  Claude or ChatGPT use. Five parallel agents hit the limit five times as fast.
- **Terms.** Subscription credentials are for the subscriber's own use. sbx runs the vendor's
  own CLI, as that person, on their machine, which is the intended use. The docs link each
  vendor's terms rather than paraphrasing them, and phase 0 re-reads them.

The "boundary" flags are **not** added when the user passes the agent's own permission flags, and
`--keep-prompts` leaves them off entirely. Adding them is the point of running inside sbx, and
the docs say so plainly. It is not hidden.

Gemini CLI is the next candidate after v1. It is not in v1 because it is Node-based and its
proxy support needs checking (`NODE_USE_ENV_PROXY=1` on Node ≥ 24, or its own setting). If it
ignores `HTTPS_PROXY`, the filter cannot see it. `aider`, `opencode`, `goose` and `cursor-agent`
are candidates after that. None goes in
until a user asks and the e2e below is written for it.

## Credentials

This is the part to get right, so it is decided here and not left to the implementer.

**Rule: a credential exists in the box only while the agent process runs.** It is never in
`sandbox.json`, never in the container's create-time env, never in `sbx history`, never in a
snapshot, fork or checkpoint.

| mechanism | why |
|---|---|
| **env at exec, never at create** | create-time `-e` lands in `docker inspect`, in every `sbx fork`, and in a snapshot. Exec-time env dies with the process |
| **`docker exec -e NAME` with the value in the CLI's environment, not its argv** | `-e NAME=value` on argv is readable in the host's `ps`. `-e NAME` alone inherits the value from the docker CLI's environment. The same care applies to podman |
| **firecracker: over the execd channel** | execd already takes env per command. `forgetSecrets` already strips tokens from snapshots (`internal/provider/firecracker_osb.go`). That is the precedent |
| **file credentials (Codex `auth.json`) copied in at exec, removed on exit** | written 0600 to a tmpfs path (`/run/sbx-agent/`) and pointed at with `CODEX_HOME`. Never a bind mount of `~/.codex`: a writeable bind would let the agent rewrite anything in the host's directory, and a read-only one exposes every other file in it |
| **a refreshed subscription token is written back, and only that** | Codex refreshes its ChatGPT token during a run. If the refresh token rotates, the host's copy is dead once the box has used it. So on exit sbx compares the box's `auth.json` with what it copied in. If it changed, parses as the same account and is newer, sbx writes it back to `~/.codex/auth.json` atomically (temp file + rename, 0600). One writer at a time, so two parallel agents cannot interleave. This is the only file sbx ever writes into the host's agent config. Claude's `setup-token` token does not refresh, so it needs none of this |
| **an explicit source, always** | no flag (subscription, then API key) · `--auth login` (subscription only) · `--auth key` (API key only) · `--auth none` (the image's own login flow, inside the box, whose token dies with the box) |
| **redaction** | `history.Redact` already matches `key|token|auth`, and a test pins that every registry variable matches it |

What this does **not** protect against, stated in the docs: the agent itself can read its own
key while it runs, and can send it to any host on the allow-list. The allow-list is the bound on
where it can go. For a model API that is the vendor who issued it.

## Platform support

| provider | v1 | why |
|---|---|---|
| docker (native, colima, Docker Desktop) | **yes** | `build`, `mounts`, container egress filter, `ExecTTY` all present |
| podman | yes | same paths. `checkpoint` bonus |
| firecracker | **phase 2** | needs `Builder` on that provider, or a pre-built image. Egress filter is a Linux-host listener. Mount semantics differ |
| kubernetes | **refused by name** | refuses `mounts` and filtered egress today. An agent with no repo and unbounded egress is exactly the box not to build |

Refusal follows the house rule: one line naming the reason and the way out, at `sbx agent` time,
before anything is created.

## Phases

| phase | what | size (eng-weeks, estimate) |
|---|---|---|
| **0 · spike** | By hand, on docker + colima: build the Claude Code and Codex images; run each headless through `egress_allow` with **only** the minimum hosts; confirm the proxy is honoured; confirm the box stays awake through a long run and sleeps after; confirm the non-root / nested-sandbox flags. **Output: the registry table, with every "verify" cleared or changed** | 0.5 |
| **1 · `sbx agent`, docker/podman, Claude Code + Codex** | `internal/agents` registry · gate `agents` · the command · exec-time credentials · `--allow`, `--rm`, `--idle`, `--auth` · refusals · docs below | 1.5-2 |
| **2 · spec field + worktrees + more agents** | Gemini CLI once its proxy behaviour is verified · `"agent": "claude"` on a service (gated, expands to the same build/egress/idle), so a repo can commit its agent box · `--worktree` · firecracker | 1.5 |
| **3 · graduate** | delete the gate after one release in preview with no contract change. Locks 2-4 stay | - |

Per ROADMAP.md, this item needs a DECISIONS.md entry before it goes on the roadmap. Draft title:
*"An agent runs inside a sandbox only when asked, with a credential that lives as long as its
process."*

## Testing

Same bar as the rest of the repo: every fix has a test that fails without it.

- **Unit**: the registry, which checks every entry has hosts, an auth variable and a pinned version,
  and that every auth variable matches `history.Redact`. Credential plumbing asserts that the
  create-time spec never contains a registry variable, and that the exec argv never contains a
  value. Resolution order (subscription, then key, then refusal; `--auth` pins one). Codex write-back only happens for a changed, same-account, newer file, and is atomic under concurrent runs. Refusal messages for: gate off, no credential, kubernetes, unknown integration.
- **Fuzz**: `--allow` host parsing, which reuses the `egress_allow` validator rather than a new one.
- **UAT** (`scripts/commands-e2e.sh`): with the gate off, `sbx agent` is refused and nothing is
  created. With it on and a fake integration (an image whose "agent" is `curl` to an allowed and
  a denied host), the allowed call succeeds and the denied one fails, and the box sleeps after
  `idle`. No real model key in CI.
- **Real-agent e2e, manual, per release**: each integration, headless, one small task, on docker
  and colima. The results table goes in BENCHMARKS.md the way wake latency does.
- **Business**: GUIDES.md gains the how-to below, or it does not ship.

## Documentation - what ships with it, and who it is for

Docs are part of the feature, not a follow-up. Each piece is written in the phase that makes it
true, never ahead of the code.

| doc | audience | content |
|---|---|---|
| **`docs/GUIDES.md#ai-agents`** → a how-to, *Run Claude Code or Codex inside a sandbox* | people using sbx with agents | the four locks in one paragraph · **setup, subscription first**: `claude setup-token` / `codex login` once, then run; the API key as the alternative · the three commands · credentials table (what goes in, when it leaves) · the "what this does not protect against" paragraph · the table of integrations and their hosts |
| **`docs/GUIDES.md`** (same section) | evaluators | the why (skip-permissions needs a boundary) · N agents × forked DB recipe |
| **`docs/CLI.md`** | everyone | the `sbx agent` row, its flags, and the `agents` gate under "Gated features" (docs contract) |
| **`docs/ARCHITECTURE.md#platform-status`** | everyone | where `sbx agent` runs and is verified |
| **`docs/SPEC.md`** (phase 2) | spec authors | the `agent` field, what it expands to, what it refuses |
| **`docs/TROUBLESHOOTING.md`** | users stuck | "agent hangs on first call" (proxy not honoured) · "refuses to run as root" · "401 inside the box" (wrong `--auth`) · "sleeps mid-session" (interactive, no model calls for `idle`) |
| **`SECURITY.md`** | security reviewers | credential lifetime, what the allow-list bounds and what it does not |
| **`docs/DECISIONS.md`** | maintainers | the entry above |
| **`sbx help agent`**, `sbx agent list` | everyone, at the terminal | generated from the registry, so it cannot drift from the code |
| **`CONTRIBUTING.md`** → *Adding an integration* | contributors | the registry fields, the pin, the required e2e, the bar for acceptance |
| **README + release notes** | everyone | one line under AI agents, marked preview · one line in `docs/release-notes/UNRELEASED.md` |

## Verify before building

Everything here came from memory or from the vendors' docs as last read. It is exactly what
phase 0 is for.

- [ ] Each CLI's current npm package name, and a pinned version that works.
- [ ] Both CLIs honour `HTTPS_PROXY` / `NO_PROXY` through sbx's filter, with an API key and with a
      subscription login.
- [ ] Minimum egress host set per agent, with telemetry off. Which hosts are needed for auth only.
- [ ] `claude --dangerously-skip-permissions` as non-root in a container, no extra env needed.
- [ ] Codex's own sandbox inside docker: does `danger-full-access` avoid landlock/seccomp errors.
- [ ] **Subscription auth, first, since it is the default:** `CLAUDE_CODE_OAUTH_TOKEN` from
      `claude setup-token` works headless and interactive in the box. The Codex `auth.json` copy
      works with `CODEX_HOME` pointed at tmpfs. Whether Codex rotates its refresh token, and so
      whether write-back is required or only polite. The exact hosts a ChatGPT-login Codex calls.
      Also the vendors' terms on
      using subscription credentials in automation.
- [ ] Does an open `sbx exec -t` session count as activity? If not, an interactive session where
      the human reads for longer than `idle` sleeps under them. The fix is the exec session
      stamping activity, which is a small daemon change, but it has to be known first.
- [ ] `docker exec -e NAME` (value from env) on docker, podman and colima.

## Out of scope

- A hosted agent service, or sbx calling any model itself. sbx is the box, not the agent.
- Managing agent config, MCP servers or skills inside the box. `files` / `mounts` already allow a
  user to supply their own.
- Any default-on behaviour, now or after graduation.
