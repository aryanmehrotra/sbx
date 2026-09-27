# Documentation style

The house style for every page in this repository: what kind of page each file is, how to
write it, and the words we use. For contributors and coding agents; the rules for *when* a doc
must change are the docs contract in [AGENTS.md](../AGENTS.md#docs-contract).

For anything not covered here, follow the
[Google developer documentation style guide](https://developers.google.com/style/highlights).

## Page types (Diátaxis)

Each page is one type. If a page needs two, split it or link out.

| Type | Answers | Pages |
|---|---|---|
| Landing | "What is this, should I try it?" | `README.md` |
| Tutorial | "Take me through it once." | `docs/QUICKSTART.md` |
| How-to | "How do I do X?" | [USE-CASES.md](USE-CASES.md), [AI-AGENTS.md](AI-AGENTS.md), [TROUBLESHOOTING.md](TROUBLESHOOTING.md), `examples/*/README.md` |
| Reference | "What exactly does X accept?" | `docs/CLI.md`, [SPEC.md](SPEC.md), [BENCHMARKS.md](BENCHMARKS.md), release notes |
| Explanation | "Why is it like this?" | [ARCHITECTURE.md](ARCHITECTURE.md), [DECISIONS.md](DECISIONS.md), [COMPARISON.md](COMPARISON.md), [ROADMAP.md](ROADMAP.md), `SECURITY.md` |
| Record | "What did we plan or measure then?" | `docs/design/*` (dated; not user docs, never linked from release notes) |

- Reference states the current behaviour once. No "used to", "since v0.13", "an earlier draft".
  History goes to DECISIONS.md or the release notes.
- Every page opens with one to three lines: what the page is and who it is for.
- A new feature is folded into each page's opening and tables, not appended as a new section.

## Voice

- Second person, active voice, present tense: "sbx wakes the service", not "the service will be woken".
- Lead with what the reader gets, then how, then why. Rationale last, or in DECISIONS.md.
- Sentences of about 25 words or fewer. One idea per paragraph.
- Table cells of about 20 words or fewer. Longer than that, it is prose.
- Define a term the first time a page uses it, or link to the glossary below.
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

## Status vocabulary

Use exactly these labels for what is proven. Do not invent softer ones.

| Label | Means |
|---|---|
| **verified in CI** | A CI job exercises this path end to end on every change. Name the job. |
| **unit-tested** | Tests cover the logic with fakes; no real end-to-end run is gated. |
| **not yet run end to end** | Built, maybe unit-tested, but nobody has run the real path (say where: "on a Windows host"). |

Platform and backend caveats live in one place: the platform-status table in `README.md`. Other
pages link to it instead of repeating caveats in hero text or feature rows. Update the table in the
PR that changes what CI covers.

## Headings, anchors and links

- Other files link to headings by `#anchor`. Before renaming a heading, run
  `grep -rn "FILE.md#" .` and update every linker in the same change. DECISIONS.md headings are
  never renamed; add an index above them instead.
- Do not renumber `ROADMAP §N`: code comments cite section numbers.
- Runtime errors print `docs/ROADMAP.md`, so ROADMAP keeps a findable list of what is not built.
- Relative links inside the repo; absolute, tag-pinned links in release notes (see below).
- A reference-style link needs its definition line (`[name]: https://...`) in the same file.
- `scripts/lint-docs.sh` checks all of the above except the first bullet's grep. Run it.

## Release notes

`docs/release-notes/TEMPLATE.md` is the format. In short: title names the feature the way the
user types it; breaking changes first; absolute links pinned to the tag; about 600 words for a
minor release and 250 for a patch; rationale goes to DECISIONS.md. Between releases, each PR adds
one line to [UNRELEASED.md](release-notes/UNRELEASED.md).

## Glossary

Use these words with these meanings, in every page.

| Term | Meaning |
|---|---|
| sandbox | One named, isolated copy of a project's services (one per branch, task or agent). Not "box" or "env". |
| service | One container or process inside a sandbox, such as `postgres`, with its own ports. |
| spec | The `sandbox.json` file that declares a sandbox's services. Reference: [SPEC.md](SPEC.md). |
| template | A built-in spec used with `--template` (`postgres`, `browser`, `nginx`, `web-stack`, `analytics`). |
| daemon | `sbx serve`: the long-running process that holds the ports, wakes and sleeps sandboxes, and serves the APIs. |
| provider | The backend that runs sandboxes: `docker`, `kubernetes` or `firecracker`. |
| sleep | Stop a service after its idle window so it uses no memory or CPU. On a microVM, sleep is a snapshot. |
| wake | Start a sleeping service because something connected. The first connection is held, not refused, until it is ready. |
| freeze | Pause a service with its memory kept (`docker pause`), so it resumes faster than a wake. An API `pause` is a freeze that traffic does not undo. |
| helper VM | The Linux VM sbx runs (via lima or colima) on a Mac or Windows host so Firecracker microVMs can run there. |
| microVM | A Firecracker VM with its own kernel; the `firecracker` provider's unit of isolation. |
| snapshot, fork | A saved copy of a sandbox's data; a new sandbox created from a snapshot. |
