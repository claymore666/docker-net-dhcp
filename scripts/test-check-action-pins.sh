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
    # ACTION_SCAN_ROOT defaults to an EMPTY tree so a case says what it
    # means. Left at its real default every case below would also be
    # asserting something about the repository's composite actions, and
    # would go red the day one is added -- for reasons that have nothing
    # to do with the case. The cases that DO drive the composite scan set
    # SCAN themselves, and the real-tree case at the bottom runs the
    # check bare, which is what exercises the default.
    WORKFLOW_DIR="$TMP/wf" ACTION_SCAN_ROOT="${SCAN:-$TMP/scan}" \
        bash "$CHECK" > "$TMP/out" 2>&1
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

fresh() { rm -rf "$TMP/wf" "$TMP/scan"; mkdir -p "$TMP/wf" "$TMP/scan"; unset SCAN; }

# --- the good state, and it must say how much it looked at -------------
# BOTH EXTENSIONS. This tree holds 24 .yml and one .yaml, and a gate that
# matched only .yml would pass over that file forever. The count in the
# success line is what proves the .yaml was opened.
#
# AND BOTH SPELLINGS. `uses:` appears with a leading dash when it is the
# first key of a step, and WITHOUT one when the step leads with `- name:`
# -- measured on this tree, 54 dashed and 43 dashless, so the dashless
# form is 45% of the corpus. Every fixture here used to be dashed, which
# left the count assertion nothing to lose: making the dash mandatory
# dropped 43 references, the suite stayed green at the 16 cases it then
# held, and the real tree
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
      - name: "unpinned, and the only uses: in the tree is dashless"
        uses: actions/checkout@v7
EOF
run "an unpinned dashless ref is caught" 1 "a.yml:5" "not a commit SHA"

# THE EXTENSION IS DRIVEN, NOT ASSUMED. The violation lives ONLY in the
# .yaml file. If the discovery expression drops that extension this case
# exits 0 and says so, instead of quietly agreeing.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/checkout@$SHA
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

# A PIN, NOT THE SHAPE OF ONE. Testing for the `@sha256:` substring alone
# accepted these three as pinned: MEASURED, all exit 0 before the fix.
# The ref is unpullable, so the outcome was a failed run rather than an
# unreviewed one -- but it is the same pin-versus-pin-shaped distinction
# the 40-hex rule enforces on the other branch.
mutant "a docker digest of two characters is not a pin" \
    "docker://alpine@sha256:zz" 1 "not a 64-hex digest"
mutant "a docker digest of the right length but not hex is not a pin" \
    "docker://alpine@sha256:$(printf 'z%.0s' $(seq 1 64))" 1 "not a 64-hex digest"
mutant "an uppercase docker digest is not a pin" \
    "docker://alpine@sha256:$(printf 'B%.0s' $(seq 1 64))" 1 "not a 64-hex digest"

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
mkdir -p "$TMP/scan"
n=$((n + 1))
WORKFLOW_DIR="$TMP/wf" ACTION_SCAN_ROOT="$TMP/scan" bash "$CHECK" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -F "no workflow directory" "$TMP/out" >/dev/null; then
    echo "PASS: a missing workflow directory refuses (exit 2)"
else
    echo "FAIL: a missing workflow directory -- want exit 2 naming the directory"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# --- a uses: that is REAL but unparsed must refuse, not pass -----------
#
# The parser above reads a plain `uses: <ref>` line. GitHub reads more
# than that, and so does actionlint: a flow mapping and a value on the
# following line are both real action references -- substituting an
# invalid ref in either produces actionlint's "ref is missing"
# diagnostic, identically to the plain form. Before this refusal, either
# shape sat in a workflow beside one parseable reference and the gate
# printed a confident success line: MEASURED, both exit 0.
#
# The refusal is the reason nobody has to settle whether GitHub's runtime
# parser and actionlint agree on a given form. An unrecognised form is
# not judged pinned; it is not judged at all.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - {uses: actions/checkout@v7}
EOF
run "a flow-mapping uses: is residue, not a silent pass" 2 "were not resolved" "a.yml: 2 'uses:' occurrence(s) present, 1 resolved"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - uses:
          actions/checkout@v7
EOF
run "a next-line uses: value is residue, not a silent pass" 2 "were not resolved"

# A quoted KEY is a reference too, and the count must see it even though
# the parser cannot.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - "uses": actions/checkout@v7
EOF
run "a quoted uses: key is residue, not a silent pass" 2 "were not resolved"

# THE PRESERVATION CONTROL FOR THAT COUNT. A refusal keyed on a bare
# `grep uses:` would fire on a step name, on a shell line in a `run:`
# body, and on this project's own gate scripts quoted in a workflow --
# a gate that cries wolf gets discharged, and then the refusal above is
# worth nothing. None of the four below is a reference and none may
# count. Measured on the real tree: 97 parsed, 97 counted, no file
# differing.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - name: "a step name that says uses: out loud"
        uses: actions/checkout@$SHA
      - name: not a reference either
        run: |
          echo "uses: actions/evil@v1"
          grep -n 'uses:' .github/workflows/test.yaml
          printf '%s\\n' --uses: x
EOF
run "prose, shell and step names are not references" 0 "all 1 'uses:'"

# --- a file that cannot be READ is partial vacuity ---------------------
#
# The two refusals above close "no files" and "no uses: anywhere". A file
# the gate cannot open closes neither: its references count as zero, the
# total silently shrinks, and the success line is as confident as ever.
# MEASURED on the pre-fix script with `actions/checkout@v7` planted in a
# chmod-000 workflow: exit 0, and the count fell with nothing comparing
# it to anything.
fresh
cat > "$TMP/wf/a.yml" <<'EOF'
jobs:
  x:
    steps:
      - uses: actions/checkout@v7
EOF
cat > "$TMP/wf/b.yml" <<EOF
jobs:
  y:
    steps:
      - uses: actions/setup-go@$SHA
EOF
chmod 000 "$TMP/wf/a.yml"
n=$((n + 1))
if cat -- "$TMP/wf/a.yml" >/dev/null 2>&1; then
    # Not a skip. If this environment cannot make a file unreadable the
    # case has measured nothing, and a case that measured nothing must
    # not report a pass.
    echo "FAIL: an unreadable workflow file refuses -- the fixture is still readable (running as root?), so the refusal was never exercised"
    failures=$((failures + 1))
else
    WORKFLOW_DIR="$TMP/wf" ACTION_SCAN_ROOT="$TMP/scan" bash "$CHECK" > "$TMP/out" 2>&1
    got=$?
    if [ "$got" -eq 2 ] && grep -F "cannot read" "$TMP/out" >/dev/null; then
        echo "PASS: an unreadable workflow file refuses (exit 2)"
    else
        echo "FAIL: an unreadable workflow file -- want exit 2 naming the file, got $got"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
    fi
fi
chmod 644 "$TMP/wf/a.yml"

# --- the composite-action boundary carries a check, not a paragraph ----
#
# `uses: ./...` is exempt because the REFERENCE names this repository.
# The ACTION it names is a different question: a composite action's own
# `action.yml` can `uses:` a third party at a tag, discovery never opens
# it, and bare actionlint does not catch that either. The limit used to
# be a comment telling whoever adds the first composite action to widen
# discovery -- in a file they have no reason to open.
fresh
printf 'jobs:\n  x:\n    steps:\n      - uses: ./.github/actions/setup\n' > "$TMP/wf/a.yml"
mkdir -p "$TMP/scan/.github/actions/setup"
printf 'runs:\n  using: composite\n  steps:\n    - uses: actions/checkout@v7\n' \
    > "$TMP/scan/.github/actions/setup/action.yml"
SCAN="$TMP/scan" run "a composite action outside discovery refuses" 2 "discovery does not cover it"

# The .yaml spelling of the same file, because GitHub honours both and a
# check that matched one extension would pass over the other forever.
fresh
printf 'jobs:\n  x:\n    steps:\n      - uses: ./.github/actions/setup\n' > "$TMP/wf/a.yml"
mkdir -p "$TMP/scan/.github/actions/setup"
printf 'runs:\n  using: composite\n' > "$TMP/scan/.github/actions/setup/action.yaml"
SCAN="$TMP/scan" run "the .yaml spelling of a composite action refuses too" 2 "discovery does not cover it"

# THE PRESERVATION CONTROL. A file discovery already opens is judged,
# not refused, or the gate refuses to run on any tree that happens to
# hold a workflow named action.yml.
fresh
printf 'jobs:\n  x:\n    steps:\n      - uses: actions/checkout@%s\n' "$SHA" > "$TMP/wf/action.yml"
SCAN="$TMP/wf" run "an action.yml discovery already covers is judged, not refused" 0 "all 1 'uses:'"

# A near miss must not refuse either: only the two exact basenames are a
# composite action.
fresh
printf 'jobs:\n  x:\n    steps:\n      - uses: actions/checkout@%s\n' "$SHA" > "$TMP/wf/a.yml"
mkdir -p "$TMP/scan"
printf 'not a composite action\n' > "$TMP/scan/my-action.yml"
SCAN="$TMP/scan" run "a file merely ending in action.yml is not a composite action" 0 "all 1 'uses:'"

# BOTH DISCOVERY ARMS ARE DRIVEN. Inside a checkout the scan asks git,
# so that a maintainer's ignored trees -- whole other branches in git
# worktrees -- cannot produce a red that CI never sees. `--others`
# is there because a composite action added and not yet committed is
# precisely the case worth catching.
fresh
printf 'jobs:\n  x:\n    steps:\n      - uses: actions/checkout@%s\n' "$SHA" > "$TMP/wf/a.yml"
mkdir -p "$TMP/scan/.github/actions/setup"
git -C "$TMP/scan" init -q 2>/dev/null
printf 'runs:\n  using: composite\n' > "$TMP/scan/.github/actions/setup/action.yml"
SCAN="$TMP/scan" run "an uncommitted composite action inside a checkout refuses" 2 "discovery does not cover it"

# ...and an IGNORED one does not, which is the whole reason the git arm
# exists.
fresh
printf 'jobs:\n  x:\n    steps:\n      - uses: actions/checkout@%s\n' "$SHA" > "$TMP/wf/a.yml"
mkdir -p "$TMP/scan/ignored/.github/actions/setup"
git -C "$TMP/scan" init -q 2>/dev/null
printf 'ignored/\n' > "$TMP/scan/.gitignore"
printf 'runs:\n  using: composite\n' > "$TMP/scan/ignored/.github/actions/setup/action.yml"
SCAN="$TMP/scan" run "an ignored tree does not produce a red CI never sees" 0 "all 1 'uses:'"

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

# A SUITE THAT MEASURED NOTHING MUST NOT REPORT A PASS. This is the same
# argument the gate itself makes about an empty corpus, applied to the
# file that makes it: delete the case list and "all 0 case(s) passed"
# exits 0, which is a green tick over nothing. The floor is the count
# this file is known to run; raise it when cases are added, and a
# deletion has to be deliberate rather than silent.
FLOOR=31
if [ "$n" -lt "$FLOOR" ]; then
    echo "REFUSING: ran $n case(s), fewer than the $FLOOR this suite is known to hold."
    echo "  Either cases were lost, or the floor is stale and should be raised with them."
    exit 1
fi
echo "all $n case(s) passed"
