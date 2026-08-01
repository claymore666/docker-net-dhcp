#!/usr/bin/env bash
# integration-timing.sh — summarize per-test wall-clock from one or more
# `go test -v` logs. Prints the slowest tests + total to stdout and, when
# running under Actions, to the job's step summary.
#
# Informational only: it never fails the build (the test steps own
# pass/fail), so it is safe to wire with `if: always()`.
#
# Why this exists: the integration suite is ~20 min of mostly deliberate
# DHCP-protocol waits. This surfaces where the time actually goes — so
# speedup work (e.g. #253's renewal cut) can target the biggest waits
# without grepping verbose logs by hand. See test/integration/README.md.
#
# Usage: integration-timing.sh LOG [LOG...]
#   TOPN=N  cap the table to the N slowest tests (default 25).
set -u
export LC_ALL=C   # stable numeric parse/sort on the locale-set runner

TOPN="${TOPN:-25}"

# Top-level results only: subtests are indented (leading whitespace) and
# carry a '/' in the name, so anchoring at column 0 with a name class
# that excludes '/' drops them and avoids double-counting under parents.
rows="$(
  for f in "$@"; do
    [ -f "$f" ] || continue
    grep -aoE '^--- (PASS|FAIL|SKIP): [A-Za-z0-9_]+ \([0-9]+\.[0-9]+s\)' "$f" || true
  done \
    | sed -E 's/^--- (PASS|FAIL|SKIP): ([A-Za-z0-9_]+) \(([0-9.]+)s\)/\3 \1 \2/' \
    | sort -rn -k1,1
)"

if [ -z "$rows" ]; then
  echo "integration-timing: no test result lines found in: $*"
  exit 0
fi

total="$(printf '%s\n' "$rows" | awk '{s+=$1} END{printf "%.0f", s}')"
count="$(printf '%s\n' "$rows" | grep -c '')"

# Per-phase spans emitted by the harness (#368). The per-test table
# above says which tests are slow; this says which part of a test is.
#
# Absent lines are normal, not an error: logs predating the
# instrumentation have none, and so does any suite whose helpers are
# not instrumented. In that case the phase table is simply omitted —
# an empty table would read as "no time spent in phases", which is a
# different and false claim.
phase_rows="$(
  for f in "$@"; do
    [ -f "$f" ] || continue
    grep -aoE 'PHASE [a-z_]+ [0-9]+\.[0-9]+s' "$f" || true
  done \
    | sed -E 's/^PHASE ([a-z_]+) ([0-9.]+)s$/\1 \2/'
)"

emit() {
  printf 'Integration test timing — %s tests, sum %ss (~%s min)\n\n' \
    "$count" "$total" "$((total / 60))"
  printf '| # | secs | result | test |\n'
  printf '|--:|-----:|:------:|:-----|\n'
  printf '%s\n' "$rows" | head -n "$TOPN" \
    | awk '{printf "| %d | %s | %s | %s |\n", NR, $1, $2, $3}'

  [ -n "$phase_rows" ] || return 0

  printf '\nPhase totals — where the time inside those tests went\n\n'
  printf '| phase | n | sum s | mean s | max s |\n'
  printf '|:------|--:|------:|-------:|------:|\n'
  # Sum/mean/max per phase, heaviest total first. Sorting by sum rather
  # than mean is deliberate: a 0.2s phase paid 50 times is a bigger
  # target than a 3s phase paid twice, and the whole point is to rank
  # what is worth attacking.
  printf '%s\n' "$phase_rows" | awk '
    { n[$1]++; s[$1]+=$2; if ($2 > mx[$1]) mx[$1]=$2 }
    END { for (p in n) printf "%s %d %.2f %.2f %.2f\n", p, n[p], s[p], s[p]/n[p], mx[p] }
  ' | sort -rn -k3,3 \
    | awk '{printf "| %s | %d | %s | %s | %s |\n", $1, $2, $3, $4, $5}'

  printf '\nPhase sum %ss across %s spans.\n' \
    "$(printf '%s\n' "$phase_rows" | awk '{s+=$2} END{printf "%.0f", s}')" \
    "$(printf '%s\n' "$phase_rows" | grep -c '')"
}

emit
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  { printf '## ⏱ integration test timing\n\n'; emit; } >> "$GITHUB_STEP_SUMMARY"
fi
