#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-action-pins.sh (#831).
#
# WORKFLOW_DIR MOVES DISCOVERY ONLY. Every case below runs the same
# judging code the real invocation runs -- there is no stub of `uses:`
# parsing and no seam above the decision. That is deliberate: the defect
# that produced #827 was a seam placed one line too high, so the gate
# scored perfectly on every axis while its safety logic had never
# executed.
#
# THE COUNT IS ASSERTED, NOT JUST THE EXIT CODE. A fixture that is never
# discovered and a fixture that is discovered and passes both exit 0. The
# success line reports how many `uses:` lines were judged, and the
# passing cases check that number, so a discovery expression that
# silently stops matching turns this suite red instead of green.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-action-pins.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

SHA="$(printf 'a%.0s' $(seq 1 40))"
failures=0
n=0

# run NAME WANT_EXIT [WANT_SUBSTRING...]
run() {
    local name="$1" want="$2"; shift 2
    n=$((n + 1))
    WORKFLOW_DIR="$TMP/wf" bash "$CHECK" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -ne "$want" ]; then
        echo "FAIL: $name -- want exit $want, got $got"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    local missing=""
    for s in "$@"; do
        grep -F -- "$s" "$TMP/out" > /dev/null || missing="$missing '$s'"
    done
    if [ -n "$missing" ]; then
        echo "FAIL: $name -- exit $got as wanted, but output lacks:$missing"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    echo "PASS: $name (exit $got)"
}

fresh() { rm -rf "$TMP/wf"; mkdir -p "$TMP/wf"; }

# --- the good state, and it must say how much it looked at -------------
# BOTH EXTENSIONS. This tree holds 23 .yml and one .yaml, and a gate that
# matched only .yml would pass over that file forever. The count in the
# success line is what proves the .yaml was opened.
#
# AND BOTH SPELLINGS. `uses:` appears with a leading dash when it is the
# first key of a step, and WITHOUT one when the step leads with `- name:`
# -- measured on this tree, 53 dashed and 43 dashless, so the dashless
# form is 45% of the corpus. Every fixture here used to be dashed, which
# left the count assertion nothing to lose: making the dash mandatory
# dropped 43 references, the suite stayed 16/16 green, and the real tree
# still exited 0 with a confident success line whose only difference was
# a smaller number that nothing compared to anything. The dashless entry
# below is what turns that edit red.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/checkout@$SHA
      - uses: actions/setup-go@$SHA
      - name: the dashless spelling, which is 45% of the real corpus
        uses: actions/upload-artifact@$SHA
EOF
cat > "$TMP/wf/b.yaml" <<EOF
jobs:
  y:
    steps:
      - uses: docker/login-action@$SHA
EOF
run "all pinned, across .yml and .yaml, dashed and dashless" 0 "all 4 'uses:'" "2 workflow file(s)"

# And the dashless form must be JUDGED, not merely counted. A gate that
# found the line but skipped the ref check would still say "all 4".
fresh
cat > "$TMP/wf/a.yml" <<'EOF'
jobs:
  x:
    steps:
      - name: unpinned, and the only uses: in the tree is dashless
        uses: actions/checkout@v7
EOF
run "an unpinned dashless ref is caught" 1 "a.yml:5" "not a commit SHA"

# THE EXTENSION IS DRIVEN, NOT ASSUMED. The violation lives ONLY in the
# .yaml file. If the discovery expression drops that extension this case
# exits 0 and says so, instead of quietly agreeing.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs: {x: {steps: [{uses: "actions/checkout@$SHA"}]}}
EOF
cat > "$TMP/wf/b.yaml" <<'EOF'
jobs:
  y:
    steps:
      - uses: actions/setup-go@v5
EOF
run "an unpinned ref in the .yaml file is caught" 1 "b.yaml:4" "not a commit SHA"

# --- the shapes that are not pins --------------------------------------
mutant() {  # mutant NAME REF WANT_EXIT [SUBSTR...]
    local name="$1" ref="$2" want="$3"; shift 3
    fresh
    printf 'jobs:\n  x:\n    steps:\n      - uses: %s\n' "$ref" > "$TMP/wf/a.yml"
    run "$name" "$want" "$@"
}

mutant "a version tag is not a pin"      "actions/checkout@v7"        1 "a.yml:4" "repoint it at any commit"
mutant "a branch is not a pin"           "actions/checkout@main"      1 "a.yml:4"
mutant "a short sha is not a pin"        "actions/checkout@abc1234"   1 "a.yml:4"
mutant "an uppercase sha is not a pin"   "actions/checkout@$(printf 'A%.0s' $(seq 1 40))" 1 "a.yml:4"
mutant "no ref at all"                   "actions/checkout"           1 "default branch"
mutant "a full 40-hex sha passes"        "actions/checkout@$SHA"      0 "all 1 'uses:'"

# --- shapes that are legitimately exempt, and must not be flagged ------
mutant "a local action needs no pin"     "./.github/actions/setup"    0 "all 1 'uses:'"
mutant "a docker action pinned by digest" "docker://alpine@sha256:$(printf 'b%.0s' $(seq 1 64))" 0
mutant "a docker action pinned by tag"   "docker://alpine:3.20"       1 "@sha256: digest"

# A commented-out reference is not executed, so it is not a violation.
# Without this case the obvious `grep uses:` implementation looks correct.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      # - uses: actions/checkout@v7   <- deliberately disabled
      - uses: actions/checkout@$SHA
EOF
run "a commented-out ref is not a violation" 0 "all 1 'uses:'"

# --- non-vacuity: the universal must not be satisfied by an empty set --
# "Every uses: is pinned" is TRUE when there are no workflows and TRUE
# when there are no uses:. Both must refuse.
fresh
run "an empty workflow directory refuses" 2 "nothing to judge"

fresh
printf 'name: nothing\non: push\njobs:\n  x:\n    steps:\n      - run: echo hi\n' > "$TMP/wf/a.yml"
run "workflow files with no uses: refuse" 2 "not one 'uses:' line" "the match is wrong"

rm -rf "$TMP/wf"
n=$((n + 1))
WORKFLOW_DIR="$TMP/wf" bash "$CHECK" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -F "no workflow directory" "$TMP/out" >/dev/null; then
    echo "PASS: a missing workflow directory refuses (exit 2)"
else
    echo "FAIL: a missing workflow directory -- want exit 2 naming the directory"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# --- the real tree, judged by the same code ---------------------------
n=$((n + 1))
if bash "$CHECK" > "$TMP/out" 2>&1; then
    echo "PASS: the repository's own workflows are all pinned"
else
    echo "FAIL: the repository's own workflows are not all pinned"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
