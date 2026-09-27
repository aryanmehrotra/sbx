<!--
Copy to docs/release-notes/vX.Y.Z.md. The file becomes the GitHub release body verbatim
(.github/workflows/release.yaml), with GitHub's commit list appended. Do not restate that list.

Rules:
- Length: at most 60 lines. Link BENCHMARKS, DECISIONS or GUIDES at the tag for detail.
- Every link is absolute and pinned to the tag. Relative links 404 in a release body, and
  scripts/lint-docs.sh rejects them:
    github.com/aryanmehrotra/sbx/blob/vX.Y.Z/PATH           (docs, code)
    raw.githubusercontent.com/aryanmehrotra/sbx/vX.Y.Z/PATH (images)
- Title: `# sbx vX.Y.Z — <what you can now do>`, naming what the user types. About 12 words.
- Summary: 2-3 sentences. What you can do now, and who should upgrade.
- Every breaking or behaviour change goes in "Before you upgrade" with what to do, even if a
  highlight mentions it too.
- Highlights: one line each, with a command where it helps. At most one number, already in BENCHMARKS.
- Security release: the summary starts with "Upgrade now if" and names the affected versions.
- Explain an outside term in a few words at first use. The note is read alone.
- No design narrative, no measurement tables, no bold lead-ins. Promise no future version.
- Omit empty sections. Keep the order. Fixes are user-visible symptoms, one line each.
- A published note is not rewritten to describe a later release. Add a dated "Update:" line.
-->

# sbx vX.Y.Z — <what you can now do>

Released YYYY-MM-DD.

<2-3 sentences: what you can do now that you couldn't, and who should upgrade.>

## Before you upgrade

- Breaking: <old behaviour> becomes <new behaviour>. Do this: `<exact command, flag or field>`.
- Behaviour change: <what you will notice>. Do this: <how to keep the old behaviour, or nothing>.
- New requirement: <host prerequisite>. Do this: `sbx doctor`.

## Highlights

- <What you can do now>: `<a command that runs as written>`.

## Fixes

- <The symptom you saw> no longer happens <when or where>.

## Known limitations

- <What does not work yet, where it has not been run end to end, security caveats.>

## Upgrade

```sh
brew upgrade sbx                  # first install: brew install aryanmehrotra/tap/sbx
curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | VERSION=vX.Y.Z sh
go install github.com/aryanmehrotra/sbx@vX.Y.Z
sbx doctor
```

Binaries for macOS, Linux, FreeBSD and Windows (amd64, arm64) are attached with `SHA256SUMS`.
Cluster activator: `ghcr.io/aryanmehrotra/sbx-activator:vX.Y.Z`.

Full changelog: https://github.com/aryanmehrotra/sbx/compare/vPREV...vX.Y.Z
