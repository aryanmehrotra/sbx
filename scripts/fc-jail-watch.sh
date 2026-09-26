#!/usr/bin/env bash
# Watch every Firecracker VMM on this host while something else drives them, then say whether
# every one of them was jailed.
#
#   sudo scripts/fc-jail-watch.sh start OUT &   # samples until killed
#   sudo scripts/fc-jail-watch.sh check OUT     # exit 1 unless jailed VMMs were seen, and only those
#
# CI's microvm job runs the OpenSandbox conformance tier against `sbx serve --provider
# firecracker` with the jailer on (the default). The suite knows nothing of how a VMM runs, so
# this samples /proc beside it: each firecracker's uid and gid (never 0, one uid throughout, and
# never the same uid as another VMM in the same sweep - a uid is per VM, reused only by a later VM
# at the same address) and whether the host's /etc is visible from its root (it must not be - it
# is chrooted). As root: /proc/<pid>/root is readable by nobody else.
#
# A sample line is `<sweep> <pid> <jail|host> <uid x4> <gid x4>`; scripts/fc-jail-watch_test.sh
# holds check to its verdicts.
set -euo pipefail

cmd="${1:?start OUT | check OUT}"
out="${2:?start OUT | check OUT}"

case "$cmd" in
  start)
    : >"$out"
    sweep=0
    while :; do
      sweep=$((sweep + 1))
      for pid in $(pgrep -x firecracker || true); do
        ids="$(awk '/^Uid:|^Gid:/ { printf "%s %s %s %s ", $2, $3, $4, $5 }' "/proc/$pid/status" 2>/dev/null)" || continue
        [ -n "$ids" ] || continue
        if [ -e "/proc/$pid/root/etc/passwd" ]; then where=host; else where=jail; fi
        echo "$sweep $pid $where $ids" >>"$out"
      done
      sleep 1
    done
    ;;
  check)
    n="$(awk 'NF { print $2 }' "$out" | sort -u | wc -l | tr -d " ")"
    if [ "$n" -eq 0 ]; then
      echo "fc-jail-watch: no firecracker process was seen - nothing was verified" >&2
      exit 1
    fi
    bad="$(awk 'NF { if ($3 != "jail") { print; next } for (i = 4; i <= NF; i++) if ($i == 0 || $i != $4) { print; next } }' "$out" | sort -u)"
    if [ -n "$bad" ]; then
      echo "fc-jail-watch: a VMM ran unjailed, as root, or with mixed ids (sweep pid where uid*4 gid*4):" >&2
      echo "$bad" | head -20 >&2
      exit 1
    fi
    shared="$(awk 'NF { k = $1 " " $4; if ((k in pid) && pid[k] != $2) print "sweep " $1 ": uid " $4 " is pid " pid[k] " and pid " $2; else pid[k] = $2 }' "$out" | sort -u)"
    if [ -n "$shared" ]; then
      echo "fc-jail-watch: two VMMs ran as one uid at the same time - one could signal, trace or open the other's files:" >&2
      echo "$shared" | head -20 >&2
      exit 1
    fi
    echo "fc-jail-watch: $n VMMs seen, every one chrooted and running as a non-root uid no other running VMM had"
    ;;
  *)
    echo "usage: $0 start OUT | check OUT" >&2
    exit 2
    ;;
esac
