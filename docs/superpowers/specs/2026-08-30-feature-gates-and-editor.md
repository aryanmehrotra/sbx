# Feature gates, an editor story, and the rest of the v0.9.0 slate

**Status:** building
**Date:** 2026-08-30

## Why these, in this order

The comparison re-check (`docs/COMPARISON.md`, 2026-08-30) put a number on where sbx actually
stands. It keeps the row nobody else has — traffic to a port counts as activity, so a sandbox in
use is awake whatever is using it, where Codespaces and Ona both state the opposite in their own
docs. It loses one row outright: **there is no editor story at all**, and every dev-environment
product has one.

So the slate is: close the editor gap, close the two capability gaps that make features dead on
some platforms, and put a gate in front of anything not yet proven.

## The business case, per item

Written first because "it would be nice" is not a reason to add a flag to a tool whose pitch is
that it has few of them.

| | Who is blocked today | What it unblocks |
|---|---|---|
| **SSH service** | anyone who wants to edit code in a sandbox | VS Code Remote-SSH, JetBrains Gateway, `scp`, `rsync`, an agent with a shell — through the wake mechanism that already exists |
| **devcontainer import** | every repo that already has `.devcontainer/` | adoption without authoring a second file. DevPod proved the demand for a backend-agnostic client on that spec |
| **egress activity** | an agent working inside a box | the box sleeps when the agent stops, instead of `idle: "never"` holding its RAM for the sandbox's life |
| **egress_allow in a container** | every macOS and Windows user | an allow-list that works at all off native Linux |
| **waiting page** | anyone sharing `sbx url` with a non-engineer | a first click that explains itself instead of hanging |
| **feature gates** | the maintainer | shipping something unproven without it becoming a promise |

## Feature gates

The thing this must not become is a configuration surface. sbx has one committed file and a
handful of flags, and a registry of toggles would undo that.

So: **a gate is a temporary contract about maturity, not a preference.** Every gate has a
stability, and stable features have no gate at all — the gate is deleted when the feature
graduates. Nothing in the spec file changes; a gate is environment, because it belongs to the
person running the binary rather than to the project the spec describes.

    SBX_FEATURES=ssh,devcontainer sbx create dev
    sbx features                          # what exists, what is on, and why

Three states, and only two are reachable by accident:

- **stable** — on, ungateable, no entry in the registry.
- **preview** — off by default, `SBX_FEATURES=name` turns it on. It works; the contract may move.
- **experimental** — off, and turning it on prints one line saying what is unproven about it.

`SBX_FEATURES=all` is deliberately NOT supported: a person who wants everything on wants each
thing for a reason, and a blanket switch is how an unproven feature ends up in somebody's CI
without them choosing it.

## The editor story: SSH, not attach-to-container

Decided by mechanism, and this reverses an earlier draft.

VS Code has three remote modes. **Attach to Container** goes through the docker socket and never
opens a connection to the container's network, so it cannot wake a sandbox — sbx would have to
special-case it, and that is a second wake path to keep correct forever. **Remote-Tunnels** needs
a process already running inside. **Remote-SSH is a plain inbound TCP dial**, which is exactly
what sbx already wakes on.

So the editor story is a service in the spec that runs sshd, and one command that prints the URI:

    sbx ssh feature-x                     # the address, and the VS Code URI
    code --remote ssh-remote+... /work    # what the URI does

No new wake path, no editor-specific code in the daemon, and it works for JetBrains Gateway and
`rsync` for free because they are all just SSH.

**What it does not do, and the page says so:** it does not sleep while the editor is attached.
Nothing in the field does — measured, and mechanical: VS Code's PersistentProtocol pings every
5 s unconditionally, and CRIU cannot restore a TCP socket across a network hop, which is why
zeropod dropped `--tcp-established`. An attached editor keeps its sandbox awake, deliberately.

## Testing, per feature

Same standard as the rest of the repo: every fix has a test that fails without it.

- **Unit** — the gate registry, its parsing, and each feature's own logic.
- **Fuzz** — `SBX_FEATURES` parsing, and the devcontainer JSON reader, which reads a file
  somebody else wrote.
- **UAT** — `scripts/commands-e2e.sh` gains the new commands; a feature that is off must be
  invisible, and one that is on must work.
- **Benchmark** — the gate check is on no hot path, and that has to be shown rather than assumed:
  a gate consulted per connection would be a regression the connection benchmark would catch.
- **Business** — `docs/USE-CASES.md` gains the case each feature exists for, or the feature does
  not ship.
