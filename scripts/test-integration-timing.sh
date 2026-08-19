#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for integration-timing.sh: feeds synthetic
# `go test -v` output and asserts the summary picks top-level tests,
# ignores subtests, sorts by duration desc, totals correctly, and
# tolerates missing log files (it runs `if: always()`, after failures).
set -u

TOOL="$(dirname "$0")/integration-timing.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail=0

log="$TMP/a.log"
cat > "$log" <<'EOF'
=== RUN   TestAlpha
--- PASS: TestAlpha (10.00s)
=== RUN   TestBeta
    --- PASS: TestBeta/sub_one (0.50s)
--- PASS: TestBeta (3.00s)
=== RUN   TestGamma
--- FAIL: TestGamma (7.25s)
PASS
ok      example/pkg     20.300s
EOF

# Force the step-summary path off so the test never writes outside TMP.
out="$(GITHUB_STEP_SUMMARY="" "$TOOL" "$log")"

want() { # description, pattern
  if printf '%s\n' "$out" | grep -E "$2" >/dev/null; then
    :
  else
    echo "FAIL: $1"; fail=1
  fi
}
wantnot() { # description, pattern
  if printf '%s\n' "$out" | grep -E "$2" >/dev/null; then
    echo "FAIL: $1"; fail=1
  fi
}

# total = 10.00 + 3.00 + 7.25 = 20.25 -> %.0f -> 20
want "total sums top-level only"            'sum 20s'
want "count is 3 top-level tests"           '3 tests'
want "slowest row is TestAlpha at 10s"      '\| 1 \| 10\.00 \| PASS \| TestAlpha \|'
want "FAIL row is present"                  'FAIL \| TestGamma'
wantnot "subtests are excluded"             'sub_one'

# Missing files must not error (always() after a failed test step).
if "$TOOL" "$TMP/does-not-exist.log" >/dev/null 2>&1; then :; else
  echo "FAIL: missing log file errored"; fail=1
fi

# Step-summary file is appended to when GITHUB_STEP_SUMMARY is set.
sumfile="$TMP/summary.md"
GITHUB_STEP_SUMMARY="$sumfile" "$TOOL" "$log" >/dev/null
if grep -q 'integration test timing' "$sumfile" 2>/dev/null; then :; else
  echo "FAIL: step summary not written"; fail=1
fi

# --- per-phase aggregation (#368) -------------------------------------
#
# The log above deliberately has NO phase lines, and the first
# assertion here is that it therefore gets no phase table. That is the
# back-compat case and it is the one most likely to break silently: an
# empty table would read as "no time spent in phases", which is a
# different and false claim from "not instrumented".
wantnot "no phase table without PHASE lines" 'Phase totals'

# The container_remove rows exist to make the ordering assertion below
# capable of failing. With only the other three phases, ranking by sum
# and ranking by mean produce the SAME order, so an assertion on the
# order would pass against either implementation and prove nothing.
# container_remove is cheap-but-frequent (6 x 0.60 = 3.60, mean 0.60)
# and ip_acquisition is dearer-but-rarer (2 x ~1.15 = 2.30, mean 1.15),
# so the two rankings disagree about them specifically.
plog="$TMP/phases.log"
cat > "$plog" <<'EOF'
=== RUN   TestAlpha
    network.go:44: PHASE network_create 8.010s
    container.go:230: PHASE ip_acquisition 1.200s
    container.go:196: PHASE container_stop 0.150s
    container.go:201: PHASE container_remove 0.600s
    container.go:201: PHASE container_remove 0.600s
    container.go:201: PHASE container_remove 0.600s
--- PASS: TestAlpha (10.30s)
=== RUN   TestBeta
    network.go:44: PHASE network_create 0.900s
    container.go:230: PHASE ip_acquisition 1.100s
    container.go:201: PHASE container_remove 0.600s
    container.go:201: PHASE container_remove 0.600s
    container.go:201: PHASE container_remove 0.600s
--- PASS: TestBeta (2.00s)
EOF
out="$(GITHUB_STEP_SUMMARY="" "$TOOL" "$plog")"

want "phase table appears when PHASE lines exist"  'Phase totals'
# network_create: n=2, sum 8.010+0.900=8.91, mean 4.455->4.46, max 8.01
want "phase sum/mean/max are aggregated"           '\| network_create \| 2 \| 8\.91 \| 4\.46 \| 8\.01 \|'
# ip_acquisition: n=2, sum 2.30, mean 1.15, max 1.20
want "second phase aggregates independently"       '\| ip_acquisition \| 2 \| 2\.30 \| 1\.15 \| 1\.20 \|'
# A single-sample phase must still appear — dropping n=1 rows would
# hide exactly the rare-but-expensive phase worth finding.
want "single-sample phase is kept"                 '\| container_stop \| 1 \|'
# Ordering is by SUM, not mean: a cheap phase paid often outranks an
# expensive one paid rarely, because that is what is worth attacking.
# container_remove (sum 3.60, mean 0.60) must outrank ip_acquisition
# (sum 2.30, mean 1.15) — the pair chosen precisely because sorting by
# mean puts them the other way round.
if [ "$(printf '%s\n' "$out" | grep -n 'container_remove' | cut -d: -f1)" -lt \
     "$(printf '%s\n' "$out" | grep -n 'ip_acquisition' | cut -d: -f1)" ]; then :; else
  echo "FAIL: phase rows not ordered by sum desc (mean-ordering would swap these two)"; fail=1
fi
want "cheap-but-frequent phase aggregates"         '\| container_remove \| 6 \| 3\.60 \| 0\.60 \| 0\.60 \|'
want "phase span total is reported"                'Phase sum 15s across 11 spans'

# The per-test table must survive alongside the phase table — the two
# answer different questions and the phase work must not displace #276.
want "per-test table still present with phases"    '\| 1 \| 10\.30 \| PASS \| TestAlpha \|'

# Phase lines but no test result lines: the tool must not claim a
# clean parse. This is the shape of a log truncated mid-run.
olog="$TMP/orphan.log"
printf '    network.go:44: PHASE network_create 1.000s\n' > "$olog"
if "$TOOL" "$olog" >/dev/null 2>&1; then :; else
  echo "FAIL: phase-only log errored"; fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "PASS: integration-timing.sh"
else
  echo "SOME TESTS FAILED"
fi
exit "$fail"
