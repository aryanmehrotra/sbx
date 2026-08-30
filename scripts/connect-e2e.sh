#!/usr/bin/env bash
# `sbx connect`, end to end - the one surface reachable from off the machine.
#
#   scripts/connect-e2e.sh
#
# Everything else in scripts/ drives sandboxes on this machine. This drives the other shape:
# `sbx serve --connect-addr` fronting a port beside a workload, and `sbx connect` on the far
# side presenting it as a local one. It is the only part of sbx a stranger can send bytes to,
# and until now nothing ran it - the unit tests cover the frame parser and the allow-list, and
# the live proof was done once by hand against a deployment and never again.
#
# No cloud account and no container: the "deployment" is a second sbx on loopback, which is the
# same code path a platform runs. What it cannot cover is the layer 7 proxy in between; that
# claim is tested in Go, against a real httptest reverse proxy.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
WORK="$(mktemp -d)"

[ -x "$SBX" ] || { echo "connect-e2e: build first: go build -o sbx ." >&2; exit 1; }

# One daemon per machine, so this needs the machine to itself: the connect endpoint is served
# BY an `sbx serve`, and a second one refuses to start rather than fight the first for ports.
if pgrep -f "sbx serve" >/dev/null 2>&1; then
  echo "connect-e2e: an sbx serve is already running - stop it first, this needs to start its own" >&2
  exit 1
fi

pass=0; fail=0
ok()  { pass=$((pass + 1)); printf '  ✓ %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  ✗ %s\n' "$1"; [ -n "${2:-}" ] && printf '      %s\n' "$2"; }

# The workload and the probes are one Go program: this repo's own rule is that a helper is Go,
# and it keeps the large-payload check honest - `nc` will not tell you how many bytes came back.
cat > "$WORK/helper.go" <<'GOEOF'
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
)

func main() {
	switch os.Args[1] {
	case "echo": // a workload on loopback, standing in for the database beside a deployed sbx
		ln, err := net.Listen("tcp", "127.0.0.1:"+os.Args[2])
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Println("ready")
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Prefix, then everything: a single Read returns one chunk, which would make
				// the large-payload check measure the reader rather than the tunnel.
				_, _ = c.Write([]byte("ECHO:"))
				_, _ = io.Copy(c, c)
			}(c)
		}

	case "probe": // one exchange through whatever port it is given
		c, err := net.Dial("tcp", "127.0.0.1:"+os.Args[2])
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		defer c.Close()
		if _, err := c.Write([]byte("hello")); err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		// Until the whole reply is in: the echo streams, so one Read can return just the
		// prefix and the check would be measuring the reader.
		got := make([]byte, 10)
		_, _ = io.ReadFull(c, got)
		fmt.Println(string(got))

	case "bulk": // more than one relay chunk, so the frame loop is exercised, not just a ping
		want, _ := strconv.Atoi(os.Args[3])
		c, err := net.Dial("tcp", "127.0.0.1:"+os.Args[2])
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		defer c.Close()
		go func() { _, _ = c.Write(make([]byte, want)) }()
		got, err := io.ReadAll(io.LimitReader(c, int64(want)+5))
		if err != nil && len(got) == 0 {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Println(len(got))
	}
}
GOEOF

( cd "$WORK" && go mod init helper >/dev/null 2>&1 && go build -o helper helper.go ) \
  || { echo "connect-e2e: could not build the helper" >&2; exit 1; }

WORKLOAD_PORT=19555
CONNECT_PORT=19556
LOCAL_PORT=24555   # 19555 + the offset below

"$WORK/helper" echo "$WORKLOAD_PORT" >"$WORK/echo.log" 2>&1 &
ECHO_PID=$!

export SBX_CONNECT_TOKEN="connect-e2e-$$-secret"

# Loopback needs no TLS waiver: the plaintext refusal exempts it already, which is itself worth
# knowing - a deployment on a real address refuses to start without TLS or an explicit waiver.
"$SBX" serve --connect-addr "127.0.0.1:$CONNECT_PORT" --front "echo=$WORKLOAD_PORT" \
  >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!

CLIENT_PID=""
cleanup() {
  [ -n "$CLIENT_PID" ] && kill "$CLIENT_PID" 2>/dev/null
  kill "$SERVE_PID" "$ECHO_PID" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

for _ in $(seq 1 40); do
  curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$CONNECT_PORT/healthz" && break
  sleep 0.25
done

echo
echo "── the endpoint, before anyone is let in ───────────────────"

code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$@"; }

[ "$(code "http://127.0.0.1:$CONNECT_PORT/healthz")" = "200" ] \
  && ok "healthz answers with no credential - a platform probe cannot carry one" \
  || bad "healthz did not answer" "$(tail -3 "$WORK/serve.log")"

[ "$(code "http://127.0.0.1:$CONNECT_PORT/v1/fleet")" = "401" ] \
  && ok "fleet with no token is refused" || bad "an unauthenticated fleet was served"

[ "$(code -H "Authorization: Bearer wrong-token" "http://127.0.0.1:$CONNECT_PORT/v1/fleet")" = "401" ] \
  && ok "fleet with the wrong token is refused" || bad "a wrong token was accepted"

# A browser can be made to open a WebSocket to any URL but cannot set a header on it, so a
# token accepted in the query string would reopen exactly the CSRF hole the design closed.
qs="$(code "http://127.0.0.1:$CONNECT_PORT/v1/fleet?token=$SBX_CONNECT_TOKEN")"
case "$qs" in
  4*) ok "a token in the query string is refused ($qs)" ;;
  *)  bad "the token was accepted in a query string" "got $qs" ;;
esac

curl -s --max-time 5 "http://127.0.0.1:$CONNECT_PORT/v1/fleet?token=$SBX_CONNECT_TOKEN" \
  | grep -qi "Authorization header" \
  && ok "and it says where the token belongs" || bad "the refusal does not explain itself"

body="$(curl -s --max-time 5 -H "Authorization: Bearer $SBX_CONNECT_TOKEN" \
  "http://127.0.0.1:$CONNECT_PORT/v1/fleet")"

echo "$body" | grep -q "$WORKLOAD_PORT" \
  && ok "fleet with the token names the fronted port" || bad "fleet body" "$body"

echo
echo "── and what it carries ─────────────────────────────────────"

"$SBX" connect "http://127.0.0.1:$CONNECT_PORT" --port-offset 5000 >"$WORK/connect.log" 2>&1 &
CLIENT_PID=$!

for _ in $(seq 1 40); do
  "$WORK/helper" probe "$LOCAL_PORT" >/dev/null 2>&1 && break
  sleep 0.25
done

grep -q "$LOCAL_PORT" "$WORK/connect.log" \
  && ok "connect opened the local port the offset asked for" \
  || bad "no local port map" "$(cat "$WORK/connect.log")"

got="$("$WORK/helper" probe "$LOCAL_PORT" 2>&1)"
[ "$got" = "ECHO:hello" ] \
  && ok "bytes crossed the tunnel and came back" || bad "round trip through the tunnel" "$got"

# One relay chunk is 32 KiB on the connect path. A payload under that never exercises the
# frame loop, which is where a hand-rolled RFC 6455 implementation would go wrong.
big="$("$WORK/helper" bulk "$LOCAL_PORT" 200000 2>&1)"
[ "$big" = "200005" ] \
  && ok "a payload larger than one relay chunk survives whole" \
  || bad "large payload came back as $big, want 200005"

echo
echo "───────────────────────────────────────────────────────────"
echo "  $pass passed, $fail failed"
[ "$fail" -eq 0 ]

# A half-close is deliberately not asserted here, and the reason is worth writing down.
#
# `write; shutdown(SHUT_WR); read` is an ordinary client shape - `nc -N`, an HTTP client sending
# Connection: close, a Go io.Copy pipeline - and it does not survive this tunnel. Measured
# against the echo workload: direct returns "ECHO:hello"; through `sbx connect` it returns ""
# with a nil error, the reply truncated to nothing and reported as a clean EOF.
#
# It is not a one-line bug. Both relays tear the whole tunnel down when their read side ends,
# because the wire has no way to say "my write side is finished" - a WebSocket close is
# bidirectional, and this protocol carries no half-close signal. Fixing it means a wire change on
# the one surface reachable from off the machine, plus version negotiation against deployments
# already running an older sbx. That is a feature, not a patch, so it is recorded rather than
# half-done: see docs/release-notes/v0.8.0.md.
