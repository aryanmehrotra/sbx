#!/usr/bin/env bash
# Record the README's dashboard picture from a real run.
#
#   scripts/ui-shot.sh                 # record: run sbx ui, write docs/ui.svg + docs/ui.ansi
#   scripts/ui-shot.sh --render-only   # redraw docs/ui.svg from the committed capture
#
# Same rule as scripts/demo.sh: the picture is a recording, not a drawing. A hand-drawn
# dashboard drifts - it keeps a column that was renamed and a key that was rebound - and the
# one in the README is the first thing anyone sees. This drives the real binary against the
# real daemon in a pty and renders whatever came back.
#
# It waits before capturing, because the two graphs are the point and a frame taken at startup
# has nothing in them: usage history begins when the dashboard opens.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${OUT:-$ROOT/docs/ui.svg}"
ANSI="${OUT%.svg}.ansi"
ALT="The sbx dashboard: a table of every sandbox and service with its state, cpu and memory \
against the limit it is allowed, a detail block for the selected service showing its address, \
connect command and a trace of cpu and memory over time, a log of recent wake and sleep \
events, and the key hints along the bottom."

COLS="${COLS:-150}"
ROWS="${ROWS:-24}"
SETTLE="${SETTLE:-30}"

if [ "${1-}" = "--render-only" ]; then
  [ -f "$ANSI" ] || { echo "no capture at $ANSI - run without --render-only first" >&2; exit 1; }
  python3 "$ROOT/scripts/lib/render-ui.py" "$ANSI" "$OUT" "$ALT"
  exit $?
fi

SBX="${SBX:-$ROOT/sbx}"
[ -x "$SBX" ] || { echo "build it first: go build -o $SBX ." >&2; exit 1; }

echo "recording ${COLS}x${ROWS}, settling ${SETTLE}s so the graphs have something in them..."

python3 - "$SBX" "$ANSI" "$COLS" "$ROWS" "$SETTLE" <<'PY' || exit 1
import fcntl, os, pty, re, struct, subprocess, sys, termios, time

binary, out, cols, rows, settle = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), float(sys.argv[5])

master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))

# Its own process group in this session: a setsid() would orphan it, and the kernel discards
# stop signals sent to an orphaned process group.
p = subprocess.Popen([binary, "ui"], stdin=slave, stdout=slave, stderr=slave,
                     preexec_fn=os.setpgrp, close_fds=True)
os.close(slave)
os.set_blocking(master, False)

buf = b""


def drain(seconds):
    global buf
    end = time.time() + seconds
    while time.time() < end:
        try:
            chunk = os.read(master, 1 << 20)
            if chunk:
                buf += chunk
        except (BlockingIOError, OSError):
            pass
        time.sleep(0.02)


drain(2.0)
os.write(master, b"j")          # select the second row, which is usually something awake
drain(settle)

os.write(master, b"q")
time.sleep(0.5)
try:
    p.wait(timeout=5)
except Exception:
    p.kill()

# The last frame that is actually complete. Frames are delimited by the home-cursor sequence,
# and the final one is often still being written when the read stopped.
frames = buf.split(b"\x1b[H")
whole = [f for f in frames if f.count(b"\n") >= rows - 1]
frame = whole[-1] if whole else (frames[-1] if frames else buf)

with open(out, "wb") as f:
    f.write(frame)

print("captured %d bytes, %d lines" % (len(frame), frame.count(b"\n")))
PY

python3 "$ROOT/scripts/lib/render-ui.py" "$ANSI" "$OUT" "$ALT"
