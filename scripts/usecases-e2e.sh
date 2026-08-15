#!/usr/bin/env bash
# Every documented use case, exercised end to end.
#
#   scripts/usecases-e2e.sh            all
#   scripts/usecases-e2e.sh branches   only cases whose name contains "branches"
#
# This asserts that things WORK, never that they produce a particular value. A wake has to
# return a correct reply; it does not have to take 191 ms. A sandbox has to report its ports;
# they do not have to be 20002. Timings and addresses differ per machine, and a suite that
# pins them fails for reasons that have nothing to do with the software — which trains people
# to ignore it, which is worse than not having it.
#
# What it is for: usage, stability and reliability. Every case here is something the README
# tells someone to do, so a failure means the documentation is lying.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
FILTER="${1:-}"
TAG="uc$$"
WORK="$(mktemp -d)"

pass=0; fail=0; skipped=0
DAEMON=""

ok()   { pass=$((pass + 1));    printf '    ✓ %s\n' "$1"; }
bad()  { fail=$((fail + 1));    printf '    ✗ %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
skip() { skipped=$((skipped + 1)); printf '    – %s (%s)\n' "$1" "$2"; }
case_() { printf '\n  %s\n' "$1"; }

want() { case "$1" in *"$FILTER"*) return 0;; *) return 1;; esac; }

sandboxes() { "$SBX" list 2>/dev/null | awk -v t="$TAG" '$1 ~ t {print $1}' | sort -u; }

cleanup() {
  [ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
  for s in $(sandboxes); do "$SBX" rm "$s" >/dev/null 2>&1; done
  docker rmi -f "$(docker images -q "sbx-snap-$TAG-*" 2>/dev/null)" >/dev/null 2>&1
  docker volume rm "$(docker volume ls -q --filter "name=sbx-snapvol-$TAG" 2>/dev/null)" >/dev/null 2>&1
  docker network rm "sbx-noegress-$TAG-egress" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

[ -x "$SBX" ] || { echo "build first: go build -o sbx ." >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "these are end-to-end cases and need a docker daemon" >&2; exit 1; }

echo
echo "sbx — the documented use cases"
echo "=============================="

# ── the tool tells you what it can do ─────────────────────────────────────────
if want "doctor"; then
  case_ "doctor: before you rely on anything"

  out=$("$SBX" doctor 2>&1)
  echo "$out" | grep -q 'docker' && ok "reports the provider" || bad "no provider line" "$out"
  echo "$out" | grep -q 'isolation gvisor' && ok "reports the isolation tiers" || bad "no isolation line"

  # Machine-readable, because a script deciding whether to run should not scrape a table.
  "$SBX" doctor --json 2>/dev/null | python3 -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null \
    && ok "--json parses" || bad "--json is not valid JSON"
fi

# ── case 1: you, with branches ────────────────────────────────────────────────
if want "branches"; then
  case_ "branches: a sandbox per branch, from a committed spec"

  cp "$ROOT/examples/postgres/sandbox.json" "$WORK/sandbox.json"
  name="$TAG-branch"

  if "$SBX" create "$name" --spec "$WORK/sandbox.json" >/dev/null 2>&1; then
    ok "create from a spec file"
  else
    bad "create from a spec file"
  fi

  # exports, the seam that makes adoption cheap. The NAMES must be there; the values are
  # whatever this machine had free.
  env_out=$("$SBX" env "$name" --spec "$WORK/sandbox.json" 2>/dev/null)
  echo "$env_out" | grep -q 'DATABASE_PORT=' && ok "env exports the declared variable" \
    || bad "env has no DATABASE_PORT" "$env_out"
  echo "$env_out" | grep -q '^export ' && ok "env is shell-evalable" || bad "env is not shell syntax"

  eval "$env_out"
  [ -n "${DATABASE_PORT:-}" ] && ok "the port is a real value" || bad "DATABASE_PORT is empty"

  # The thing the whole product is: a client connects and it works.
  if docker exec "sbx-$name-postgres" psql -U app -d app -tAc 'select 1' 2>/dev/null | grep -q 1; then
    ok "the service actually serves"
  else
    bad "the service does not serve"
  fi

  "$SBX" list 2>/dev/null | grep -q "$name" && ok "list shows it" || bad "list does not show it"
fi

# ── case 2: an agent, mid-task ────────────────────────────────────────────────
if want "agent"; then
  case_ "agent: no spec on disk, machine-readable output, ad-hoc services"

  name="$TAG-agent"

  "$SBX" create "$name" --template postgres >/dev/null 2>&1 \
    && ok "create from a built-in template, nothing on disk" \
    || bad "create --template failed"

  # An agent parses this. If it is not JSON, the integration story is a lie.
  js=$("$SBX" env "$name" --template postgres --shell json 2>/dev/null)
  if echo "$js" | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get("DATABASE_PORT") else 1)' 2>/dev/null; then
    ok "--shell json is parseable and carries the port"
  else
    bad "--shell json is not usable" "$js"
  fi

  # The affordance that lets an agent add what the spec never declared.
  if "$SBX" add "$name" cache --image redis:7-alpine --port 6379 --health 'redis-cli ping' >/dev/null 2>&1; then
    ok "add a service mid-task"
    "$SBX" list 2>/dev/null | grep -q cache && ok "the added service is listed" || bad "added service missing from list"
  else
    bad "add failed"
  fi

  # exec, the way an agent runs anything inside.
  "$SBX" exec "$name" postgres psql -U app -d app -tAc 'select 1' 2>/dev/null | grep -q 1 \
    && ok "exec runs a command inside" || bad "exec failed"

  # exec -t must accept piped stdin rather than demanding a terminal.
  echo 'select 1;' | "$SBX" exec -t "$name" postgres psql -U app -d app -tA 2>/dev/null | grep -q 1 \
    && ok "exec -t passes stdin through" || bad "exec -t did not pass stdin"

  "$SBX" logs "$name" --tail 5 >/dev/null 2>&1 && ok "logs are readable" || bad "logs failed"

  echo "hello" > "$WORK/f.txt"
  if "$SBX" cp "$name" postgres "$WORK/f.txt" :/tmp/f.txt >/dev/null 2>&1; then
    "$SBX" exec "$name" postgres cat /tmp/f.txt 2>/dev/null | grep -q hello \
      && ok "cp puts a file inside" || bad "cp copied nothing"
  else
    bad "cp failed"
  fi
fi

# ── case 3: one seeded database, many agents ──────────────────────────────────
if want "fork"; then
  case_ "fork: seed once, hand every agent its own copy"

  seed="$TAG-seed"; a="$TAG-a"; b="$TAG-b"
  cp "$ROOT/examples/postgres/sandbox.json" "$WORK/fork.json"

  "$SBX" create "$seed" --spec "$WORK/fork.json" >/dev/null 2>&1
  "$SBX" exec "$seed" postgres psql -U app -d app \
    -c "create table t(v text); insert into t values ('seeded')" >/dev/null 2>&1

  "$SBX" snapshot "$seed" "$TAG" >/dev/null 2>&1 && ok "snapshot" || bad "snapshot failed"

  for s in "$a" "$b"; do
    "$SBX" fork "$TAG" "$s" --spec "$WORK/fork.json" >/dev/null 2>&1 \
      && ok "fork $s" || bad "fork $s failed"
  done

  for s in "$a" "$b"; do
    got=$("$SBX" exec "$s" postgres psql -U app -d app -tAc 'select v from t' 2>/dev/null | tr -d ' \n')
    [ "$got" = "seeded" ] && ok "$s carries the seeded data" || bad "$s lost the data" "got [$got]"
  done

  "$SBX" exec "$a" postgres psql -U app -d app -c "insert into t values ('a')" >/dev/null 2>&1
  ca=$("$SBX" exec "$a" postgres psql -U app -d app -tAc 'select count(*) from t' 2>/dev/null | tr -d ' \n')
  cb=$("$SBX" exec "$b" postgres psql -U app -d app -tAc 'select count(*) from t' 2>/dev/null | tr -d ' \n')
  [ "$ca" = "2" ] && [ "$cb" = "1" ] \
    && ok "the forks are independent" \
    || bad "the forks share state" "wrote to one: got $ca and $cb"
fi

# ── case 4: CI ────────────────────────────────────────────────────────────────
if want "ci"; then
  case_ "ci: create, wait until it really serves, run tests"

  name="$TAG-ci"
  "$SBX" create "$name" --template nginx >/dev/null 2>&1

  # The daemon fronts the ports env exports. Without it `ready` refuses, which is the whole
  # point of the check it grew: this suite is what proved the README's "no daemon needed"
  # wrong, so the suite should not repeat the mistake.
  "$SBX" serve --idle 10m --refresh 5s >/dev/null 2>&1 &
  DAEMON=$!
  sleep 3

  # ready takes no --spec and no --template: it works off the labels docker already carries.
  if "$SBX" ready "$name" >/dev/null 2>&1; then
    ok "ready returns"
    eval "$("$SBX" env "$name" --template nginx 2>/dev/null)"
    curl -sf -m 20 "http://127.0.0.1:${WEB_PORT:-0}/" >/dev/null 2>&1 \
      && ok "and it really is serving when ready returns" \
      || bad "ready returned before it served"
  else
    bad "ready failed"
  fi

  kill "$DAEMON" 2>/dev/null; DAEMON=""
fi

# ── the sleep/wake cycle, which is the product ────────────────────────────────
if want "wake"; then
  case_ "wake: sleeps when idle, wakes on a connection, keeps its data"

  name="$TAG-wake"
  "$SBX" create "$name" --template postgres >/dev/null 2>&1
  eval "$("$SBX" env "$name" --template postgres 2>/dev/null)"

  "$SBX" exec "$name" postgres psql -U app -d app \
    -c "create table w(v text); insert into w values ('survives')" >/dev/null 2>&1

  "$SBX" serve --idle 5s --refresh 2s >/dev/null 2>&1 &
  DAEMON=$!
  sleep 3

  waited=0
  until [ "$(docker inspect -f '{{.State.Running}}' "sbx-$name-postgres" 2>/dev/null)" != "true" ]; do
    sleep 2; waited=$((waited + 2))
    [ "$waited" -ge 60 ] && break
  done

  if [ "$(docker inspect -f '{{.State.Running}}' "sbx-$name-postgres" 2>/dev/null)" != "true" ]; then
    ok "sleeps to zero when nobody is using it"
  else
    bad "never slept in 60s"
  fi

  # Reliability, not speed: three cycles in a row have to behave the same way. A wake that
  # works once and not twice is the failure people actually hit.
  cycles_ok=0
  for _ in 1 2 3; do
    if docker run --rm --add-host=host.docker.internal:host-gateway -e PGPASSWORD=app \
         postgres:16-alpine psql -h host.docker.internal -p "$DATABASE_PORT" -U app -d app \
         -tAc 'select v from w' 2>/dev/null | grep -q survives; then
      cycles_ok=$((cycles_ok + 1))
    fi

    waited=0
    until [ "$(docker inspect -f '{{.State.Running}}' "sbx-$name-postgres" 2>/dev/null)" != "true" ]; do
      sleep 2; waited=$((waited + 2)); [ "$waited" -ge 40 ] && break
    done
  done

  [ "$cycles_ok" = "3" ] \
    && ok "wakes on a connection and keeps its data, three cycles running" \
    || bad "only $cycles_ok of 3 wake cycles served the stored row"

  kill "$DAEMON" 2>/dev/null; DAEMON=""
fi

# ── the regression this suite found ───────────────────────────────────────────
if want "reachable"; then
  case_ "ready: refuses to call a sandbox serving on an address nothing answers"

  name="$TAG-reach"
  "$SBX" create "$name" --template nginx >/dev/null 2>&1

  # No daemon. The services are healthy; the public ports have nothing behind them. Before
  # this check, ready printed "is serving" and CI got a port that accepted nothing.
  if "$SBX" ready "$name" >/dev/null 2>&1; then
    bad "ready reported serving with no daemon fronting the ports"
  else
    ok "ready refuses when nothing answers on the exported ports"
  fi
fi

# ── egress control ────────────────────────────────────────────────────────────
if want "egress"; then
  case_ "egress: denied service cannot reach out, and is still reachable"

  name="$TAG-egress"
  cat > "$WORK/egress.json" <<'JSON'
{
  "version": 1,
  "services": {
    "web": { "image": "nginx:alpine", "ports": [80],
             "health": "wget -qO- http://127.0.0.1/ >/dev/null", "egress": "deny" }
  },
  "exports": { "WEB_PORT": "web:80" }
}
JSON

  if "$SBX" create "$name" --spec "$WORK/egress.json" >/dev/null 2>&1; then
    ok "a service declaring egress deny creates"

    docker exec "sbx-$name-web" sh -c 'wget -q -T 5 -O /dev/null http://example.com' 2>/dev/null \
      && bad "it reached the internet — egress is not denied" \
      || ok "it cannot reach the internet"

    "$SBX" serve --idle 10m --refresh 5s >/dev/null 2>&1 &
    DAEMON=$!
    sleep 3

    eval "$("$SBX" env "$name" --spec "$WORK/egress.json" 2>/dev/null)"
    curl -sf -m 30 "http://127.0.0.1:${WEB_PORT:-0}/" >/dev/null 2>&1 \
      && ok "and it is still reachable, which the obvious implementations break" \
      || bad "denying egress also broke the way in"

    kill "$DAEMON" 2>/dev/null; DAEMON=""
  else
    bad "create with egress deny failed"
  fi
fi

# ── spec validation refuses rather than guesses ───────────────────────────────
if want "validation"; then
  case_ "validation: a typo fails loudly instead of silently doing nothing"

  cat > "$WORK/bad.json" <<'JSON'
{ "version": 1, "services": { "x": { "image": "nginx:alpine", "ports": [80], "egress": "den" } } }
JSON
  "$SBX" create "$TAG-bad" --spec "$WORK/bad.json" >/dev/null 2>&1 \
    && bad "a misspelled egress value was accepted" \
    || ok "a misspelled security control is refused"

  cat > "$WORK/bad2.json" <<'JSON'
{ "version": 1, "services": { "x": { "ports": [80] } } }
JSON
  "$SBX" create "$TAG-bad2" --spec "$WORK/bad2.json" >/dev/null 2>&1 \
    && bad "a service with no image was accepted" \
    || ok "a service with no image is refused"

  "$SBX" env "$TAG-does-not-exist" --template postgres >/dev/null 2>&1 \
    && bad "env answered for a sandbox that does not exist" \
    || ok "env refuses an unknown sandbox"
fi

# ── templates ─────────────────────────────────────────────────────────────────
if want "templates"; then
  case_ "templates: an agent can start one with nothing on disk"

  list=$("$SBX" templates 2>/dev/null)
  for t in postgres nginx browser analytics web-stack; do
    echo "$list" | grep -q "$t" && ok "template $t is offered" || bad "template $t is missing"
  done
fi

echo
echo "──────────────────────────────────────────────────────────"
printf '  %d passed · %d failed · %d skipped\n' "$pass" "$fail" "$skipped"
echo

[ "$fail" -eq 0 ]
