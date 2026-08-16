# sbx connect — reaching a deployed sandbox through one HTTPS endpoint

**Status:** design, not yet built
**Date:** 2026-08-16

## The problem

sbx assumes the sandbox and the client are on the same machine. `Endpoints()` hands out
`127.0.0.1:20040`, and off-cluster the daemon binds loopback
(`internal/daemon/proxy.go:listenAddr`). That is correct for a laptop and it is the only shape
that exists today.

It leaves out the person this project should serve best: somebody whose laptop cannot
comfortably run a Postgres, a ClickHouse and a browser at once. They have a deploy platform —
ZopDay ships an image to a Kubernetes cluster or a VM — and an agent that could use a sandbox
if it could reach one. Today it cannot.

**The constraint that decides the design:** almost no platform gives you a raw TCP port. Vercel
runs containers on top of Functions, caps duration, scales to zero after five idle minutes and
cannot host a raw TCP listener at all. Cloud Run, Railway, Render, App Runner and any
Kubernetes Ingress give you exactly one HTTPS endpoint. A dedicated IP is the exception, not
the rule.

So the transport has to survive a layer 7 proxy that terminates TLS.

## What the prior art says

| | one port? | how clients connect | where auth lives |
|---|---|---|---|
| Teleport | 443 | SNI/ALPN routing — **but** `psql`/`mysql` do STARTTLS, so `tsh proxy db` runs a client-side local proxy | client certs, at the proxy |
| Coder | UDP 41641 + DERP relay | Coder Desktop presents `workspace.coder:PORT` locally | Tailscale identity |
| Codespaces | TLS tunnel | per-port `*.app.github.dev` URLs, HTTP-oriented | GitHub token |
| SSH | 22 | `-L` / `-D` present local ports | keys |

Two findings, and both are load-bearing:

1. **One port on the wire always comes with something on the client presenting local ports.**
   Nobody makes `psql` speak a multiplexing protocol. Teleport — a company whose product is
   this — still ships a client-side local proxy, because database clients negotiate TLS *inside*
   their own protocol rather than before it.
2. **Every one of them authenticates at the tunnel, none inside the service.** That is the
   evidence for keeping auth thin here and letting a gateway do org-level authz.

Teleport also hit our exact wall: behind a layer 7 load balancer, ALPN is stripped at TLS
termination. Their answer (RFD 123) is a **WebSocket upgrade** — `GET /webapi/connectionupgrade`,
`Upgrade: websocket`, `101`, then binary frames — because L7 proxies understand WebSocket
natively and will not interfere with it.

We adopt the same transport for the same reason.

### What this eliminates

- **SSH front door** — needs a raw TCP port. A PaaS will not give one.
- **WireGuard/Tailscale** — needs UDP, and imports a network stack and an identity system into
  a project whose headline claim is zero non-stdlib dependencies, CI-gated.
- **TLS + ALPN routing** — stripped by the L7 proxy in front of us.

One transport survives. That is a finding, not a preference.

## The use cases this has to serve

Written before the design so the design can be checked against them.

| | what happens | does the design cover it |
|---|---|---|
| **1. Weak laptop, agent needs a database** | deploy sbx via ZopDay, `sbx connect`, agent runs `psql` and a test suite against `127.0.0.1:20040` | yes — this is the driving case |
| **2. Agent discovers what exists** | agent asks what sandboxes are there before using one | yes — `GET /v1/fleet` |
| **3. Sandbox is asleep when the agent connects** | first connection wakes it, agent waits and proceeds | yes — the tunnel dials the daemon's own wake proxy, so this is free |
| **4. Laptop already runs a local sbx** | local daemon already owns 20000–21199 on loopback, so the client cannot bind | **partly — see Port collision below.** This is the most likely first bug |
| **5. Two clients at once** | a laptop and a CI job both connect | yes — each TCP connection is its own WebSocket; the server holds no per-client state |
| **6. CI job needs the sandbox** | job runs `sbx connect` in the background, then its tests | yes, and `--sandbox` keeps it to the ports it needs |
| **7. Deployment restarts** | slots are reassigned on recreate, so ports can move | client must be restarted; `/v1/fleet` is fetched once. Called out in Open questions |
| **8. Someone wants the dashboard against a remote** | `sbx ui` on the laptop showing the deployed fleet | **no — out of scope.** `sbx ui` talks to a provider, not to this endpoint. Noted below |
| **9. A service the sandbox does not publish** | agent asks for a port sbx is not fronting | yes — `403`, never dialled |

### Port collision (use case 4)

A laptop already running `sbx serve` owns the whole 20000–21199 range on loopback, so
`sbx connect` cannot open the same numbers. Three ways out, and the trade-off is real:

- `--sandbox my-branch` — only that sandbox's ports. Collides only if the local daemon happens
  to hold the same slot, which it often will.
- `--bind 127.0.0.2` — a second loopback address. Works on Linux without setup; on macOS it
  needs `ifconfig lo0 alias`, which is a sudo step this project should not require.
- `--port-offset 1000` — 20040 becomes 21040. Always works, and **it breaks the property the
  whole design is built on**: `sbx env` output from the server no longer matches the laptop.

v1 ships `--sandbox` and `--port-offset`, and the offset prints a loud note that the remote's
`sbx env` values no longer apply. Preserving the identity is worth more than avoiding the
collision, so the default stays "same numbers" and the offset is the deliberate exit.

## Non-goals

- **Multi-tenancy inside sbx.** One deployed sbx is one person's workspace. Want another? Deploy
  another. This removes users, roles and per-sandbox ACLs from the design entirely.
- **A control API.** `create`, `rm` and `exec` are not exposed over the network by this work.
  `sbx connect` carries *data plane* traffic only — the ports a sandbox already publishes.
  Managing the fleet remotely is a separate piece of work with a much larger blast radius.
- **Replacing `sbx url`.** That stays what it is: a public link to one HTTP service.
- **A remote `sbx ui`.** The dashboard talks to a provider, not to this endpoint. Pointing it at
  a deployment would mean a provider that speaks `/v1/fleet`, which is a different piece of work
  and would want the usage and limits endpoints too. `/v1/fleet` is shaped so that could be
  built later without changing the wire.
- **Hosting sbx on a serverless platform.** sbx needs a long-lived process that owns ports and
  talks to a container runtime. Vercel and friends are the wrong shape of host, and saying so is
  better than half-supporting it.

## Architecture

```
laptop                                one HTTPS endpoint          deployed sbx
──────                                ──────────────────          ────────────
psql       ──▶ :20040 ┐
redis-cli  ──▶ :20003 ┤ sbx connect ──▶ wss://…/v1/connect ──▶ daemon :20040 ──▶ wakes postgres
test run   ──▶ :20000 ┘  (local        Authorization: Bearer     (loopback, never exposed)
                          listeners)
```

Three pieces:

**1. A connect server inside `sbx serve`.** One `net/http` listener, off by default, enabled by
`--connect-addr`. It serves three routes and nothing else. The sandbox ports stay bound to
loopback exactly as they are today — the only thing reachable from outside is this handler.

**2. The wire.** One WebSocket per TCP connection, binary frames each way, splicing the client's
bytes to `127.0.0.1:<port>` on the server.

**3. `sbx connect` on the laptop.** Asks the server what sandboxes exist and on which ports, then
opens local listeners **on the same port numbers**.

### The property that makes it worth building

Because `Endpoints()` hands out `127.0.0.1:20040` and the client listens on `20040`, the output
of `sbx env` on the server is **literally correct on the laptop**:

```sh
eval "$(sbx env my-branch)"   # DATABASE_PORT=20040
psql -h 127.0.0.1 -p 20040    # connects, through the tunnel, and wakes the sandbox
```

No rewriting, no client that knows sbx exists, no sandbox-aware tooling. The socket is still the
only signal — which is the whole product claim, preserved over a network.

## The wire protocol

Three routes, versioned under `/v1`.

### `GET /healthz`

Unauthenticated, returns `200 ok`. Platforms need a probe that does not carry a credential —
this mirrors the reasoning already written down for `console/`'s health endpoints.

### `GET /v1/fleet`

Authenticated. Returns the same data `sbx list` shows, as JSON:

```json
{
  "provider": "docker",
  "sandboxes": [
    {"sandbox": "my-branch", "service": "postgres", "awake": false,
     "ports": [20040], "ref": "sbx-my-branch-postgres"}
  ]
}
```

The client needs this to know which local listeners to open. It is read-only and deliberately
carries nothing that is not already on a `sbx list` screen.

### `GET /v1/connect?port=<n>` with `Upgrade: websocket`

Authenticated. The server:

1. Rejects the request unless `port` is one it is actually fronting. **An arbitrary port would
   make this an open proxy into the deployment's network** — the check is the difference between
   a tunnel and an SSRF hole.
2. Upgrades to WebSocket (`101`).
3. Dials `127.0.0.1:<port>` — which is the daemon's own wake proxy, so connecting through the
   tunnel wakes a sleeping sandbox exactly as a local connection does.
4. Splices bytes both ways until either side closes.

Binary frames, no framing of our own. A WebSocket ping every 30s keeps L7 proxies from reaping
an idle connection — the same keepalive reasoning Teleport documents.

### Why not raw HTTP CONNECT

`CONNECT` is what a forward proxy speaks and most L7 platforms will not pass it through.
WebSocket is the only upgrade that every one of them handles, because they all support it for
ordinary applications.

## Authentication

A single bearer token, supplied at deploy time:

```
Authorization: Bearer <token>
```

- Read from `SBX_CONNECT_TOKEN`. Platforms inject env vars; that is the path of least friction
  for ZopDay, Cloud Run and a systemd unit alike.
- **`--connect-addr` without a token is a startup error, not a warning.** The one failure mode
  worth engineering against is a deployment that is reachable and open, and the way that
  happens is a flag that "worked" in testing.
- Compared with `subtle.ConstantTimeCompare`.
- No sessions, no users, no rotation endpoint. The token proves you own this deployment, which
  is all it has to prove given one sbx is one workspace.

**What this is not.** It is not org-level authorization. ZopDay's gateway already validates JWTs
and checks org access, and its MCP server owns no authz for exactly this reason. A gateway in
front adds that without sbx knowing about it.

**Stated plainly:** anyone holding the token has TCP access to every service in the deployment,
and those services have their own credentials but sbx does not check them. That is the same
posture as an SSH key on a dev box. It is a deliberate choice, and it is the reason the control
plane (`create`, `exec`) is explicitly out of scope.

## Server-side changes

| | |
|---|---|
| `internal/daemon/serve.go` | `--connect-addr` (default off; `:$PORT` when `PORT` is set, which is what PaaS platforms provide). Refuses to start if set without `SBX_CONNECT_TOKEN`. |
| `internal/connect/` (new) | the HTTP handlers, the WebSocket upgrade, the splice loop, and the port allow-list check. |
| `internal/connect/ws.go` (new) | a minimal RFC 6455 server: the handshake, binary frames, close and ping. Stdlib only — `crypto/sha1` and `encoding/base64` for the accept key, and `net/http`'s `Hijacker` for the connection. |

**On writing a WebSocket implementation.** The root module has zero non-stdlib dependencies and
that is a CI-gated product claim, so `gorilla/websocket` is not available to us. What we need is
a small subset — server-side handshake, binary frames, close, ping — and it is well specified.
The alternative is breaking the claim the project is partly sold on. If the subset turns out to
be larger than it looks, that is the moment to stop and reconsider, not to quietly add a
dependency.

## Client-side: `sbx connect`

```sh
sbx connect https://sbx.my-org.zop.dev            # everything the fleet has
sbx connect https://…  --sandbox my-branch        # only that sandbox's ports
sbx connect https://…  --token-env MY_VAR
```

- Fetches `/v1/fleet`, opens a local listener per port, prints the map it created.
- `--port-offset N` shifts every local port by N, for a laptop already running its own daemon.
  It prints a warning that the remote's `sbx env` values no longer apply, because that is the
  one thing the offset costs and it should not be discovered later.
- Runs in the foreground and closes its listeners on exit. It is a tunnel, not a daemon.
- **Refuses to bind a port already in use** and says which sandbox wanted it, rather than
  silently forwarding one service to another's listener.
- `--sandbox` exists because a laptop that already runs a local sbx will collide on the whole
  20000–21199 range; naming one sandbox is the escape hatch.

## Failure modes

| what happens | what the user sees |
|---|---|
| no token, `--connect-addr` set | startup error naming `SBX_CONNECT_TOKEN` |
| wrong token | `401`, and the client says the token was rejected rather than "connection failed" |
| `port=` not fronted by this daemon | `403`, logged server-side; never dialled |
| local port already bound | client refuses to start, names the port and the sandbox that wanted it, and suggests `--port-offset` |
| two clients connected at once | both work; the server keeps no per-client state, so this needs no code |
| server unreachable | client fails at `/v1/fleet` with the URL it tried, before opening any listener |
| L7 proxy reaps idle connection | ping every 30s; a dropped connection closes the local socket so the client's client sees a normal EOF |
| sandbox asleep | connecting wakes it — the tunnel dials the daemon's wake proxy, so this needs no code |

## Testing

The point of the design is that it is testable without a cloud account.

- **Unit:** the RFC 6455 handshake against the vectors in the RFC; frame encode/decode including
  a fragmented message and a close; `subtle` comparison of tokens.
- **Port allow-list:** a table of ports the daemon fronts and does not, asserting `403` and that
  nothing was dialled. This is the security-relevant test and it gets the most cases.
- **End-to-end, in-process:** an `httptest.Server` running the connect handler in front of a
  throwaway TCP echo server; `sbx connect`'s client half against it; assert bytes survive both
  ways, that a large payload is not truncated, and that closing either end closes the other.
- **Behind a real L7 proxy:** the claim this design rests on is "a layer 7 proxy will not break
  it". A test that only ever talks to `httptest` never checks that claim. One test puts a
  reverse proxy in front and runs the same assertions.
- **Live, once:** against a real deployment, wake a sleeping Postgres through the tunnel with
  `psql`. Recorded in the spec's follow-up notes, not automated.

## Deploying on ZopDay

ZopDay ships an image to a Kubernetes cluster or a VM. sbx already builds a container image
(`deploy/Dockerfile`) that carries `kubectl` for the in-cluster provider.

**On a VM with docker** — the full capability set. The container needs the docker socket mounted
so it manages sibling containers, and `--connect-addr :$PORT`.

**On Kubernetes** — the activator path and the reduced capability set from the matrix in
`docs/COMPARISON.md`: no snapshot/fork, gc, `build:`, prewarm, egress deny, `sbx url`, `gpus:`
or usage metrics. Limits and wake/sleep work.

Either way the deployment exposes exactly one HTTP port, which is the shape every platform
supports.

## Open questions

1. **Does the fleet endpoint need to stream?** Today the client fetches once at startup, so a
   sandbox created afterwards needs a restart to appear. Polling or an SSE stream would fix it;
   neither is needed for the first version.
2. **Should `sbx connect` re-open listeners when the server's fleet changes?** Same question,
   same answer for now.
3. **Compression.** Not in v1. Database protocols are mostly small messages and a `permessage-deflate`
   negotiation is more RFC surface for little gain.
