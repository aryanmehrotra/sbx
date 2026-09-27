# FAQ

Short answers to what people ask before they adopt sbx, each with a link to the page that holds
the detail. Figures come from [BENCHMARKS.md](BENCHMARKS.md), and vendor facts from
[COMPARISON.md](COMPARISON.md).

The ten asked most often:

- [What problem does sbx solve, and who is it for?](#what-problem-does-sbx-solve-and-who-is-it-for)
- [Is a container safe enough for untrusted agent code, or do I need a microVM?](#is-a-container-safe-enough-for-untrusted-agent-code-or-do-i-need-a-microvm)
- [How fast is a create, and how fast is a wake?](#how-fast-is-a-create-and-how-fast-is-a-wake)
- [Does state persist across sleep?](#does-state-persist-across-sleep)
- [Can I self-host it, and how hard is it?](#can-i-self-host-it-and-how-hard-is-it)
- [Does it run on macOS or Windows?](#does-it-run-on-macos-or-windows)
- [How do I give an agent API keys without it leaking them?](#how-do-i-give-an-agent-api-keys-without-it-leaking-them)
- [Can I restrict outbound network to a list of domains?](#can-i-restrict-outbound-network-to-a-list-of-domains)
- [Does it support GPUs?](#does-it-support-gpus)
- [How does sbx compare with E2B, Daytona, Modal and the rest?](#how-does-sbx-compare-with-e2b-daytona-modal-and-the-rest)

## Getting started

### What problem does sbx solve, and who is it for?

sbx gives every agent, test run or branch its own sandbox on your machine or cluster: a Firecracker
microVM (a small VM with its own Linux kernel), or a container. Idle sandboxes sleep at 0 B of RAM
and wake when anything connects. It is for people who run coding agents, CI or per-branch databases
on hardware they control ([README](../README.md)).

### Is the sandbox for the agent itself, or for what it runs?

For what it runs: the commands, databases and browsers an agent needs. The agent calls sbx
through the OpenSandbox SDKs (an open API standard for agent sandboxes), MCP tools (the standard
way AI assistants call outside tools) or the CLI ([GUIDES](GUIDES.md#ai-agents)). To run the
coding agent itself inside a microVM, Docker Sandboxes is built for that
([COMPARISON](COMPARISON.md#choose-something-else-if)).

### Do Claude Code, Codex or other agents come preinstalled?

No. sbx ships no agent CLIs. A sandbox runs whatever image you name, and the agent on your host
drives it with `claude mcp add sbx -- sbx mcp` or `codex mcp add sbx -- sbx mcp` while
`sbx serve --osb-addr 127.0.0.1:8080` runs ([GUIDES](GUIDES.md#mcp)).

### Does it need an account or a login?

No. There is no account, no sign-in and nothing hosted. The credentials are the OpenSandbox API
key, which `sbx serve --osb-addr` generates into `~/.sbx/osb/key` on your machine, and, if you turn
on `sbx connect`, its token ([SECURITY.md](../SECURITY.md#access-and-exposure)).

### How is this different from a Dockerfile or devcontainer I write myself?

sbx runs your image, and adds what a plain container lacks. It sleeps a sandbox to 0 B when idle
and wakes it on the first connection. Each copy gets its own ports, and it snapshots and forks
data per branch or agent.

A `build:` field takes your Dockerfile, and `sbx init --from-devcontainer` (preview feature) imports
a devcontainer ([SPEC](SPEC.md#image-or-build)).

### Can I use my own image, and install extra packages?

Yes. Set `image` to any image, or `build` to a Dockerfile context. The tag is a hash of the
context, so an unchanged context is a cache hit.

`build` works on docker only. Kubernetes and firecracker refuse it, so push the image and name it
([SPEC](SPEC.md#provider-support)).

### One sandbox per agent session, per tool call, or per task?

Per task or branch, named after it. Sandboxes share nothing, so a migration in one affects no other.
For a one-off test run, `sbx with <name> -- <cmd>` creates, runs and removes it even when the
command fails ([GUIDES](GUIDES.md#ai-agents)).

### Can I create and drive sandboxes from code?

Yes, three ways. `sbx serve --osb-addr` serves the OpenSandbox API, with SDKs in 5 languages.
`sbx mcp` exposes 19 tools to an MCP client. Every CLI command that reports state has `--json`
([GUIDES](GUIDES.md#ai-agents)).

### Is my project copied or mounted into the sandbox?

Neither by default. On docker, `"mounts": {".": "/work"}` bind-mounts your directory read-write,
so changes appear on the host at once ([GUIDES](GUIDES.md#work-inside-a-sandbox)). Otherwise copy
with `sbx cp`, or the API's file calls. sbx has no git or pull-request integration.

## Security and isolation

### Is a container safe enough for untrusted agent code, or do I need a microVM?

Use a microVM for code you did not write. A container shares the host's kernel, so a kernel bug
escapes to the host. With `--provider firecracker`, an escape lands as an unprivileged, jailed
user ([SECURITY.md](../SECURITY.md#the-boundary-per-backend)).

The default is a container, and the microVM is opt-in.

### What is the threat model, and can a sandbox be escaped?

sbx holds the boundary between a sandbox and the host, and between sandboxes. It is not a
multi-tenant platform. The microVM daemon runs as root, and nothing caps the total disk all
microVMs use. [SECURITY.md](../SECURITY.md) lists each boundary, what
counts as a vulnerability and how to report one.

### How does it compare with gVisor, Kata or seccomp tools?

sbx uses gVisor (a user-space kernel for containers) and Kata (a lightweight VM per container) as
options: `--isolation gvisor` or `--isolation kata` on docker or kubernetes. A missing runtime is
refused. Docker's default seccomp profile applies to containers ([SPEC](SPEC.md#cap_add)).

gVisor on docker is verified in CI. Kata is not in the platform status table, and the `kata-fc`
RuntimeClass on Kubernetes is not yet run end to end ([platform
status](ARCHITECTURE.md#platform-status)).

### What is a microVM, and what does it cost against a container?

A small VM with its own Linux kernel, here run by Firecracker. Its `memory` is fixed at boot
(default `256m`) and held in RAM while awake. Asleep, it holds no RAM and keeps a memory file on
disk about that size ([SELF-HOSTING](SELF-HOSTING.md)). Some fields are refused inside one
([SPEC](SPEC.md#on---provider-firecracker)).

### Can the agent reach my home directory or other projects?

Only what you give it. A container sees host paths named in `mounts` or `files`, and the
OpenSandbox API binds a host directory only under `--osb-host-paths`. A microVM refuses host
mounts and `files` ([SPEC](SPEC.md#provider-support), [SECURITY.md](../SECURITY.md#what-would-be-a-real-vulnerability)).

### Does the agent run as root inside?

In a container, it runs as the image's `USER`. Files it writes through `mounts` carry that user's
id on the host. A microVM needs an image that runs as root, and a non-root image is refused
([SPEC](SPEC.md#on---provider-firecracker)).

### Is it multi-tenant safe?

No. There is no authentication, per-user isolation or quota: anyone who can reach the daemon can
use any sandbox, and this will not change ([SECURITY.md](../SECURITY.md#access-and-exposure)).
For several users, put a gateway in front, and use microVMs for their code.

### Can I get SOC 2 or ISO 27001 evidence?

Not from sbx itself: nothing is hosted, so there is nothing to certify. It keeps a local audit
trail: `sbx history --json` lists the commands that changed something, and every wake and sleep
([CLI](CLI.md#finding-out)).

## Performance

### How fast is a create, and how fast is a wake?

On one Linux machine at v0.14.0, a sleeping Postgres answered `psql` in 348 ms (median). A frozen
service resumed in 34 ms.

An OpenSandbox create to first command took 12.8 ms from a warm pool and 342 ms without. On microVMs
in CI's nested KVM, it took 144 ms from a frozen pool and 2,637 ms with none. Every figure and its
method: [BENCHMARKS](BENCHMARKS.md#sbx-by-itself).

### What is the overhead compared with plain Docker?

Once awake, the proxy adds 0.17 ms to a new connection and 9.6 µs per query. Throughput through
it was 1.39 GB/s, 55% of a direct connection ([BENCHMARKS](BENCHMARKS.md#sbx-by-itself)).
MicroVM overhead against a container has not been measured.

### Is memory returned to the host?

Yes, when a sandbox sleeps. A sleeping container is stopped and holds 0 B. A sleeping microVM
writes its memory to disk. An awake microVM keeps its fixed `memory`, and sbx does not balloon it
([SELF-HOSTING](SELF-HOSTING.md), `internal/provider/firecracker.go`).

### How many sandboxes fit on one machine, and across machines?

No maximum has been measured. Twenty idle Postgres databases held 17.6 MB (the daemon alone), and
100 API creates at once from a warm pool had a median of 309-460 ms across two runs
([BENCHMARKS](BENCHMARKS.md)). Awake sandboxes need their `cpu` and `memory`. Asleep, they cost
disk ([SELF-HOSTING](SELF-HOSTING.md)).

One daemon serves one host, with no scheduler across hosts. For a cluster, the kubernetes provider
makes each service a Deployment ([GUIDES](GUIDES.md#kubernetes)).

## Features

### Does it support GPUs?

On docker only. The `gpus` field is passed to `docker run --gpus` (`"all"`, `"1"`,
`"device=0"`). Kubernetes ignores it, and firecracker refuses it because Firecracker has no device
passthrough ([SPEC](SPEC.md#provider-support)). For GPUs at scale, see Modal or Fly
([COMPARISON](COMPARISON.md#choose-something-else-if)).

### Does state persist across sleep?

Yes, differently per backend. A container keeps its `volume` and restarts its processes on wake.
With `"on_idle": "freeze"` it is paused instead and keeps memory, which then stays in RAM
([SPEC](SPEC.md#on_idle-freeze-keeps-memory-instead)). A microVM saves memory and disk as a snapshot,
so processes come back where they were ([ARCHITECTURE](ARCHITECTURE.md#microvm-firecracker)).

### Can I snapshot, fork or clone a sandbox, even mid-write?

Yes, its data. `sbx snapshot` copies every service's volume, and `sbx fork` makes independent
copies with their own ports. Forks start cold against warm data.

A microVM snapshot keeps memory but restores only as the same sandbox. Forking a VM's memory under a
new name is on the [ROADMAP](ROADMAP.md#later) ([GUIDES](GUIDES.md#seed-once-fork-many)).

`sbx snapshot` does not stop the service, so a copy taken mid-write is crash-consistent. Stop writes
first if it must be exact
([TROUBLESHOOTING](TROUBLESHOOTING.md#a-fork-is-missing-the-write-i-just-made)).

### How long can a sandbox run, and can I keep it alive?

As long as you want. `"idle": "never"` keeps a service awake until you sleep or remove it
([SPEC](SPEC.md#idle-keeps-a-sandbox-awake-while-it-works)). An API sandbox created without a
`timeout` never expires. A `timeout` must be at least 60 s and can be extended with
`sandbox_renew` (`internal/osb/provision.go`).

### What counts as idle, and do background jobs keep running?

Idle means no bytes crossed the service's ports for `--idle` (default `5m`). Work inside, such as
a build or a cron job, sends none, so the sandbox can sleep mid-task. Give it a longer `idle`,
`"never"`, or `egress_allow`, whose calls out count as activity
([TROUBLESHOOTING](TROUBLESHOOTING.md#a-sandbox-that-works-inside-itself-sleeps-mid-task)).

### Can a sandbox listen on ports and expose a preview URL?

Yes. Each port in `ports` gets a public port on `127.0.0.1`, and `sbx env` prints it. The proxy
splices bytes, so any TCP protocol works, WebSockets included.

`sbx url` opens an HTTPS link through cloudflared, ngrok or ssh that wakes the service when opened
([GUIDES](GUIDES.md#share-a-preview-link)).

### How do I copy files in and out?

`sbx cp <sandbox> <service> <src> <dst>`, with a `:` before the path inside the sandbox. API
sandboxes also have file calls, which MCP exposes as `file_read`, `file_write` and six more
([CLI](CLI.md#while-you-work)). There is no S3 mount. The API attaches `pvc` volumes, and `host`
volumes on containers only.

### Can I run a browser or Playwright inside?

Yes. `--template browser` runs a headless Chrome that Playwright, Puppeteer and chromedp drive over
CDP (the Chrome DevTools Protocol) at `$CDP_HOST:$CDP_PORT`
([examples/browser](../examples/browser/README.md)). A GUI or VNC desktop has no template.

### Can I run Docker or Kubernetes inside a sandbox?

Not in a container sandbox. Docker-in-docker and k3s need `privileged`, which sbx does not offer
([ROADMAP](ROADMAP.md#not-doing)). Running Docker inside a microVM is not documented or tested.

### Can I set CPU and memory limits?

Yes. `cpu` and `memory` per service in `sandbox.json`, or `resourceLimits` in an API create. On
firecracker, `cpu` rounds up to whole vCPUs and `memory` is fixed at create
([SPEC](SPEC.md#cpu-and-memory)).

Open-file limits have no field. A microVM's disk is capped by `SBX_FC_DISK_SIZE` (default `10g`).

### Can I SSH into a sandbox?

Yes, as a preview feature. With an image that runs an ssh server, `SBX_FEATURES=ssh sbx ssh
<sandbox>` prints the ssh and `code --remote` lines, and connecting wakes it. For a shell without
ssh, use `sbx exec -t <sandbox> <service> sh` ([GUIDES](GUIDES.md#work-inside-a-sandbox)).

### Where does a microVM's kernel come from?

sbx downloads a pinned Linux 6.18.48 kernel per CPU architecture, checked by sha256, and boots
every microVM with it. The image's own kernel is not used
([ARCHITECTURE](ARCHITECTURE.md#microvm-firecracker)). Loading kernel modules in the guest is not
documented.

## Networking and secrets

### How do I give an agent API keys without it leaking them?

Today you cannot fully: a key the agent uses sits in its environment, and it can leak it. Keeping
credentials out of the sandbox, added by the egress filter on the way out, is planned
([ROADMAP](ROADMAP.md#secrets-kept-out-of-the-sandbox)).

Until then, `egress_allow` limits where a leaked key can be sent. `${VAR}` keeps it out of a
committed spec only.

### Can I restrict outbound network to a list of domains?

Yes, on docker and on microVMs. `"egress_allow": ["api.openai.com", "pypi.org"]` allows those
hosts and their subdomains. `"egress": "deny"` allows nothing, and `egress_policy` takes rules
with wildcards and CIDRs.

`sbx egress` changes a running sandbox's policy without a restart. Kubernetes refuses all of them
([SPEC](SPEC.md#egress-the-network-a-service-may-reach)).

### Does domain filtering hold when DNS changes or a program ignores the proxy?

A hostname is judged by name, then by each address it resolves to. A client that ignores
`HTTP_PROXY` has no route out, because the filtered network has none.

The filter carries HTTP and HTTPS only, so raw TCP does not pass, though DNS still resolves. It does
not decrypt TLS ([ARCHITECTURE](ARCHITECTURE.md#egress-filter)).

### How do I stop a sandbox reaching my LAN or cloud metadata?

Cloud metadata at `169.254.0.0/16` and loopback are refused unless a rule names them. A microVM's
filter also refuses private ranges, the host and other sandboxes. Only `--vm-egress-allow` opens
private ranges ([SECURITY.md](../SECURITY.md#microvms---provider-firecracker)). On docker, use a
default-deny policy or `sbx egress <sandbox> --deny 10.0.0.0/8`.

### Can a sandbox have a fixed outbound IP or join Tailscale?

No built-in option. Outbound traffic leaves through the host's network, so it uses the host's
address. There is no Tailscale or WireGuard integration.

## Self-hosting and operations

### Can I self-host it, and how hard is it?

Yes, it only runs self-hosted. Install one binary with `brew`, the install script or
`go install`, and run `sbx serve` under systemd. It needs Docker or a Kubernetes cluster, and
Linux with `/dev/kvm` for microVMs.

An air-gapped install is not documented. Steps: [SELF-HOSTING](SELF-HOSTING.md).

### What does it need from the host?

A Docker engine for containers. For microVMs: Linux with `/dev/kvm`, a root daemon (or
`CAP_NET_ADMIN`), `iptables`, iproute2, `mkfs.ext4`, and network namespaces
([SELF-HOSTING](SELF-HOSTING.md)). On a cloud VM, KVM needs nested virtualisation, and CI runs
microVMs that way. `sbx doctor` says what your machine can do.

### Can I run sbx inside a container or with docker compose?

For containers, the daemon needs a Docker socket and runs on the host. For microVMs, run it on the
host, not in a container that forbids network namespaces ([SELF-HOSTING](SELF-HOSTING.md)). On
Kubernetes, the in-cluster daemon runs as a pod from `deploy/activator.yaml`.

### How do upgrades work? Do they break existing sandboxes?

Replace the binary, restart the daemon and run `sbx doctor`. A restart finds every sandbox again.
Each release note's "Before you upgrade" section lists breaking changes. For example, microVM
snapshots taken before v0.13 cold-boot once, keeping the disk ([SELF-HOSTING](SELF-HOSTING.md)).

### What happens when the host reboots or the daemon crashes?

Sandboxes survive a daemon crash. `scripts/recovery.sh` kills the daemon with SIGKILL on docker
and checks that running sandboxes keep running, a restart re-adopts them, and data is kept
([CONTRIBUTING](../CONTRIBUTING.md#test-tiers)).

A microVM that died awake cold-boots against its disk on the next wake
([ARCHITECTURE](ARCHITECTURE.md#microvm-firecracker)). A full host reboot is not a tested path. Run
the daemon as a systemd service so it starts again ([SELF-HOSTING](SELF-HOSTING.md)).

### How do I see what an agent did?

`sbx history [sandbox]` lists commands that changed something and every wake and sleep, with
`--json` for tools. `sbx logs` shows a service's output without waking it, and `sbx ui` is a live
dashboard ([CLI](CLI.md)).

### What happens to orphaned sandboxes?

`sbx with` removes its sandbox even when the command fails. An API sandbox with a `timeout` is
deleted when it expires. `sbx gc` lists volumes and images that dead sandboxes left, and deletes
them with `--force` ([CLI](CLI.md#data)).

A create that names an existing docker service keeps it. Firecracker refuses and says to `sbx rm`
first.

## Cost

### What does it cost? Am I billed while a sandbox is idle?

sbx is free and MIT licensed, and there is no bill. You pay for your own servers. A sleeping
sandbox holds no RAM, only disk. Someone on your team runs and patches those servers
([SELF-HOSTING](SELF-HOSTING.md)).

### Is self-hosting cheaper than E2B, Modal or Daytona?

For a team with idle capacity, usually. [COMPARISON](COMPARISON.md#monthly-cost-for-one-developer)
prices one developer environment per month from each vendor's pricing page. For a team that owns
no hardware, sbx is not a saving.

### Can I cap spend or the number of sandboxes?

No. sbx has no quotas and will not add them ([ROADMAP](ROADMAP.md#not-doing)). Limit `cpu` and
`memory` per service, and put a gateway in front of the API to cap creates.

## Compatibility

### Does it run on macOS or Windows?

On macOS, containers run on colima or Docker Desktop: unit-tested in CI, and run by hand with colima
on an M4. MicroVMs on an M3+ Mac with macOS 15 run in a helper Linux VM, run by hand on an M4 at
v0.11.

On Windows, containers run inside WSL2, and microVMs use a WSL2 helper VM on Windows 11. Both are
unit-tested and not yet run end to end on a Windows host ([platform
status](ARCHITECTURE.md#platform-status)).

Inside, a sandbox is always Linux: a Linux image (on a Mac, in the engine's VM) or a microVM's
own kernel.

### Is it Linux-only because of Firecracker?

The microVM backend needs Linux with `/dev/kvm`, directly or through a helper VM. Containers run
anywhere Docker runs. No distribution list is maintained. CI runs `ubuntu-24.04`
([BENCHMARKS](BENCHMARKS.md#how-these-were-measured)).

### Does it run on Kubernetes?

Yes, unit-tested and run by hand on minikube, not in CI. Each service becomes a Deployment scaled
between 0 and 1. The OpenSandbox API and egress rules are not built there yet
([SELF-HOSTING](SELF-HOSTING.md), [ROADMAP](ROADMAP.md#not-built-yet-in-the-opensandbox-api)).

### Which SDK languages are there?

OpenSandbox's SDKs, in 5 languages, drive sbx unchanged. The Python SDK is the one the docs show,
and upstream's Go test suite runs against sbx in CI ([GUIDES](GUIDES.md#opensandbox-sdks)).

### Is there an MCP server or a plain HTTP API?

Both. `sbx mcp` serves 19 tools on stdio, the same as OpenSandbox's own MCP server. The
OpenSandbox API is plain HTTP with an API key ([GUIDES](GUIDES.md#mcp)).

### Does it work with my agent framework?

If the framework can run a shell command, call MCP tools or use an OpenSandbox SDK, yes. sbx ships
no framework-specific plugins. [GUIDES](GUIDES.md#ai-agents) has a block to paste into an agent's
instructions.

### Can I use Podman instead of Docker?

Partly. sbx drives the `docker` CLI, so point `DOCKER_HOST` at podman's socket. `sbx checkpoint`
and `resume` were run by hand on podman at v0.7.0. The rest is not in the
[platform status](ARCHITECTURE.md#platform-status) table.

## Comparisons

### How does sbx compare with E2B, Daytona, Modal and the rest?

E2B, Daytona and Modal are hosted sandbox services.
[COMPARISON](COMPARISON.md#feature-matrix) has a feature matrix with dated vendor links.

sbx is self-hosted with no account, wakes on any TCP connection and speaks the OpenSandbox API. It
ranks last on community and hosted options, and lacks a secrets vault
([COMPARISON](COMPARISON.md#honest-ranking)).

### Why not use Firecracker, Kata or QEMU directly?

Firecracker boots a kernel and an ext4 disk. sbx adds images, networking, Firecracker's jailer
(which confines each VM's process), sleep and wake, warm pools and the agent APIs. Use Firecracker
or Kata directly if you are building your own platform or serving many untrusted tenants
([COMPARISON](COMPARISON.md#why-not-use-firecracker-directly)).

### Why not docker compose or Testcontainers?

Compose keeps services running and clashes on fixed ports across branches. Testcontainers starts
containers from inside tests and has a larger module catalogue. sbx fits when many copies share a
machine and sleep when unused ([COMPARISON](COMPARISON.md#local-tools)).

### Why not a cloud VM or a separate dev machine?

A VM per agent holds its RAM while idle. sbx runs on that VM and splits it into many sandboxes
that sleep at 0 B. sbx has not been compared with WASM sandboxes or git worktrees.

## Project

### Is it open source? Could it go closed?

Yes, MIT licensed ([LICENSE](../LICENSE)). The root Go module has zero dependencies, and hosting
sbx for others is ruled out ([DECISIONS](DECISIONS.md#sbx-is-a-tool-people-run-not-a-service-anyone-offers)).
An MIT release stays MIT.

### How mature is it, and who uses it?

Young. It is pre-1.0 at v0.14.0, and only the latest release gets fixes
([SECURITY.md](../SECURITY.md#supported-versions)). It had 2 GitHub stars on 2026-09-27, and no
production users are listed ([COMPARISON](COMPARISON.md#honest-ranking)).

### Is this Docker's sbx?

No. Docker ships its own `sbx` CLI (`docker/tap/sbx`) for Docker Sandboxes. If both are installed,
the first on `PATH` wins ([COMPARISON](COMPARISON.md#choose-something-else-if)).

### Is there a hosted version or paid support?

No, and a hosted sbx is not planned ([ROADMAP](ROADMAP.md#not-doing)). Report bugs as GitHub
issues and vulnerabilities through the private advisory form ([SECURITY.md](../SECURITY.md#reporting)).

### Does sbx send telemetry?

No. Only `sbx ui` checks for updates: at most once a day, one GET to GitHub's latest-release API
with no identifiers. `SBX_NO_UPDATE_CHECK=1` turns it off, and CI environments skip it
([CLI](CLI.md#update-check)).
