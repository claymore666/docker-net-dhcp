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

# WHICH `@` THE RULE SPLITS ON, DRIVEN. The gate read the ref as the tail
# after the LAST `@` and GitHub reads it as the tail after the FIRST, so
# `owner/repo@v7@<40 hex>` was judged SHA-pinned here and resolves at the
# MUTABLE tag `v7@<40 hex>` there. An ordinary single-line `uses:` in the
# ordinary position, so it defeated a human reader as well as the gate.
#
# NOTHING IN THIS SUITE CONSTRAINED THE SPLIT BEFORE THESE CASES, and
# that is the finding rather than a footnote: every fixture here and
# every reference in the tree carries exactly ONE `@`, the position where
# the two rules agree, so flipping the split killed nothing and the
# mutation table was full while the rule was free to move.
#
# THE FIRST-@ READING IS MEASURED, NOT PREFERRED. actionlint v1.7.12
# accepts `actions/checkout@v7@` and rejects `actions/checkout@` for an
# empty ref -- so the former's ref is `v7@`, which only a first-@ split
# yields; `actions/checkout@@v2` is accepted with ref `@v2`; and
# `git check-ref-format refs/tags/v7@<40 hex>` accepts, so the tag is
# creatable. actionlint is one implementation and not GitHub's runtime
# parser: MEASURED there, INFERRED for GitHub, and used only in the
# direction that makes this gate stricter.
mutant "a 40-hex tail after a SECOND @ is not a pin" \
    "actions/checkout@v7@$SHA"   1 "a.yml:4" "repoint it at any commit"
mutant "a doubled @ with a 40-hex tail is not a pin" \
    "actions/checkout@@$SHA"     1 "a.yml:4"
mutant "a 40-hex tail after THREE @ is not a pin" \
    "actions/checkout@v7@v8@$SHA" 1 "a.yml:4"
# A leading `@` reaches the same hole from the other end -- and it is the
# spelling a typo produces rather than an adversary. actionlint rejects it
# outright (`owner is missing`), so nothing legitimate is lost by the red.
# Quoted, because an unquoted `@` cannot start a YAML scalar at all.
mutant "a leading @ with a 40-hex tail is not a pin" \
    "\"@actions/checkout@$SHA\"" 1 "a.yml:4"
# THE OTHER DIRECTION OF THE SAME EDIT. This shape was caught before and
# must stay caught: a rule that split at the first `@` and then compared
# the wrong end of the result would let it through.
mutant "a second @ AFTER the sha is still not a pin" \
    "actions/checkout@$SHA@v7"   1 "a.yml:4"

# ...AND THE SHAPE AS IT WOULD ACTUALLY ARRIVE, beside an ordinary pinned
# reference. The failure this closes was not a missing red, it was a
# CONFIDENT SUCCESS LINE counting the unpinned reference among the
# pinned, which is the one outcome this gate exists to refuse. The
# mutants above would all still fail loudly if the whole file were
# rejected for some unrelated reason; this one asserts the tally.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - uses: evil/action@v7@$SHA
EOF
run "a two-@ ref beside a pinned one does not produce a success line" 1 \
    "a.yml:5" "1 of 2 'uses:'"

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

# THE SAME FIRST-VERSUS-LAST SPLIT ON THIS BRANCH, found by measuring the
# NEIGHBOUR of the ref-side fix rather than only the spot it was reported
# at. `@sha256:` can appear twice, and taking the LAST occurrence found a
# well-formed digest inside a reference that names no image. MEASURED
# that the shape is not executable in either ordering -- `docker pull`
# answers `invalid reference format` -- so the exposure was a failed run
# rather than an unreviewed one, which is the same standing as the
# two-character digest above and the same reason to refuse it.
mutant "a docker digest after a SECOND @sha256: is not a pin" \
    "docker://alpine@sha256:zz@sha256:$(printf 'b%.0s' $(seq 1 64))" 1 "not a 64-hex digest"

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

# THE COMMENT EXPRESSION IS A SPELLING ASSUMPTION TOO, and it ran in the
# permissive direction -- which is the one direction this gate is not
# allowed to be wrong in. A `#` inside a quoted scalar is not a comment,
# but comments were stripped before quotes, so the `#` below deleted the
# rest of its own line and took the `uses:` with it: occurrences fell to
# zero, matched a parsed zero, no residue was reported, and the gate
# printed "all 1 'uses:' reference(s) ... are SHA-pinned" over a
# reference it had never seen. MEASURED on the pre-fix script: exit 0.
# actionlint answers `ref is missing` on this shape byte-identically to
# the plain form, so it is a real reference by the same oracle the
# flow-mapping case above rests on.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - {name: "release #1", uses: actions/checkout@v7}
EOF
run "a quoted # ahead of uses: is residue, not a silent pass" 2 "were not resolved"

# Both quote characters, because the neutraliser is two expressions and a
# suite that drove only one left the other with nothing holding it:
# deleting the single-quote expression alone left all cases green.
# MEASURED -- it survived, which is what put this case here.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - {name: 'release #1', uses: actions/checkout@v7}
EOF
run "a single-quoted # ahead of uses: is residue too" 2 "were not resolved"

# THE SAME HOLE REACHED THROUGH AN ESCAPED QUOTE, which is what separates
# a fix from one that only happens to work on the reported input. A
# remedy that tracks quote state character by character loses that state
# at the `\"` and strips from the `#` again; a remedy that splits flow
# punctuation before stripping comments survives this but turns an
# ordinary comment into a refusal, which the control further down
# catches. Deleting quoted `#`s outright is neither: it can only leave
# MORE text standing, so it can only raise the count, and the count is a
# lower bound that is allowed to err upward.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - {name: "say \\"hi\\" #1", uses: actions/checkout@v7}
EOF
run "an escaped quote before the # does not reopen the hole" 2 "were not resolved"

# YAML PUTS NO CONSTRAINT ON THE SPACE BEFORE A `:`. The count spelled
# the key `uses:` with none, and the parser has the same blind spot, so
# `uses :` was neither counted nor parsed -- occurrences and parsed were
# both zero, the difference was zero, and nothing refused. MEASURED on
# the pre-fix script: exit 0 with the success line. actionlint reads it
# as an action reference and answers `ref is missing`.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - uses : actions/checkout@v7
EOF
run "whitespace before the colon is residue, not a silent pass" 2 "were not resolved"

# A tab is whitespace too, and an expression widened with a literal
# space would still pass the case above.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - uses$(printf '\t'): actions/checkout@v7
EOF
run "a tab before the colon is residue too" 2 "were not resolved"

# AND THE KEY CAN BE WRITTEN WITHOUT A COLON ON ITS LINE AT ALL. `? uses`
# with the value on the following line is an explicit mapping key in
# YAML and an action reference to actionlint -- the same `ref is missing`
# diagnostic -- so an expression anchored on `uses:` cannot see it in ANY
# spelling of the space before the colon, because there is no colon.
# MEASURED on the pre-fix script: exit 0. This shape was not among the
# pair the review reported; it came out of the search the review asked
# for, which is the point of asking.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - ? uses
        : actions/checkout@v7
EOF
run "an explicit ? uses key is residue, not a silent pass" 2 "were not resolved"

# ...and inside a flow mapping, where the key arrives at the head of a
# line only after flow punctuation has been split.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - {? uses: actions/checkout@v7}
EOF
run "an explicit key inside a flow mapping is residue too" 2 "were not resolved"

# ============ A DEFECT PINNED AS A CASE. READ THE EXIT 0 AS A RECORD
# ============ OF A HOLE, NEVER AS A CLAIM THAT THE GATE IS RIGHT HERE.
#
# The two cases above cover the `?` indicator SHARING A LINE with its
# key. The indicator may also stand ALONE on its line, putting the key
# on a LATER line, where it shares a line with neither a `?` nor a `:`.
# The counter is line-oriented, so it counts nothing, produces no
# residue, and the gate prints its success line over an unpinned
# `actions/checkout@v7`. MEASURED: exit 0 -- which is what the case
# below asserts, and it asserts the WRONG answer on purpose.
#
# Both oracles call that step a real action reference, so this is not a
# quibble about an exotic non-reference: actionlint answers `specifying
# action ... in invalid format because owner and repo and ref should not
# be empty` on the invalid-ref probe -- the same oracle every shape case
# above rests on -- and PyYAML parses the step to
# `{'uses': 'actions/checkout@v7'}`. Ten spellings of this shape were
# measured (indicator alone; folded and literal keys; either quote
# character on the key; a comment between indicator and key; the colon a
# line further down; the dashless spelling; deeper indentation; inside a
# flow mapping) and all ten exit 0.
#
# THE TRADE, WRITTEN DOWN SO THAT "FIXING" IT HAS TO CONFRONT WHAT IT
# BUYS -- and MEASURED, not argued. The obvious widening is to join a
# bare `?` to the line after it. Applied to the counter as a mutant, it
# turns THIS case red and leaves the boundary control below GREEN: the
# widened counter closes the simple spellings and still cannot see the
# split key. That is the whole finding -- a widening MOVES the boundary
# and does not remove it -- and it is measured by the two cases sitting
# next to each other rather than claimed in a sentence.
#
# So the widening buys the spellings someone has already thought of, at
# the price of a second mechanism to maintain and mutation-test, against
# an adversary who writes the next spelling instead.
#
# Closing the class needs a YAML PARSER, and that route OPENED under
# this PR's feet, which is worth recording because the first draft of
# this comment said the opposite. It said PyYAML was used by ZERO gates
# here; MEASURED against the current base, that is false. #844 landed
# check-fork-execution-policy.sh, which parses the workflow tree with
# PyYAML and refuses without it, and test.yaml installs PyYAML in the
# SAME job that runs this gate. The remaining cost is a rewrite of the
# judging mechanism plus a refusal wherever PyYAML is absent, and
# local-lane.sh runs this gate while carrying no PyYAML-dependent gate
# today. Still a separate change; no longer a blocked one. What
# this PR does instead is stop CLAIMING the class is refused: the gate's
# header states the bound, and its refusal message already says its list
# is open rather than closed. Whoever does widen it should keep both
# cases -- the first one flips to a refusal, the second one is the
# reason the words "closed" and "complete" still may not be used.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - ?
          uses
        : actions/checkout@v7
EOF
run "PINNED DEFECT: a ? alone on its line escapes the count (exit 0 is wrong)" 0 "all 1 'uses:'"

# THE BOUNDARY CONTROL FOR THE PARAGRAPH ABOVE, and the reason the case
# above is pinned rather than fixed. A double-quoted key may be split
# across lines with a `\` escape, so the key `uses` can be spelled `us\`
# + `es` and the TOKEN never appears contiguously anywhere in the file.
# No line-oriented counter can ever see this one, whatever expression it
# is given -- so "a widening cannot close the class" is checked here
# rather than asserted in a comment.
fresh
printf 'jobs:\n  x:\n    steps:\n      - uses: actions/setup-go@%s\n      - ? "us\\\n          es"\n        : actions/checkout@v7\n' "$SHA" > "$TMP/wf/a.yml"
run "PINNED DEFECT: a key split across lines is beyond any line counter" 0 "all 1 'uses:'"

# ...and the premise that control rests on is itself asserted, because a
# fixture that quietly stopped splitting the key would make the case
# above pass for the wrong reason. TWO references live in that file and
# the token occurs on ONE line.
n=$((n + 1))
if [ "$(grep -c -F 'uses' "$TMP/wf/a.yml")" -eq 1 ]; then
    echo "PASS: the token 'uses' occurs on one line of a fixture holding two references"
else
    echo "FAIL: the split-key fixture no longer hides the token -- the boundary control above is testing nothing"
    sed 's/^/    /' "$TMP/wf/a.yml"; failures=$((failures + 1))
fi

# ============ A YAML NODE PROPERTY BETWEEN THE DASH AND THE KEY.
#
# This class is NOT pinned -- it is fixed, and these cases are what hold
# the fix. It is recorded here because it FALSIFIED the bound the two
# pinned cases above rest on. That bound said the counter sees any form
# leaving the token `uses` at a key position on SOME LINE; a node
# property leaves it exactly there and escaped anyway, because the
# expression admitted only block-sequence dashes ahead of the key.
# Before the fix every case below exited 0 with the success line, over
# an unpinned `actions/checkout@v7`.
#
# Both oracles were consulted per shape rather than assumed together,
# because they DISAGREE here and the disagreement is the point. On
# `- !!map uses: ...` the tag attaches to the KEY SCALAR, so PyYAML
# refuses to construct the mapping ("found unhashable key") while
# actionlint -- the tool that models GitHub's own workflow parsing --
# resolves the step and answers `specifying action ... in invalid
# format because owner and repo and ref should not be empty` on the
# invalid-ref probe. A form this gate PASSES must not depend on
# GitHub's parser being stricter than this one, so actionlint calling
# it a reference is sufficient to require a verdict here.
#
# Fifteen spellings were measured. The twelve either oracle called a
# real reference now exit 1; the three neither called a reference
# refuse rather than pass. The four below are the load-bearing ones:
# anchor, tag, the dashless position, and a pinned ref that must still
# pass.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - &a uses: actions/checkout@v7
EOF
run "an anchor before the key does not hide the ref" 1 "a.yml:5" "not a commit SHA"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - !!map uses: actions/checkout@v7
EOF
run "a tag before the key does not hide the ref" 1 "a.yml:5" "not a commit SHA"

# The dashless position matters separately: a node property may precede
# a key that is not a sequence entry's first, and the parser's dash is
# optional, so the two arms had to be widened in lockstep.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - name: n
        !!str uses: actions/checkout@v7
EOF
run "a tag before a dashless key does not hide the ref either" 1 "a.yml:6" "not a commit SHA"

# THE PRESERVATION CONTROL FOR THE WIDENING. Admitting a node property
# must not turn every property-bearing step into a finding: a PINNED ref
# behind an anchor is still pinned, and the count must reach 2 rather
# than stopping at the reference the widening was written for. If the
# ref extraction had been widened wrongly -- stripping too little, or
# too much -- this case goes red, because the garbage it would extract
# is not 40 hex characters.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - &a !x uses: actions/checkout@$SHA
EOF
run "two properties before a PINNED ref still pass, and still count" 0 "all 2 'uses:'"

# THE COUNTER'S OWN CONTRIBUTION, WHICH THE FOUR CASES ABOVE DO NOT
# TEST. The count is a LOWER bound and is only compared upward -- the
# gate acts on `occurrences > parsed` -- so narrowing the COUNTER alone
# changes no verdict on a form the PARSER can still read. MEASURED as a
# mutant: reverting the counter's node-property group, and reverting it
# to anchors-only, both SURVIVED the four cases above. That is a missing
# test, not a no-op change, and these two are it.
#
# A node property in front of an EXPLICIT `? uses` key is the shape that
# separates them: the counter sees a key, the parser cannot read it, and
# the difference is residue. Residue REFUSES -- exit 2, not exit 0 --
# which is the whole contract, and without the widened counter the
# occurrence is never counted, no residue is produced, and the gate
# prints its success line over a step it never judged. Neither oracle
# calls this shape a real reference; refusing an unrecognised form is
# the honest answer either way, and it is the direction that fails
# loudly.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - &a ? uses
        : actions/checkout@v7
EOF
run "an anchor before an explicit ? key is residue, not a silent pass" 2 "were not resolved"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - !!map ? uses
        : actions/checkout@v7
EOF
run "a tag before an explicit ? key is residue too" 2 "were not resolved"

# AND THE REPETITION IS LOAD-BEARING SEPARATELY. YAML allows an anchor
# AND a tag before the same node, in either order, so the group repeats.
# MEASURED as a mutant: changing that `*` to a `?` -- one property only
# -- SURVIVED both cases above, because a single-property fixture cannot
# tell the two apart. This case is what tells them apart.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - &a !x ? uses
        : actions/checkout@v7
EOF
run "two properties before an explicit ? key are residue as well" 2 "were not resolved"

# ============ THE POSITION THE ROUND ABOVE DID NOT REACH: A PROPERTY
# ============ *BETWEEN* THE `?` INDICATOR AND THE KEY.
#
# Every case above puts the property BEFORE the `?`. It may equally sit
# AFTER it -- `- ? &a uses` -- and that position escaped the counter
# entirely, because the group admitting properties sat ahead of the
# ALTERNATION: it reached a property in front of `uses:` and a property
# in front of `?`, and not one in front of the key inside the `?` arm.
#
# MEASURED before the fix, at the head that shipped the cases above:
# exit 0 with the success line, over an unpinned `actions/checkout@v7`,
# and PyYAML composes that step to a real `uses` key. A SILENT PASS ON
# AN UNPINNED REFERENCE is the one failure this gate exists to refuse,
# and it sat inside the arm the gate's own comment said was handled.
#
# It is the defect ONE STEP TO THE SIDE of the fix that preceded it,
# which is why check-action-pins.sh now enumerates the POSITIONS a node
# property can occupy rather than the spellings -- as a lower bound on
# that class, never as a completeness claim.
#
# Five spellings are driven, because the ones sharing a mechanism are
# exactly the ones a single fixture cannot tell apart: block; the flow
# mapping, which is entirely on ONE LINE and so is not excused by the
# line-oriented bound the pinned cases above rest on; the quoted key,
# which only reaches the counter after quote stripping; the dashless
# position; and two properties, which makes the repetition load-bearing
# in THIS arm as well as in the other one.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - ? &a uses
        : actions/checkout@v7
EOF
run "an anchor BETWEEN ? and the key is residue, not a silent pass" 2 "were not resolved"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - {? &a uses : actions/checkout@v7}
EOF
run "the same shape inside a flow mapping, entirely on one line" 2 "were not resolved"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - ? &a "uses"
        : actions/checkout@v7
EOF
run "a property before a QUOTED explicit key is residue too" 2 "were not resolved"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - name: n
        ? &a uses
        : actions/checkout@v7
EOF
run "a property between ? and a DASHLESS explicit key is residue" 2 "were not resolved"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - ? &a !x uses
        : actions/checkout@v7
EOF
run "two properties between ? and the key are residue as well" 2 "were not resolved"

# AND THE TWO POSITIONS COMPOSE, which is its own case because each
# alone was already covered and the combination was NOT: a property
# before the `?` AND another after it. MEASURED at the previous head:
# exit 0 with the success line, while `- &a ? uses` -- one property,
# before the indicator -- correctly refused. Two arms that each work
# alone can still fail together.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - &a ? !x uses
        : actions/checkout@v7
EOF
run "a property on BOTH sides of the ? indicator is residue" 2 "were not resolved"

# THE NARROWING CONTROL FOR THAT WIDENING, and the reason it is not a
# licence to count `? <anything> uses`. Only a NODE PROPERTY may stand
# there. A bare word does not: `? x uses` is the plain key `x uses`,
# which PyYAML confirms, and it is not a reference -- so exit 0 here is
# the RIGHT answer, not a pinned defect. Widening the group to any
# first token turns this case red, which is what makes it a control.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - ? x uses
        : actions/checkout@v7
EOF
run "a bare word between ? and uses is a different key, and does not count" 0 "all 1 'uses:'"

# THE OTHER SIDE OF THE SAME BOUNDARY. A property may precede a node; it
# may not follow one. `uses &a :` is the plain key `uses &a` -- MEASURED
# with PyYAML -- so it is not a `uses` key and must not count. This case
# is what stops a later widening from admitting a property between the
# key and its colon on the theory that YAML is permissive there.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - uses &a : actions/checkout@v7
EOF
run "a token AFTER the key is part of the key, and does not count" 0 "all 1 'uses:'"

# AND THE INTERSECTION OF THE TWO CLASSES, PINNED. A property in front
# of the key does not rescue the `?`-alone class: with the indicator on
# its own line the key line carries neither a `?` nor a `:`, so no
# expression in this file can reach it however the property group is
# widened. MEASURED: exit 0, and PyYAML composes a real `uses` key. It
# is recorded here so the enumeration in check-action-pins.sh has a case
# for the square it leaves open, rather than only prose.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - ?
          &a uses
        : actions/checkout@v7
EOF
run "PINNED DEFECT: a property does not rescue the ?-alone class (exit 0 is wrong)" 0 "all 1 'uses:'"

# THE COPIES OF THE PROPERTY GROUP MUST AGREE, AND THAT IS CHECKED
# RATHER THAN REMEMBERED. The same subexpression appears in the counter
# twice, in the ref extraction and in the parser feed. The defect this
# round fixed was precisely a widening that reached some of those
# positions and not another, and the cases above only catch the copies
# a fixture happens to exercise -- a future widening of what a PROPERTY
# may look like (a different leading character, a different terminator)
# could again land on three of four and leave the fourth behind, with
# every case above still green because no fixture distinguishes them.
#
# So the population is DERIVED from the script rather than counted here:
# no number is written down. A refactor to a single shared variable
# leaves one site pair and passes, which is the right answer: there is
# then nothing to drift.
#
# THE FIRST VERSION OF THIS CHECK WAS KEYED ON `[&!]` AND WAS INERT.
# Driving the absence is what found it: widening ONE copy to `[&!%]`
# made that copy invisible to the pattern, so the check reported "all 2
# copies identical" over a script whose four copies no longer agreed.
#
# THE SECOND VERSION HAD THE SAME DEFECT ONE STEP TO THE SIDE, and the
# paragraph here claimed otherwise. It said the copies were located "by
# the thing that cannot move without this whole gate moving -- the
# `uses` token inside a regular expression". They were not: the locator
# was `uses:?[[:space:]]`, which carries a whitespace CLASS, and a class
# can be respelled. MEASURED by a reviewer 2026-08-28 -- rewriting ONE
# site to `[[:blank:]]` dropped it out of the domain, the suite stayed
# 67/67, the real tree behaved identically, and this check printed "all
# 3 sites agree" over four. Composing that with a widening of the other
# three reproduces exactly the defect a3e6bbe was written to prevent,
# with everything green. A check keyed on a spelling reproduces its own
# silence, twice in this file now.
#
# SO THE DOMAIN IS DERIVED TWICE, BY KEYS THAT FAIL IN DIFFERENT
# DIRECTIONS FOR A SINGLE-KEY CHANGE -- which is a weaker claim than the
# one this paragraph made a round ago, and the weaker one is the true
# one.
#   A locates a `uses` key site by the whitespace class the expressions
#     use today -- `uses:?[[:space:]]`.
#   B locates a site that ADMITS a property by the group's closing `)*`
#     sitting immediately ahead of the `uses` token, and contains no
#     whitespace class at all.
# A respelt class drops a site from A and not from B; a site that stops
# admitting a property -- the a3e6bbe defect -- drops from B and not
# from A. Either of those ALONE makes the two disagree, and the
# disagreement is the failure. Neither derivation is trusted; their
# AGREEMENT is what is asserted, and a floor refuses an emptied domain
# rather than congratulating it.
#
# THE BOUND, AND THERE ARE TWO ESCAPES, not one. This paragraph said
# "BY KEYS THAT CANNOT FAIL TOGETHER" for exactly one round, and that
# was a completeness claim in the file whose subject is that a check can
# lose part of its own domain. It was false:
#
#   1. A COMPOSED change at ONE site defeats both. Take a single site,
#      remove its property group AND respell its whitespace class in the
#      same edit -- or simply delete the site outright -- and it drops
#      out of A and out of B together. The count falls 4 -> 3, both
#      floors still hold, and this check prints PASS over a script whose
#      remaining sites can then be widened at leisure. MEASURED at two
#      different sites; both are PINNED DEFECT cases below, asserting
#      the wrong verdict this check gives today so that closing it turns
#      them red. What killed those mutants was the BEHAVIOURAL cases,
#      not this check -- which is the honest reading of why the gate is
#      still sound and this observer is not.
#
#      AND ADDING A SITE IS THE SAME ESCAPE, QUIETER. Everything above
#      describes REMOVAL, and both pinned cases drive removal -- but the
#      realistic route is a FIFTH `uses` matcher appended to the gate and
#      written outside both keys: `[[:blank:]]` for the class, no
#      property group ahead of the token. MEASURED: the real file gives
#      `OK  4 sites, one group` and so does the file with that fifth
#      matcher added, so the count does not even MOVE, where removal at
#      least drops it 4 -> 3. The control that makes this a statement
#      about the KEYS rather than about additions in general: the same
#      fifth matcher written the ordinary way IS seen by both, and the
#      verdict becomes `OK  5 sites, one group`. Not separately pinned --
#      it is escape 1 reached from the other end -- but named here
#      because the paragraph above, read alone, describes only half of it.
#   2. Both derivations still key on the literal token `uses`. Rename it
#      throughout and both go to zero together -- which the floor turns
#      into a REFUSAL, not a pass.
#
# Escape 2 is affordable because it fails closed. Escape 1 is not, and
# it is recorded rather than closed: catching it needs a derivation that
# does not mention `uses` at all, which means parsing the expressions,
# and an ERE parser inside a self-test is a larger thing than what it
# guards. A fix carries the defect class of its own finding, and this
# one did.
# The verdict is a FUNCTION of a file, not a block of inline code, so
# that its own absence can be driven: the cases below hand it copies of
# check-action-pins.sh, each broken in one way, and pin what it answers
# for each. A check nobody can run against a broken input is a check
# nobody has tested.
#
# THIS PARAGRAPH ALSO PROMISED A DRIVE THAT DID NOT EXIST. It said the
# cases hand the check a copy "with one site removed", and no such case
# was written -- the very shape that would have found escape 1 above.
# Prose promising a test nobody wrote is indistinguishable from the test
# for exactly as long as nobody looks. Both removal shapes are cases
# now, and they are PINNED DEFECT cases because the check gets them
# WRONG: they assert today's answer, not the right one.
#
# OCCURRENCES, NOT LINES. `grep -c` counts LINES, and the counter holds
# TWO of these sites on ONE line -- so a line count reported three sites
# where there are four and the check went red against a correct script.
# Caught by running it, which is why the control is driven and not read.
prop_verdict() {   # prop_verdict <file> -> prints "TOKEN<TAB>detail"
    local f="$1" a b admit groups distinct
    a="$(grep -oE 'uses:?\[\[:space:\]\]' "$f" | grep -c . || true)"
    admit="$(grep -oE '\([^()]*\)\*\(?uses' "$f" || true)"
    b="$(printf '%s' "$admit" | grep -c . || true)"
    groups="$(printf '%s\n' "$admit" | sed -E 's/\(?uses$//' | sort -u)"
    distinct="$(printf '%s' "$groups" | grep -c . || true)"
    if [ "$b" -lt 2 ] || [ "$a" -lt 2 ]; then
        printf 'REFUSE\t%s site(s) by the class key and %s by the group key\n' "$a" "$b"
        return
    fi
    if [ "$a" -ne "$b" ]; then
        printf 'DISAGREE\t%s site(s) by the class key, %s by the group key\n' "$a" "$b"
        return
    fi
    if [ "$distinct" -ne 1 ]; then
        printf 'DIFFER\t%s spelling(s) across %s sites:\n%s\n' \
            "$distinct" "$b" "$(printf '%s\n' "$groups" | sed 's/^/        /')"
        return
    fi
    printf 'OK\t%s sites, one group\n' "$b"
}

n=$((n + 1))
prop_out="$(prop_verdict "$CHECK")"
case "${prop_out%%$'\t'*}" in
    OK)
        echo "PASS: all 'uses' key sites admit the same node-property group (${prop_out#*$'\t'})"
        ;;
    REFUSE)
        echo "FAIL: REFUSING -- the domain is too small to be judged (${prop_out#*$'\t'})."
        echo "    There are several sites by construction (the counter's two arms, the ref"
        echo "    extraction, the parser feed). This check was emptied of its domain, not"
        echo "    satisfied."
        failures=$((failures + 1))
        ;;
    DISAGREE)
        echo "FAIL: the two derivations of the site population disagree (${prop_out#*$'\t'})."
        echo "    Either a site stopped admitting a node property -- the defect a3e6bbe was"
        echo "    written to prevent -- or one site's whitespace class was respelt and fell"
        echo "    out of the other derivation's view. Both are findings."
        failures=$((failures + 1))
        ;;
    *)
        echo "FAIL: the node-property group is spelled more than one way -- ${prop_out#*$'\t'}"
        failures=$((failures + 1))
        ;;
esac

# DRIVE THE ABSENCE of the check just run. Each mutant below is applied
# to a COPY, and each must produce a different verdict token from the
# real file's -- otherwise the check above is decoration.
prop_case() {   # prop_case <label> <want-token> <sed-program>
    local label="$1" want="$2" prog="$3"
    local m="$TMP/propmut.sh"
    sed -E "$prog" "$CHECK" > "$m"
    n=$((n + 1))
    if cmp -s "$CHECK" "$m"; then
        echo "FAIL: mutant '$label' did not apply -- its pattern matched nothing"
        failures=$((failures + 1))
        return
    fi
    local got; got="$(prop_verdict "$m")"
    if [ "${got%%$'\t'*}" = "$want" ]; then
        echo "PASS: $label -> $want"
    else
        echo "FAIL: $label -> ${got%%$'\t'*}, want $want (${got#*$'\t'})"
        failures=$((failures + 1))
    fi
}

# The reviewer's surviving mutant, verbatim in shape: respell ONE site's
# whitespace class. It leaves the real tree's behaviour unchanged, which
# is why nothing else in this suite can see it.
prop_case "one site's whitespace class respelt" DISAGREE \
    '0,/uses:\[\[:space:\]\]/s/uses:\[\[:space:\]\]/uses:[[:blank:]]/'
# A site that stops admitting a property -- the a3e6bbe defect itself.
prop_case "one site stops admitting a property" DISAGREE \
    '0,/\(\[&!\]\[\^\[:space:\]\]\*\[\[:space:\]\]\+\)\*uses/s/\(\[&!\]\[\^\[:space:\]\]\*\[\[:space:\]\]\+\)\*uses/uses/'
# One group widened and the others left behind.
prop_case "one group widened, the rest left behind" DIFFER \
    '0,/\[&!\]/s/\[&!\]/[\&!%]/'
# The domain emptied by renaming the token both derivations key on.
# This is escape 2 above, and it must REFUSE rather than pass.
prop_case "the 'uses' token renamed throughout" REFUSE 's/uses/utilises/g'

# ESCAPE 1, PINNED AS A DEFECT RATHER THAN CORRECTED IN PROSE. Both of
# these drop ONE site out of BOTH derivations at once, so the two agree
# at 3 and this check answers OK over a script that has lost a quarter
# of its domain. The `OK` asserted below is the WRONG answer and is
# recorded as such: whoever closes escape 1 turns these two red, which
# is the point of pinning them. In the tree they are harmless only
# because the behavioural cases above catch the widening that follows.
prop_case "PINNED DEFECT: a site loses its group AND respells its class -> not seen" OK \
    's|\(\[&!\]\[\^\[:space:\]\]\*\[\[:space:\]\]\+\)\*uses:\[\[:space:\]\]\*//|uses:[[:blank:]]*//|'
prop_case "PINNED DEFECT: a site removed outright -> not seen" OK \
    's|\(\[&!\]\[\^\[:space:\]\]\*\[\[:space:\]\]\+\)\*uses:\[\[:space:\]\]\*\[\^\[:space:\]\]|REMOVED|'

# AN ALIAS IS NOT AN ESCAPE, AND THAT IS MEASURED RATHER THAN ARGUED.
# `*a` looks like the dangerous neighbour of a node property -- it
# stands where the mapping would be and carries no `uses` token. But to
# alias a step you must first WRITE that step, and the anchor's
# definition site carries the literal `uses:` line this gate reads. Both
# routes are driven, because the comment in the gate now claims this and
# a claim in a comment decays silently: a plain alias, and a `<<:` merge
# key, each pulling in an anchored UNPINNED step. Each is caught at the
# definition site.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - &s
        uses: actions/checkout@v7
      - *s
EOF
run "an alias reusing an anchored unpinned step is caught" 1 "a.yml:6" "not pinned to a 40-hex commit SHA"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - &s
        uses: actions/checkout@v7
      - <<: *s
        name: second
EOF
run "a merge key pulling in an anchored unpinned step is caught too" 1 "a.yml:6" "not pinned to a 40-hex commit SHA"

# A THIRD FALSE RED, FOUND BY ATTACKING THIS ROUND'S OWN FIX AND PINNED
# RATHER THAN FIXED. A node property may also sit on the VALUE side --
# `uses: &a actions/checkout@<40 hex>` -- where the ref extraction does
# not strip it, so `&a` is judged as the ref and a legitimately PINNED
# reference is reported unpinned. MEASURED: identical on the pre-fix
# script, so this round neither introduced it nor widened it.
#
# THE TRADE, WRITTEN DOWN. Stripping a property after `uses:` is a small
# edit to the same sed, and it was deliberately NOT made here. The ref
# extraction is the one expression in this gate where a mistake produces
# a WRONG REF rather than a wrong count -- a wrong count refuses, a
# wrong ref is judged. Widening it belongs in a change that can carry
# its own mutants, next to the value-side alias (`uses: *a`) which has
# the same shape. Recorded here so that whoever does it starts from a
# case rather than from a surprise.
#
# THIS COMMENT USED TO CLAIM THE DEFECT FAILS IN THE SAFE DIRECTION,
# "never the reverse". That universal is FALSE and the case after this
# one is the escape: a tag whose own text ends in `@` plus 40 hex is
# returned by `awk '{print $1}'`, satisfies the pin test, and the real
# reference is never judged -- exit 0 over `actions/checkout@v7`. A
# bound with its escape named beside it, not a guarantee.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - uses: &a actions/checkout@$SHA
EOF
run "PINNED COST: a property on the VALUE side false-reds a pinned ref" 1 "a.yml:5" "not pinned to a 40-hex commit SHA"

# THE DASH QUANTIFIER IS SPELLED TWO WAYS ACROSS THE FOUR SITES, AND THE
# CHECK ABOVE CANNOT SEE IT. Asked one level up -- what else here is
# keyed on a spelling rather than a property -- this is the answer, and
# it is the only divergence found: the counter admits
# `(-[[:space:]]+)*`, ZERO OR MORE dashes, while the ref extraction and
# the parser feed admit `(-[[:space:]]+)?`, zero or ONE. prop_verdict
# compares only the node-property group, so a suite that is green about
# the property group says nothing at all about this.
#
# It is NOT unified, and the reason is the direction it fails in. A
# nested block sequence -- `- - uses: x` -- is COUNTED by the counter
# and NOT extracted by the other two, so occurrences exceed parsed and
# the residue check REFUSES. Widening the extraction to `*` would make
# the gate claim to judge a shape it cannot attribute to a step;
# refusing is the safe direction and the one this gate is built on.
#
# The cost is a real false red, so it is pinned as a case in BOTH
# directions -- a nested sequence refuses whether or not its reference
# is pinned, which is what makes it a bound and not a judgement.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - - uses: actions/checkout@v7
EOF
run "PINNED COST: a nested sequence refuses (unpinned)" 2 "" "were not resolved to an action reference"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - - uses: actions/checkout@$SHA
EOF
run "PINNED COST: a nested sequence refuses even when PINNED" 2 "" "were not resolved to an action reference"

# THE ESCAPE FROM THE "SAFE DIRECTION" CLAIM, PINNED AS A CASE RATHER
# THAN LEFT AS A CORRECTED SENTENCE -- a rule with no observer is prose,
# and prose decays silently. The value-side property is returned by
# `awk '{print $1}'` and JUDGED as the ref. Usually that is a false red.
# When the property's own text ends in `@` plus 40 hex it is a false
# GREEN instead: the gate prints `all 2 ... are SHA-pinned` over
# `actions/checkout@v7`. MEASURED here and identical on the pre-fix
# script, so it is pre-existing rather than introduced.
#
# It asserts the WRONG answer on purpose, exactly like the two `?`
# cases above. Whoever strips a property from the VALUE side turns this
# case red, and that is the point: the fix has to arrive at a case, not
# at a surprise.
#
# THE BOUND ON THE EXPOSURE, measured rather than assumed -- and HALF OF
# WHAT THIS PARAGRAPH USED TO SAY WAS WRONG, which is why it now names
# which oracle answered what. It leaned on PyYAML refusing the tag.
# PyYAML does refuse it -- `could not determine a constructor for the tag
# '!a@<40 zeros>'` -- but that is not a bound, because the SECOND oracle
# disagrees: MEASURED with actionlint v1.7.12, the tagged scalar is read
# as a REAL action reference and the tag is ignored entirely. Given
# `!a@<40 zeros> actions/checkout` it answers `specifying action
# "actions/checkout" ... ref is missing`, the same diagnostic every shape
# case above rests on. So under the oracle this suite treats as modelling
# GitHub, the line below IS a live reference at the mutable ref `v7`, and
# the exit 0 is a real hole rather than a curiosity about a shape nothing
# would execute.
#
# WHAT STILL BOUNDS IT: no ordinary anchor or tag name ends in `@` plus
# 40 hex; the ref rule's first-`@` split narrows it further, since the
# property token must now carry exactly ONE `@` where any number of them
# used to do; and an adversary who can write this file already has the
# disclosed `?`-alone class, so it widens no reach. The clause saying
# actionlint had not been run against this shape is gone with the rest of
# it -- it has been now.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - uses: !a@$(printf '0%.0s' $(seq 1 40)) actions/checkout@v7
EOF
run "PINNED DEFECT: a 40-hex-tailed tag on the VALUE side passes (exit 0 is wrong)" 0 "all 2 'uses:'"

# THE PRICE OF THE WIDENING, PINNED AS A CASE rather than left for a
# reader to discover. This gate already false-REDS on a bare `uses:` at
# the start of a line inside a `run: |` block scalar (the case further
# down asserts the shapes that do NOT fire). Admitting a leading `!` or
# `&` token extends that false red: a block-scalar line whose FIRST
# token is such a word, followed by `uses:`, now goes red too. That is
# the noisy direction and it is deliberate -- but it is a real cost and
# it is asserted here so that a later widening cannot enlarge it
# silently. The control immediately after is what bounds it: the
# property must be the FIRST token, so ordinary shell does not fire.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - run: |
          ! uses: not-a-reference
EOF
run "PINNED COST: a '!' token before uses: in a run body false-reds" 1 "a.yml:6" "not pinned to a 40-hex commit SHA"

fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/setup-go@$SHA
      - run: |
          grep uses: f
          cmd & uses: x
          make TARGET=x && echo done
EOF
run "shell lines that merely CONTAIN & or ! still do not fire" 0 "all 1 'uses:'"

# THE PRESERVATION CONTROL FOR THAT COUNT. A refusal keyed on a bare
# `grep uses:` would fire on a step name, on a shell line in a `run:`
# body, and on this project's own gate scripts quoted in a workflow --
# a gate that cries wolf gets discharged, and then the refusal above is
# worth nothing. None of the four below is a reference and none may
# count. The real-tree case at the bottom of this file is what holds
# that claim: it runs the gate bare and requires exit 0, and exit 0
# already implies no file counted more than it parsed -- so the
# assertion is executable and re-derived every run. No tree-wide total
# is quoted here on purpose; the one that used to be went stale.
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

# AND THE ORDERING PRESERVATION CONTROL, which is a separate claim from
# the one above. Comments must be stripped BEFORE flow punctuation is
# split, not after. Splitting first also closes the quoted-`#` and
# `uses :` shapes -- MEASURED, it does -- but it promotes the tail of the
# comment below to a line of its own, where `uses:` stands at a key
# position and counts, and an ordinary comment becomes a refusal nobody
# can act on. A gate that cries wolf gets discharged, and the residue
# refusal above is then worth nothing. MEASURED both ways on this
# fixture: exit 0 with the order kept, exit 2 with it reversed.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      # note: pinned by sha, uses: actions/evil@v1 was the old one
      - uses: actions/checkout@$SHA
EOF
run "a comment holding a comma is still just a comment" 0 "all 1 'uses:'"

# A SECOND DEFECT PINNED AS A CASE, DELIBERATELY, so that "fixing" it
# has to confront what it would cost -- and unlike the pair above, this
# one is a false RED rather than a false green. A comma inside a quoted
# scalar splits
# like flow punctuation, so `echo "a, uses: b"` in a run body counts one
# occurrence the parser cannot resolve and REFUSES. That is a false red,
# and it is NOT new: MEASURED on the pre-fix script, `"a, uses: b"` and
# `- name: "a, uses: b"` already exited 2. What changed is that the
# quoted-`#` bug used to MASK one route to it -- `"issue #831, uses: x"`
# exited 0 before, because the comment expression ate the line, which is
# the same swallow that let a real reference through.
#
# Splitting only punctuation that is OUTSIDE quotes would need a scanner
# that tracks quote state, and every such scanner MEASURED here loses
# that state at a `\"` and strips from the `#` again -- trading this
# refusal for the fail-open the whole commit exists to close. For a gate
# whose count is a lower bound compared upward, the noisy direction is
# the correct one to keep. This case exists so that the trade is made on
# purpose rather than rediscovered.
fresh
cat > "$TMP/wf/a.yml" <<EOF
jobs:
  x:
    steps:
      - uses: actions/checkout@$SHA
      - run: |
          echo "a, uses: b"
EOF
run "a comma inside a quoted scalar over-counts, and refuses rather than passing" 2 "were not resolved"

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

# BOTH DISCOVERY ARMS ARE DRIVEN, AND BOTH WAYS ROUND. Inside a checkout
# the scan asks git, so that a maintainer's ignored trees -- whole other
# branches in git worktrees -- cannot produce a red that CI never sees.
# `--others` is there because a composite action added and not yet
# committed is precisely the case worth catching.
#
# This comment used to end here, and it was prose standing in for a case:
# it read as though both arms were covered on both sides, and only the
# find arm's basename precision actually had one. Mutating the find arm's
# `-name 'action.yml'` to `-name '*action.yml'` turned this suite red;
# mutating the git arm's `(^|/)action\.ya?ml$` to `action\.ya?ml` left
# all 31 cases passing. MEASURED, both ways. The near-miss case below
# closes that side.
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

# The near miss, INSIDE a checkout. `my-action.yml` ends in the basename
# and is not one; the find arm has this case above, the git arm did not,
# and its `(^|/)` anchor therefore had nothing holding it.
fresh
printf 'jobs:\n  x:\n    steps:\n      - uses: actions/checkout@%s\n' "$SHA" > "$TMP/wf/a.yml"
mkdir -p "$TMP/scan"
git -C "$TMP/scan" init -q 2>/dev/null
printf 'not a composite action\n' > "$TMP/scan/my-action.yml"
SCAN="$TMP/scan" run "a near miss inside a checkout is not a composite action" 0 "all 1 'uses:'"

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
FLOOR=82
if [ "$n" -lt "$FLOOR" ]; then
    echo "REFUSING: ran $n case(s), fewer than the $FLOOR this suite is known to hold."
    echo "  Either cases were lost, or the floor is stale and should be raised with them."
    exit 1
fi
echo "all $n case(s) passed"
