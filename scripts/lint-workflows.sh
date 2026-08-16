#!/usr/bin/env bash
# Parse every workflow before pushing one.
#
#   scripts/lint-workflows.sh
#
# A workflow with a syntax error does not fail a job - GitHub rejects the whole run in
# under a second, with no logs, and the previous run's green tick is still the last thing
# you see on the branch. That happened here: an embedded multi-line python block inside a
# `run: |` step ended the YAML scalar at its first column-0 line, and the failure was
# invisible until someone went looking for a job that never existed.
#
# Ruby ships with macOS and every Linux runner has it, so this needs nothing installed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0

for f in "$ROOT"/.github/workflows/*.yaml "$ROOT"/.github/workflows/*.yml; do
  [ -e "$f" ] || continue

  if out=$(ruby -ryaml -e '
    d = YAML.load_file(ARGV[0])
    abort("no jobs")     unless d.is_a?(Hash) && d["jobs"].is_a?(Hash)
    abort("no triggers") unless d["on"] || d[true]   # YAML turns a bare `on:` into true
    d["jobs"].each do |name, job|
      abort("job #{name}: no steps") unless job["steps"].is_a?(Array)
      job["steps"].each_with_index do |s, i|
        abort("job #{name} step #{i}: neither run nor uses") unless s["run"] || s["uses"]
      end
    end
    puts d["jobs"].keys.join(", ")
  ' "$f" 2>&1); then
    printf '  ✓ %-28s %s\n' "$(basename "$f")" "$out"
  else
    printf '  ✗ %-28s %s\n' "$(basename "$f")" "$out"
    fail=1
  fi
done

[ "$fail" -eq 0 ] || { echo; echo "a workflow will be rejected by GitHub before any job runs"; exit 1; }
