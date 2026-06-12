#!/usr/bin/env bash
# Regression test for the actionlint gate (#127): assert actionlint
# still flags the known-bad fixture, in particular the v0.8.0 trap
# (secrets context in a step-level `if:`), which GitHub rejects at
# parse time and thereby silently disables every trigger of the
# workflow. If this script fails, the linter has lost the teeth we
# rely on — treat it like a failing unit test, not a flaky check.
#
# Usage: test-actionlint.sh [path-to-actionlint]
set -u

ACTIONLINT="${1:-actionlint}"
FIXTURE="$(dirname "$0")/testdata/bad-workflow.yml"
OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

if "$ACTIONLINT" -no-color "$FIXTURE" >"$OUT" 2>&1; then
    echo "FAIL: actionlint exited 0 on $FIXTURE — it must flag the fixture"
    exit 1
fi

if ! grep -q 'context "secrets" is not allowed' "$OUT"; then
    echo "FAIL: actionlint no longer flags 'secrets' in a step-level if: (the v0.8.0 trap)"
    echo "--- actionlint output:"
    cat "$OUT"
    exit 1
fi

echo "PASS: actionlint flags the known-bad fixture (including the v0.8.0 secrets-in-step-if trap)"
