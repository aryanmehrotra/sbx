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

# ── a remote docker host ──────────────────────────────────────────────────────
if want "remote"; then
  case_ "remote: drive a docker daemon that is not this machine's default"

  # A socat container in front of the socket, which is a remote endpoint as far as sbx is
  # concerned: it dials tcp and never touches the local socket path.
  docker rm -f "$TAG-proxy" >/dev/null 2>&1
  if docker run -d --name "$TAG-proxy" -p 127.0.0.1:23751:2375 \
       -v /var/run/docker.sock:/var/run/docker.sock \
       alpine/socat tcp-listen:2375,fork,reuseaddr unix-connect:/var/run/docker.sock >/dev/null 2>&1; then
    sleep 3
    remote="tcp://127.0.0.1:23751"

    DOCKER_HOST="$remote" "$SBX" list >/dev/null 2>&1 \
      && ok "list works against a tcp endpoint" || bad "list failed over tcp"

    if DOCKER_HOST="$remote" "$SBX" create "$TAG-remote" --template nginx >/dev/null 2>&1; then
      ok "create works against a tcp endpoint"
      DOCKER_HOST="$remote" "$SBX" rm "$TAG-remote" >/dev/null 2>&1
    else
      bad "create failed over tcp"
    fi

    docker rm -f "$TAG-proxy" >/dev/null 2>&1
  else
    skip "tcp endpoint" "could not start the socat proxy"
  fi

  # The narrow part of the capability, which the docs must not oversell: TLS is refused
  # rather than silently dialled without client certificates.
  err=$(DOCKER_HOST=https://docker.example.com:2376 "$SBX" list 2>&1)
  echo "$err" | grep -qi 'TLS' \
    && ok "a TLS endpoint is refused with a reason, not attempted" \
    || bad "https was not refused clearly" "$err"
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

# ── building an image instead of pulling one ──────────────────────────────────
if want "build"; then
  case_ "build: your own Dockerfile, and an unchanged context does not rebuild"

  # The marker carries $TAG, so this run's context hashes to a tag no previous run produced.
  # Otherwise "the first create builds" quietly depends on cleanup having worked last time,
  # and the case passes or fails on leftover docker state rather than on the code.
  mkdir -p "$WORK/ctx"
  cat > "$WORK/ctx/Dockerfile" <<DOCKER
FROM nginx:alpine
RUN echo "marker-$TAG-v1" > /usr/share/nginx/html/index.html
DOCKER
  cat > "$WORK/build.json" <<JSON
{ "version": 1, "services": { "web": { "build": { "context": "ctx" }, "ports": [80],
  "health": "wget -qO- http://127.0.0.1/ >/dev/null" } }, "exports": { "WEB_PORT": "web:80" } }
JSON

  out=$("$SBX" create "$TAG-b1" --spec "$WORK/build.json" 2>&1)
  if echo "$out" | grep -q 'building'; then
    ok "the first create builds"
  else
    bad "the first create did not build" "$out"
  fi

  # The Dockerfile's own content has to be what is served, or something else was run.
  # Through the daemon, the way anyone actually reaches a sandbox.
  "$SBX" serve --idle 10m --refresh 5s >/dev/null 2>&1 &
  DAEMON=$!
  sleep 3

  eval "$("$SBX" env "$TAG-b1" --spec "$WORK/build.json" 2>/dev/null)"
  curl -sf -m 30 "http://127.0.0.1:${WEB_PORT:-0}/" 2>/dev/null | grep -q "marker-$TAG-v1" \
    && ok "it serves what the Dockerfile put there" \
    || bad "the built image did not serve its own content"

  # The whole point: same context, no build. Not "a faster build" — no build.
  "$SBX" rm "$TAG-b1" >/dev/null 2>&1
  out=$("$SBX" create "$TAG-b2" --spec "$WORK/build.json" 2>&1)
  echo "$out" | grep -q 'cached' && ok "an unchanged context is a cache hit" \
    || bad "an unchanged context rebuilt" "$out"

  # And the other half, or the cache would serve a stale image forever.
  sed -i.bak "s/marker-$TAG-v1/marker-$TAG-v2/" "$WORK/ctx/Dockerfile"
  "$SBX" rm "$TAG-b2" >/dev/null 2>&1
  out=$("$SBX" create "$TAG-b3" --spec "$WORK/build.json" 2>&1)
  echo "$out" | grep -q 'building' && ok "a changed context rebuilds" \
    || bad "a changed context was served from cache" "$out"

  # b3 was created after the daemon started, so give the daemon a refresh interval to notice
  # it. Unset first: a stale WEB_PORT from b1 would point at a sandbox that no longer exists
  # and fail for a reason that has nothing to do with the build.
  unset WEB_PORT
  sleep 7

  eval "$("$SBX" env "$TAG-b3" --spec "$WORK/build.json" 2>/dev/null)"
  curl -sf -m 30 "http://127.0.0.1:${WEB_PORT:-0}/" 2>/dev/null | grep -q "marker-$TAG-v2" \
    && ok "the rebuild is what gets served" \
    || bad "the rebuilt image did not serve the new content"

  kill "$DAEMON" 2>/dev/null; DAEMON=""

  cat > "$WORK/both.json" <<'JSON'
{ "version": 1, "services": { "web": { "image": "nginx:alpine", "build": { "context": "ctx" },
  "ports": [80] } }, "exports": { "WEB_PORT": "web:80" } }
JSON
  "$SBX" create "$TAG-b4" --spec "$WORK/both.json" >/dev/null 2>&1 \
    && bad "a service with both image and build was accepted" \
    || ok "image and build together are refused rather than ranked"

  for s in "$TAG-b1" "$TAG-b2" "$TAG-b3" "$TAG-b4"; do "$SBX" rm "$s" >/dev/null 2>&1; done
  # Guarded rather than quoted: rmi takes many ids, so the split is wanted, and "$(...)"
  # would pass one empty argument when there is nothing to remove. `xargs -r` is the other
  # answer and is GNU-only, which this suite also runs on macOS.
  built=$(docker images -q 'sbx-build-*' 2>/dev/null)
  # shellcheck disable=SC2086 # word splitting is the point: one argument per image id
  [ -n "$built" ] && docker rmi -f $built >/dev/null 2>&1
fi

# ── it remembers what a sandbox was made from ─────────────────────────────────
if want "remembers"; then
  case_ "remembers: name the spec once, not on every command"

  name="$TAG-mem"

  # Created with --template; every later command must work without repeating it, from a
  # directory that has no sandbox.json at all.
  if "$SBX" create "$name" --template postgres >/dev/null 2>&1; then
    ok "create with --template"

    ( cd "$WORK" && "$SBX" env "$name" 2>/dev/null | grep -q 'DATABASE_PORT=' ) \
      && ok "env with no flags, from a directory with no sandbox.json" \
      || bad "env could not resolve the spec it was created from"

    # The seed-and-fan-out lineage: the snapshot inherits it, and so does the fork.
    "$SBX" snapshot "$name" "$TAG-golden" >/dev/null 2>&1 && ok "snapshot" || bad "snapshot failed"

    if ( cd "$WORK" && "$SBX" fork "$TAG-golden" "$TAG-child" >/dev/null 2>&1 ); then
      ok "fork with no flags"

      ( cd "$WORK" && "$SBX" env "$TAG-child" 2>/dev/null | grep -q 'DATABASE_PORT=' ) \
        && ok "and the fork remembers it too" || bad "the fork did not inherit the spec"

      "$SBX" rm "$TAG-child" >/dev/null 2>&1
    else
      bad "fork with no flags failed"
    fi

    # An explicit flag still wins over what was recorded.
    "$SBX" env "$name" --template postgres 2>/dev/null | grep -q 'DATABASE_PORT=' \
      && ok "an explicit --template still wins" || bad "an explicit flag was ignored"

    "$SBX" rm "$name" >/dev/null 2>&1
  else
    bad "create --template failed"
  fi
fi

# ── validate, depends_on and ${VAR} ───────────────────────────────────────────
if want "spec"; then
  case_ "spec: check it without creating it, order it, and keep secrets out of git"

  mkdir -p "$WORK/dep"
  cat > "$WORK/dep/sandbox.json" <<'JSON'
{
  "version": 1,
  "services": {
    "api":      { "image": "nginx:alpine", "ports": [80], "depends_on": ["postgres"],
                  "health": "wget -qO- http://127.0.0.1/ >/dev/null" },
    "postgres": { "image": "postgres:16-alpine", "ports": [5432],
                  "env": { "POSTGRES_USER": "app", "POSTGRES_PASSWORD": "${UC_PW}", "POSTGRES_DB": "app" },
                  "health": "psql -U app -d app -c 'select 1'" }
  },
  "exports": { "API_PORT": "api:80", "DATABASE_PORT": "postgres:5432" }
}
JSON

  # A committed file that a pre-commit hook or a lint job can check, with no docker and
  # nothing created.
  UC_PW=x "$SBX" validate "$WORK/dep/sandbox.json" >/dev/null 2>&1 \
    && ok "validate accepts a good spec" || bad "validate rejected a valid spec"

  # The referenced variable is not set: this must refuse rather than start a database with
  # an empty password, which is a failure that looks like success.
  out=$("$SBX" validate "$WORK/dep/sandbox.json" 2>&1)
  echo "$out" | grep -q 'UC_PW' \
    && ok "an unset \${VAR} is refused, and named" || bad "an unset variable was accepted" "$out"

  # Order, not ports: postgres is created first even though "api" sorts before it.
  order=$(UC_PW=x "$SBX" validate "$WORK/dep/sandbox.json" 2>/dev/null | grep -nE '^  (api|postgres) ' | tr '\n' ' ')
  case "$order" in
    *postgres*api*) ok "depends_on creates the dependency first" ;;
    *) bad "creation order ignored depends_on" "$order" ;;
  esac

  if UC_PW=uc-secret "$SBX" create "$TAG-dep" --spec "$WORK/dep/sandbox.json" >/dev/null 2>&1; then
    ok "a spec with depends_on and \${VAR} creates"

    # The point of the indirection: the value is in the environment, never in the file.
    docker exec "sbx-$TAG-dep-postgres" printenv POSTGRES_PASSWORD 2>/dev/null | grep -q 'uc-secret' \
      && ok "the referenced value reached the container" || bad "the variable did not expand"

    grep -q 'uc-secret' "$WORK/dep/sandbox.json" \
      && bad "the secret is in the committed spec" || ok "and it is not in the spec file"

    "$SBX" rm "$TAG-dep" >/dev/null 2>&1
  else
    bad "create with depends_on failed"
  fi

  # A dependency naming something that does not exist is a rule that silently never applies,
  # which looks exactly like the race it was added to prevent.
  cat > "$WORK/badorder.json" <<'JSON'
{ "version": 1, "services": { "api": { "image": "nginx:alpine", "ports": [80],
  "depends_on": ["nope"] } }, "exports": { "API_PORT": "api:80" } }
JSON
  "$SBX" validate "$WORK/badorder.json" >/dev/null 2>&1 \
    && bad "a dependency on an undeclared service was accepted" \
    || ok "a dependency on an undeclared service is refused"
fi

# ── templates ─────────────────────────────────────────────────────────────────
if want "templates"; then
  case_ "templates: an agent can start one with nothing on disk"

  list=$("$SBX" templates 2>/dev/null)
  for t in postgres nginx browser analytics web-stack; do
    echo "$list" | grep -q "$t" && ok "template $t is offered" || bad "template $t is missing"
  done

  # Pinned, so the sandbox somebody creates today is the one CI tested — and dated, so the
  # staleness that pinning buys is visible rather than silent.
  echo "$list" | grep -qE 'refreshed [0-9]{4}-[0-9]{2}-[0-9]{2}' \
    && ok "templates say when their images were pinned" \
    || bad "no refresh date in sbx templates" "$list"

  bash "$ROOT/scripts/pin-templates.sh" --check >/dev/null 2>&1 \
    && ok "every template image is pinned by digest" \
    || bad "a template image is on a floating tag"
fi

# ── prewarm ───────────────────────────────────────────────────────────────────
if want "prewarm"; then
  case_ "prewarm: pull now, so the first create is not a download"

  # Everything is already present after the cases above, which is the case worth asserting:
  # a warm cache must pull nothing, or the CI step this exists for is not cheap.
  out=$("$SBX" prewarm 2>&1)
  echo "$out" | grep -qE '^[0-9]+ pulled, [0-9]+ already present' \
    && ok "reports what it pulled and what it skipped" || bad "no summary line" "$out"

  # A spec, not just the templates — the shape CI actually uses.
  out=$("$SBX" prewarm --spec "$ROOT/examples/postgres/sandbox.json" 2>&1)
  echo "$out" | grep -q 'postgres' && ok "--spec warms that spec's images" || bad "--spec warmed nothing" "$out"

  # Twice in a row must be free the second time, or it is not a cache.
  out=$("$SBX" prewarm --spec "$ROOT/examples/postgres/sandbox.json" 2>&1)
  echo "$out" | grep -q '^0 pulled' && ok "a warm cache pulls nothing" || bad "it re-pulled a present image" "$out"
fi

echo
echo "──────────────────────────────────────────────────────────"
printf '  %d passed · %d failed · %d skipped\n' "$pass" "$fail" "$skipped"
echo

[ "$fail" -eq 0 ]
