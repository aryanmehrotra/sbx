#!/usr/bin/env bash
# The OpenSandbox-API use cases, end to end: what a user or an agent actually does with
# `sbx serve --osb-addr`, against a real daemon on a real engine.
#
#   scripts/osb-usecases-e2e.sh [--docker-host URL] [--keep-logs] [FILTER]
#
#   FILTER    run only the cases whose name contains it, e.g. "egress" or "fork"
#
# scripts/osb-conformance.sh proves each call answers the way upstream's tests expect. This
# proves the SEQUENCES come out right - write, snapshot, fork, read back; freeze, wake, find the
# process still counting; renew, restart the daemon, get reaped on time - and that the classic
# sandbox.json world still behaves beside it. Every case asserts on something a caller can
# observe, removes only what it made, and prints PASS or FAIL; the script exits non-zero on any
# FAIL. The Go half is test/osb/cmd/osbuse.
#
# Like the conformance run it wants an engine with no sbx sandboxes on it, because a daemon
# fronts and can sleep every sandbox in its scope (see scripts/lib/osb.sh). It waits up to
# SBX_OSB_WAIT seconds (default 900) for one to become free rather than refusing at once, since
# the usual occupant is another harness run that will finish.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/osb.sh
. "$ROOT/scripts/lib/osb.sh"

DOCKER_URL=""
FILTER=""
IDLE_SHORT="15s"
TAG="osbuc$$"

usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --docker-host) DOCKER_URL="${2:?--docker-host needs a URL}"; shift 2 ;;
    --keep-logs)   OSB_KEEP_WORK=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    -*)            usage >&2; osb_die "unknown argument '$1'" ;;
    *)             FILTER="$1"; shift ;;
  esac
done

want() { case "$1" in *"$FILTER"*) return 0 ;; *) return 1 ;; esac; }

PASSED=""; FAILED=""; SKIPPED=""
pass() { PASSED="$PASSED $1"; printf '  PASS %s\n' "$1"; }
fail() { FAILED="$FAILED $1"; printf '  FAIL %s%s\n' "$1" "${2:+ - $2}"; }
skip() { SKIPPED="$SKIPPED $1"; printf '  SKIP %s - %s\n' "$1" "$2"; }
head_() { printf '\n== %s: %s\n' "$1" "$2"; }

# use CASE [osbuse flags...] - run one Go case and grade it on its exit status alone.
use() {
  local c=$1 others; shift
  others="$(foreign_daemons)"
  [ -z "$others" ] || printf '    WARN another sbx serve with an API is up (pid %s): it fronts this engine too, so this case may be disturbed\n' "$(printf '%s' "$others" | tr '\n' ' ')"
  # DOCKER_HOST explicitly: osbuse inspects containers, and the library exports the engine
  # only inside the daemon's own subshell.
  DOCKER_HOST="$OSB_DOCKER_HOST" "$OSBUSE" "$c" -url "$OSB_URL" -key "$OSB_KEY" "$@"
}

# The CLI as the daemon sees the world: same HOME, history file and engine.
sbxc() { osb_daemon_env "$OSB_SBX" "$@"; }

# Classic sandbox.json sandboxes this run made, removed on exit - before the library's own
# teardown, which only knows about osb-* names.
CLASSIC=""
cleanup_classic() {
  local n
  for n in $CLASSIC; do sbxc rm "$n" >/dev/null 2>&1; done
  CLASSIC=""
}

stop_daemon() {
  [ -n "$OSB_DAEMON" ] || return 0
  kill -TERM "$OSB_DAEMON" 2>/dev/null
  local n=0
  while kill -0 "$OSB_DAEMON" 2>/dev/null && [ "$n" -lt 40 ]; do sleep 0.25; n=$((n + 1)); done
  kill -KILL "$OSB_DAEMON" 2>/dev/null
  OSB_DAEMON=""
}

# restart_daemon IDLE SCOPE - the same address and key, so an SDK client holding the URL
# carries on, which is part of what "survives a restart" means.
restart_daemon() {
  local idle=$1 scope=$2 n=0
  stop_daemon
  set -- serve --osb-addr "${OSB_URL#http://}" --osb-key "$OSB_KEY" \
    --osb-host-paths "$OPENSANDBOX_TEST_HOST_VOLUME_DIR" --idle "$idle" --only "$scope"
  ( osb_daemon_vars; exec "$OSB_SBX" "$@" ) >>"$OSB_WORK/daemon.log" 2>&1 &
  OSB_DAEMON=$!

  until "$OSB_HARNESS" ready -url "$OSB_URL" -key "$OSB_KEY" -timeout 1s >/dev/null 2>&1; do
    n=$((n + 1))
    if [ "$n" -ge 60 ] || ! kill -0 "$OSB_DAEMON" 2>/dev/null; then
      osb_die "restarted daemon never answered; log: $OSB_WORK/daemon.log"
    fi
  done
}

# Anything else driving this engine would make a failure here meaningless, so each case checks
# for a foreign `sbx serve` and says so rather than blaming the product.
foreign_daemons() {
  ps -axo pid=,command= | awk -v me="$OSB_DAEMON" '/ serve / && /--osb-addr/ && $1 != me {print $1}'
}

osb_init
# After osb_init, which sets its own EXIT trap: this one must replace it, not be replaced by it.
# The status is carried across cleanup_classic: osb_teardown reads $? and exits with it.
trap 'rc=$?; cleanup_classic; (exit "$rc"); osb_teardown' EXIT
osb_resolve_docker "${DOCKER_URL:-}"
osb_build_tools

OSBUSE="$OSB_WORK/osbuse"
(cd "$ROOT/test/osb" && GOWORK=off go build -o "$OSBUSE" ./cmd/osbuse) || osb_die "could not build test/osb/cmd/osbuse"

waited=0
limit="${SBX_OSB_WAIT:-900}"
while [ -n "$(osb_sbx_sandboxes)" ] || [ -n "$(foreign_daemons)" ]; do
  [ "$waited" -ge "$limit" ] && break   # osb_start_daemon refuses and names them
  [ "$waited" = 0 ] && osb_say "engine $OSB_DOCKER_HOST is busy (sandboxes, or another API daemon); waiting up to ${limit}s"
  sleep 10; waited=$((waited + 10))
done

# Scoped from the start: osb- is every API sandbox, $TAG- the classic ones this run makes.
export SBX_ONLY="osb-,$TAG-"
OSB_IDLE="10m"
osb_start_daemon
SCOPE="$SBX_ONLY"

echo
echo "sbx - OpenSandbox API use cases (daemon $OSB_URL, engine $OSB_DOCKER_HOST)"
echo "================================================================"

# ── 1. an agent that only has MCP ─────────────────────────────────────────────
if want mcp; then
  head_ mcp "sbx mcp over stdio: create, run, write, read, search, kill"
  if use mcp -sbx "$OSB_SBX"; then pass mcp; else fail mcp; fi
fi

# ── 2. a coding agent's loop through the SDK ──────────────────────────────────
if want coding; then
  head_ coding "write a .py, run it, stream it, background, logs, interrupt"
  if use coding; then pass coding; else fail coding; fi
fi

# ── 3. the code interpreter ───────────────────────────────────────────────────
if want interpreter; then
  head_ interpreter "opensandbox/code-interpreter: state across cells, a plot, an interrupt"
  if ! DOCKER_HOST="$OSB_DOCKER_HOST" docker image inspect opensandbox/code-interpreter:latest >/dev/null 2>&1; then
    skip interpreter "opensandbox/code-interpreter:latest is not pulled on this engine (10 GB); pull it to run this"
  elif use interpreter -timeout 10m; then pass interpreter; else fail interpreter; fi
fi

# ── 5. pause and resume through the API ───────────────────────────────────────
if want pause; then
  head_ pause "a paused sandbox keeps its process, refuses traffic, and resumes"
  if use pause; then pass pause; else fail pause; fi
fi

# ── 7. egress, changed live ───────────────────────────────────────────────────
if want egress; then
  head_ egress "deny by default, allow one host, add another while it runs; sbx egress agrees"
  ok=1
  if use egress -out "$OSB_WORK/egress.id"; then
    id="$(cat "$OSB_WORK/egress.id")"
    shown="$(sbxc egress "$id" --show 2>&1)"
    printf '    sbx egress %s --show:\n%s\n' "$id" "$(printf '%s\n' "$shown" | sed 's/^/      /')"
    case "$shown" in *example.com*) ;; *) ok=0; echo "    FAIL sbx egress does not show example.com" ;; esac
    case "$shown" in *".example.org"*) ;; *) ok=0; echo "    FAIL sbx egress does not show the rule added through the API" ;; esac

    # And the other direction: a rule added with the CLI is what the API reports.
    if sbxc egress "$id" --allow api.github.com >/dev/null 2>&1 &&
      curl -sf -H "OPEN-SANDBOX-API-KEY: $OSB_KEY" "$OSB_URL/v1/sandboxes/$id/networkpolicy" | grep -q 'api.github.com'; then
      echo "    ok   a rule added with sbx egress shows up in GET networkpolicy"
    else
      ok=0; echo "    FAIL a rule added with sbx egress is not what the API reports"
    fi
    use kill -id "$id" >/dev/null || ok=0
  else
    ok=0
  fi
  if [ "$ok" = 1 ]; then pass egress; else fail egress; fi
fi

# ── 8. snapshot, then fork ────────────────────────────────────────────────────
if want fork; then
  head_ fork "write, snapshot, create from snapshotId; new token, old token refused"
  if use fork; then pass fork; else fail fork; fi
fi

# ── 9. volumes ────────────────────────────────────────────────────────────────
if want volumes; then
  head_ volumes "pvc shared across sandboxes, a host dir under --osb-host-paths, refusal outside"
  if use volumes -hostdir "$OPENSANDBOX_TEST_HOST_VOLUME_DIR" -outside "$OSB_REAL_HOME"; then pass volumes; else fail volumes; fi
fi

# ── 10. an interactive terminal ───────────────────────────────────────────────
if want pty; then
  head_ pty "PTY over WebSocket: type, stty size, resize, reconnect with since="
  if use pty; then pass pty; else fail pty; fi
fi

# ── 11. a web server inside, reached from outside ─────────────────────────────
if want proxy; then
  head_ proxy "python -m http.server 8000, reached through GetEndpoint(8000)"
  if use proxy; then pass proxy; else fail proxy; fi
fi

# ── 14. ten agents at once, and nothing left behind ───────────────────────────
if want concurrency; then
  head_ concurrency "10 parallel create -> run -> kill; no containers, volumes or state left"
  ok=1
  vols_before="$(DOCKER_HOST="$OSB_DOCKER_HOST" docker volume ls -q | sort)"
  use concurrent -n 10 -image alpine:3 -timeout 8m || ok=0

  left="$(osb_sbx_sandboxes | grep '^osb-')"
  [ -z "$left" ] && echo "    ok   no osb- container is left" || { ok=0; echo "    FAIL left behind: $left"; }

  new_vols="$(comm -13 <(printf '%s\n' "$vols_before") <(DOCKER_HOST="$OSB_DOCKER_HOST" docker volume ls -q | sort) | grep -v '^sbx-execd-')"
  [ -z "$new_vols" ] && echo "    ok   no volume is left" || { ok=0; echo "    FAIL volumes left: $new_vols"; }

  states=0
  for f in "$OSB_WORK"/home/.sbx/osb/osb-*; do [ -e "$f" ] && states=$((states + 1)); done
  [ "$states" = 0 ] && echo "    ok   no state file is left" || { ok=0; echo "    FAIL $states state file(s) left in $OSB_WORK/home/.sbx/osb"; }

  gc="$(sbxc gc 2>&1)"
  case "$gc" in
    "nothing to reclaim"*) echo "    ok   sbx gc: $gc" ;;
    *) ok=0; echo "    FAIL sbx gc offers to reclaim something:"; printf '%s\n' "$gc" | sed 's/^/      /' ;;
  esac
  if [ "$ok" = 1 ]; then pass concurrency; else fail concurrency; fi
fi

# ── everything below changes the daemon's flags ───────────────────────────────

# ── 4. the idle freeze ────────────────────────────────────────────────────────
if want idle; then
  head_ idle "idle freezes it with memory kept, a call wakes it; sbx.idle=sleep stops it"
  restart_daemon "$IDLE_SHORT" "$SCOPE"
  if use idle -idle "$IDLE_SHORT" -timeout 8m; then pass idle; else fail idle; fi
fi

# ── 12. a mixed fleet under one daemon ────────────────────────────────────────
if want mixed; then
  head_ mixed "a sandbox.json sandbox and API sandboxes under one daemon"
  restart_daemon "$IDLE_SHORT" "$SCOPE"
  ok=1
  pg="$TAG-pg"
  CLASSIC="$CLASSIC $pg"

  if sbxc create "$pg" --template postgres >/dev/null 2>&1; then
    echo "    ok   created $pg from the postgres template"
  else
    ok=0; echo "    FAIL could not create $pg"
  fi

  use create -out "$OSB_WORK/mixed.id" >/dev/null || ok=0
  api="$(cat "$OSB_WORK/mixed.id" 2>/dev/null)"

  # Evaluated, not scraped: the value is quoted, and eval is exactly what a user does with it.
  port="$(eval "$(sbxc env "$pg" --template postgres 2>/dev/null)"; printf '%s' "${DATABASE_PORT:-}")"

  # Asleep means no container running - 0 B, not a paused process.
  n=0
  while DOCKER_HOST="$OSB_DOCKER_HOST" docker ps --filter "label=sbx.sandbox=$pg" -q | grep -q . && [ "$n" -lt 90 ]; do
    sleep 2; n=$((n + 2))
  done
  if DOCKER_HOST="$OSB_DOCKER_HOST" docker ps --filter "label=sbx.sandbox=$pg" -q | grep -q .; then
    ok=0; echo "    FAIL $pg did not sleep within 90s of --idle $IDLE_SHORT"
  else
    echo "    ok   $pg slept to 0 B (no running container) after ${n}s"
  fi

  # A real Postgres client's first bytes: SSLRequest. The answer ('N' or 'S') can only come
  # from a running postgres, so getting it on the first connection is the wake-on-connect.
  reply="$( { exec 3<>"/dev/tcp/127.0.0.1/${port:-0}" && printf '\000\000\000\010\004\322\026\057' >&3 && head -c1 <&3; } 2>/dev/null)"
  case "$reply" in
    N|S) echo "    ok   the first connection to :$port was held and answered by postgres ('$reply')" ;;
    *)   ok=0; echo "    FAIL connecting to :${port:-?} got '${reply}' instead of a postgres reply" ;;
  esac

  lst="$(sbxc list 2>&1)"
  case "$lst" in *"$pg"*"$api"*|*"$api"*"$pg"*) echo "    ok   sbx list shows both" ;; *) ok=0; echo "    FAIL sbx list is missing one:"; printf '%s\n' "$lst" | sed 's/^/      /' ;; esac

  lj="$(sbxc list --json 2>&1)"
  case "$lj" in *"\"$pg\""*) case "$lj" in *"\"$api\""*) echo "    ok   sbx list --json shows both" ;; *) ok=0; echo "    FAIL list --json lacks $api" ;; esac ;; *) ok=0; echo "    FAIL list --json lacks $pg" ;; esac

  ui="$(sbxc ui </dev/null 2>&1 | cat)"
  case "$ui" in *"$pg"*) case "$ui" in *"$api"*) echo "    ok   sbx ui (no terminal) shows both" ;; *) ok=0; echo "    FAIL sbx ui lacks $api" ;; esac ;; *) ok=0; echo "    FAIL sbx ui lacks $pg: $(printf '%s' "$ui" | head -3)" ;; esac

  hist="$(sbxc history --json 2>&1)"
  case "$hist" in *"\"$pg\""*) case "$hist" in *"\"$api\""*) echo "    ok   sbx history has both" ;; *) ok=0; echo "    FAIL history lacks $api" ;; esac ;; *) ok=0; echo "    FAIL history lacks $pg" ;; esac

  if sbxc rm "$pg" >/dev/null 2>&1; then
    CLASSIC=""
    use poke -id "$api" >/dev/null && echo "    ok   after sbx rm $pg, $api still answers" || { ok=0; echo "    FAIL removing $pg broke $api"; }
  else
    ok=0; echo "    FAIL sbx rm $pg"
  fi
  use kill -id "$api" >/dev/null || ok=0
  if [ "$ok" = 1 ]; then pass mixed; else fail mixed; fi
fi

# ── 6. expiry, across a daemon restart ────────────────────────────────────────
if want expiry; then
  head_ expiry "timeout=60, renew, restart the daemon, reaped on time, recorded as expiry"
  restart_daemon 10m "$SCOPE"
  ok=1
  if use expiry-make -out "$OSB_WORK/expiry.id"; then
    rec="$(cat "$OSB_WORK/expiry.id")"
    id="${rec%% *}"

    # SIGKILL: the state has to be on disk already, not flushed on a clean exit.
    kill -KILL "$OSB_DAEMON" 2>/dev/null; OSB_DAEMON=""
    restart_daemon 10m "$SCOPE"
    echo "    ok   daemon killed with SIGKILL and restarted"

    use expiry-gone -id "$rec" -within 60s -timeout 5m || ok=0

    h="$(sbxc history "$id" --json 2>&1)"
    if printf '%s\n' "$h" | grep '"removed"' | grep -q '"actor":"expiry"'; then
      echo "    ok   sbx history records the removal with actor expiry"
    else
      ok=0; echo "    FAIL no removed/expiry record in history:"; printf '%s\n' "$h" | tail -5 | sed 's/^/      /'
    fi
  else
    ok=0
  fi
  if [ "$ok" = 1 ]; then pass expiry; else fail expiry; fi
fi

# ── 13. a scoped daemon leaves everything else alone ──────────────────────────
if want scope; then
  head_ scope "an --only osb- daemon never touches a sandbox outside its scope"
  ok=1
  other="$TAG-other"
  CLASSIC="$CLASSIC $other"
  spec="$OSB_WORK/other.json"
  printf '%s\n' '{"version":1,"services":{"redis":{"image":"redis:7-alpine","ports":[6379],"health":"redis-cli ping"}}}' >"$spec"

  if ! sbxc create "$other" --spec "$spec" >/dev/null 2>&1; then
    ok=0; echo "    FAIL could not create $other"
  fi
  cname="sbx-$other-redis"
  before="$(DOCKER_HOST="$OSB_DOCKER_HOST" docker inspect -f '{{.State.Status}} {{.State.Paused}} {{.State.StartedAt}}' "$cname" 2>&1)"

  # The narrowest scope and the shortest idle: if it were going to touch $other, this is where.
  restart_daemon 5s "osb-"
  use create -out "$OSB_WORK/scope.id" >/dev/null || ok=0
  sid="$(cat "$OSB_WORK/scope.id" 2>/dev/null)"
  sleep 20                                  # four idle windows
  use poke -id "$sid" >/dev/null || ok=0    # a freeze and a wake inside the scope
  use kill -id "$sid" >/dev/null || ok=0
  sleep 10

  after="$(DOCKER_HOST="$OSB_DOCKER_HOST" docker inspect -f '{{.State.Status}} {{.State.Paused}} {{.State.StartedAt}}' "$cname" 2>&1)"
  if [ "$before" = "$after" ] && [ "${before%% *}" = running ]; then
    echo "    ok   $other is untouched: $after"
  else
    ok=0; echo "    FAIL $other changed: '$before' -> '$after'"
  fi

  if sbxc history "$other" --json 2>/dev/null | grep -Eq '"event":"(sleep|slept|wake|woke|stopped|paused)"'; then
    ok=0; echo "    FAIL the scoped daemon recorded a wake or sleep of $other"
  else
    echo "    ok   history has no wake or sleep of $other"
  fi
  restart_daemon 10m "$SCOPE"
  if [ "$ok" = 1 ]; then pass scope; else fail scope; fi
fi

# ── 15. the warm pool ─────────────────────────────────────────────────────────
if want pool; then
  head_ pool "--osb-pool: a create served from a warm container"
  if "$OSB_SBX" serve --help 2>&1 | grep -q -- '--osb-pool'; then
    fail pool "this sbx has --osb-pool but no case exercises it yet; add one"
  else
    skip pool "this sbx has no --osb-pool (it lands in a later merge); rerun after it does"
  fi
fi

[ -z "$(foreign_daemons)" ] || echo "note: another sbx serve was running during this run: $(foreign_daemons | tr '\n' ' ')"

# Word splitting is the point: each list is space-separated names.
# shellcheck disable=SC2086
count() { set -- $1; echo $#; }
echo
echo "================================================================"
printf 'passed %s  failed %s  skipped %s\n' "$(count "$PASSED")" "$(count "$FAILED")" "$(count "$SKIPPED")"
[ -z "$FAILED" ] || { printf 'FAILED:%s\n' "$FAILED"; exit 1; }
exit 0
