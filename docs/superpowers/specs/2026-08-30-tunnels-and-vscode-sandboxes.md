# sbx — tunnels, networking, VS Code: what history says and what I measured

Date: 2026-08-30. Corpus: 73 sbx transcripts (~101 MB) across
`~/.claude/projects/-Users-raramuri-Projects-aryanmehrotra-sbx` and the two `personal-sbx`
dirs, mined with a Go scanner (350 keyword-correlated hits reviewed), plus the repo's own
`docs/DECISIONS.md`, `docs/TROUBLESHOOTING.md` and the `sbx connect` design spec.

Everything below marked **measured** was run on this machine today against a live daemon,
colima 29.2.1, cloudflared 2026.8.2.

---

## A. Already fixed — do not redo

| what | where |
|---|---|
| `sbx url` handed out cloudflared's control plane (`api.trycloudflare.com`), answering 405 while printed as success | `8c29b5d` |
| `StderrPipe` dead code in the tunnel scanner (`Cmd.StderrPipe` errors when `Cmd.Stderr` is set, so the goroutine never ran) | in `tunnel.go` today |
| 64 KB `bufio` overflow that hung `sbx url` on a long banner | `scanner.Buffer(..., 1 MB)` |
| `sbx connect` splicing a client into a different service after a redeploy | every dial carries `ref`/instance → 409 |
| L7 proxies stripping ALPN and refusing CONNECT | WebSocket transport, proven through a real reverse proxy and live on zopcloud |

**One transcript scare that was not a bug:** "a payload larger than one relay chunk →
ConnectionResetError". It was sbx's own 405 on a GET-only route, reproduced identically
*without* the tunnel. 20 pipelined GETs through the tunnel returned 20 responses.

---

## B. Two new defects, found by measurement, now fixed

### B1 — `sbx url` breaks every host-checking dev server

cloudflared rewrites `Host` to the tunnel hostname. Measured against a Host-echo origin:

```
direct:      Host=127.0.0.1:19999
via tunnel:  Host=myself-attribute-troops-percentage.trycloudflare.com
```

Against a **real Vite dev server**:

```
curl direct                                   -> 200
curl -H 'Host: <name>.trycloudflare.com'      -> 403
  Blocked request. This host ("…trycloudflare.com") is not allowed.
  To allow this host, add … to `server.allowedHosts` in vite.config.js.
```

Same class in webpack-dev-server (`Invalid Host header`), Rails
(`ActionDispatch::HostAuthorization`), Django (`ALLOWED_HOSTS`), Next.js dev.
This is the README's headline use case — share a branch preview with a reviewer — failing on
the most common kind of thing a reviewer gets shown.

**Fixed** — `--http-host-header 127.0.0.1:<port>`, behind `sbx url --host-header rewrite|pass`,
defaulting to `rewrite`. Verified live end to end: `sbx url vitelab web` → **200**, page renders.

### B2 — cloudflared squats inside sbx's own port block

sbx allocates public ports `20000 + slot*20 + i`, blockSize 20, maxSlots 60 → **20000–21199**
(`internal/provider/docker_provider.go:33`). cloudflared's default metrics listener takes the
first free port of **20241–20245** = sbx **slot 12**, indices 1–5. Three concurrent cloudflareds
took 20241 / 20244 / 20245.

```
before:  nothing on 20241-20245
start cloudflared (what `sbx url` does)
  -> "Starting metrics server on 127.0.0.1:20241"
  -> lsof: cloudflared LISTEN 127.0.0.1:20241
  -> net.Listen 127.0.0.1:20241 => bind: address already in use
```

So `sbx url` can take a port out from under the daemon it depends on — and only when slot 12
happens to be unallocated, so it is an intermittent bind failure with no visible cause.

**Fixed** — `--metrics 127.0.0.1:0`. Verified live: metrics moved to 51025, outside the range.

Both fixes have tests (`internal/tunnel/tunnel_test.go`). Full suite green, `go vet` clean.

---

## C. VS Code sandboxes — measured, and the obvious design does not survive it

VS Code is essentially absent from sbx history. This is a new feature, not a regression.

### What works today, unchanged

A code-server sandbox is just a service. Built one (`codercom/code-server`, port 8080) and it
behaved correctly: wake on first connection, real workbench in a browser, sleeps when the
client goes away.

### The cost, measured

| | |
|---|---|
| resident while idle, one tab open | **764 MiB** |
| CPU while idle, one tab open | **5.8 %** |
| image | **1.54 GB** |
| client↔server traffic, editor open, nobody typing | **927 / 956 / 927 B/s** over three 60 s windows |
| client↔server traffic, sustained typing (497 chars in 60 s) | **3136 B/s** |

### The finding that decides the design

sbx measures idleness on **last byte** (`internal/daemon/proxy.go:45`, touched in `pipe()` both
ways) — `DECISIONS.md`, "Bytes, not connections". Correct for a connection pool. An editor is
the case it does not cover: the client heartbeats, so bytes never stop.

Measured with a headless-Chrome workbench and the daemon at `--idle 60s`, Chrome liveness
re-checked every tick: **awake continuously from t=20 s to t=180 s+**, four established
connections, against a 60-second window. It sleeps normally once the client goes away.

So the behaviour is "awake exactly as long as the tab is open" — defensible, but it means a tab
left open overnight holds **764 MiB** for the night.

**The obvious fix does not work.** A byte-rate floor (`idle_floor: 2KB/s` — don't count traffic
below a rate as activity) is the natural refinement of "bytes, not connections". The measurement
kills it: idle **927 B/s** vs active **3136 B/s** is only **3.3×**, and *reading code on screen
produces exactly the idle rate*. Any threshold that sleeps an abandoned tab also sleeps a
developer who is reading, and sleeping an editor costs them their terminals and server-side
state. There is no byte-level signal that separates "present and reading" from "gone".

This is a negative result and it should be written down rather than rediscovered.

### What to build instead — see PLAN.md
# Plan — tunnels and VS Code sandboxes

Read `FINDINGS.md` first; every recommendation here is downstream of a number in it.

---

## Already done in this branch (code + tests, verified live)

### 1. `sbx url --host-header rewrite|pass`, default `rewrite`
`internal/tunnel/tunnel.go`, `internal/app/app.go`, tests in `internal/tunnel/tunnel_test.go`.

Fixes a **403 on every host-checking dev server** (vite, webpack-dev-server, Rails, Django,
Next.js). Verified end to end: `sbx url vitelab web` → 200 against a real Vite.

`pass` keeps the old behaviour, for a service that needs its public name — absolute links,
OAuth callbacks, a cookie domain. The command prints which it did, either way.

**Decision for you:** `rewrite` as the default is a behaviour change. It is the right one on the
measurement (the documented use case is broken today), but it is your call. Flipping the default
to `pass` is a one-word change.

Only cloudflared can do this: ngrok removed `--host-header` in v3 in favour of a traffic-policy
file. Asking for it on ngrok or ssh is an error naming cloudflared, not a silent no-op.

### 2. cloudflared no longer squats on sbx's port block
`--metrics 127.0.0.1:0`. Removes an intermittent `bind: address already in use` on sbx slot 12
that had no visible cause. No flag; there is no reason to want the old behaviour.

---

## VS Code sandboxes — what I recommend building

The measurement rules out the design people reach for first (a byte-rate idle floor: idle
927 B/s vs active 3136 B/s, only 3.3× apart, and *reading* code sits at the idle rate). So the
answer is not a smarter idle timer. It is to stop making the editor a thing that has to sleep.

### V1 — `sbx code <sandbox> [service]`  ← the one I'd build

Attach the developer's **own** VS Code to the sandbox container, via Dev Containers' attach URI:

```
vscode://vscode-remote/attached-container+<hex of {"containerName":"/sbx-<sandbox>-<service>"}>/<workdir>
```

- Nothing added to the image. No 1.54 GB pull, no 764 MiB resident, no tunnel, no auth.
- VS Code Server lives in the container only while attached and exits on disconnect, so the
  sandbox goes back to sleeping on its own rule with no new mechanism.
- "Window open" is a far tighter proxy for attention than "browser tab open".
- Composes with everything: `sbx env`, `sbx logs`, egress rules, snapshot/fork are unchanged.

**Two things to verify before shipping, neither verifiable on this machine (no local VS Code
installed here):**
1. Attaching makes VS Code run `docker start` out of band. `116e61e` ("revoke an awake belief
   the provider contradicts") should reconcile that, but it needs a live check.
2. The workdir to open. Read it from the image config, or take it as an argument.

Size: small. One CLI command, a hex-encoded URI, an `open`/`xdg-open`, and a printed fallback
for anyone whose editor is not on PATH.

### V2 — a browser editor, as an example rather than a template

`examples/vscode/` with a `sandbox.json` for code-server, **not** a bundled `sbx templates`
entry: a 1.54 GB digest-pinned image is a lot of weight in the template list for a service most
sandboxes will not want.

The example's job is to be honest about the trade: the editor is awake while the tab is open and
costs 764 MiB, and the services it edits against sleep to 0 B independently, which is the actual
win. That pairing is already expressible — per-service `idle` exists.

Worth it for the weak-laptop case and for handing someone a link to an editor, which `sbx url`
now serves correctly.

### V3 — `sbx init --from-devcontainer`

Read `.devcontainer/devcontainer.json`, emit `sandbox.json`. Independent of everything above,
and the highest-adoption item on this page: every repo that already has a devcontainer becomes
an sbx sandbox without anyone writing a spec.

---

## Explicitly rejected, with the reason

| | why |
|---|---|
| `idle_floor` / byte-rate activity threshold | idle 927 B/s vs active 3136 B/s is 3.3×, and reading code sits at the idle rate. Any threshold that sleeps an abandoned tab also sleeps someone reading — and sleeping an editor costs them terminals and unsaved server-side state. |
| `idle: "never"` on an editor service | pins 764 MiB for the sandbox's life. Already the known anti-pattern. |
| Parsing WebSocket control frames to discount keepalives | needs the daemon to understand a protocol. Breaks "the socket is the only signal", and would not help anyway — the 927 B/s is application traffic, not just pings. |

---

## Smaller things this research surfaced

- **`sbx url --json`** — the URL is only ever printed. An agent or a CI job that wants to hand
  someone a link has to scrape stdout. Small, and it makes `sbx url` scriptable.
- **`sbx doctor` has no `sbx serve` row** — flagged in a past review in the transcripts and still
  true: doctor reports docker, cloudflared, kubectl and redis-cli, and never checks the one thing
  whose absence breaks everything. `daemon.Running()` is already pid-verified.
- **`sbx connect` on kubernetes is still assumption, not measurement.** The design spec names
  three unverified items (the activator multiplexes many wake ports onto one pod; `activator.yaml`
  has no container port or Service for the connect endpoint; limits need a wider Role). Worth
  closing before anyone relies on it.

---

# Part 2 — waking, dependencies, and the agent box

Added after the first pass, chasing "containers not waking up / the dependency thing not
working". Three defects, all reproduced on a live two-service sandbox, all now fixed.

## Already fixed before this session

- **`fb02e29`** — a connection woke only what it addressed, so a woken service dialled a
  stopped peer and got `no such host`. Six services on a fourteen-service sandbox died that way
  within a minute of their datastores being slept. `depends_on` is now walked before a start.
- **`116e61e`** — a dependency stopped out of band stayed believed-awake forever, because
  nothing dials a dependency from the daemon and so nothing corrected the belief.

Both hold. What follows is what they did not cover.

## D1 — a service could not be reached by its own name

`depends_on: ["db"]` wakes db correctly, and then the dependent configured with `db:6379`
still fails. Docker's embedded DNS knows the *container*, `sbx-<sandbox>-<service>`, and sbx
never registered the short name that the spec, `depends_on`, `sbx logs` and `sbx exec` all use.

Measured on the sandbox network:

```
nslookup db              -> ** server can't find db: NXDOMAIN
nslookup sbx-deplab-db   -> Address: 172.19.0.2
```

The app dialling `db:6379` once a second: **ok=0 failed=23**.

The error is `no such host` — *identical* to the one `fb02e29` fixed — so it reads as the wake
bug coming back, which is what makes it expensive.

**Fixed** — `--network-alias <service>` on create. After: **ok=20 failed=0**.

## D2 — the reaper slept a dependency out from under an awake dependent

`reap()` consulted `keepAwake` and each unit's own idle clock, and never `dependsOn`.

A dependency is used over the sandbox's own network, which the daemon is not on: none of that
traffic touches a proxy leg, so the dependency's idle clock expires while it is in constant use.
`fb02e29` fixed waking; nothing stopped the reaper from re-creating the same condition during
steady operation.

Measured, `--idle 45s`, `app` polled every 15 s and `db` never touched directly:

```
t= 45s  ok=60  failed=0
t= 60s  ok=72  failed=4     <- db slept underneath it
t= 75s  ok=83  failed=8
t= 90s  ok=94  failed=12    <- +4 every 15s, indefinitely
```

and `sbx list` said `db awake` throughout.

**Fixed** — the reaper skips a unit an awake unit depends on. A stack now sleeps from the top
down: dependents idle out first, their datastores become eligible a tick later. After the fix,
**165 seconds, failed frozen at 35, ok climbing 175 -> 326, zero new failures.**

And the stack still reaches 0 B, in the right order - `app slept - idle for 54s` at 01:36:41,
`db slept - idle for 45s` at 01:37:41, both containers then at 0 B, and one connection wakes the
pair back in 0.18 s with `ok=1 failed=0`.

Regression test confirmed to fail without the guard and pass with it, plus one that an
unneeded service still sleeps and one that `depends_on` does not cross sandboxes.

## D3 — `egress_allow` cannot work on a VM-backed docker, and said so only in a log

The allow-list is enforced by a filtering proxy the daemon binds **on the sandbox's bridge
gateway**. On colima and Docker Desktop that gateway is inside the Linux VM and `sbx serve` runs
on the host, so the bind cannot succeed:

```
WARN  egress filter could not bind 172.20.0.1:20999:
      listen tcp 172.20.0.1:20999: bind: can't assign requested address
```

Confirmed: `172.20.0.1/16` is on `br-…` inside the VM; the host's only `172.20.x` is `en0`; and
`net.Listen("tcp","172.20.0.1:20999")` fails outright.

The consequence is the failure shape this project keeps naming: `sbx create` reported success,
the service came up healthy, and the box had **no egress at all** — not even to the allowed
hosts. Proven from inside: `api.anthropic.com -> 000` and `example.com -> 000`. It fails closed,
which is the safe direction and the wrong report; the only signal was a WARN every refresh tick.

**Fixed** — create now probes the gateway and refuses with the reason and the way out, the same
way `--isolation gvisor|kata` and `egress: "deny"` on kubernetes are refused rather than
silently approximated.

**Not fixed, and it is the real fix:** run the filter as a container on the sandbox network
instead of as a host listener. That would make `egress_allow` work on every platform rather than
only where docker is native — and it matters directly for the agent box, which is the shape that
wants an allow-list most.

## What this means for running an agent inside a sandbox

The pieces exist: use case 9 is the box, `egress_allow` is the boundary, `idle` is the timer.
A `node:22-alpine` image with the Claude Code CLI baked in builds and runs (`claude 2.1.251`).

Two things stand between that and a working agent box on this machine:

1. **`egress_allow` does not work here at all** (D3). Until the filter moves into a container,
   an agent box on macOS gets `egress: "deny"` (no API) or unrestricted egress (no boundary).
2. **`idle` has no signal for work happening inside the box.** The daemon measures bytes through
   its own proxy; an agent thinking, compiling or calling an API sends none of them. The only
   setting that works is `idle: "never"`, which never sleeps.

For (2) there is a signal sbx already has and throws away: **the egress filter is in the data
path**, so every API call an agent box makes passes through code sbx owns — and `touch()` is
called only from `proxy.go`. Stamping activity from the egress filter would make an agent box
that is calling an API count as busy, and one that has been silent for the window sleep, with no
new mechanism and no protocol knowledge. That depends on D3's real fix landing first, and it is
the thing I would build next.
