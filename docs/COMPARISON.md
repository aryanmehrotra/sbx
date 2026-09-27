# sbx compared with other sandboxes

How sbx compares with hosted sandboxes (E2B, Daytona, Modal, Vercel, Cloudflare, Fly), OpenSandbox,
and local tools like docker compose and Testcontainers. sbx numbers come from
[BENCHMARKS.md](BENCHMARKS.md). Vendor facts are quoted from the vendor's page, linked, and were
checked on 2026-09-27 unless a line gives another date. A `–` means nobody checked.

## Which one should I use?

| You want | Use |
|---|---|
| A database or stack per branch, PR or agent, on your own machine | sbx |
| Unmodified clients (`psql`, pools, Playwright) to wake a sleeping service | sbx |
| Self-hosted OpenSandbox or E2B-style agent sandboxes from one binary | sbx |
| One spec on a laptop, a cluster and a microVM | sbx |
| Containers started and stopped from inside test code | Testcontainers |
| A few always-on services for one project | docker compose |
| Nothing to run yourself | E2B, Daytona, Modal, Vercel or Cloudflare |
| A microVM on a Mac or Windows that has been run end to end | microsandbox |
| A coding agent itself running in a microVM | Docker Sandboxes |
| Only Postgres branches | Neon |

sbx's helper-VM path is run by hand on a Mac (v0.11) and not yet run end to end on Windows. See
[platform status](ARCHITECTURE.md#platform-status).

## Feature matrix

● yes · ◐ partial · ○ no · – not documented or not checked. "microVM" means
`--provider firecracker`.

| | sbx | E2B | Daytona | Modal | OpenSandbox | Vercel | Cloudflare | Fly |
|---|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|
| Wakes on an unmodified client connection | ● any TCP | ○ SDK | ○ API | ○ SDK | ○ API | ○ SDK | ◐ HTTP² | ◐ HTTP¹ |
| Runs on your laptop | ● | ◐ Linux + KVM, evaluation only⁶ | ○ | ○ | ● | ○ | ○ | ○ |
| Self-hosted, no account | ● MIT | ◐ infra³ | ◐ frozen⁴ | ○ | ● Apache-2.0 | ○ | ○ | ○ |
| Hosted, nothing to run | ○ | ● | ● | ● | ○ | ● | ● | ● |
| Several services in one spec | ● | ○ | ○ | ○ | – | ○ | ○ | ◐ |
| Idle cost | $0, 0 B RAM | $0 while paused [src][e2b-bill] | storage | snapshot storage | $0, your infra | snapshot storage | $0, files deleted [src][cf-price] | storage |
| Sleep keeps RAM and processes | ● microVM · ◐ docker⁵ | ● | ◐ VM only | ◐ alpha | ● | ○ files only | ○ | ● suspend |
| Resume time, vendor's own figure | [BENCHMARKS](BENCHMARKS.md) | ~1 s [src][e2b-p] | – | – | – | – | none, sleep deletes files; cold start 1-3 s [src][cf-arch] | a few hundred ms [src][fly-sr] |
| Isolation | container · jailed microVM | Firecracker | container; VM optional | gVisor | container to Firecracker | Firecracker | container in a VM | Firecracker |
| SDKs | OpenSandbox's 5 · CLI · MCP | Python, JS | 5 languages | Python, JS, Go [src][modal-sdk] | 5 languages | TS, Python | TS | REST API |
| Speaks the OpenSandbox API | ● | ○ | ○ | ○ | ● reference | ○ | ○ | – |
| Egress policy, changed live | ◐ docker, microVM | ● | ◐ by tier | ● CIDR; domains beta | ● | ● | ● HTTP | – |
| Credentials kept out of the sandbox | ○ [planned](ROADMAP.md#next) | – | – | – | ● vault | ● brokering | ◐ header injection | – |
| GPU | ◐ docker only | – | – | ● | – | – | ○ | ● |

1. Through [Fly Proxy][fly-proxy], which starts a stopped Machine on a request. A raw TCP wake is not documented (checked 2026-09-27).
2. A request through your Worker. On sleep, files are deleted and processes end ([lifecycle][cf-sb]).
3. Its infrastructure code is Apache-2.0 ([e2b-dev/infra][e2b-infra]).
4. AGPL. The repo says the open-source core "will receive no further updates" ([repo][dt-gh]).
5. Opt-in [`on_idle: "freeze"`](SPEC.md#on_idle-freeze-keeps-memory-instead), which holds RAM while idle.
6. E2B Embed runs the stack on one Linux host with KVM; E2B calls it "an evaluation package, not a production deployment pattern" ([infra][e2b-infra], checked 2026-09-27).

Other vendor details:

- E2B's default timeout kills the sandbox after 5 min; auto-pause is opt-in ([fork][e2b-fork]).
- Daytona advertises creation "sub 90ms" ([pricing][dt-price]); memory is kept only for VM sandboxes ([sandboxes][dt-sb]).
- Modal memory snapshots are alpha ([snapshots][modal-snap]); isolation is gVisor ([security][modal-sec]).
- OpenSandbox has a credential vault and "~80ms" Firecracker pools ([repo][osb], [performance][osb-perf]). Its isolated sessions and vault are [not built in sbx yet](ROADMAP.md#not-built-yet-in-the-opensandbox-api).
- Vercel saves the filesystem, not memory; any SDK call resumes a stopped sandbox ([persistence][vc-p], [firewall][vc-fw]).
- Neon wakes on a Postgres connection in "a few hundred milliseconds" ([latency][neon-lat]).

For create time, the one comparable number is ComputeSDK's Burst TTI: 100 concurrent creates,
each timed to a successful `node -v` ([methodology][csdk-m], [results][csdk-r]). sbx is not on it.
sbx's runs of the same shape are in [BENCHMARKS.md](BENCHMARKS.md).

## Honest ranking

The project's own judgement, not a measurement. sbx among the eight tools above plus microsandbox;
1 is best.

| Dimension | sbx rank | Why |
|---|:---:|---|
| Idle cost | 1 (tied with OpenSandbox on $) | 0 B RAM asleep on your hardware. E2B and Cloudflare also charge nothing while idle; most others bill storage |
| Multi-service stacks | 1 (OpenSandbox not checked) | One `sandbox.json` with Postgres, Redis and a browser |
| Self-hosting | 2 | One binary, laptop to cluster. OpenSandbox has more runtimes and users |
| Docs | 4 | Measured numbers with scripts. No docs site or search |
| SDK breadth | 4 | OpenSandbox's 5 SDKs work, but not its isolated sessions or vault |
| Onboarding | 5 | Needs a Docker runtime. Vercel and Docker are one command |
| Isolation | 6 (tied) | Default is a container; the jailed microVM is opt-in |
| Community | 10 (last) | 2 GitHub stars on 2026-09-27; OpenSandbox 15.5k, E2B 14k, microsandbox 8.4k |
| Hosted option | 10 (last, tied) | None, by design |

Wake latency and create time are not ranked: no like-for-like measurement exists.

## Local tools

- [docker compose][compose] runs services until you stop them. A fixed host port like `5432:5432` clashes between branches ([ports][compose-ports]).
- [Testcontainers][tc] starts containers from inside your tests, each on a random free port ([networking][tc-net]). It has a large module catalogue; sbx has 5 templates.
- sbx fits when many copies of a stack share one machine, sleep when unused, and are reached by tools that know nothing about sbx. `sbx with -- <test command>` covers the test-run case in any language.

Moving a compose file: [SPEC.md](SPEC.md#coming-from-docker-compose).

Self-hosted projects that also wake idle services:

| | wakes on any TCP | no runtime to replace | laptop-first | restores RAM |
|---|---|---|---|---|
| sbx | ● | ● | ● | ◐ microVM |
| [zeropod][zeropod] | ● | ○ shim, CRIU, eBPF | ○ | ● |
| [Lazytainer][lazy] | ● | ◐ owns networking | ● | ○ |
| [Sablier][sablier] | ○ HTTP only (v1.17.0) | ● | ● | ○ |
| [KubeElasti][elasti] | ○ HTTP/gRPC | ● | ○ | ○ |

zeropod needs a Kubernetes cluster. An unofficial [sablier-proxy][sablier-proxy] adds TCP to
Sablier (checked 2026-08-30). Head-to-head numbers against zeropod and Lazytainer are in
[BENCHMARKS.md](BENCHMARKS.md).

## Monthly cost for one developer

One environment of about 2 vCPU and 4 GB, 8 h a day, 20 days (160 h). Computed from each
vendor's pricing page on 2026-08-30 (Modal rechecked on 2026-09-27), not quoted.

| | per month | what drives it |
|---|---:|---|
| sbx, zeropod, Sablier, Lazytainer | $0 | your own machine |
| Northflank | ~$12 | $0.01667/vCPU-h + $0.00833/GB-h |
| GitHub Codespaces (Pro) | ~$13 | 180 core-h free, then $0.09/core-h |
| Daytona | ~$27 | $200 credit covers roughly the first 1,200 h |
| Modal | ~$8 | $38 of use (2 vCPU is one "physical core"), less the $30/month Starter credit |
| Vercel Sandbox | ~$34 | plus $0.08/GB-month for snapshots while stopped |
| Cloudflare Containers | ~$39 | $5 base; CPU billed on active use |
| E2B | ~$177 | usage ~$27; Pro at $150/month needed for sessions over 1 h |

Modal charges "$0.00003942 / core / sec" for CPU, where a core is a "Physical core (2 vCPU
equivalent)", and "$0.00000667 / GiB / sec" for memory ([pricing](https://modal.com/pricing),
checked 2026-09-27). Vercel Sandbox charges Active CPU at "$0.128/hour" and memory at
"$0.0212/GB-hour" ([pricing](https://vercel.com/docs/sandbox/pricing), checked 2026-09-27).

sbx is free because it runs on your hardware. For a team that owns none, that is not a saving.

## Why not use Firecracker directly?

Firecracker boots a VM from a kernel and a disk image. Everything that turns that into a sandbox
you can hand to an agent is left to you. In sbx it is about 20,000 lines of Go at v0.14.0, not
counting the OpenSandbox API (non-test `.go` files in `internal/fc*`, `internal/execd*` and
`internal/provider/firecracker*.go`):

- Images: Firecracker boots an ext4 disk, not a Docker image. sbx pulls the image, flattens its
  layers into a cached disk, and gives each VM a read-only copy with an empty layer of its own.
- A pinned guest kernel for each CPU architecture.
- A way in: Firecracker has no exec or file copy. sbx runs an agent inside each VM for commands,
  files and logs.
- Networking: a tap device and bridge per VM, an outbound allow-list, and host rules that fail
  closed.
- Confinement: Firecracker's jailer with its own user and network namespace per VM, a file-size
  limit and no privileges ([SECURITY.md](../SECURITY.md)).
- Sleep and wake: snapshot a VM's memory to disk, stop it, restore it on demand, and hold the
  client's connection open while that happens.
- Warm pools of VMs parked asleep or paused, so a create is answered in 144 ms (median) from a
  frozen microVM pool ([BENCHMARKS](BENCHMARKS.md#sbx-by-itself)).
- An interface agents already use: the OpenSandbox API and SDKs, and MCP tools.
- Macs and Windows, where Firecracker cannot run, through a helper Linux VM
  ([status](ARCHITECTURE.md#platform-status)).

Use Firecracker directly, or Kata Containers or firecracker-containerd, if you are building your own
sandbox platform, need control of VM configuration or snapshots, or serve many untrusted tenants
on shared hosts: the sbx microVM daemon runs as root on one host and is not a multi-tenant control plane.

## Choose something else if

- You need the fastest hosted create: isorun (43.6 ms median) and createos (124.9 ms) lead ComputeSDK's 100-at-once run of 2026-09-25 ([results][csdk-r]); see [BENCHMARKS](BENCHMARKS.md#hosted-sandboxes-published-figures).
- You need secrets kept out of the sandbox now: OpenSandbox's vault or Vercel's brokering.
- You want a browser IDE on a managed machine: Codespaces, Coder or Ona. sbx has no browser IDE.
- Your apps are HTTP and already behind Traefik or Caddy: Sablier.
- You want memory restore on a Kubernetes cluster you already run: zeropod.
- You need GPUs at scale: Modal or Fly.
- You want to run the agent itself in a microVM: [Docker Sandboxes][docker-sb]. Its CLI is also named `sbx`; the first on `PATH` wins.
- You need self-hosted microVMs on Linux, macOS and Windows with no daemon: [microsandbox][msb], "beta software".
- You need a large community and paid support. Every tool here is bigger.

[cf-arch]: https://developers.cloudflare.com/containers/platform-details/architecture/
[cf-sb]: https://developers.cloudflare.com/sandbox/concepts/sandboxes/
[compose]: https://docs.docker.com/compose/
[compose-ports]: https://docs.docker.com/reference/compose-file/services/#ports
[csdk-m]: https://github.com/computesdk/benchmarks/blob/master/METHODOLOGY.md
[csdk-r]: https://github.com/computesdk/benchmarks/blob/master/results/burst_tti/latest.json
[docker-sb]: https://www.docker.com/products/docker-sandboxes/
[dt-gh]: https://github.com/daytonaio/daytona
[dt-price]: https://www.daytona.io/pricing
[dt-sb]: https://www.daytona.io/docs/en/sandboxes
[e2b-bill]: https://docs.e2b.dev/billing
[cf-price]: https://developers.cloudflare.com/containers/pricing/
[e2b-fork]: https://docs.e2b.dev/sandbox/fork
[e2b-infra]: https://github.com/e2b-dev/infra
[e2b-p]: https://docs.e2b.dev/sandbox/persistence
[elasti]: https://github.com/truefoundry/KubeElasti
[fly-proxy]: https://fly.io/docs/reference/fly-proxy-autostop-autostart/
[fly-sr]: https://fly.io/docs/reference/suspend-resume/
[lazy]: https://github.com/vmorganp/Lazytainer
[modal-sdk]: https://modal.com/docs/guide/sdk-javascript-go
[modal-sec]: https://modal.com/docs/guide/security
[modal-snap]: https://modal.com/docs/guide/sandbox-snapshots
[msb]: https://github.com/superradcompany/microsandbox
[neon-lat]: https://neon.com/docs/connect/connection-latency
[osb]: https://github.com/opensandbox-group/OpenSandbox
[osb-perf]: https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/architecture/fast-sandbox/performance.md
[sablier]: https://github.com/sablierapp/sablier
[sablier-proxy]: https://github.com/vbrandl/sablier-proxy
[tc]: https://testcontainers.com/
[tc-net]: https://java.testcontainers.org/features/networking/
[vc-fw]: https://vercel.com/docs/sandbox/concepts/firewall
[vc-p]: https://vercel.com/docs/sandbox/concepts/persistent-sandboxes
[zeropod]: https://github.com/laravel/zeropod
