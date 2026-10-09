# Design records

Dated design specs, plans, spikes and research notes written while building sbx. They record what
was intended and why, at the time; they are not reference docs. Where a record and the code
disagree, the code is right, and the current *why* is in [DECISIONS.md](../DECISIONS.md).

| date | record | kind | status |
|---|---|---|---|
| 2026-08-16 | [sbx connect: reaching a deployed sandbox through one HTTPS endpoint](2026-08-16-sbx-connect-design.md) | design | shipped in v0.3.0 |
| 2026-08-30 | [Feature gates, an editor story, and the rest of the v0.9.0 slate](2026-08-30-feature-gates-and-editor.md) | design | shipped in v0.8.0 (`SBX_FEATURES`; gates `ssh`, `devcontainer`, `waiting-page`) |
| 2026-08-30 | [Tunnels, networking, VS Code: what history says and what was measured](2026-08-30-tunnels-and-vscode-sandboxes.md) | research + plan | fixes shipped in v0.8.0; editor shipped as `sbx ssh`, not the proposed `sbx code` |
| 2026-08-31 | [v0.8.0, in detail](2026-08-31-v0.8.0-engineering-log.md) | engineering log | record of v0.8.0 |
| 2026-09-25 | [OpenSandbox compatibility](2026-09-25-opensandbox-compat-design.md) | design | partially shipped: v0.9.0 and v0.10.0 scope; isolated sessions, credential vault and server proxy not built |
| 2026-09-26 | [Spike: a Firecracker provider, on a Mac and on Linux](2026-09-26-firecracker-spike.md) | spike | complete; led to `--provider firecracker` in v0.11.0 |
| 2026-09-26 | [The OpenSandbox API and warm pool on Firecracker](2026-09-26-api-on-microvms.md) | plan | shipped: API in v0.12.0; warm pool, and the API through the helper VM, in v0.13.0 |
| 2026-09-27 | [Agent integrations: Claude Code and Codex inside a sandbox, off by default](2026-09-27-agent-integrations.md) | research + plan | proposed; not built |

"ROADMAP §N" in these records means the numbered roadmap as it stood before v0.14's rewrite,
readable at [v0.14.0](https://github.com/aryanmehrotra/sbx/blob/v0.14.0/docs/ROADMAP.md); §1 was the
microVM provider.

Each record carries a **Status:** line at its top that matches this table. When a record's status
changes, update both.
