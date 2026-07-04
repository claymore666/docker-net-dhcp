#!/usr/bin/env bash
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
  if printf '%s\n' "$out" | grep -qE "$2"; then
    :
  else
    echo "FAIL: $1"; fail=1
  fi
}
wantnot() { # description, pattern
  if printf '%s\n' "$out" | grep -qE "$2"; then
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

if [ "$fail" -eq 0 ]; then
  echo "PASS: integration-timing.sh"
else
  echo "SOME TESTS FAILED"
fi
exit "$fail"
