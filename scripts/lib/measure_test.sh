#!/usr/bin/env bash
# Tests for scripts/lib/measure.sh — the maths every published number goes through.
#
#   bash scripts/lib/measure_test.sh
#
# These run first and they run on fixed inputs, because a distribution function that is
# wrong by one index is not something you notice by looking at a benchmark table. Every
# expectation below was computed by hand from the sample set beside it.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$HERE/measure.sh"

PASS=0; FAIL=0

ok()  { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n       %s\n' "$1" "$2"; }

eq() { # name, want, got
  [ "$2" = "$3" ] && ok "$1" || bad "$1" "want [$2] got [$3]"
}

echo
echo "measure.sh"
echo "=========="

# ---------------------------------------------------------------- distribution ----
# 1..10, so every statistic is checkable by hand:
#   n=10  min=1  max=10  median=5.5  stdev=3 (sample)
#   p90 index = round(0.9 * 9) = 8 → xs[8] = 9
SAMPLES="1 2 3 4 5 6 7 8 9 10"

eq "n"      "10"  "$(printf '%s\n' $SAMPLES | measure_stat n)"
eq "min"    "1"   "$(printf '%s\n' $SAMPLES | measure_stat min)"
eq "max"    "10"  "$(printf '%s\n' $SAMPLES | measure_stat max)"
eq "median" "6"   "$(printf '%s\n' $SAMPLES | measure_stat median)"   # 5.5 → %.0f banker's → 6
eq "p90"    "9"   "$(printf '%s\n' $SAMPLES | measure_stat p90)"
eq "stdev"  "3"   "$(printf '%s\n' $SAMPLES | measure_stat stdev)"

# Order must not matter: the function sorts.
eq "unsorted input sorts" "9" "$(printf '%s\n' 10 3 1 9 5 7 2 8 4 6 | measure_stat p90)"

# A single sample has no spread. Reporting stdev 0 would imply we measured it.
eq "n=1 stdev is n/a" "n/a" "$(echo 5 | measure_stat stdev)"

# Empty input must not print a number. A benchmark that reports 0 for "no data"
# is the failure this whole plan is about.
eq "empty is n/a" "n/a" "$(printf '' | measure_stat median)"

# ------------------------------------------------------------------- pairing ----
# Paired differences, because p90 of an unpaired subtraction is undefined.
#   wake:     120 130 140 150
#   baseline:  20  30  30  50
#   diffs:    100 100 110 100  → median 100, max 110
PAIRS="120:20 130:30 140:30 150:50"

eq "paired median" "100" "$(printf '%s\n' $PAIRS | measure_pairs | measure_stat median)"
eq "paired max"    "110" "$(printf '%s\n' $PAIRS | measure_pairs | measure_stat max)"
eq "paired n"      "4"   "$(printf '%s\n' $PAIRS | measure_pairs | measure_stat n)"

# A negative delta is real data — the target answered faster than the baseline — and
# must survive rather than being clamped to zero, which would hide a broken baseline.
eq "negative delta survives" "-10" "$(echo '90:100' | measure_pairs)"

# A pair missing its baseline is not a pair. Dropping it silently would let a wake
# sample masquerade as a delta.
eq "unpaired sample is dropped" "1" "$(printf '%s\n' '120:20' '130' | measure_pairs | measure_stat n)"

# ------------------------------------------------------------- docker stats ----
# Real `docker stats --no-stream --format '{{.Name}} {{.MemUsage}}'` output.
STATS='sbx-bench-redis 4.523MiB / 7.653GiB
spike-nginx 12.4MiB / 7.653GiB
tiny 512KiB / 7.653GiB
big 1.5GiB / 7.653GiB'

# 4.523 × 1024 = 4631.552 → 4632. Rounded, not truncated: truncating a MiB figure
# throws away half a KiB per container, which is noise here and would not be at GiB.
eq "MiB parsed to KiB"  "4632"    "$(echo "$STATS" | measure_rss_kib sbx-bench-redis)"
eq "KiB parsed"         "512"     "$(echo "$STATS" | measure_rss_kib tiny)"
eq "GiB parsed"         "1572864" "$(echo "$STATS" | measure_rss_kib big)"

# A name that is not in the output is not 0 KiB of memory — it is an absent container,
# and reporting 0 would publish a rival's control plane as free.
eq "absent container is n/a" "n/a" "$(echo "$STATS" | measure_rss_kib not-running)"

# A prefix must not match a different container.
eq "no prefix matching" "n/a" "$(echo "$STATS" | measure_rss_kib sbx-bench)"

echo
echo "----------------------------------------"
printf 'passed %d · failed %d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
