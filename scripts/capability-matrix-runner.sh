#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Run ONE integration scenario against ONE installed plugin, for
# scripts/capability-matrix.sh (#690).
#
# Usage: capability-matrix-runner.sh <plugin-ref> <TestName>
#
# Exit: 0 the scenario passed
#       1 the scenario failed
#       2 the run did not produce a verdict
#
# THE THIRD EXIT IS THE POINT. `go test` exits 1 both for a failing
# assertion and for a package that would not build, a harness that could
# not reach the daemon, or a -run pattern that selected nothing -- and
# the caller records exit 1 as "this capability is required", which is
# the answer the whole job exists to find. So this reads the summary line
# rather than the exit code: a run that neither reports ok nor reports a
# failing test has not measured the scenario, whatever it exited with.
set -uo pipefail

REF="${1:-}"
TEST="${2:-}"
if [ -z "$REF" ] || [ -z "$TEST" ]; then
    echo "usage: $0 <plugin-ref> <TestName>" >&2
    exit 2
fi

OUT=$(mktemp); trap 'rm -f "$OUT"' EXIT

INTEGRATION_PLUGIN_REF="$REF" \
    go test -tags integration -count=1 -timeout 10m -v \
    -run "^${TEST}\$" ./test/integration/... > "$OUT" 2>&1
rc=$?

if grep -qE "^--- PASS: ${TEST}\b" "$OUT"; then
    exit 0
fi
if grep -qE "^--- (FAIL|SKIP): ${TEST}\b" "$OUT"; then
    # A SKIP is not a pass. A scenario that skips itself under one
    # capability set and runs under another would otherwise read as
    # "this capability is not needed".
    grep -qE "^--- SKIP: ${TEST}\b" "$OUT" && {
        echo "$TEST SKIPPED under $REF -- a skip is not a verdict:" >&2
        tail -20 "$OUT" >&2
        exit 2
    }
    exit 1
fi

echo "no verdict for $TEST under $REF (go test exited $rc, and the log carries neither a PASS nor a FAIL line for it):" >&2
tail -30 "$OUT" >&2
exit 2
