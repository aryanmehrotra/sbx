#!/usr/bin/env bash
# Measurement primitives shared by scripts/bench.sh and scripts/compare.sh.
#
# Computation and parsing only. Acquisition — running a client, reading `docker stats`,
# asking a cluster — belongs to the caller, because the moment this file knows what a
# contender is it has two reasons to change and drags cluster knowledge into bench.sh,
# which needs none of it.
#
#   . scripts/lib/measure.sh
#   printf '%s\n' 191 204 232 | measure_stat median      # 204
#   printf '%s\n' 120:20 130:30 | measure_pairs          # 100 100
#   docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' | measure_rss_kib NAME
#
# Every function answers `n/a` rather than a number when it has nothing to measure.
# A benchmark that prints 0 for "no data" is the failure mode this whole harness exists
# to prevent: 0 ms looks like a result, and it is the absence of one.

# measure_ms — wall clock in milliseconds. Shared so the two scripts cannot drift into
# timing the same thing two ways.
measure_ms() { python3 -c 'import time;print(int(time.time()*1000))'; }

# measure_conditions — what the machine was doing, printed before any result.
# A wake on an idle laptop and a wake on one that is paging are not the same
# measurement, and the numbers should say which.
# `extended` adds the host triple and docker version, which compare.sh needs to make a
# cross-contender run reproducible. bench.sh passes nothing and gets byte-identical output
# to what it printed before the extraction — the acceptance criterion was that its behaviour
# does not change, and "we improved it" is not the same thing as "unchanged".
measure_conditions() { # path to an sbx binary, [extended]
  local sbx="${1:-}" mode="${2:-}"
  echo "── conditions ──────────────────────────────────────────────"
  [ "$mode" = extended ] && printf '  host           %s\n' "$(uname -sm)"
  printf '  host load      %s\n' "$(uptime | sed 's/.*averages*: *//')"
  if command -v colima >/dev/null 2>&1; then
    printf '  vm memory      %s\n' \
      "$(colima ssh -- free -m 2>/dev/null | awk 'NR==2{print $3" MB used, "$4" MB free of "$2" MB"}')"
  fi
  if [ "$mode" = extended ] && command -v docker >/dev/null 2>&1; then
    printf '  docker         %s\n' "$(docker info --format '{{.ServerVersion}} · {{.Architecture}}' 2>/dev/null)"
  fi
  [ -n "$sbx" ] && [ -x "$sbx" ] && printf '  sbx            %s\n' "$("$sbx" version 2>/dev/null)"
  return 0
}

# measure_stat — one statistic from whitespace/newline separated numbers on stdin.
#   n · min · max · median · p90 · stdev
# Integers in, integers out. p90 uses the nearest-rank index on the sorted set, which is
# what bench.sh has always reported; changing it would silently move every published p90.
measure_stat() {
  local what="$1"
  python3 -c '
import sys, statistics
want = sys.argv[1]
xs = sorted(int(float(t)) for t in sys.stdin.read().split() if t.strip())
if not xs:
    print("n/a"); raise SystemExit(0)
def pct(p):
    return xs[min(len(xs) - 1, int(round((p / 100) * (len(xs) - 1))))]
if   want == "n":      print(len(xs))
elif want == "min":    print(xs[0])
elif want == "max":    print(xs[-1])
elif want == "median": print(f"{statistics.median(xs):.0f}")
elif want == "p90":    print(pct(90))
elif want == "stdev":  print(f"{statistics.stdev(xs):.0f}" if len(xs) > 1 else "n/a")
else:
    print("n/a")
' "$what"
}

# measure_pairs — paired differences from `wake:baseline` lines on stdin.
#
# Paired, not "median(wake) − median(baseline)": p90 and stdev of an unpaired subtraction
# are undefined, and for a containerised client the client's own variance sits in both
# terms and would dominate the spread of any unpaired delta.
#
# A negative difference is kept. It means the target answered faster than its own awake
# baseline, which is real data about a noisy baseline — clamping it to zero would hide
# exactly that. A line with no baseline is dropped: it is a sample, not a pair.
measure_pairs() {
  python3 -c '
import sys
for line in sys.stdin.read().split():
    if ":" not in line:
        continue
    wake, _, base = line.partition(":")
    try:
        print(int(float(wake)) - int(float(base)))
    except ValueError:
        continue
'
}

# measure_rss_kib — resident memory in KiB for one container, from
# `docker stats --no-stream --format '{{.Name}} {{.MemUsage}}'` on stdin.
#
# Exact name match. A prefix match would let `sbx-bench` silently report
# `sbx-bench-redis`, and an absent container answers `n/a` rather than 0 — a rival's
# control plane that is not running is not a control plane that is free.
measure_rss_kib() {
  python3 -c '
import re, sys
want = sys.argv[1]
UNITS = {"B": 1 / 1024, "KIB": 1, "KB": 1, "MIB": 1024, "MB": 1024,
         "GIB": 1024 ** 2, "GB": 1024 ** 2}
for line in sys.stdin:
    parts = line.split()
    if len(parts) < 2 or parts[0] != want:
        continue
    m = re.match(r"([0-9.]+)\s*([A-Za-z]+)", parts[1])
    if not m:
        print("n/a"); raise SystemExit(0)
    size, unit = m.group(1), m.group(2).upper()
    if unit not in UNITS:
        print("n/a"); raise SystemExit(0)
    print(f"{float(size) * UNITS[unit]:.0f}")
    raise SystemExit(0)
print("n/a")
' "$1"
}

# measure_overhead_verdict — is a measured delta bigger than the instrument?
#
#   measure_overhead_verdict <through_us> <floor_us> <jitter_us>
#
# `jitter` is the harness's own noise, measured direct-vs-direct: the same client against
# the same directly published target, twice. A delta smaller than that is not a small
# overhead, it is the measurement apparatus, and publishing it as a number would be a
# figure invented by the instrument. Prints either a number or the refusal.
measure_overhead_verdict() {
  python3 -c '
import sys
through, floor, jitter = (int(float(x)) for x in sys.argv[1:4])
d = through - floor
if abs(d) <= jitter:
    print(f"below harness resolution (delta {d}us, jitter +/-{jitter}us) - see proxy_bench_test.go")
else:
    print(f"{d}us/req over floor (jitter +/-{jitter}us)")
' "$1" "$2" "$3"
}
