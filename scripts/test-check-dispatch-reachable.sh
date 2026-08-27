#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-dispatch-reachable.sh. The failure it guards is a
# workflow_dispatch workflow that exists on the working branch but not on
# the default branch: GitHub will not expose it, `gh workflow run` answers
# 404, and any documentation naming it as a route is wrong until the next
# release. capture-fixtures.yml shipped in exactly that state (#665).
#
# Cases run against a real throwaway repository rather than a stub tree,
# because the property under test is "is this path present on another
# git ref" — a mock would test the mock.
set -uo pipefail

CHECK="$(cd "$(dirname "$0")" && pwd)/check-dispatch-reachable.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

# THE LEDGER'S ENFORCEMENT CLASSES ARE DRIVEN, NOT READ (#849). The
# ledger header said "the gate enforces all of them"; one of its four
# rules -- that the workflow's documentation must say it is unavailable
# -- was enforced by nothing, in the PR whose subject was claims in that
# file wider than the code behind them. Narrowing the sentence would
# have been another sentence.
#
# So each rule now carries `[enforced: id]` or `[unenforced: id]`, and
# the cases below are the demonstrations. `demo` records an id ONLY when
# the case it wraps actually produced the verdict its class claims, so a
# demonstration that silently stopped running cannot satisfy the
# correspondence check at the end of this file. The two sets are then
# compared with the tags parsed out of the real ledger, in both
# directions: an untagged rule, a rule whose id has no demonstration, or
# a demonstration for an id no rule declares is a failure.
#
# Keyed on which SET a rule is in and on the gate's observed verdict --
# never on the wording of the rule, which is free to be rewritten. A
# guard keyed on the spelling of that header sentence would reproduce
# exactly the silence it exists to prevent.
DEMO_ENFORCED=""
DEMO_UNENFORCED=""
demo() {
    local class="$1" id="$2" desc="$3" want="$4" got="$5"
    check "$desc" "$want" "$got"
    [ "$got" = "$want" ] || return 0
    case "$class" in
        enforced)   DEMO_ENFORCED="$DEMO_ENFORCED $id" ;;
        unenforced) DEMO_UNENFORCED="$DEMO_UNENFORCED $id" ;;
        *) echo "FAIL: demo called with unknown class '$class'"; fails=1 ;;
    esac
}

REPO="$TMP/repo"
mkdir -p "$REPO/.github/workflows"
git -C "$REPO" init -q -b main
git -C "$REPO" config user.email t@example.com
git -C "$REPO" config user.name t
git -C "$REPO" config commit.gpgsign false

# A well-formed ledger entry. Bare paths stopped being legal in #849, so
# every fixture that declares a workflow has to write one -- including the
# fixtures that predate the rule, which is the half of a rule change that
# gets forgotten.
entry() {
    printf '%s\n    Reason:  a test fixture.\n    Clears:  never; this is a test.\n    Triggers: %s\n' \
        "$1" "${2:-workflow_dispatch}"
}

dispatchable() { printf 'name: %s\non:\n  workflow_dispatch:\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' "$1"; }
push_only()    { printf 'name: %s\non:\n  push:\n    branches: [main]\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' "$1"; }

# main carries one dispatchable workflow.
dispatchable released > "$REPO/.github/workflows/released.yml"
git -C "$REPO" add -A && git -C "$REPO" commit -qm base
git -C "$REPO" checkout -q -b work

verdict() {
    ( cd "$REPO" && BASE_REF=main bash "$CHECK" >"$TMP/out" 2>&1 ) \
        && echo pass || echo "rc$?"
}

# --- the baseline ------------------------------------------------------
check "a workflow present on the default branch passes" pass "$(verdict)"

# --- the directory scan is part of the check (#832) --------------------
# The gate reads `*.yml` AND `*.yaml`, but every fixture in this file is
# a `.yml`, so narrowing the scan to one extension passed the whole
# suite. GitHub Actions honours both and `.github/workflows/` holds a
# `.yaml` today, so half the corpus could fall out of the domain in
# silence. Run it here, while the tree is otherwise clean, so the verdict
# can only come from the planted file — and restore the baseline after.
#
# ORTHOGONALITY: the narrowed scan is reproduced and asserted to ACCEPT
# this fixture. Without that, a case that merely fails proves nothing
# about which half of the glob did the work.
dispatchable planted > "$REPO/.github/workflows/planted.yaml"
narrowed="$TMP/narrowed.sh"
sed -e 's|^WF_FILES=(.*)$|WF_FILES=("$WF_DIR"/*.yml)|' "$CHECK" > "$narrowed"
if ( cd "$REPO" && BASE_REF=main bash "$narrowed" >/dev/null 2>&1 ); then
    echo "PASS: a *.yml-only scan accepts the planted .yaml (orthogonality confirmed)"
else
    echo "FAIL: the *.yml-only scan did not accept the planted .yaml, so the case"
    echo "      below would go red for some other reason"
    fails=1
fi
check "a dispatchable .yaml workflow is inspected too" rc1 "$(verdict)"
grep -F 'planted.yaml' "$TMP/out" >/dev/null \
    && echo "PASS: and the .yaml file is the one reported" \
    || { echo "FAIL: the .yaml workflow was not named in the output"; fails=1; }
rm -f "$REPO/.github/workflows/planted.yaml" "$narrowed"
check "removing it restores the baseline" pass "$(verdict)"

# --- the defect --------------------------------------------------------
dispatchable newone > "$REPO/.github/workflows/newone.yml"
check "a dispatchable workflow absent from the default branch fails" rc1 "$(verdict)"

grep -F 'not on main' "$TMP/out" >/dev/null \
    && echo "PASS: the message names the default branch" \
    || { echo "FAIL: the message does not name the default branch"; fails=1; }

# --- declaring it is the release valve ---------------------------------
{ printf '# reason: lands in the next release\n'
  entry .github/workflows/newone.yml
} > "$REPO/.github/dispatch-pending.txt"
check "declaring it passes" pass "$(verdict)"

# --- and the declaration has to stay true ------------------------------
git -C "$REPO" add -A && git -C "$REPO" commit -qm add-newone
git -C "$REPO" checkout -q main
git -C "$REPO" merge -q work
git -C "$REPO" checkout -q work
demo enforced stale-entry-pruned \
    "a declaration for a workflow now on the default branch fails" rc1 "$(verdict)"

grep -F 'stopped meaning anything' "$TMP/out" >/dev/null \
    && echo "PASS: the stale message says to remove it" \
    || { echo "FAIL: stale entry message missing"; fails=1; }

# --- scope: only dispatchable workflows are in scope -------------------
rm -f "$REPO/.github/dispatch-pending.txt"
push_only pushonly > "$REPO/.github/workflows/pushonly.yml"
check "a non-dispatchable workflow absent from the default branch is out of scope" pass "$(verdict)"

# --- an unreadable default branch is NOT a pass ------------------------
dispatchable another > "$REPO/.github/workflows/another.yml"
out=$( cd "$REPO" && BASE_REF=refs/heads/no-such-branch bash "$CHECK" 2>&1 )
rc=$?
check "an unreadable default branch exits 0" 0 "$rc"
printf '%s\n' "$out" | grep -F 'NOT INSPECTED' >/dev/null \
    && echo "PASS: and says NOT INSPECTED rather than passing silently" \
    || { echo "FAIL: silent pass on an unreadable default branch"; fails=1; }

# --- cannot-check is distinct from broken ------------------------------
out_rc=$( cd "$REPO" && BASE_REF=main bash "$CHECK" .github/nope >/dev/null 2>&1; echo $? )
check "a missing workflow directory is rc2, not rc1" 2 "$out_rc"

# --- allowlist parsing --------------------------------------------------
# Two workflows missing from the default branch, one declared and one
# not: the declared one must be accepted THROUGH the comments and blank
# lines, and the undeclared one must still be reported. A single-entry
# case cannot tell "parsed the file" from "ignored the file".
dispatchable undeclared > "$REPO/.github/workflows/undeclared.yml"
{ printf '\n# a comment\n\n'
  entry .github/workflows/another.yml
} > "$REPO/.github/dispatch-pending.txt"
check "an undeclared workflow alongside a declared one fails" rc1 "$(verdict)"
grep -F 'undeclared.yml' "$TMP/out" >/dev/null \
    && echo "PASS: and the undeclared one is the one reported" \
    || { echo "FAIL: undeclared workflow not reported"; fails=1; }
grep -F 'another.yml' "$TMP/out" >/dev/null \
    && { echo "FAIL: the declared entry was not honoured through comments"; fails=1; } \
    || echo "PASS: the declared entry was honoured through comments and blanks"

# --- an empty directory is not a clean bill of health (#743) ------------
# A MISSING directory was already rc2; an EMPTY one passed, printing
# "PASS  every workflow_dispatch workflow is on origin/main" having read
# no files at all. Both of these fail against the pre-#743 gate.
mkdir -p "$TMP/emptywf"
empty_rc=$( cd "$REPO" && BASE_REF=main bash "$CHECK" "$TMP/emptywf" >/dev/null 2>&1; echo $? )
check "an empty workflow directory is rc2, not a pass" 2 "$empty_rc"

# --- and neither is a directory where nothing is dispatchable -----------
# Zero subjects out of N files is the shape a BROKEN DETECTOR takes. It
# is a legitimate answer today, so the gate states it instead of folding
# it into a PASS — the point is that it can never again be silent.
mkdir -p "$TMP/nodispatch"
push_only only > "$TMP/nodispatch/only.yml"
none_rc=$( cd "$REPO" && BASE_REF=main bash "$CHECK" "$TMP/nodispatch" >/dev/null 2>&1; echo $? )
check "a directory with no dispatchable workflow is rc2, not a pass" 2 "$none_rc"

# --- the inline `on:` spellings are workflows too (#743) ----------------
# The comment above the detector said "`on:` may be block or inline";
# the pattern was '^[[:space:]]*workflow_dispatch:' and matched only the
# block form. Every workflow in the tree happens to use the block form,
# so this was latent — and a latent blind spot in a gate that ALSO
# passed over an empty input set is how a reformat silently retires a
# check. GitHub accepts all three spellings below.
REPO2="$TMP/repo2"
mkdir -p "$REPO2/.github/workflows"
git -C "$REPO2" init -q -b main
git -C "$REPO2" config user.email t@example.com
git -C "$REPO2" config user.name t
git -C "$REPO2" config commit.gpgsign false
dispatchable onmain > "$REPO2/.github/workflows/onmain.yml"
git -C "$REPO2" add -A && git -C "$REPO2" commit -qm base
git -C "$REPO2" checkout -q -b work

verdict2() {
    ( cd "$REPO2" && BASE_REF=main bash "$CHECK" >"$TMP/out2" 2>&1 ) \
        && echo pass || echo "rc$?"
}
check "the baseline repo passes" pass "$(verdict2)"

printf 'name: seq\non: [workflow_dispatch]\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' \
    > "$REPO2/.github/workflows/seq.yml"
check "a flow-sequence 'on: [workflow_dispatch]' is detected" rc1 "$(verdict2)"
grep -F 'seq.yml' "$TMP/out2" >/dev/null \
    && echo "PASS: and the flow-sequence workflow is the one named" \
    || { echo "FAIL: flow-sequence workflow not named"; fails=1; }
rm -f "$REPO2/.github/workflows/seq.yml"

printf 'name: map\non: {workflow_dispatch: null}\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' \
    > "$REPO2/.github/workflows/map.yml"
check "a flow-mapping 'on: {workflow_dispatch: null}' is detected" rc1 "$(verdict2)"
grep -F 'map.yml' "$TMP/out2" >/dev/null \
    && echo "PASS: and the flow-mapping workflow is the one named" \
    || { echo "FAIL: flow-mapping workflow not named"; fails=1; }
rm -f "$REPO2/.github/workflows/map.yml"

# The other direction: widening a detector must not make it match prose.
# A workflow that only TALKS about workflow_dispatch does not declare it,
# and reporting it would be a false failure on a file nobody can fix.
{ push_only prose; printf '# this one is not run by workflow_dispatch on purpose\n'; } \
    > "$REPO2/.github/workflows/prose.yml"
check "a workflow merely mentioning workflow_dispatch in prose is out of scope" pass "$(verdict2)"

# --- THE LEDGER'S OWN RULES (#849) --------------------------------------
# The header said "An entry requires a reason and what clears it. No bare
# paths." Nothing read an entry for content, so stripping the Reason and
# Clears blocks off the real entry produced a BYTE-IDENTICAL pass. These
# cases are that finding, one mutation at a time.
REPO3="$TMP/repo3"
mkdir -p "$REPO3/.github/workflows"
git -C "$REPO3" init -q -b main
git -C "$REPO3" config user.email t@example.com
git -C "$REPO3" config user.name t
git -C "$REPO3" config commit.gpgsign false
dispatchable base3 > "$REPO3/.github/workflows/base3.yml"
git -C "$REPO3" add -A && git -C "$REPO3" commit -qm base
git -C "$REPO3" checkout -q -b work

# A workflow with BOTH default-branch-only triggers -- the shape #846 was
# about, where reasoning about which one survives is the trap.
printf 'name: cron\non:\n  workflow_dispatch:\n  schedule:\n    - cron: "0 3 * * *"\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' \
    > "$REPO3/.github/workflows/cron.yml"

led() { cat > "$REPO3/.github/dispatch-pending.txt"; }
verdict3() {
    ( cd "$REPO3" && BASE_REF=main bash "$CHECK" >"$TMP/out3" 2>&1 ) \
        && echo pass || echo "rc$?"
}

# the control FIRST: a complete, correct entry passes. Without it every
# case below could be failing for a reason that has nothing to do with
# the mutation.
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
EOF
check "a complete entry passes (control for the mutations below)" pass "$(verdict3)"

led <<'EOF'
.github/workflows/cron.yml
EOF
check "a BARE PATH fails -- the #849 finding" rc1 "$(verdict3)"
grep -F "no 'Reason:'" "$TMP/out3" >/dev/null \
    && echo "PASS: and it names the missing reason" \
    || { echo "FAIL: a bare path was rejected for some other reason"; fails=1; }

led <<'EOF'
.github/workflows/cron.yml
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
EOF
demo enforced reason-and-clears \
    "an entry with no Reason fails" rc1 "$(verdict3)"

led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Triggers: workflow_dispatch schedule
EOF
demo enforced reason-and-clears \
    "an entry with no Clears fails" rc1 "$(verdict3)"

# THE #846 CLAIM, in structured form. The false sentence was "the schedule
# works from any branch". Keyed on the file's triggers, the entry that
# omits `schedule` is refused no matter how it is worded.
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle. The schedule works from any branch, so only
             the manual trigger is dead here.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
EOF
demo enforced derived-triggers \
    "omitting a trigger the workflow declares fails, however fluent the reason" rc1 "$(verdict3)"
# The gate sorts the set, so the remediation string is the sorted form.
# Asserting the exact sentence rather than "schedule appears somewhere"
# keeps the message a paste-able fix rather than a hint.
grep -F "Write 'Triggers: schedule workflow_dispatch'" "$TMP/out3" >/dev/null \
    && echo "PASS: and the message hands back the exact line to write" \
    || { echo "FAIL: the derived trigger set is not in the message"; fails=1; }

# THE OPPOSITE DIRECTION. A guard fails in one direction until something
# checks the other: an entry that CLAIMS a dead trigger the workflow does
# not have is also false, and would let a copied entry drift unnoticed.
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule push
EOF
check "claiming a trigger the workflow does NOT declare also fails" rc1 "$(verdict3)"

# A workflow with only workflow_dispatch must not be required to claim a
# schedule -- otherwise the rule is "write both words", not "say what is
# true", and every entry would pass by boilerplate.
dispatchable manual3 > "$REPO3/.github/workflows/manual3.yml"
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
.github/workflows/manual3.yml
    Reason:  also new.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
EOF
check "a dispatch-only workflow needs only workflow_dispatch" pass "$(verdict3)"

# THE JUNK-TOKEN CLASS. The old parser took `awk '{print $1}'` of every
# non-comment line, so the first word of a prose continuation became a
# declared path. It was harmless only because no sentence in the file
# happened to begin with one. Here one does, and the workflow it names
# must still be reported as undeclared.
dispatchable sneaky > "$REPO3/.github/workflows/sneaky.yml"
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle. See also
             .github/workflows/sneaky.yml which is a different matter.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
.github/workflows/manual3.yml
    Reason:  also new.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
EOF
check "a path inside prose does NOT declare a workflow" rc1 "$(verdict3)"
grep -F 'sneaky.yml' "$TMP/out3" >/dev/null \
    && echo "PASS: and the workflow named only in prose is reported undeclared" \
    || { echo "FAIL: a prose continuation silently declared a workflow"; fails=1; }
rm -f "$REPO3/.github/workflows/sneaky.yml"

# An entry for a file that no longer exists declares nothing.
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
.github/workflows/manual3.yml
    Reason:  also new.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
.github/workflows/deleted.yml
    Reason:  it used to be here.
    Clears:  never.
    Triggers: workflow_dispatch
EOF
check "an entry for a file that does not exist fails" rc1 "$(verdict3)"

# NON-VACUITY ON THE PARSER. A ledger with content that parses to nothing
# means the format moved; reading that as "nothing is declared" would let
# every workflow through while reporting a clean failure for each.
led <<'EOF'
    .github/workflows/cron.yml
    Reason:  indented, so there is no entry here at all.
EOF
check "a ledger that parses to no entries is rc2, not a verdict" 2 "$(verdict3 | sed 's/^rc//;s/^pass$/0/')"

# --- #849 review: an ABSENT field with a PRESENT one after it ----------
#
# The suite had no case where one field is missing and a later one is
# there, which is exactly why this shipped. The parser emitted
# tab-separated fields and the reader split on tab, which is IFS
# whitespace: a run of separators collapsed, every later value shifted
# left, and an empty Reason made Clears read the TRIGGERS text while
# Triggers read empty. Empty is the CORRECT derived answer for a
# push-only workflow, so the mismatch rule agreed and the entry passed.
#
# Two rules disabled together by the absence of one of them. Driven with
# ONE VARIABLE MOVED: the same entry with a Reason added.
#
# manual3.yml goes first: it is left over from the cases above and an
# UNDECLARED dispatchable workflow fails the gate on its own. Measured
# by deleting this `rm -f` and diffing the suite output: EVERY
# `pass`-expecting case below goes red, and every `rc1`-expecting case
# below stays GREEN -- green for a reason that is not its own mutation,
# which is the more dangerous half and the one that leaves no trace.
# The `rm -f` is load-bearing in both directions.
#
# STATED AS A PROPERTY, NOT A COUNT, ON PURPOSE. This comment first
# said "exactly two cases go red", which was true when it was written
# and false an hour later, when this same review added a third
# pass-expecting case to the derivation block below. A count of a
# population that later grows is a claim that decays silently -- the
# defect this whole PR is about, committed in the sentence describing
# it. The assertion below is the part that cannot go stale.
rm -f "$REPO3/.github/workflows/manual3.yml"
[ -e "$REPO3/.github/workflows/manual3.yml" ] \
    && { echo "FAIL: the leftover fixture is still present, so every case below"
         echo "      measures the gate's verdict on IT, not on the mutation"; fails=1; } \
    || echo "PASS: the leftover fixture from the cases above is gone"
push_only pushonly > "$REPO3/.github/workflows/pushonly.yml"

led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
.github/workflows/pushonly.yml
    Clears:  never.
    Triggers: schedule
EOF
check "an entry with NO Reason and a false Triggers fails" rc1 "$(verdict3)"
grep -F "no 'Reason:'" "$TMP/out3" >/dev/null \
    && echo "PASS: and the missing Reason is named" \
    || { echo "FAIL: the missing Reason is not named"; fails=1; }
grep -F "but its entry says [schedule]" "$TMP/out3" >/dev/null \
    && echo "PASS: and the false Triggers is named in the same run" \
    || { echo "FAIL: the false Triggers is not named"; fails=1; }

# the one-variable control: adding the Reason must NOT be what makes the
# false Triggers visible. Before the fix it was.
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
.github/workflows/pushonly.yml
    Reason:  because.
    Clears:  never.
    Triggers: schedule
EOF
check "adding a Reason changes nothing about the Triggers verdict" rc1 "$(verdict3)"

# ...and the remedy for a workflow with no default-branch-only trigger
# must not tell the reader to write an empty line.
grep -F "declares NO default-branch-only trigger" "$TMP/out3" >/dev/null \
    && echo "PASS: and the remedy says the claim is unsupported, not 'write an empty list'" \
    || { echo "FAIL: the empty-derivation remedy is nonsense"; fails=1; }

# A MISSING Clears MUST NAME Clears, and must not manufacture a trigger
# mismatch whose remedy line is already in the file.
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Triggers: workflow_dispatch schedule
EOF
rm -f "$REPO3/.github/workflows/pushonly.yml"
check "an entry missing only Clears fails" rc1 "$(verdict3)"
grep -F "no 'Clears:'" "$TMP/out3" >/dev/null \
    && echo "PASS: and it names the rule actually broken" \
    || { echo "FAIL: the missing Clears is not named"; fails=1; }
grep -F 'declares triggers' "$TMP/out3" >/dev/null \
    && { echo "FAIL: a missing Clears still manufactures a trigger mismatch"; fails=1; } \
    || echo "PASS: and it does not manufacture a trigger mismatch"

# --- #849 review: the trigger set comes from `on:`, not from the file ---
#
# dead_triggers grepped the whole file, so a header comment saying the
# workflow deliberately has NO schedule derived one. The only way to
# green was to write a false claim about the workflow into the ledger --
# the failure #846 was about, manufactured by the gate built to prevent
# it.
cat > "$REPO3/.github/workflows/commented.yml" <<'EOF'
# This workflow deliberately has no schedule: a daily run would cost
# pool time for nothing, and a schedule here would fire on main only.
name: commented
on:
  workflow_dispatch:
jobs:
  a:
    runs-on: ubuntu-latest
    steps: [{run: "true"}]
EOF
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
.github/workflows/commented.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
EOF
check "a truthful entry passes beside a comment that mentions schedule" pass "$(verdict3)"

# The opposite direction, or the case above would also pass with the
# derivation deleted: a REAL schedule inside `on:` must still be derived.
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
.github/workflows/commented.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
EOF
check "a real schedule in on: is still derived" rc1 "$(verdict3)"
grep -F "Write 'Triggers: schedule workflow_dispatch'" "$TMP/out3" >/dev/null \
    && echo "PASS: and cron.yml is the one named" \
    || { echo "FAIL: the real schedule was not derived"; fails=1; }

# And a `schedule:` key BELOW the on: block -- inside jobs -- is not a
# trigger either. `jobs:` is a column-zero key, so it closes on:.
cat > "$REPO3/.github/workflows/jobkey.yml" <<'EOF'
name: jobkey
on:
  workflow_dispatch:
jobs:
  schedule:
    runs-on: ubuntu-latest
    steps: [{run: "true"}]
EOF
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
.github/workflows/commented.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
.github/workflows/jobkey.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
EOF
check "a job NAMED schedule is not a trigger" pass "$(verdict3)"

# ...and a comment INSIDE the `on:` block is not a trigger either. This
# is the previous defect one scope smaller: narrowing the scan from the
# whole file to the `on:` mapping still counted comment text within the
# mapping, so a trailing `# not on a schedule: manual only` derived
# [schedule workflow_dispatch] and the only way to green was, again, to
# write a false claim into the ledger.
#
# BOTH STRIPS ARE LOAD-BEARING AND THE FIXTURE HAS TO PROVE IT. The
# first version of this fixture used an INDENTED whole-line comment,
# which the trailing-comment rule already removes -- deleting the
# whole-line strip left the case green. The comment below is at COLUMN
# ZERO, which no amount of trailing-comment stripping touches, and
# `jobs:` has not closed the block yet because a `#` line is not a
# column-zero key. Scored one strip at a time: each deletion now goes
# red on its own.
cat > "$REPO3/.github/workflows/inblock.yml" <<'EOF'
name: inblock
on:
  workflow_dispatch:  # not on a schedule: manual only
# a stray column-zero note, no schedule: manual only
jobs:
  a:
    runs-on: ubuntu-latest
    steps: [{run: "true"}]
EOF
led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch schedule
.github/workflows/commented.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
.github/workflows/jobkey.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
.github/workflows/inblock.yml
    Reason:  new this cycle.
    Clears:  when it reaches main.
    Triggers: workflow_dispatch
EOF
# ORTHOGONALITY, the same bargain as the *.yml-only scan above: a case
# that merely passes proves nothing unless the un-stripped derivation is
# shown to REJECT this fixture. Without it, deleting the strip would
# leave this case green and the finding unmeasured.
unstripped="$TMP/unstripped.sh"
sed -e '/sub(\/\^\[\[:space:\]\]\*#/d' -e '/sub(\/\[\[:space:\]\]+#/d' "$CHECK" > "$unstripped"
if ( cd "$REPO3" && BASE_REF=main bash "$unstripped" >/dev/null 2>&1 ); then
    echo "FAIL: the un-stripped derivation accepted the in-block comment, so the"
    echo "      case below would be green whether or not comments are stripped"
    fails=1
else
    echo "PASS: the un-stripped derivation rejects the in-block comment (orthogonality confirmed)"
fi
check "a comment inside the on: block is not a trigger" pass "$(verdict3)"
rm -f "$unstripped" "$REPO3/.github/workflows/inblock.yml"

rm -f "$REPO3/.github/workflows/commented.yml" "$REPO3/.github/workflows/jobkey.yml"

rm -f "$REPO3/.github/workflows/cron.yml" "$REPO3/.github/workflows/manual3.yml" \
      "$REPO3/.github/dispatch-pending.txt"

# --- THE BOUNDARY, DRIVEN (#849) ----------------------------------------
#
# The ledger's third rule is that the workflow's documentation must say
# it is not available yet. NOTHING CHECKS THAT, and the header used to
# read as though the gate did. This case is the boundary itself: a tree
# with a complete, truthful ledger entry AND a doc page presenting the
# 404-ing workflow as its primary route -- the #665 failure verbatim --
# and the gate PASSES it.
#
# A case asserting the un-enforced answer is deliberate, not a weakened
# assertion. It pins a stated limit: the day someone teaches the gate to
# read documentation, this goes red and the `[unenforced:]` tag in the
# ledger has to move with it. That is the whole point -- the tag and the
# behaviour cannot drift apart in silence.
REPO4="$TMP/repo4"
mkdir -p "$REPO4/.github/workflows" "$REPO4/docs"
git -C "$REPO4" init -q -b main
git -C "$REPO4" config user.email t@example.com
git -C "$REPO4" config user.name t
git -C "$REPO4" config commit.gpgsign false
dispatchable base4 > "$REPO4/.github/workflows/base4.yml"
git -C "$REPO4" add -A && git -C "$REPO4" commit -qm base
git -C "$REPO4" checkout -q -b work

dispatchable newwf > "$REPO4/.github/workflows/newwf.yml"
printf 'Run `gh workflow run newwf.yml`. This is the PRIMARY route and works right now.\n' \
    > "$REPO4/docs/internals.md"
entry .github/workflows/newwf.yml > "$REPO4/.github/dispatch-pending.txt"

demo unenforced docs-say-unavailable \
    "documentation calling a pending workflow the primary route is NOT caught" \
    pass \
    "$( ( cd "$REPO4" && BASE_REF=main bash "$CHECK" >/dev/null 2>&1 ) && echo pass || echo "rc$?" )"

# --- the ledger's classes must match what was just demonstrated ---------
#
# Read the tags out of the REAL ledger and compare them, in both
# directions, with the ids recorded by the driven cases above. This is
# what makes the header's enforcement claim a checked statement instead
# of a sentence: an untagged rule, a rule claiming a class no case
# demonstrates, or a demonstration for a rule that no longer exists all
# go red.
LEDGER="$(cd "$(dirname "$CHECK")/.." && pwd)/.github/dispatch-pending.txt"
rules_block=$(awk '/^#[[:space:]]*Rules[.,[:space:]]/ { b = 1 }
                   b && /^[^#]/                       { exit }
                   b' "$LEDGER")
bullets=$(printf '%s\n' "$rules_block" | grep -E '^#[[:space:]]+-[[:space:]]')

# NON-VACUITY. Every assertion below is universally quantified over the
# bullets, so an empty bullet set satisfies all of them while checking
# nothing -- the way a universal gate is satisfied by emptying its
# domain. A moved header or a renamed marker must be loud here.
if [ -z "$rules_block" ] || [ -z "$bullets" ]; then
    echo "FAIL: no rules block or no rule bullets found in $LEDGER —"
    echo "      the class correspondence below would pass having read nothing"
    fails=1
else
    n_bullets=$(printf '%s\n' "$bullets" | grep -c .)
    untagged=$(printf '%s\n' "$bullets" | grep -vE '\[(enforced|unenforced): [a-z0-9-]+\]')
    if [ -z "$untagged" ]; then
        echo "PASS: all $n_bullets ledger rules declare an enforcement class"
    else
        echo "FAIL: a ledger rule declares no enforcement class:"
        printf '      %s\n' "$untagged"
        fails=1
    fi

    tags() { printf '%s\n' "$bullets" | sed -n "s/.*\[$1: \([a-z0-9-]*\)\].*/\1/p" | sort -u | tr '\n' ' ' | sed 's/ $//'; }
    norm() { printf '%s' "$1" | tr ' ' '\n' | grep -v '^$' | sort -u | tr '\n' ' ' | sed 's/ $//'; }

    check "the ledger's [enforced:] rules are exactly the ones demonstrated to fail" \
        "$(tags enforced)" "$(norm "$DEMO_ENFORCED")"
    check "the ledger's [unenforced:] rules are exactly the ones demonstrated to pass" \
        "$(tags unenforced)" "$(norm "$DEMO_UNENFORCED")"
fi

# --- the real repository ------------------------------------------------
# The shipped state must satisfy its own gate.
real=$( cd "$(dirname "$CHECK")/.." && bash "$CHECK" >/dev/null 2>&1 && echo pass || echo "rc$?" )
check "this repository passes its own dispatch-reachability gate" pass "$real"

exit "$fails"
