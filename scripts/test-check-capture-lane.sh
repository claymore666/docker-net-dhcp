#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-capture-lane.sh. The failure it guards is the one
# the drift gate structurally cannot see: move the fixture capture off
# the dhcp-ci lane and capture and check agree with each other while both
# describe a daemon the integration suite never talks to (#644).
#
# Every case below is a whole workflow rather than a fragment, because
# the check reads the file as text and a fragment would not exercise the
# comment stripping — which is load-bearing in BOTH directions here. The
# real workflow's prose names `ubuntu-latest` while forbidding it, so a
# check that read comments would fail on the very file it protects; and
# a workflow whose only mention of the lane is in a comment must not
# pass. Both are cases.
set -euo pipefail

CHECK="$(dirname "$0")/check-capture-lane.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

# Every case runs against a STUB scripts dir holding the drift gate the
# workflow references. That keeps this file testing the checker rather
# than the repository: whether scripts/check-fixture-engine-drift.sh is
# present here today is a fact about the tree, and asserting it from a
# self-test would mean editing this file the day it lands. The checker
# asserts it against the real tree when the lane runs it; that verdict
# corrects itself, this one would not.
mkdir -p "$TMP/scripts"
: > "$TMP/scripts/check-fixture-engine-drift.sh"
export SCRIPTS_DIR="$TMP/scripts"

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}
verdict() { bash "$CHECK" "$TMP/wf.yml" >/dev/null 2>&1 && echo pass || echo "rc$?"; }

# A minimal workflow with every property the check wants. The cases
# below each break exactly one of them.
write_wf() {
    local runs_on="$1" capture="$2" drift="$3" extra="${4-}"
    cat > "$TMP/wf.yml" <<YAML
name: Capture fixtures
on:
  workflow_dispatch:
jobs:
  capture:
    runs-on: ${runs_on}
    steps:
      - uses: actions/checkout@v7
      - name: Capture the fixtures
        run: ${capture}
      - name: Fixtures now describe this engine
        run: ${drift}
${extra}
YAML
}

LANE='[self-hosted, dhcp-ci]'
CAP='make capture-fixtures CAPTURE_COMMIT="$(git rev-parse --short HEAD)"'
DRIFT='bash scripts/check-fixture-engine-drift.sh'

# --- the shipped workflow is the primary case -------------------------
# Not a synthetic stand-in: the check exists to protect this exact file,
# and its rationale comments quote every literal the check rejects.
REAL="$(dirname "$0")/../.github/workflows/capture-fixtures.yml"
if [ -r "$REAL" ]; then
    real_verdict=$(bash "$CHECK" "$REAL" >/dev/null 2>&1 && echo pass || echo "rc$?")
    check "the shipped capture workflow passes" pass "$real_verdict"
else
    echo "FAIL: cannot read $REAL — the workflow this check protects is missing"
    fails=1
fi

# --- the defect: capture moved off the lane ---------------------------
write_wf "$LANE" "$CAP" "$DRIFT"
check "lane + capture + drift check passes" pass "$(verdict)"

write_wf 'ubuntu-latest' "$CAP" "$DRIFT"
check "a hosted runner fails" rc1 "$(verdict)"

write_wf '[self-hosted]' "$CAP" "$DRIFT"
check "self-hosted without the dhcp-ci label fails" rc1 "$(verdict)"

write_wf '[self-hosted, some-other-pool]' "$CAP" "$DRIFT"
check "a different self-hosted pool fails" rc1 "$(verdict)"

# --- the capture must still happen ------------------------------------
write_wf "$LANE" 'go test ./...' "$DRIFT"
check "a workflow that captures nothing fails" rc1 "$(verdict)"

# --- and must verify its own claim ------------------------------------
write_wf "$LANE" "$CAP" 'echo done'
check "dropping the post-capture drift check fails" rc1 "$(verdict)"

# --- comments are not content, in both directions ---------------------
# Forbidding a literal in prose must not trip the check...
write_wf "$LANE" "$CAP" "$DRIFT" '      # Never move this to ubuntu-latest.'
check "prose naming ubuntu-latest does not trip the check" pass "$(verdict)"

# ...and naming the lane in prose must not satisfy it.
write_wf 'ubuntu-latest' "$CAP" "$DRIFT" '      # This used to be [self-hosted, dhcp-ci].'
check "the lane named only in a comment does not satisfy the check" rc1 "$(verdict)"

# --- the referenced gate has to exist ---------------------------------
# Naming a script is not shipping one. A step that calls a missing file
# fails at dispatch time — on the one run somebody reaches for precisely
# when the fixtures are already known to be stale.
write_wf "$LANE" "$CAP" "$DRIFT"
SCRIPTS_DIR="$TMP/empty" check "a drift gate that does not exist fails" rc1 \
    "$(SCRIPTS_DIR="$TMP/empty" bash "$CHECK" "$TMP/wf.yml" >/dev/null 2>&1 && echo pass || echo "rc$?")"
check "and passes once it is there" pass "$(verdict)"

# --- a second job is a question, not a silent pass --------------------
write_wf "$LANE" "$CAP" "$DRIFT" '  summarize:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi'
check "a second job fails rather than passing on the first" rc1 "$(verdict)"

# --- cannot-check is distinct from broken -----------------------------
rm -f "$TMP/wf.yml"
check "a missing workflow is rc2, not rc1" rc2 "$(verdict)"

printf '# only comments\n# and nothing else\n' > "$TMP/wf.yml"
check "a comments-only workflow is rc2, not rc1" rc2 "$(verdict)"

exit "$fails"
