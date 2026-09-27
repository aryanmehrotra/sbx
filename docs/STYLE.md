# Documentation style

The house style for every page in this repository: what kind of page each file is, how to
write it, and the words we use. For contributors and coding agents. This page owns *how to
write*; the rules for *when* a doc must change, and every other project rule, are in
[AGENTS.md](../AGENTS.md#docs-contract).

For anything not covered here, follow the
[Google developer documentation style guide](https://developers.google.com/style/highlights).

## Page types (Diátaxis)

Each page is one type. If a page needs two, split it or link out.

| Type | Answers | Pages |
|---|---|---|
| Landing | "What is this, should I try it?" | `README.md` |
| Tutorial | "Take me through it once." | `docs/QUICKSTART.md` |
| How-to | "How do I do X?" | [USE-CASES.md](USE-CASES.md), [AI-AGENTS.md](AI-AGENTS.md), [TROUBLESHOOTING.md](TROUBLESHOOTING.md), `examples/*/README.md` |
| Reference | "What exactly does X accept?" | `docs/CLI.md`, [SPEC.md](SPEC.md), [BENCHMARKS.md](BENCHMARKS.md) |
| Explanation | "Why is it like this?" | [ARCHITECTURE.md](ARCHITECTURE.md), [DECISIONS.md](DECISIONS.md), [COMPARISON.md](COMPARISON.md), `SECURITY.md`'s threat model |
| Project | "Where is it going, what changed, how do I take part?" | [ROADMAP.md](ROADMAP.md), release notes, `CONTRIBUTING.md`, `SECURITY.md`'s reporting and support policy |
| Record | "What did we plan or measure then?" | `docs/design/*` (dated; not user docs, never linked from release notes) |

- Reference states the current behaviour once. No "used to", "since v0.13", "an earlier draft".
  History goes to DECISIONS.md or the release notes.
- Every page opens with one to three lines: what the page is and who it is for.
- A new feature is folded into each page's opening and tables, not appended as a new section.
- DECISIONS.md entries are records: its headings are frozen and old entries are not restyled.
  This page applies to new entries only, so a review does not flag the old ones.

## Voice

- Second person, active voice, present tense: "sbx wakes the service", not "the service will be woken".
- Lead with what the reader gets, then how, then why. Rationale last, or in DECISIONS.md.
- Sentences of about 25 words or fewer. One idea per paragraph.
- Table cells of about 20 words or fewer. Longer than that, it is prose.
- **Assume zero prior knowledge.** Explain every outside term in plain words at its first use on
  each user-facing page: one short clause, even if another page already explained it. Use the
  wording in [Outside terms](#outside-terms). House words link to the [glossary](#glossary).
- **Internal names stay internal.** `execd`, seal, slot, activator, jailer and similar
  never appear in a user-facing page without a plain explanation next to them. If the reader
  never types it and never sees it in output, leave it out.
- **Persuade with proof.** Every benefit claim carries a number (with its source), a test that
  proves it, or a link. "Fast", "secure" and "lightweight" alone are not claims. Never trade an
  honest caveat for a stronger sentence; the caveat is part of the proof.
- No meta-commentary about the project's past mistakes in user docs. Those belong in
  DECISIONS.md, CONTRIBUTING.md or the Lessons log in AGENTS.md.
- No emoji. For warnings use **Warning:** in bold. Match the file's dash style (`-` or `—`).
- Sentence-case headings. `sbx` is lower-case and in code font; so are commands, flags, fields,
  env vars and file names.

## Code blocks

- Copy-pasteable and correct against the code at the current release. Check commands and flags in
  `internal/app/app.go` and `internal/app/help.go`, `sbx serve` flags in `internal/daemon/serve.go`,
  spec fields in `internal/spec/`.
- Comments in a block say what the line gets you (`# ~492 ms once the image is local`), with a
  number only if it is sourced (below).

## Numbers and sources

- Every number we measured names the script that produced it and where it was measured (machine,
  version or date) in [BENCHMARKS.md](BENCHMARKS.md). Pages that reuse it link there.
- Every vendor number is quoted from the vendor, linked, and dated ("checked 2026-09-27").
- Never round in our favour, never drop a caveat that changes the meaning, never infer a figure.
- A stale figure is labelled with the version it was measured on, not silently kept or removed.
- A/B comparisons interleave and alternate their rounds: running A then B hands B a docker daemon
  A has just loaded, and that once reversed the sign of a result here.
- A delta inside the run-to-run spread is "not resolvable", not a result. Say so; a change that
  is less work in theory but measures as nothing is fine, described that way.
- A vendor link is checked by `lint-docs.sh` for resolving, not for still saying the figure. The
  reviewer opens it.

## Status vocabulary

Use exactly these four labels for what is proven, and start a status cell with one of them, then
the evidence. Do not invent softer ones ("verified", "works", "supported").

| Label | Means |
|---|---|
| **verified in CI** | A CI job exercises this path end to end on every change. Name the job. |
| **unit-tested** | Tests cover the logic with fakes; no real end-to-end run is gated. |
| **run by hand** | Someone ran the real path outside CI. Say where and at what version ("on an M4 with colima, v0.11"). |
| **not yet run end to end** | Built, maybe unit-tested, but nobody has run the real path (say where: "on a Windows host"). |

Platform and backend caveats live in one place: the platform-status table in `README.md`. Other
pages link to it instead of repeating caveats in hero text or feature rows. Update the table in the
PR that changes what CI covers.

## Headings, anchors and links

- Other files link to headings by `#anchor`. Before renaming a heading, run
  `grep -rn "FILE.md#" .` and update every linker in the same change. DECISIONS.md headings are
  never renamed; add an index above them instead.
- Runtime errors print docs/ROADMAP.md, and COMPARISON links its
  `#not-built-yet-in-the-opensandbox-api` anchor, so keep that heading's text. CI checks it.
- Relative links inside the repo; absolute, tag-pinned links in release notes (see below).
- A reference-style link needs its definition line (`[name]: https://...`) in the same file.
- `scripts/lint-docs.sh` checks links, anchors and reference links; `scripts/lint-docs-contract.sh`
  checks the ROADMAP heading. Run both. Neither runs the first bullet's grep for you.

## Release notes

`docs/release-notes/TEMPLATE.md` is the format. In short: title names the feature the way the
user types it; breaking changes first; absolute links pinned to the tag; about 120 lines at most
(the one length limit, set in TEMPLATE); rationale goes to DECISIONS.md. Between releases, each PR adds
one line to [UNRELEASED.md](release-notes/UNRELEASED.md).

## Glossary

Use these words with these meanings, in every page.

| Term | Meaning |
|---|---|
| sandbox | One named, isolated copy of a project's services (one per branch, task or agent). Not "box" or "env". |
| service | One container or process inside a sandbox, such as `postgres`, with its own ports. |
| spec | The `sandbox.json` file that declares a sandbox's services. Reference: [SPEC.md](SPEC.md). |
| template | A built-in spec used with `--template` (see `sbx templates` or `examples/README.md`). |
| daemon | `sbx serve`: the long-running process that holds the ports, wakes and sleeps sandboxes, and serves the APIs. |
| provider | The backend that runs sandboxes: `docker`, `kubernetes` or `firecracker`. |
| sleep | Stop a service after its idle window so it uses no memory or CPU. On a microVM, sleep is a snapshot. |
| wake | Start a sleeping service because something connected. The first connection is held, not refused, until it is ready. |
| freeze | Pause a service with its memory kept (`docker pause`), so it resumes faster than a wake. An API `pause` is a freeze that traffic does not undo. |
| helper VM | The Linux VM sbx runs (via lima or colima) on a Mac or Windows host so Firecracker microVMs can run there. |
| microVM | A Firecracker VM with its own kernel; the `firecracker` provider's unit of isolation. |
| snapshot, fork | A saved copy of a sandbox's data; a new sandbox created from a snapshot. |
| preview feature | Off until named in `SBX_FEATURES`; `sbx features` lists them. Also "gated feature". |

## Outside terms

Explain these at first use on each user-facing page, in these words or close to them. A new
outside term gets a row here in the PR that first uses it.

| Term | Say |
|---|---|
| OpenSandbox | an open-source API standard for AI-agent sandboxes, with SDKs in 5 languages |
| MCP | Model Context Protocol, the standard way AI assistants such as Claude or Cursor call outside tools |
| Firecracker | the open-source virtual machine monitor AWS built for Lambda |
| microVM | a small virtual machine with its own kernel, so a sandbox does not share the host's |
| gVisor | a runtime that runs containers on its own user-space kernel instead of the host's |
| Kata Containers | a runtime that runs each container inside its own lightweight VM |
| CRIU | a Linux tool that saves a running process's memory to disk and restores it later |
| E2B, Daytona | hosted sandbox services for AI agents, run in the vendor's cloud |
| egress | traffic leaving a sandbox for the network |
| warm pool | sandboxes started ahead of time, so a create is answered at once |
| execd | the small agent sbx runs inside a sandbox to execute commands for the API |
| jailer | Firecracker's launcher that locks each VM's process into its own directory as an unprivileged user |
