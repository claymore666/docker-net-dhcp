#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-golden-fixture-name-keyed.sh.
#
# The gate has three verdicts and this drives all three against real
# trees rather than fixtures, because the thing it measures is a `go
# test` run and a golden file -- there is nothing meaningful to stub.
#
# The case that matters is the FAILING one. A gate that has only ever
# been run against a tree where it passes has one possible verdict, and
# a check with one possible verdict is indistinguishable from a check
# that always passes. So case 2 rebuilds the index-numbered fixture the
# gate exists to reject and requires exit 1.
set -uo pipefail

GATE="$(cd "$(dirname "$0")" && pwd)/check-golden-fixture-name-keyed.sh"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
check() {
    local name="$1" want="$2" root="$3"
    local got out
    out="$(bash "$GATE" "$root" 2>&1)"
    got=$?
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name (exit $got)"
    else
        echo "FAIL: $name -- want exit $want, got $got"
        printf '%s\n' "$out" | sed 's/^/    /'
        failures=$((failures + 1))
    fi
}

# A working copy the cases can damage. Only the Go module is needed;
# the gate builds pkg/plugin and reads its testdata.
copy_tree() {
    local dst="$1"
    mkdir -p "$dst"
    ( cd "$ROOT" && git ls-files -z ) \
        | ( cd "$ROOT" && xargs -0 tar -cf - ) \
        | ( cd "$dst" && tar -xf - )
}

# --- case 1: the real tree, name-keyed, passes --------------------------
check "name-keyed fixture passes" 0 "$ROOT"

# --- case 2: index-keyed fixture is REJECTED ----------------------------
#
# The defect itself, rebuilt: values that descend with field index. This
# is the verdict the gate exists to render, and without this case the
# suite would prove only that the gate can say yes.
INDEX="$TMP/index"
copy_tree "$INDEX"
python3 - "$INDEX/pkg/plugin/metrics_test.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = "f.SetInt(fixtureValue(v.Type().Field(i).Name))"
new = "f.SetInt(int64((n - i) * 10))"
assert s.count(old) == 1, "anchor for the index-numbering mutant not found"
open(p, "w").write(s.replace(old, new, 1))
PY
# The mutant must be present, or case 2 measures the tree it was meant
# to damage and reports a PASS that means nothing.
grep -q '(n - i) \* 10' "$INDEX/pkg/plugin/metrics_test.go" \
    || { echo "FAIL: the index-numbering mutant did not apply"; failures=$((failures + 1)); }
# Regenerate so the tree is self-consistent under the mutant: the gate
# must fail on the COUPLING, not on a golden that was merely stale.
( cd "$INDEX" && UPDATE_GOLDEN=1 go test ./pkg/plugin/ -run TestMetrics_GoldenExposition -count=1 ) >/dev/null 2>&1
check "index-keyed fixture is rejected" 1 "$INDEX"

# --- case 3: a tree it cannot judge REFUSES -----------------------------
#
# Not a pass and not a failure. An absent HealthResponse means the gate
# measured nothing, and a gate that reports success over nothing is the
# failure mode the whole lane exists to remove.
MISSING="$TMP/missing"
copy_tree "$MISSING"
rm -f "$MISSING/pkg/plugin/endpoints.go"
check "a tree with no HealthResponse refuses" 2 "$MISSING"

# --- case 4: a tree whose regeneration cannot run REFUSES ---------------
#
# The positive control inside the gate. If `go test` does not run, the
# golden never changes, the diff is empty, and a gate without this guard
# reports PASS -- a measurement that never ran, reported as a result.
BROKEN="$TMP/broken"
copy_tree "$BROKEN"
printf '\nfunc zzSyntaxError( {\n' >> "$BROKEN/pkg/plugin/metrics_test.go"
check "a tree that cannot build refuses" 2 "$BROKEN"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures case(s) failed"
    exit 1
fi
echo "all cases passed"
