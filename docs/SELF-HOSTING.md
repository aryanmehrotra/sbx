# Self-hosting

Run sbx on your own servers so a team's agents and CI get sandboxes without a hosted service.
Every command here is for the current release; flags are listed in [CLI.md](CLI.md).

## Who runs it, and what it saves

### Teams inside a company

sbx runs inside your own network or VPC. Agent code, data and secrets stay there: there is no
vendor account and nothing hosted, which fits on-premises and compliance rules.

- One daemon per host serves every sandbox on it, under systemd or in a Kubernetes cluster.
- A sleeping sandbox holds no RAM, so one server can keep many idle agent sandboxes. Twenty idle
  Postgres databases held 17.6 MB with sbx and 629 MB with docker compose
  ([BENCHMARKS](BENCHMARKS.md#databases-for-branches-and-tests)).

What you add yourself: sbx has no multi-user auth, SSO, per-tenant quotas or central control
plane, and will not grow them ([ROADMAP](ROADMAP.md#not-doing)). Anyone who can reach the daemon
can use any sandbox ([SECURITY.md](../SECURITY.md#access-and-exposure)). The microVM daemon runs as
root on one host. Put a gateway in front for identity and limits.

### Startups and small teams

One Linux server with KVM can run every agent's sandboxes and every branch's database. In CI,
`sbx with` removes the sandbox even when the tests fail:

```sh
sbx with test-db --template postgres -- go test ./...
```

### What it costs

Hosted sandboxes bill for CPU and memory while a sandbox runs
([rates](COMPARISON.md#monthly-cost-for-one-developer)).

As an example, take 1 vCPU and 2 GiB, the size the OpenSandbox SDKs (clients for an open API
standard for agent sandboxes) request by default, running 8 h a day for 22 days (633,600 s). Modal
bills a "Physical core (2 vCPU equivalent)", so 1 vCPU is half of one of Modal's physical cores.
CPU is 633,600 × 0.5 × $0.00003942 = $12.49, and memory is 633,600 × 2 × $0.00000667 = $8.45.

That is $20.94 per sandbox per month, before the $30 monthly credit on Modal's Starter plan
([pricing](https://modal.com/pricing), checked 2026-09-27). Other vendors are in
[COMPARISON.md](COMPARISON.md#monthly-cost-for-one-developer).

With sbx you pay for servers you already run, and the sandbox's cost is its share of one. An idle
sandbox costs disk, not RAM. The flip side: someone on your team runs and patches those servers,
which a hosted service does for you.

## Pick a shape

- A Linux server with KVM: each sandbox is a microVM, the main use for untrusted agent code.
  KVM is Linux's built-in hypervisor; check that `/dev/kvm` exists. A microVM is a small VM with
  its own kernel, here run by [Firecracker](https://firecracker-microvm.github.io/).
- A Docker host without KVM: each sandbox is a container sharing the host's kernel. Needs Docker.
- A Kubernetes cluster: each service is a Deployment, woken by an in-cluster daemon. Needs
  `kubectl` and a context.
- A Mac for development: microVMs run in a helper VM on an M3 or later with macOS 15, run by hand
  only ([platform status](ARCHITECTURE.md#platform-status)).

A microVM host also needs Docker (to pull images and build their root filesystems), `mkfs.ext4`
from e2fsprogs, `iptables`, and iproute2 with `ip netns`. Its kernel needs `CONFIG_NET_NS` and
`CONFIG_VETH`, with a writable `/var/run/netns`. Run sbx on the host, not in a container that
forbids network namespaces.

## Install on a Linux server

```sh
curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh
```

The script installs to `/usr/local/bin` and checks the binary against the release's `SHA256SUMS`.
If that file is missing, it warns and installs unchecked.
`VERSION=v0.14.0` pins a release and `DIR=~/bin` picks another directory. To do it by hand:

```sh
V=v0.14.0
curl -fsSLO "https://github.com/aryanmehrotra/sbx/releases/download/$V/sbx_${V}_linux_amd64"
curl -fsSL "https://github.com/aryanmehrotra/sbx/releases/download/$V/SHA256SUMS" | grep " sbx_${V}_linux_amd64\$" | sha256sum -c
sudo install -m 0755 "sbx_${V}_linux_amd64" /usr/local/bin/sbx
```

`go install github.com/aryanmehrotra/sbx@latest` also works. Then check the machine:

```sh
sbx doctor          # what this machine can do
sbx fc backend      # can it run microVMs, and how
```

On a microVM host, look for these `sbx doctor` rows. `microVM` says whether Firecracker runs
directly. `mkfs.ext4` and `iptables` must be present. `vm host guard` and `vm bridges isolated` say
whether guests are closed off from the host and each other. `vm jailer` confirms each VM's process
runs under Firecracker's jailer, a confined unprivileged user. `microVM state filesystem` warns
when VM state shares a filesystem with `/`.

sbx downloads the pinned Firecracker, jailer and guest kernel into its state directory on first use,
checked by sha256. `sudo SBX_FC_STATE=/srv/sbx/fc sbx prewarm --provider firecracker IMAGE...` pulls
images and builds their root filesystems ahead of time, into the state directory the daemon below
uses.

## Run the daemon as a service

`sbx serve` is the daemon: it fronts every sandbox's ports, wakes on connect and sleeps after
`--idle`. Run one per machine; a second copy refuses to start. Restarting it is safe: it finds
every sandbox again at startup and puts nothing to sleep on the way out.

### Containers on Docker

[`deploy/sbx.service`](../deploy/sbx.service) is a systemd user unit. It runs as you, since it
only needs the Docker socket you can already reach:

```sh
mkdir -p ~/.config/systemd/user && cp deploy/sbx.service ~/.config/systemd/user/
systemctl --user enable --now sbx
loginctl enable-linger "$USER"      # keep it running after you log out
```

Edit its `ExecStart` to add the flags below. On a Mac,
[`deploy/dev.sbx.daemon.plist`](../deploy/dev.sbx.daemon.plist) does the same with launchd.

### MicroVMs with Firecracker

`sbx serve --provider firecracker` runs as root (or with `CAP_NET_ADMIN`). It makes a bridge and
a network namespace per sandbox and writes the iptables rules that guard the host; with
`--osb-addr` it refuses to start without that permission. So it needs a system unit, which
`deploy/` does not ship. A minimal one, as `/etc/systemd/system/sbx.service`:

```ini
[Unit]
Description=sbx microVM daemon
After=docker.service network-online.target
Wants=docker.service network-online.target

[Service]
Environment=HOME=/root SBX_FC_STATE=/srv/sbx/fc
EnvironmentFile=/etc/sbx/env
ExecStart=/usr/local/bin/sbx serve --provider firecracker --osb-addr 127.0.0.1:8080 --osb-pool python:3.11-slim=4 --idle 5m
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
sudo install -d -m 0700 /etc/sbx /srv/sbx
echo "SBX_OSB_KEY=$(openssl rand -hex 32)" | sudo tee /etc/sbx/env >/dev/null && sudo chmod 600 /etc/sbx/env
sudo systemctl daemon-reload && sudo systemctl enable --now sbx
```

SECURITY.md lists the capabilities to bound it with `CapabilityBoundingSet=`. CI runs the daemon
as full root, so a bounded daemon is not tested. Keep the jailer on: with `SBX_FC_JAILER=off` the
API refuses to start unless you pass `--osb-insecure-no-jailer`.

The flags an agent server uses:

- `--osb-addr 127.0.0.1:8080` serves the OpenSandbox API, the one its SDKs and `sbx mcp` call.
- `--osb-pool IMAGE=N` keeps N sandboxes of an image ready, so a create is answered from one.
  Repeat it per image; N defaults to 8.
- `--osb-pool-freeze` keeps pool members paused in RAM on firecracker, for a faster claim.
- `--idle 5m` sleeps a sandbox after five minutes with no bytes.
- `--vm-egress-allow 10.20.0.0/16` lets microVMs reach a private range, such as a registry on
  your VPC. No sandbox's own policy can open a private range ([egress](SPEC.md#egress-the-network-a-service-may-reach)).
- `--fc-firewall unmanaged` makes sbx write no iptables rules, when your own firewall closes
  `10.231.0.0/16` to guests.

State lives in root's home when the daemon runs as root: `~/.sbx` holds the API key, API records
and history, and `SBX_FC_STATE` (default `~/.sbx/fc`) holds VM disks and snapshots. Put
`SBX_FC_STATE` on a filesystem of its own, or one with project quotas. sbx bounds each VM's disk
but not their total, so many sandboxes can fill `/` for the whole host, and `sbx doctor` warns.

Run other commands on this host as root with the same `SBX_FC_STATE`, so they see the same VMs:

```sh
sudo SBX_FC_STATE=/srv/sbx/fc sbx list --provider firecracker
```

## The API key and MCP

MCP is the standard way AI assistants call outside tools. `sbx mcp` offers sandboxes as MCP tools.

The OpenSandbox API requires a key unless you pass `--osb-insecure-no-key`, which can let every
sandbox drive it. The key comes from `--osb-key`, then `SBX_OSB_KEY`, else one is generated into
`~/.sbx/osb/key` (mode 0600). With the unit above it is the value in `/etc/sbx/env`.

Prefer the variable to the flag, which other users can read in `ps`.

```sh
export OPEN_SANDBOX_DOMAIN=127.0.0.1:8080 OPEN_SANDBOX_API_KEY="$(sudo sed -n 's/^SBX_OSB_KEY=//p' /etc/sbx/env)"
claude mcp add sbx -e SBX_OSB_KEY="$OPEN_SANDBOX_API_KEY" -- sbx mcp    # tools for Claude Code
```

`sbx mcp` reads the key file itself only for a loopback URL and only from its own user's home, so
a non-root user passes it as above. Whoever holds the key can create, run commands in and delete
every API sandbox. Setup for other agents is in [GUIDES.md](GUIDES.md#mcp).

## Reach it from other machines

Run the agents, or the harness that drives them, on the sbx host. The API binds only a loopback
address: `--osb-addr 0.0.0.0:8080` is refused, because the sandbox endpoints it hands out are
`127.0.0.1` ports on that host, and sbx cannot carry them through the API yet. sbx does not
terminate TLS itself.

From another machine, the daemon's own error message names the way: the API over SSH, and the
sandbox endpoints through `sbx connect`. `sbx connect` binds the ports that exist when it starts, so
a sandbox created later is unreachable until you restart it. That suits a fixed set of sandboxes,
not an agent that creates them freely, and it is not yet run end to end.

On the server, add `--connect-addr 127.0.0.1:7700` to the unit's `ExecStart`, put
`SBX_CONNECT_TOKEN=...` in `/etc/sbx/env`, and run `sudo systemctl restart sbx`.

On the other machine, where no `sbx serve` holds the same port numbers:

```sh
ssh -N -L 8080:127.0.0.1:8080 -L 7700:127.0.0.1:7700 agents.example.internal &
SBX_CONNECT_TOKEN=... sbx connect http://127.0.0.1:7700 &
export OPEN_SANDBOX_DOMAIN=127.0.0.1:8080 OPEN_SANDBOX_API_KEY=...
```

`--connect-addr` refuses a non-loopback address unless `--behind-proxy` says a proxy in front
terminates TLS, and refuses to start without the token. The token gives TCP access to every
service, plus wake, sleep, re-limit, remove and logs. It cannot create or exec. Treat it like a
VPN credential ([SECURITY.md](../SECURITY.md#access-and-exposure)).

## Size the server

- An awake microVM holds its `memory` in RAM: `256m` unless the spec says otherwise. The
  OpenSandbox SDKs ask for 2 GiB by default. Its jail caps the process at that plus 128 MiB.
- A sleeping microVM holds no RAM. On disk it keeps a memory file up to the size of its RAM, its
  writable layer (`SBX_FC_DISK_SIZE`, sparse, `10g`) and its volumes (`SBX_FC_VOLUME_SIZE`).
- Pool members are 1 vCPU and 2 GiB. On firecracker they wait asleep on disk; with
  `--osb-pool-freeze` they wait paused in RAM, so `--osb-pool IMAGE=8` can hold about 17 GiB.
- On Docker, pool members are running containers; `--osb-pool-freeze` stops their idle CPU.
- Set `cpu` and `memory` per service in `sandbox.json` ([SPEC.md](SPEC.md#cpu-and-memory)), or
  `resourceLimits` in an API create. On firecracker, `cpu` rounds up to whole vCPUs.

`sbx doctor` sums microVM disk use by memory, disks, snapshots and volumes, and the pool's share.
A host short of memory fails sleeps
([TROUBLESHOOTING](TROUBLESHOOTING.md#a-microvm-could-not-sleep-execd-did-not-confirm-its-seal)).
Measured wake and create times are in [BENCHMARKS.md](BENCHMARKS.md).

## Kubernetes

`sbx create my-branch --template postgres --provider kubernetes --namespace sbx` makes each service a Deployment and
a Service in that namespace, through `kubectl` and your current context. Install the in-cluster daemon
once from [`deploy/activator.yaml`](../deploy/activator.yaml). It asks for `sbx-activator:dev`,
built from [`deploy/Dockerfile`](../deploy/Dockerfile). Each release also publishes
`ghcr.io/aryanmehrotra/sbx-activator:<tag>`, which you can put in its `image:` instead. Its
commented `--connect-addr` lines expose `sbx connect` through an Ingress.

- Kubernetes is unit-tested and run by hand on minikube, not in CI.
- The OpenSandbox API does not run on Kubernetes yet ([ROADMAP](ROADMAP.md#not-built-yet-in-the-opensandbox-api)).
- `--isolation firecracker` (Kata's `kata-fc` RuntimeClass) is not yet run end to end on a cluster.
- Some fields are refused there ([TROUBLESHOOTING](TROUBLESHOOTING.md#a-create-on---provider-kubernetes-is-refused)).

## Upgrade, back up, remove

Only the latest release gets fixes ([SECURITY.md](../SECURITY.md#supported-versions)). Read the
"Before you upgrade" section of each [release note](release-notes/README.md) you skip: it lists
breaking changes, new host requirements, and what to do about each.

```sh
curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh   # or brew upgrade sbx
sudo systemctl restart sbx          # systemctl --user restart sbx for the user unit
sbx doctor
```

sbx has no backup command. What holds data:

- `SBX_FC_STATE`: each microVM's disks and memory file, and `sbx snapshot` copies. An awake VM's
  snapshot is marked invalid, so `sbx sleep <sandbox>` before copying it.
- Docker volumes, for container sandboxes.
- `~/.sbx` of the user the daemon runs as: the API key and records, live egress policies, history.

Restoring a copy on another host is not yet run end to end. To remove sandboxes and sbx, use
`sbx rm <sandbox>` (deletes its data), then `sbx gc --snapshots --force` for leftover snapshots and
volumes. The full sequence is in [TROUBLESHOOTING](TROUBLESHOOTING.md#removing-sbx); for the
system unit, `sudo systemctl disable --now sbx` and remove `SBX_FC_STATE` too.

## When something is wrong

- Firecracker refused on this host: [TROUBLESHOOTING](TROUBLESHOOTING.md#sbx-doctor-or-sbx-fc-backend-refuses-firecracker-on-this-host)
- A network namespace error on create: [TROUBLESHOOTING](TROUBLESHOOTING.md#a-microvm-fails-making-sbxfcn-ms-network-namespace)
- A second daemon will not start: [TROUBLESHOOTING](TROUBLESHOOTING.md#sbx-serve-says-it-is-already-running)
- The SDK or `sbx mcp` gets a 401: [TROUBLESHOOTING](TROUBLESHOOTING.md#the-sdk-or-sbx-mcp-gets-401-missing_api_key-or-invalid_api_key)
- Pool creates are slow: [TROUBLESHOOTING](TROUBLESHOOTING.md#warm-pool-creates-are-slow-the-pool-always-misses)
- A disk fills up: [TROUBLESHOOTING](TROUBLESHOOTING.md#a-microvms-workload-says-no-space-left-on-device)
- Anything else: open an issue with the output of `sbx doctor --json`
