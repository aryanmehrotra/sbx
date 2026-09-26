# shellcheck shell=bash
# Shared by scripts/osb-conformance.sh and scripts/osb-bench.sh: fetch the pinned upstream,
# build the tools, and run a throwaway `sbx serve --osb-addr` that cannot touch anything
# it did not create.
#
#   . scripts/lib/osb.sh
#   osb_init                # work dir, teardown trap
#   osb_fetch_upstream      # -> OSB_SRC (verified checkout of the pinned commit)
#   osb_build_tools         # -> OSB_SBX, OSB_HARNESS
#   osb_start_daemon        # -> OSB_URL, OSB_KEY   (or osb_use_external URL KEY)
#
# Why a throwaway daemon needs any care at all: `sbx serve` fronts and reaps EVERY sandbox
# on its docker endpoint. A second daemon beside a live one binds the live one's ports and,
# worse, can put its sandboxes to sleep on its own idle clock. So isolation is two things,
# and both are enforced rather than assumed:
#
#   1. Its own HOME. The daemon's presence file, slot lock and history live in ~/.sbx; with
#      HOME pointed at the work dir it neither trips the one-per-machine guard nor, on exit,
#      deletes the live daemon's presence record. Docker's and Go's own locations are passed
#      explicitly so moving HOME does not also move the docker endpoint or the module cache.
#   2. A docker endpoint with no sbx sandboxes on it. Checked before the daemon starts; if
#      there are any, this refuses and names them. CI runners are empty; on a laptop with a
#      live daemon, point --docker-host at a second engine (e.g. `colima start osb`).
#
# Teardown removes only sandboxes whose name starts with osb- AND were not on the endpoint
# when the run began - on an endpoint that was required to have none.

# These are the library's outputs, read by the scripts that source it.
# shellcheck disable=SC2034

OSB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OSB_WORK=""
OSB_DAEMON=""
OSB_OWNED=0
OSB_URL=""
OSB_KEY=""
OSB_DOCKER_HOST=""
OSB_PREEXISTING=""
OSB_KEEP_WORK=0
OSB_IDLE=""   # passed to sbx serve --idle when set (the bench's wake-from-frozen)
OSB_REAL_HOME="$HOME"
OSB_HOSTVOL=""         # a host-volume root this run made, removed on teardown
OSB_PREEXISTING_PVC="" # sbx-osb-pvc-* volumes that were there before the run

osb_say() { printf '%s\n' "$*" >&2; }
osb_die() { printf 'osb: %s\n' "$*" >&2; exit 1; }

osb_init() {
  OSB_WORK="$(mktemp -d "${TMPDIR:-/tmp}/sbx-osb.XXXXXX")"
  mkdir -p "$OSB_WORK/home"
  OSB_GOPATH="$(go env GOPATH)"; OSB_GOMODCACHE="$(go env GOMODCACHE)"; OSB_GOCACHE="$(go env GOCACHE)"
  trap osb_teardown EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
}

# osb_pin KEY - a value from test/osb/UPSTREAM.
osb_pin() { sed -n "s/^$1=//p" "$OSB_ROOT/test/osb/UPSTREAM" | head -1; }

# osb_fetch_upstream - a checkout of the pinned commit, fetched once per commit and reused.
#
# The tag is fetched and then held to the commit in UPSTREAM: a tag can be moved, and a run
# against whatever it points at today is not the run this repo's claim is about. A reused
# checkout is re-verified (same commit, no modified tracked files) because "the upstream
# suite, unmodified" is the claim, and a stray edit in a cache directory would void it.
osb_fetch_upstream() {
  local repo tag commit base dir tmp got
  repo="$(osb_pin repo)"; tag="$(osb_pin tag)"; commit="$(osb_pin commit)"
  [ -n "$repo" ] && [ -n "$tag" ] && [ -n "$commit" ] || osb_die "test/osb/UPSTREAM needs repo=, tag= and commit="

  base="${SBX_OSB_CACHE:-${XDG_CACHE_HOME:-$OSB_REAL_HOME/.cache}/sbx/osb}"
  dir="$base/$commit"

  if [ -f "$dir/.git/sbx-verified" ]; then
    got="$(git -C "$dir" rev-parse HEAD 2>/dev/null)"
    [ "$got" = "$commit" ] || osb_die "cached upstream at $dir is at $got, not $commit; remove it: rm -rf '$dir'"

    if [ -n "$(git -C "$dir" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
      osb_die "cached upstream at $dir has modified files, so it is no longer the suite at $tag; remove it: rm -rf '$dir'"
    fi
  else
    osb_say "fetching $repo $tag (once per commit, into $dir)"
    mkdir -p "$base"
    tmp="$(mktemp -d "$base/.fetch.XXXXXX")"

    # Sparse and blob-filtered: the suite needs tests/go and the SDK it replaces to
    # ../../sdks/sandbox/go, not the server, the other SDKs or the docs.
    if ! { git -C "$tmp" init -q &&
      git -C "$tmp" remote add origin "$repo" &&
      git -C "$tmp" sparse-checkout set tests/go sdks/sandbox/go specs &&
      git -C "$tmp" fetch -q --depth 1 --filter=blob:none origin "refs/tags/$tag:refs/tags/$tag"; }; then
      rm -rf "$tmp"
      osb_die "could not fetch $tag from $repo (network?). Set SBX_OSB_CACHE to a directory holding a verified checkout to run offline."
    fi

    got="$(git -C "$tmp" rev-parse "refs/tags/$tag^{commit}")"
    if [ "$got" != "$commit" ]; then
      rm -rf "$tmp"
      osb_die "$tag now points at $got, but test/osb/UPSTREAM pins $commit. The tag moved upstream; re-pin deliberately, do not follow it."
    fi

    git -C "$tmp" checkout -q --detach "$got" || { rm -rf "$tmp"; osb_die "checkout of $got failed"; }
    touch "$tmp/.git/sbx-verified"
    rm -rf "$dir"
    mv "$tmp" "$dir"
  fi

  OSB_SRC="$dir"
  [ -f "$OSB_SRC/tests/go/go.mod" ] || osb_die "$OSB_SRC has no tests/go/go.mod"
}

# osb_build_tools [nosbx] - the harness always; sbx from this worktree unless told not to.
osb_build_tools() {
  OSB_HARNESS="$OSB_WORK/osbharness"
  (cd "$OSB_ROOT/test/osb" && GOWORK=off go build -o "$OSB_HARNESS" ./cmd/osbharness) ||
    osb_die "could not build test/osb/cmd/osbharness"

  if [ "${1:-}" != "nosbx" ]; then
    OSB_SBX="$OSB_WORK/sbx"
    (cd "$OSB_ROOT" && go build -o "$OSB_SBX" .) || osb_die "could not build sbx from $OSB_ROOT"
    osb_require_lifecycle_api
  fi
}

# osb_require_lifecycle_api - fail fast when the sbx just built predates the lifecycle API.
#
# Asked with an undefined flag after the real ones: Go's flag package parses in order and
# exits at the first unknown one, before Serve does anything - so this starts nothing,
# binds nothing, and the error names whichever flag is missing.
osb_require_lifecycle_api() {
  local probe
  probe="$(osb_daemon_env "$OSB_SBX" serve --osb-addr 127.0.0.1:0 --osb-key probe --harness-probe-undefined 2>&1)"
  case "$probe" in
    *"not defined: -osb-"*)
      osb_die "this sbx has no --osb-addr/--osb-key; build from a branch with the lifecycle API ($(printf '%s' "$probe" | head -1))" ;;
    *"not defined: -harness-probe-undefined"*) ;;
    *) osb_die "could not tell whether this sbx has the lifecycle API; 'sbx serve' said: $(printf '%s' "$probe" | head -3)" ;;
  esac
}

# osb_daemon_vars - the throwaway daemon's environment. Only ever called inside a subshell,
# so the caller's own HOME is never the one that moves. Go's locations were resolved in
# osb_init, under the real HOME; asked again here they would follow the fake one.
osb_daemon_vars() {
  export HOME="$OSB_WORK/home"
  export SBX_HISTORY="$OSB_WORK/home/history.jsonl" SBX_NO_UPDATE_CHECK=1 SBX_PROVIDER_KIND=docker
  export DOCKER_HOST="$OSB_DOCKER_HOST" DOCKER_CONFIG="${DOCKER_CONFIG:-$OSB_REAL_HOME/.docker}"
  export GOPATH="$OSB_GOPATH" GOMODCACHE="$OSB_GOMODCACHE" GOCACHE="$OSB_GOCACHE"
}

# osb_daemon_env CMD... - run CMD as the throwaway daemon sees the world.
osb_daemon_env() { ( osb_daemon_vars; exec "$@" ); }

# osb_resolve_docker [URL] - the endpoint, resolved while HOME is still the real one.
osb_resolve_docker() {
  OSB_DOCKER_HOST="${1:-${DOCKER_HOST:-}}"

  if [ -z "$OSB_DOCKER_HOST" ]; then
    OSB_DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null)"
  fi

  [ -n "$OSB_DOCKER_HOST" ] || OSB_DOCKER_HOST="unix:///var/run/docker.sock"

  DOCKER_HOST="$OSB_DOCKER_HOST" docker info >/dev/null 2>&1 ||
    osb_die "no docker engine answering at $OSB_DOCKER_HOST"
}

osb_pvc_volumes() {
  DOCKER_HOST="$OSB_DOCKER_HOST" docker volume ls --format '{{.Name}}' --filter name=sbx-osb-pvc- 2>/dev/null |
    grep '^sbx-osb-pvc-' | sort -u
}

osb_sbx_sandboxes() {
  DOCKER_HOST="$OSB_DOCKER_HOST" docker ps -a --filter label=sbx.sandbox \
    --format '{{.Label "sbx.sandbox"}}' 2>/dev/null | sort -u
}

# osb_start_daemon [--no-key] - start it, and wait until its lifecycle API answers.
osb_start_daemon() {
  local port n key_args
  key_args=1
  [ "${1:-}" = "--no-key" ] && key_args=0

  # The endpoint must hold no sbx sandboxes - see the header for why.
  OSB_PREEXISTING="$(osb_sbx_sandboxes)"
  if [ -n "$OSB_PREEXISTING" ]; then
    n="$(printf '%s\n' "$OSB_PREEXISTING" | wc -l | tr -d ' ')"
    osb_die "refusing to start a second sbx daemon on $OSB_DOCKER_HOST: it already has $n sbx sandbox(es):
$(printf '%s\n' "$OSB_PREEXISTING" | head -10 | sed 's/^/       /')
     A daemon fronts and reaps every sandbox on its endpoint, so a throwaway one here would
     bind your live daemon's ports and could put your sandboxes to sleep. Point this at an
     empty engine instead:  --docker-host unix://\$HOME/.colima/osb/docker.sock  (after
     'colima start osb'), or use --external against a daemon you already run."
  fi

  port="$("$OSB_HARNESS" freeport)" || osb_die "no free port"
  OSB_URL="http://127.0.0.1:$port"
  OSB_KEY=""

  # Built in the function's own positional parameters: bash 3.2 treats an empty array as
  # unbound under set -u, and every optional flag here is optional.
  set -- serve --osb-addr "127.0.0.1:$port"

  if [ "$key_args" = 1 ]; then
    OSB_KEY="osb-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
    set -- "$@" --osb-key "$OSB_KEY"
  elif osb_daemon_env "$OSB_SBX" serve --osb-insecure-no-key --harness-probe-undefined 2>&1 |
    grep -q "not defined: -harness-probe-undefined"; then
    # Since v0.9.1 a daemon with no --osb-key generates a key and requires it: loopback is
    # reachable from every container on a VM-backed engine. Keyless is an explicit flag; an
    # older sbx has no such flag and was keyless on loopback anyway, so it is left as it was.
    set -- "$@" --osb-insecure-no-key
  fi

  [ -n "$OSB_IDLE" ] && set -- "$@" --idle "$OSB_IDLE"

  # Host volumes are off unless a root is allowed, so upstream's volume tests would skip. The
  # directory they bind is allowed - and nothing else. Made under the real HOME rather than
  # /tmp (upstream's default): a VM-backed engine (colima, Docker Desktop) shares the home
  # directory, and a path it cannot see is refused by the bind rather than faked inside the VM.
  if [ -z "${OPENSANDBOX_TEST_HOST_VOLUME_DIR:-}" ]; then
    mkdir -p "$OSB_REAL_HOME/.cache/sbx" || osb_die "cannot create $OSB_REAL_HOME/.cache/sbx"
    OSB_HOSTVOL="$(mktemp -d "$OSB_REAL_HOME/.cache/sbx/osb-hostvol.XXXXXX")" || osb_die "no host volume dir"
    export OPENSANDBOX_TEST_HOST_VOLUME_DIR="$OSB_HOSTVOL/host-volume-test"
  fi
  set -- "$@" --osb-host-paths "$OPENSANDBOX_TEST_HOST_VOLUME_DIR"

  # pvc volumes outlive their sandboxes by design; the ones this run creates are removed on
  # teardown, and only those.
  OSB_PREEXISTING_PVC="$(osb_pvc_volumes)"

  # exec, so $! is sbx itself rather than a subshell that a TERM would leave it orphaned by.
  ( osb_daemon_vars; exec "$OSB_SBX" "$@" ) >"$OSB_WORK/daemon.log" 2>&1 &

  OSB_DAEMON=$!
  OSB_OWNED=1
  osb_say "sbx serve (pid $OSB_DAEMON) on $OSB_URL, docker $OSB_DOCKER_HOST, HOME $OSB_WORK/home"

  n=0
  while :; do
    if ! kill -0 "$OSB_DAEMON" 2>/dev/null; then
      OSB_DAEMON=""
      osb_die "sbx serve exited during startup:
$(tail -20 "$OSB_WORK/daemon.log" | sed 's/^/       /')"
    fi

    "$OSB_HARNESS" ready -url "$OSB_URL" -key "$OSB_KEY" -timeout 1s >/dev/null 2>"$OSB_WORK/ready.err" && break

    n=$((n + 1))
    if [ "$n" -ge 60 ]; then
      osb_die "sbx serve never answered on $OSB_URL: $(cat "$OSB_WORK/ready.err")
     daemon log: $OSB_WORK/daemon.log"
    fi
  done
}

# osb_use_external URL [KEY] - someone else's server: nothing started, nothing torn down.
osb_use_external() {
  OSB_URL="${1%/}"
  OSB_KEY="${2:-}"

  case "$OSB_URL" in
    http://*|https://*) ;;
    *) osb_die "--external wants http(s)://host:port, got '$OSB_URL'" ;;
  esac

  "$OSB_HARNESS" ready -url "$OSB_URL" -key "$OSB_KEY" -timeout 10s >&2 ||
    osb_die "the external server at $OSB_URL is not an answering OpenSandbox lifecycle API (above)"
}

# osb_export_suite_env - every variable the pinned suite and SDK read, set from OSB_URL/KEY.
# test/osb/README.md lists what each one does upstream.
osb_export_suite_env() {
  local proto hostport
  proto="${OSB_URL%%://*}"
  hostport="${OSB_URL#*://}"
  hostport="${hostport%%/*}"

  export OPENSANDBOX_TEST_DOMAIN="$hostport" OPENSANDBOX_TEST_PROTOCOL="$proto"
  export OPENSANDBOX_URL="$OSB_URL"
  export OPEN_SANDBOX_DOMAIN="$hostport" OPEN_SANDBOX_PROTOCOL="$proto"
  export OPENSANDBOX_TEST_USE_SERVER_PROXY="${OPENSANDBOX_TEST_USE_SERVER_PROXY:-false}"
  export RUN_CODE_INTERPRETER_E2E="${RUN_CODE_INTERPRETER_E2E:-true}"

  # Unset rather than empty when there is no key: upstream falls back to "e2e-test" only
  # when the variable is empty, which is what a keyless OpenSandbox server ignores anyway.
  if [ -n "$OSB_KEY" ]; then
    export OPENSANDBOX_TEST_API_KEY="$OSB_KEY" OPENSANDBOX_API_KEY="$OSB_KEY" OPEN_SANDBOX_API_KEY="$OSB_KEY"
  fi
}

osb_teardown() {
  local rc=$? n left
  trap - EXIT INT TERM
  set +e

  if [ "$OSB_FC" = 1 ]; then
    osb_fc_teardown "$rc"
  elif [ "$OSB_OWNED" = 1 ]; then
    # Through the API first, while the daemon is up: that is the path a user would take.
    if [ -n "$OSB_DAEMON" ] && kill -0 "$OSB_DAEMON" 2>/dev/null; then
      "$OSB_HARNESS" sweep -url "$OSB_URL" -key "$OSB_KEY" -prefix osb- >&2
      kill -TERM "$OSB_DAEMON" 2>/dev/null
      # Up to a minute: a daemon with warm pools (--osb-pool) removes its unclaimed members on
      # the way out, and any still being made finish their docker run first. Killed sooner,
      # it leaves them for the sweep below.
      n=0
      while kill -0 "$OSB_DAEMON" 2>/dev/null && [ "$n" -lt 120 ]; do sleep 0.5; n=$((n + 1)); done
      kill -KILL "$OSB_DAEMON" 2>/dev/null
    fi

    # Then whatever the API did not account for - a crashed daemon, a half-made create.
    # Only osb-* names, and only ones that were not there when this run started.
    for n in $(osb_sbx_sandboxes); do
      case "$n" in osb-*) ;; *) continue ;; esac
      if printf '%s\n' "$OSB_PREEXISTING" | grep -qx "$n"; then continue; fi
      osb_say "removing leftover sandbox $n"
      osb_daemon_env "$OSB_SBX" rm "$n" >/dev/null 2>&1 || osb_say "  could not remove $n; remove it with: DOCKER_HOST=$OSB_DOCKER_HOST sbx rm $n"
    done

    left="$(osb_sbx_sandboxes | grep '^osb-')"
    [ -z "$left" ] || osb_say "WARNING: still present after teardown: $left"

    for n in $(osb_pvc_volumes); do
      if printf '%s\n' "$OSB_PREEXISTING_PVC" | grep -qx "$n"; then continue; fi
      DOCKER_HOST="$OSB_DOCKER_HOST" docker volume rm "$n" >/dev/null 2>&1 ||
        osb_say "  could not remove volume $n; remove it with: DOCKER_HOST=$OSB_DOCKER_HOST docker volume rm $n"
    done
  fi

  [ -n "$OSB_HOSTVOL" ] && rm -rf "$OSB_HOSTVOL"

  if [ -n "$OSB_WORK" ] && [ -d "$OSB_WORK" ]; then
    # Binaries are rebuilt every run and are most of the size; the logs are the point.
    rm -f "$OSB_WORK/sbx" "$OSB_WORK/sbx-linux" "$OSB_WORK/e2e.test" "$OSB_WORK/osbharness" "$OSB_WORK/bench"

    if [ "$OSB_KEEP_WORK" = 1 ] || { [ "$rc" -ne 0 ] &&
      { [ -e "$OSB_WORK/daemon.log" ] || [ -e "$OSB_WORK/go-test.json" ] || [ -e "$OSB_WORK/compile.log" ]; }; }; then
      osb_say "logs kept in $OSB_WORK"
    else
      rm -rf "$OSB_WORK"
    fi
  fi

  exit "$rc"
}

# ── the firecracker provider ─────────────────────────────────────────────────────────────────
#
# osb_fc_start_daemon [VM] - `sbx serve --provider firecracker --osb-addr`, as root (taps,
# bridges and /dev/kvm need it), either here (a Linux host with /dev/kvm: the CI microvm job) or
# inside the colima profile VM (a Mac: an arm64 VM with nested virtualisation, which the caller
# made and owns). -> OSB_URL, OSB_KEY, as for docker; the URL is only reachable where the daemon
# runs, which is why a VM run executes the suite there too (osb_fc_run_suite).
#
# Isolation is its own state directory rather than a docker endpoint: the daemon only sees the
# VMs under SBX_FC_STATE, which must hold no osb-* sandbox when the run starts. The directory
# persists between runs so the root filesystems it builds (an image export and an mkfs each) are
# built once; override it with SBX_FC_STATE.
OSB_VM=""
OSB_FC=0
OSB_FC_DIR=""
OSB_FC_STATE=""
OSB_FC_VOLS_BEFORE=""

# osb_fc_sh SCRIPT - run a bash script as root where the daemon runs.
osb_fc_sh() {
  if [ -n "$OSB_VM" ]; then
    colima ssh -p "$OSB_VM" -- sudo bash -c "$1"
  else
    sudo -n bash -c "$1"
  fi
}

# osb_fc_put SRC DST - copy a file to where the daemon runs, executable.
osb_fc_put() {
  osb_fc_sh "mkdir -p $(dirname "$2") && cat > $2 && chmod 755 $2" < "$1"
}

osb_fc_arch() {
  local m
  if [ -n "$OSB_VM" ]; then m="$(colima ssh -p "$OSB_VM" -- uname -m)"; else m="$(uname -m)"; fi
  case "$m" in aarch64|arm64) echo arm64 ;; x86_64|amd64) echo amd64 ;; *) osb_die "unknown arch $m" ;; esac
}

osb_fc_sbx_env() {
  printf 'HOME=%s/home SBX_FC_STATE=%s SBX_PROVIDER_KIND=firecracker SBX_NO_UPDATE_CHECK=1 SBX_HISTORY=%s/home/history.jsonl' \
    "$OSB_FC_DIR" "$OSB_FC_STATE" "$OSB_FC_DIR"
}

osb_fc_start_daemon() {
  local arch port key n
  OSB_VM="${1:-}"
  OSB_FC=1

  if [ -n "$OSB_VM" ]; then
    command -v colima >/dev/null || osb_die "--vm needs colima"
    colima ssh -p "$OSB_VM" -- true 2>/dev/null || osb_die "colima profile $OSB_VM is not running (colima start --profile $OSB_VM --vm-type vz --nested-virtualization --runtime docker)"
    OSB_FC_DIR="/tmp/sbx-osb-run.$$"
  else
    [ "$(uname -s)" = Linux ] || osb_die "--provider firecracker runs the daemon on this host, which must be Linux with /dev/kvm; on a Mac add --vm PROFILE"
    sudo -n true 2>/dev/null || osb_die "--provider firecracker starts the daemon as root (taps, bridges, /dev/kvm): run where sudo -n works"
    OSB_FC_DIR="$OSB_WORK/fc-run"
  fi

  OSB_FC_STATE="${SBX_FC_STATE:-/var/tmp/sbx-osb-fc}"
  arch="$(osb_fc_arch)"

  OSB_SBX="$OSB_WORK/sbx-linux"
  (cd "$OSB_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$OSB_SBX" .) ||
    osb_die "could not build linux/$arch sbx from $OSB_ROOT"
  osb_fc_put "$OSB_SBX" "$OSB_FC_DIR/sbx"

  osb_fc_sh "mkdir -p $OSB_FC_DIR/home $OSB_FC_DIR/hostvol $OSB_FC_STATE"

  # The state directory must hold no API sandbox: a daemon fronts and reaps every one it sees.
  n="$(osb_fc_sh "env $(osb_fc_sbx_env) $OSB_FC_DIR/sbx list 2>/dev/null | grep -c '^osb-' || true")"
  [ "${n:-0}" = 0 ] || osb_die "$OSB_FC_STATE already holds $n osb-* sandbox(es); point SBX_FC_STATE at an empty directory"
  OSB_FC_VOLS_BEFORE="$(osb_fc_sh "ls $OSB_FC_STATE/volumes 2>/dev/null || true" | tr '\n' ' ')"

  port=$((18100 + RANDOM % 800))
  key="osb-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  OSB_URL="http://127.0.0.1:$port"
  OSB_KEY="$key"

  # A host path is allowed on purpose: the refusal the volume tests see must be the provider's
  # (a microVM has no virtio-fs), not an empty allow-list.
  # In a subshell, so the ssh session that started it can end: a direct background job keeps it.
  osb_fc_sh "cd $OSB_FC_DIR && (env $(osb_fc_sbx_env) setsid nohup $OSB_FC_DIR/sbx serve --provider firecracker \
    --osb-addr 127.0.0.1:$port --osb-key $key --osb-host-paths $OSB_FC_DIR/hostvol \
    > $OSB_FC_DIR/daemon.log 2>&1 < /dev/null &)"
  OSB_OWNED=1
  osb_say "sbx serve --provider firecracker on $OSB_URL (${OSB_VM:+inside colima $OSB_VM, }state $OSB_FC_STATE)"

  for n in $(seq 1 60); do
    osb_fc_sh "curl -sf -o /dev/null http://127.0.0.1:$port/health" && return 0
    osb_fc_sh "pgrep -f '^$OSB_FC_DIR/sbx serve' >/dev/null" ||
      osb_die "sbx serve exited during startup: $(osb_fc_sh "tail -20 $OSB_FC_DIR/daemon.log")"
    sleep 1
  done

  osb_die "sbx serve never answered on $OSB_URL: $(osb_fc_sh "tail -20 $OSB_FC_DIR/daemon.log")"
}

# osb_fc_run_suite TESTS_DIR RUN_RE TIMEOUT - the suite, compiled for the VM and run there, its
# test2json stream on stdout exactly as `go test -json` would print it here. Its exit status is
# the test binary's.
osb_fc_run_suite() {
  local arch envs
  arch="$(osb_fc_arch)"

  (cd "$1" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$OSB_WORK/e2e.test" .) >&2 ||
    osb_die "could not cross-compile the upstream suite for linux/$arch"
  osb_fc_put "$OSB_WORK/e2e.test" "$OSB_FC_DIR/e2e.test"

  envs="$(env | grep -E '^(OPENSANDBOX_|OPEN_SANDBOX_|RUN_CODE_INTERPRETER_E2E=)' | sed "s/'/'\\\\''/g; s/=\(.*\)/='\1'/" | tr '\n' ' ')"

  # As the unprivileged user where it runs: the suite is a client, and needs nothing of root's.
  colima ssh -p "$OSB_VM" -- bash -c "cd $OSB_FC_DIR && env $envs ./e2e.test -test.v=test2json -test.count=1 \
    -test.timeout $3 -test.run '$2'" | go tool test2json -t -p e2e
  return "${PIPESTATUS[0]}"
}

# osb_fc_teardown - the daemon, then any osb-* VM and pvc volume it left: only ones this run made.
osb_fc_teardown() {
  local ids
  # By its command line, which names this run's directory: setsid forks, so $! is not the daemon.
  osb_fc_sh "pkill -TERM -f '^$OSB_FC_DIR/sbx serve'; for i in \$(seq 1 60); do pgrep -f '^$OSB_FC_DIR/sbx serve' >/dev/null || break; sleep 0.5; done; pkill -KILL -f '^$OSB_FC_DIR/sbx serve'; true"

  ids="$(osb_fc_sh "env $(osb_fc_sbx_env) $OSB_FC_DIR/sbx list 2>/dev/null | awk '/^osb-/ {print \$1}' | sort -u")"
  for n in $ids; do
    osb_say "removing leftover microVM sandbox $n"
    osb_fc_sh "env $(osb_fc_sbx_env) $OSB_FC_DIR/sbx rm $n >/dev/null 2>&1" || osb_say "  could not remove $n"
  done

  for v in $(osb_fc_sh "ls $OSB_FC_STATE/volumes 2>/dev/null | grep '^sbx-osb-pvc-.*\.ext4\$' || true"); do
    case " $OSB_FC_VOLS_BEFORE " in *" $v "*) continue ;; esac
    osb_fc_sh "rm -f $OSB_FC_STATE/volumes/$v $OSB_FC_STATE/volumes/${v%.ext4}.json"
  done

  if [ "$OSB_KEEP_WORK" = 1 ] || [ "${1:-0}" -ne 0 ]; then
    osb_fc_sh "cat $OSB_FC_DIR/daemon.log" > "$OSB_WORK/daemon.log" 2>/dev/null
  fi

  osb_fc_sh "rm -rf $OSB_FC_DIR"
}
