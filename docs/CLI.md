# CLI reference

Every `sbx` command and flag, the `sbx serve` flags, and every `SBX_*` environment variable.
`sbx <command> --help` prints the synopsis and an example for one command. For a walkthrough see
[QUICKSTART.md](QUICKSTART.md); for terms see [ARCHITECTURE.md](ARCHITECTURE.md#terms).

## Backend flags

Commands marked (B) below also take these.

| flag | default | meaning |
|---|---|---|
| `--provider` | `$SBX_PROVIDER_KIND`, else `docker` | `docker`, `kubernetes` or `firecracker` |
| `--socket` | `DOCKER_HOST`, then the active docker context | Docker endpoint to use |
| `--namespace` | `$SBX_NAMESPACE`, else `sbx` | Kubernetes namespace |
| `--isolation` | `$SBX_ISOLATION`, else `container` | `container`, `gvisor`, `kata` or `firecracker` (on Kubernetes: the kata-fc RuntimeClass) |

## Commands

### Start here

| command | purpose | flags |
|---|---|---|
| `sbx doctor` | What this machine can and cannot do. Run it first | `--json` |
| `sbx init` | Write a `sandbox.json` interactively. Piped or with `--template`, prints it to stdout | `--template NAME` (default `postgres`), `--yes`, `--from-devcontainer PATH` (gated: `devcontainer`) |
| `sbx serve` | The daemon, one per machine. See [sbx serve](#sbx-serve) | see below |
| `sbx selftest` | Create, sleep, wake and check a sandbox on this machine. (B) | `--keep` |

### Every day

| command | purpose | flags |
|---|---|---|
| `sbx create <sandbox>` | Make a sandbox. Services start asleep. (B) | `--spec FILE` (default `sandbox.json`), `--template NAME`, `--optional` |
| `sbx with <sandbox> -- <cmd>` | Create, wait until ready, run `cmd` with the env, then remove. Exits with `cmd`'s status (B) | `--spec`, `--template`, `--optional`, `--keep`, `--timeout 90s` |
| `sbx env <sandbox>` | Print the services' addresses as shell exports. (B) | `--shell posix\|fish\|powershell\|cmd\|json` (detected if unset), `--spec`, `--template` |
| `sbx list` | Every sandbox, its services, state and address. (B) | `--json` |
| `sbx ui` | Live dashboard. Aliases: `dash`, `dashboard`. (B) | `--connect URL` (repeatable), `--sandbox NAME` (repeatable, with `--connect`) |
| `sbx rm <sandbox>` | Delete a sandbox and its data. No undo. (B) | none |

### While you work

| command | purpose | flags |
|---|---|---|
| `sbx logs <sandbox> [service]` | What a service printed. Does not wake anything. (B) | `--tail N` (default 100), `-f` |
| `sbx exec [-t] <sandbox> <service> <cmd>...` | Run a command inside a service. (B) | `-t` attaches a terminal |
| `sbx cp <sandbox> <service> <src> <dst>` | Copy a file in or out. Prefix the in-service path with `:`. (B) | none |
| `sbx add <sandbox> <service>` | Add a service the spec never declared. (B) | `--image` (required), `--port N[,N]` (required), `--health CMD`, `--volume PATH`, `--env K=V,...`, `--spec` |
| `sbx url <sandbox> <service>` | Public link that wakes the service when opened. (B) | `--via cloudflared\|ngrok\|ssh` (detected if unset), `--host-header rewrite\|pass` (default `rewrite`) |
| `sbx connect <url>...` | Local ports for a sandbox deployed elsewhere. Reads `SBX_CONNECT_TOKEN`. | `--port-offset N\|LABEL=N`, `--sandbox NAME` (repeatable) |
| `sbx pack [service]` | Build contexts for a platform that runs one container on one HTTP port. | `--spec FILE` (default `sandbox.json`), `--out DIR` (default `sbx-pack`) |
| `sbx ready <sandbox>` | Block until every service really answers. For CI. (B) | `--timeout 90s` |
| `sbx wake <sandbox>` | Wake now and wait until serving. (B) | `--timeout 90s` |
| `sbx sleep <sandbox>` | Stop every service now and drop to 0 B. (B) | none |
| `sbx egress <sandbox> [service]` | Read or change a running sandbox's network policy. (B) | `--allow H`, `--deny H`, `--remove H` (all repeatable), `--default allow\|deny`, `--reset`, `--show`, `--json` |
| `sbx mcp` | MCP server (tools an AI app can call) on stdio, with OpenSandbox's 19 tools. Needs `sbx serve --osb-addr`. [Setup](GUIDES.md#mcp). | `--url` (see [env](#opensandbox-api-and-mcp)), `--key` |
| `sbx ssh <sandbox> [service]` | Reach a service with an editor over ssh. Gated: `SBX_FEATURES=ssh`. (B) | `--user` (default `root`), `--folder` (default `/work`), `--template`, `--spec` |

### Data

| command | purpose | flags |
|---|---|---|
| `sbx snapshot <sandbox> <name>` | Save every service's filesystem. (B) | none |
| `sbx fork <snapshot> <new-sandbox>` | New sandbox from a snapshot. (B) | `--spec`, `--template`, `--optional` |
| `sbx checkpoint <sandbox> <name>` | Save memory and processes with CRIU. Linux with a podman runtime only. (B) | none |
| `sbx resume <sandbox> <name>` | Restore from a checkpoint. (B) | none |
| `sbx gc` | List (or with `--force`, delete) volumes and images dead sandboxes left. (B) | `--older-than DURATION`, `--snapshots`, `--force` |

### Finding out

| command | purpose | flags |
|---|---|---|
| `sbx history [sandbox]` | Commands that changed something, and every wake and sleep. Reads a file. | `--limit N` (default 50, 0 = all), `--commands`, `--events`, `--json` |
| `sbx templates` | The built-in specs and when their images were pinned. | none |
| `sbx validate [sandbox.json]` | Check a spec, create nothing. | `--spec`, `--template` |
| `sbx prewarm [IMAGE...]` | Pull images now; on firecracker also build root filesystems. (B) | `--spec FILE` |
| `sbx features` | List preview features and whether each is on. | none |
| `sbx version` | Print the version. Also `--version`, `-v`. | none |
| `sbx help` | Top-level help. Also `--help`, `-h`. | none |

### MicroVMs

| command | purpose |
|---|---|
| `sbx fc backend` | Which backend a microVM would use here, and why. |
| `sbx fc vm status [--json]` | The helper VM (macOS, Windows) and the daemon inside it. |
| `sbx fc vm start [--cpus N] [--memory GiB] [--disk GiB]` | Create the helper VM on first use, start it, install this sbx and its daemon. |
| `sbx fc vm stop` | Stop the helper VM. Its disk and microVM sandboxes stay. |
| `sbx fc vm rm --yes` | Delete the helper VM and every microVM sandbox in it. |

`sbx serve --provider firecracker` starts the helper VM on demand, so you rarely need `sbx fc vm`.

### Gated features

A preview feature is off until named in `SBX_FEATURES` (comma-separated). `sbx features` lists them.

| feature | turns on |
|---|---|
| `ssh` | `sbx ssh` |
| `devcontainer` | `sbx init --from-devcontainer` |
| `waiting-page` | a "waking up" page for a browser opening a sleeping `sbx url` link |

## sbx serve

The daemon. It owns the ports `sbx env` prints, wakes a sandbox on connect and sleeps it after
`--idle`. Run one per machine, or one per `--only` scope.

| flag | default | meaning |
|---|---|---|
| `--provider` | `$SBX_PROVIDER_KIND`, else `docker` | `docker`, `kubernetes` or `firecracker` |
| `--socket` | `DOCKER_HOST`, then the docker context | Docker endpoint |
| `--namespace` | `$SBX_NAMESPACE`, else `sbx` | Kubernetes namespace |
| `--idle` | `5m` | Sleep a service after this long with no bytes |
| `--ready` | `90s` | Give up waking a service after this long |
| `--refresh` | `15s` | How often to look for new or removed sandboxes |
| `--only PREFIX` | `$SBX_ONLY`, else all | Manage only sandboxes matching this prefix or glob. Repeatable or comma-separated |
| `--connect-addr ADDR` | `$SBX_CONNECT_ADDR`, else off | Serve the `sbx connect` endpoint here. Needs `SBX_CONNECT_TOKEN` |
| `--front SPEC` | `$SBX_FRONT`, else off | Carry non-sandbox ports: `5432`, `db=5432,cache=6379`, `db=10.0.4.7:3306` |
| `--behind-proxy` | off | A proxy in front terminates TLS, so a non-loopback address is allowed |
| `--osb-addr ADDR` | `$SBX_OSB_ADDR`, else off | Serve the OpenSandbox lifecycle API (the one its SDKs and `sbx mcp` call), e.g. `127.0.0.1:8080` |
| `--osb-key KEY` | `$SBX_OSB_KEY`, else generated into `~/.sbx/osb/key` | Required `OPEN-SANDBOX-API-KEY` |
| `--osb-insecure-no-key` | off | Serve the API with no key. Loopback only |
| `--osb-host-paths DIRS` | `$SBX_OSB_HOST_PATHS`, else none | Host directories an OpenSandbox host volume may bind from |
| `--osb-pool IMAGE[=N]` | `$SBX_OSB_POOL`, else none | Keep N (default 8) warm sandboxes of this image. Repeatable |
| `--osb-pool-freeze` | off | Freeze pool members while they wait; on firecracker, keep them paused in RAM |
| `--osb-insecure-no-jailer` | off | Allow `--osb-addr` on firecracker with `SBX_FC_JAILER=off` |
| `--vm-egress-allow CIDRS` | `$SBX_VM_EGRESS_ALLOW`, else none | Private ranges a microVM's egress filter may still reach |
| `--fc-firewall MODE` | `$SBX_FC_FIREWALL`, else `managed` | `managed`: sbx closes the host to guests. `unmanaged`: your firewall does |

`--connect-addr` and `--front` need `SBX_CONNECT_TOKEN`. Read [SECURITY.md](../SECURITY.md)
before fronting a private address.

On macOS or Windows, `sbx serve --provider firecracker` runs the daemon inside the helper VM, a
small Linux VM that sbx starts. There the API key is always on, and only these flags work:
`--idle`, `--ready`, `--refresh`, `--only`, `--osb-addr`, `--osb-key`, `--osb-host-paths`,
`--osb-insecure-no-jailer`. `--osb-insecure-no-key`, `--osb-pool`, `--osb-pool-freeze` and
`SBX_OSB_POOL` are refused with a reason. Other flags fail as "flag provided but not defined".
`SBX_CONNECT_ADDR` and `SBX_FRONT` are not read.

## Environment variables

`sbx` reads these. A flag, when given, beats its variable.

### Core

| variable | default | meaning |
|---|---|---|
| `SBX_PROVIDER_KIND` | `docker` | Default for `--provider` |
| `SBX_NAMESPACE` | `sbx` | Default for `--namespace` |
| `SBX_ISOLATION` | `container` | Default for `--isolation` |
| `SBX_ONLY` | all sandboxes | Default for `sbx serve --only` |
| `SBX_FEATURES` | none | Comma-separated gated features to turn on |
| `SBX_HISTORY` | `~/.sbx/history.jsonl` | Where `sbx history` reads and writes the journal |
| `SBX_UI_PLAIN` | unset | Any value: `sbx ui` drops colour and styling |
| `SBX_NO_UPDATE_CHECK` | unset | Any value: turn off the update check (see below) |

`sbx env` also prints `SBX_SANDBOX` and `SBX_PROVIDER` for your shell. sbx does not read them.

### Remote (connect, pack, front)

| variable | default | meaning |
|---|---|---|
| `SBX_CONNECT_TOKEN` | none | Token for `serve --connect-addr`/`--front` and for `sbx connect`, `sbx ui --connect` |
| `SBX_CONNECT_TOKEN_<NAME>` | falls back to `SBX_CONNECT_TOKEN` | Token for the deployment named `NAME=https://...` |
| `SBX_CONNECT_INSECURE` | unset | Any value: let `sbx connect` use plain `http` to a non-local host |
| `SBX_CONNECT_ADDR` | off | Default for `serve --connect-addr` |
| `SBX_FRONT` | off | Default for `serve --front` |

### OpenSandbox API and MCP

| variable | default | meaning |
|---|---|---|
| `SBX_OSB_ADDR` | off | Default for `serve --osb-addr` |
| `SBX_OSB_KEY` | `~/.sbx/osb/key` | API key for `serve` and for `sbx mcp` |
| `SBX_OSB_POOL` | none | Default for `serve --osb-pool`, comma-separated `IMAGE[=N]` |
| `SBX_OSB_HOST_PATHS` | none | Default for `serve --osb-host-paths` |
| `SBX_OSB_URL` | `http://127.0.0.1:8080` | Server `sbx mcp` talks to |
| `OPEN_SANDBOX_DOMAIN`, `OPEN_SANDBOX_API_KEY` | none | Upstream names; `sbx mcp` reads them after the `SBX_OSB_*` ones |
| `SBX_OSB_TRACE` | unset | Any value: log the time of each create phase |
| `SBX_EXECD_BINARY` | this binary, then a build, then the release image | Path to a Linux `sbx` to run inside sandboxes as the exec agent |
| `SBX_SOURCE_DIR` | searched upward from the working directory | sbx checkout used to cross-compile that agent (development) |

### Firecracker microVMs

| variable | default | meaning |
|---|---|---|
| `SBX_FC_STATE` | `~/.sbx/fc` | Where VM disks and state live |
| `SBX_FC_ROOTFS` | `layered` | `copy` gives each VM a whole copy of its image (slower to create without reflink) |
| `SBX_FC_DISK_SIZE` | `10g` | Size of a layered VM's writable layer (sparse) |
| `SBX_FC_VOLUME_SIZE` | `10g` | Size of a new volume (sparse) |
| `SBX_FC_BOOT_TIMEOUT` | `60s` | How long to wait for a VM to boot |
| `SBX_FC_BINARY`, `SBX_FC_KERNEL` | pinned downloads | Use this firecracker binary or guest kernel instead |
| `SBX_FC_JAILER_BINARY` | pinned download | Use this jailer binary instead |
| `SBX_FC_JAILER` | on | `off` runs each Firecracker process without its jailer, as root. Development only; see SECURITY.md |
| `SBX_FC_JAILER_UID_BASE` | `900000` | Start of the uid range jailed VMMs run as |
| `SBX_FC_FIREWALL` | `managed` | Default for `serve --fc-firewall` |
| `SBX_VM_EGRESS_ALLOW` | none | Default for `serve --vm-egress-allow` |
| `SBX_FC_KEEP_CONSOLES` | off | Directory to copy a removed VM's console log into (for CI) |
| `SBX_KATA_FC_RUNTIMECLASS` | `kata-fc` | RuntimeClass that Kubernetes `--isolation firecracker` asks for |
| `SBX_FC_VM_DRIVER` | lima, else colima | Helper VM driver on macOS: `lima` or `colima` |
| `SBX_FC_VM_NAME` | `sbx-fc` | Helper VM name (macOS, Windows) |
| `SBX_FC_VM_CPUS` | `2` | Helper VM CPUs, on create |
| `SBX_FC_VM_MEMORY` | `2` | Helper VM GiB of memory, on create |
| `SBX_FC_VM_DISK` | `20` | Helper VM GiB of disk, on create |
| `SBX_FC_ASSUME_NESTED` | unset | Any value: treat a Mac chip sbx cannot identify as M3 or later |

`SBX_FC_HOST_BUSY_SLOTS` is set by sbx itself when it drives the helper VM. Do not set it.

## Update check

Only `sbx ui` checks, and shows a notice when a newer release exists. It reads a cache file
(`~/.sbx/update.json`) and never blocks. At most once a day it refreshes the cache in the
background with one GET to `https://api.github.com/repos/aryanmehrotra/sbx/releases/latest`,
sending no identifiers.

It is off when `CI`, `GITHUB_ACTIONS`, `BUILDKITE`, `JENKINS_URL` or `GITLAB_CI` is set. Set
`SBX_NO_UPDATE_CHECK=1` to turn it off everywhere.
