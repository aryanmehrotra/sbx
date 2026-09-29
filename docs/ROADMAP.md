# Roadmap

What sbx builds next and what it will not build, as of v0.15.1 (2026-09-29). What already shipped
is in the [release notes](release-notes/README.md).

An item gets on this list if it makes the wake path faster, work on more workloads, or run behind
a stronger boundary. It names the [decision](DECISIONS.md) it answers to or the gap it closes.
Sizes are rough estimates for one engineer: S is under a week, M is 2–4 weeks, L is over a month.
There are no dates.

## Now

| Item | Why | Size | Status |
|---|---|:---:|---|
| `sbx create` and `sbx with` tell the daemon at once | A new sandbox waits for the daemon's 15 s refresh, so `sbx with` took 5.0 s where Testcontainers took 2.5 s ([BENCHMARKS](BENCHMARKS.md#databases-for-branches-and-tests)) | S | Not started |
| Bare-metal microVM wake and burst numbers | E2B, isorun and OpenSandbox publish microVM figures; sbx has only nested runs | S | Harness exists: `SBX_FC_E2E=1 go test -run FirecrackerE2E ./internal/provider`; needs a bare-metal host |
| The microVM's disk cost in `sbx doctor` and BENCHMARKS | A sleeping microVM keeps a snapshot about the size of its RAM on disk | S | Not started |
| The helper VM run end to end on a Mac and on Windows 11 | microsandbox and Docker Sandboxes run microVMs on all three OSes | M | See [platform status](ARCHITECTURE.md#platform-status) |
| The warm pool through the helper VM | Without it, burst creates on a Mac use the docker pool | M | Not built |
| `sbx connect` picks up sandboxes created after it starts | Agents on other machines cannot reach a new sandbox without a restart, which [SELF-HOSTING.md](SELF-HOSTING.md) works around | S | The helper VM already mirrors new sandboxes; `sbx connect` does not |
| A systemd unit for the microVM daemon in `deploy/` | `deploy/` ships one for docker only; SELF-HOSTING.md gives an untested one | S | Not started |
| A docs site with search | E2B, Daytona, Modal and OpenSandbox have one | S | Not started |

## Next

| Item | Why | Size | Status |
|---|---|:---:|---|
| Secrets kept out of the sandbox | OpenSandbox, Vercel, isorun and Cloudflare have it ([comparison](COMPARISON.md#honest-ranking)) | L | Designed below |
| Egress rules by path, method and header | Allow `POST api.x.com/v1/*` but not `DELETE` | S | Not started |
| Isolated sessions | The biggest OpenSandbox API gap: 48 upstream tests fail on it | not estimated | Needs design |
| A code-interpreter guide | sbx already serves OpenSandbox's code-interpreter image | S | Upstream `e2e` file passes; no guide |
| Guides for Claude Code, Cursor and Codex, and a GitHub Action | Agent users look for a cookbook | S each | [GUIDES.md](GUIDES.md#ai-agents) exists; no Action |
| `files`, `init` and host mounts in a microVM | The microVM refuses them today | M | Refused by name |
| Exec sessions on the CLI | The OpenSandbox API has command sessions and background commands; `sbx exec` does not | S | API shipped, CLI not |
| The daemon off root; a quota on the state filesystem | Needed before anonymous untrusted code on a shared host | M | Open; see [SECURITY.md](../SECURITY.md) |

### Secrets kept out of the sandbox

Today an agent that calls an API needs the key in its environment, so it can leak it. The plan is
to terminate TLS at the egress filter and add the credential there. The agent sends an
unauthenticated request to an allowed host, and the filter adds the header on the way out.
OpenSandbox's credential vault API then sits on top.

It needs a per-sandbox CA, leaf certificates minted on demand, and that CA in each image's trust
store. Postgres needs its own path, because it starts TLS after the TCP connection is up. Only
domains with a rule that needs it are decrypted; everything else passes through as today. The
filter's design: [DECISIONS.md](DECISIONS.md#a-live-egress-policy-is-held-by-the-filter-and-pushed-to-it).

## Later

| Item | Why | Size |
|---|---|:---:|
| Snapshot retention (expiry, keep-last-N) | A long-lived branch should not pile up snapshots | S |
| Reusable volumes with a lease | Single-writer leasing for a shared cache volume | M |
| Egress filtering on Kubernetes | A cluster refuses `egress_policy` today. Plan: NetworkPolicy plus an egress gateway | S–M |
| Forking a microVM's memory under a new name | sbx forks a VM's disk only | M |
| A local Go API | Drive sandboxes from a test harness in-process. Local only | S–M |
| `devcontainer.json` import out of preview | Coder, Ona and DevPod read it natively | S |
| Deploy recipes for your own VM or cluster | One command to your own cloud, not a hosted sbx | S |

## Not built yet in the OpenSandbox API

When the OpenSandbox API, or the command server sbx runs inside each API sandbox, answers
`501 … not built yet (docs/ROADMAP.md)`, the feature is listed here. Each is refused by name, not
approximated ([why](DECISIONS.md#the-opensandbox-api-on-a-microvm-the-agent-is-pid-1-the-token-is-the-apis-a-snapshot-is-the-disk)).

| Refused feature | What it is | Size |
|---|---|:---:|
| Isolated sessions (`/v1/isolated/*`) | A command session with its own PID namespace, `/tmp` and overlay | not estimated |
| `credentialProxy`, credential vault | Credentials added to matching outbound requests | L, on the secrets work above |
| Server-side pools (`extensions.poolRef`) | A pool the server owns, named at create. `--osb-pool` serves unnamed creates | not estimated |
| `secureAccess`, signed endpoints (`?expires=`) | Endpoints that need a signature or token | not estimated |
| Server-proxied endpoints (`use_server_proxy=true`) | The command server reached through the lifecycle server | not estimated |
| Registry credentials in `image.auth` | Pull credentials in the create request. Today, `docker login` on the host | not estimated |
| Lifecycle hooks, renew-on-access | Hooks around lifecycle events; an expiry extended by each access | not estimated |
| The API on Kubernetes | Needs an init container that puts the command server into any image | M |

Network policies answer 501 for another reason: the daemon was started without egress control.

Upstream conformance (`tests/go`, release-1.1.0, docker, CI, 2026-09-27): the v0.10.0 tier and
upstream's `pool` and `e2e` files pass in CI. The v0.11.0 tier was 70 passed, 48 failed (all
`isolated_session`), 6 skipped; `credential_vault` skips for want of a target host.

## Not doing

| | Why |
|---|---|
| Hosting sbx for anyone | [sbx is a tool people run, not a service](DECISIONS.md#sbx-is-a-tool-people-run-not-a-service-anyone-offers). Deploy recipes for your own infrastructure are fine |
| Auth, tenancy, quotas, per-user tokens | Same decision. Put a gateway in front |
| A remote control plane | Same decision. `create`, `rm` and `exec` stay local; `sbx connect` only tunnels traffic |
| A browser IDE | `sbx ssh` gives editors a Remote-SSH path. Use Codespaces, Coder or Ona |
| Per-agent users inside one sandbox | Two agents get two sandboxes |
| A VM engine on Apple's Virtualization.framework | It cannot snapshot for third parties and needs cgo. [Why](DECISIONS.md#a-microvm-off-linux-runs-in-a-helper-vm-not-on-virtualizationframework) |
| Kubernetes inside a docker sandbox | k3s and docker-in-docker need `privileged`, which sbx does not offer |

Shipped items move to the release notes. Rejected ones move to [DECISIONS.md](DECISIONS.md) with
the measurement that ruled them out. To move something up, open or upvote an issue and say what
you would use it for.
