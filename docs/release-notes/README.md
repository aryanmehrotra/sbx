# sbx release notes

What changed in each sbx release, what it means for you, and how to upgrade. Newest first.

| Version | Date | Headline |
|---|---|---|
| [v0.16.0](v0.16.0.md) | 2026-09-30 | `sbx ready` and `sbx egress` you can trust on any engine |
| [v0.15.1](v0.15.1.md) | 2026-09-29 | `sbx create` over an asleep sandbox no longer fails on `files` |
| [v0.15.0](v0.15.0.md) | 2026-09-29 | `sbx install` adds what `sbx doctor` says is missing |
| [v0.14.0](v0.14.0.md) | 2026-09-27 | Hardened microVMs: per-VM network namespace, file-size limits, no per-VM image copy |
| [v0.13.2](v0.13.2.md) | 2026-09-27 | OpenSandbox SDK pools and e2e tests now pass against sbx |
| v0.13.1 | 2026-09-27 | Tagged, never published; superseded by v0.13.2 (test-only difference) |
| [v0.13.0](v0.13.0.md) | 2026-09-27 | microVMs run confined by default, and start in 141 ms from a warm pool |
| [v0.12.0](v0.12.0.md) | 2026-09-26 | OpenSandbox API on Firecracker microVMs (not yet for untrusted code) |
| [v0.11.0](v0.11.0.md) | 2026-09-26 | Firecracker microVM sandboxes (Linux with KVM, Apple Silicon M3+) |
| [v0.10.0](v0.10.0.md) | 2026-09-26 | OpenSandbox API: 14 ms creates from a warm pool, plus code interpreter, terminals and snapshots |
| [v0.9.1](v0.9.1.md) | 2026-09-26 | Security fix: the OpenSandbox API now always requires a key |
| [v0.9.0](v0.9.0.md) | 2026-09-26 | Run OpenSandbox SDK code locally: `sbx serve` speaks the OpenSandbox API |
| [v0.8.0](v0.8.0.md) | 2026-08-31 | Multi-service sandboxes sleep correctly; `egress_allow` works on macOS |
| [v0.7.0](v0.7.0.md) | 2026-08-21 | Sandboxes for AI agents: egress allow-list, checkpoint/resume, `sbx with` |
| [v0.6.0](v0.6.0.md) | 2026-08-18 | Manage remote sandboxes from `sbx ui --connect` |
| [v0.5.0](v0.5.0.md) | 2026-08-18 | Configurable health-check interval and a readable `sbx ui` table |
| [v0.4.0](v0.4.0.md) | 2026-08-17 | One `sbx connect` for a sandbox spread over several deployments |
| [v0.3.0](v0.3.0.md) | 2026-08-17 | Run a sandbox on any container platform, use it on localhost |
| [v0.2.0](v0.2.0.md) | 2026-08-16 | Live dashboard (`sbx ui`), guided `sbx init`, Homebrew install |
| v0.1.0 | 2026-08-16 | First release (no release notes) |

Dates are the tag dates. There is deliberately no `v0.13.1.md`: a file there would let the release
workflow publish a tag that was abandoned on purpose.

The engineering log behind v0.8.0 (measurements and failures found on the way) is kept with the
design documents: [2026-08-31-v0.8.0-engineering-log.md](../design/2026-08-31-v0.8.0-engineering-log.md).

## For maintainers

Each `vX.Y.Z.md` here is published verbatim as the body of that tag's GitHub release:
`.github/workflows/release.yaml` refuses to build a tag without `docs/release-notes/<tag>.md` and
uses it as the release body, with GitHub's generated commit list appended underneath. That is why
every link inside those files is absolute and pinned to a tag; `scripts/lint-docs.sh` enforces it.
New notes follow [TEMPLATE.md](TEMPLATE.md).
