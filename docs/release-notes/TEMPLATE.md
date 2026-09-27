<!--
The release-notes standard. Copy this file to docs/release-notes/vX.Y.Z.md and fill it in.

Why the rules below exist:
- The file is published verbatim as the GitHub release body (.github/workflows/release.yaml reads
  docs/release-notes/${tag}.md as body_path). A relative link resolves against /releases/tag/…
  and 404s there, so EVERY link is absolute and pinned to the tag:
    github.com/aryanmehrotra/sbx/blob/vX.Y.Z/PATH              (docs, code)
    raw.githubusercontent.com/aryanmehrotra/sbx/vX.Y.Z/PATH    (images)
  scripts/lint-docs.sh rejects relative links in v*.md and checks each pinned file is in its ref.
- GitHub appends the generated commit/PR list underneath. Do not restate it.
- The reader is deciding whether to upgrade: lead with what they can do now, then what it costs.
  Design history belongs in docs/DECISIONS.md; measurements in docs/BENCHMARKS.md.

Rules:
- Title: `# sbx vX.Y.Z — <what you can now do>`, naming the command or field the user types.
  No puns or house phrases. At most ~12 words after the dash.
- No internal type or package names unless the user types them. Define jargon on first use.
- Every number names the machine and the script that measured it. Never round up.
- Every breaking or behaviour change goes in "Before you upgrade", with a "Do this:" line,
  even if a highlight also describes it. Omit the section only when there is nothing.
- Security release: the TL;DR starts with "Upgrade now if …" and names affected versions. It may
  add a short `## What happened` section right after the TL;DR (see v0.9.1.md).
- Promise no future version. Say "not yet" and link docs/ROADMAP.md at the tag.
- Omit empty sections. Keep the order. Aim for ≤ ~120 lines: the one length limit (STYLE.md points here).
- A published note is not rewritten to describe a later release. Add a dated "Update:" line.
- Fixes are user-visible symptoms, one line each. CI, tests and tooling stay in the changelog.
-->

# sbx vX.Y.Z — <what you can now do, using the command name>

Released YYYY-MM-DD.

<TL;DR: 2–3 sentences. What you can do now that you couldn't, and who should upgrade.>

## Before you upgrade

- **Breaking:** <old behaviour> → <new behaviour>.
  **Do this:** `<exact command, flag or sandbox.json change>`.
- **Behaviour change:** <what you will notice>.
  **Do this:** <how to get the old behaviour, or "nothing">.
- **New requirement:** <host, runtime or kernel prerequisite>.
  **Do this:** `sbx doctor` <or the exact check>.

## Highlights

### <Feature, by the name the user types>

<1–2 sentences: what you can do now.>

```sh
<a command that runs as written>
```

<Optional: one measured number, with machine and script, and a tag-pinned link for details,
e.g. [BENCHMARKS.md](https://github.com/aryanmehrotra/sbx/blob/vX.Y.Z/docs/BENCHMARKS.md).>

## Fixes

- <The symptom you saw> no longer happens <when/where>.

## Known limitations

- <What does not work yet, where it has not been run end to end, and security caveats.>

## Upgrade

```sh
brew upgrade sbx                  # first install: brew install aryanmehrotra/tap/sbx
curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | VERSION=vX.Y.Z sh
go install github.com/aryanmehrotra/sbx@vX.Y.Z
sbx doctor                        # checks this machine can run what you use
```

Binaries for macOS, Linux, FreeBSD and Windows (amd64, arm64) are attached below with
`SHA256SUMS`; `install.sh` verifies them. Cluster activator:
`ghcr.io/aryanmehrotra/sbx-activator:vX.Y.Z`.

## Full changelog

https://github.com/aryanmehrotra/sbx/compare/vPREV...vX.Y.Z
