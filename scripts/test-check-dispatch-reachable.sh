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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

CHECK="$(cd "$(dirname "$0")" && pwd)/check-dispatch-reachable.sh"
guarded_tmpdir TMP
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

# --- A LEDGER WITH NO ENTRIES AT ALL IS A LEGITIMATE STATE --------------
# The case above covers a ledger with CONTENT that parses to nothing: the
# format moved, and that is rc2. This is a DIFFERENT state, and it is the
# one the tree entered when the last pending entries were pruned after
# their workflows reached the default branch — comments only, so zero
# non-comment lines, the vacuity guard correctly does not fire, and the
# allowlist is simply empty. Nothing covered it. The two states are one
# `grep -c` apart inside the gate.
#
# Both directions are driven, because an empty allowlist that had quietly
# become a blanket pass would print the same clean line as a healthy tree,
# which is the shape this gate exists to refuse.
#
# It gets its OWN fixture rather than reusing repo3: repo3 accumulates
# off-main workflows from the cases above it, so a case written there
# would be asserting on which neighbours happened to run first, and would
# change meaning when one of them is edited.
REPO5="$TMP/repo5"
mkdir -p "$REPO5/.github/workflows"
git -C "$REPO5" init -q -b main
git -C "$REPO5" config user.email t@example.com
git -C "$REPO5" config user.name t
git -C "$REPO5" config commit.gpgsign false
dispatchable onmain5 > "$REPO5/.github/workflows/onmain5.yml"
printf '# every entry has been pruned; nothing is pending.\n' \
    > "$REPO5/.github/dispatch-pending.txt"
git -C "$REPO5" add -A && git -C "$REPO5" commit -qm base
git -C "$REPO5" checkout -q -b work

verdict5() {
    ( cd "$REPO5" && BASE_REF=main bash "$CHECK" >"$TMP/out5" 2>&1 ) \
        && echo pass || echo "rc$?"
}
check "a comment-only ledger with nothing pending passes, not rc2" pass "$(verdict5)"

dispatchable pending5 > "$REPO5/.github/workflows/pending5.yml"
check "a comment-only ledger still fails an undeclared workflow" rc1 "$(verdict5)"
grep -F 'pending5.yml' "$TMP/out5" >/dev/null \
    && echo "PASS: and the undeclared workflow is still named" \
    || { echo "FAIL: undeclared workflow not named under an empty ledger"; fails=1; }

rm -f "$REPO5/.github/workflows/pending5.yml"
check "and removing it returns the empty ledger to a pass (control)" pass "$(verdict5)"

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
# THE DOMAIN OF THESE CHECKS IS ITSELF A KEYED PATTERN, so it is a
# FUNCTION OF A LEDGER FILE rather than three inline greps over the real
# one. That is not tidiness: the first version derived the domain from
# `^#[[:space:]]+-[[:space:]]` inline, which meant nothing could drive
# the domain with a planted rule, and a rule written `*` sat outside all
# three checks in silence -- the suite printed "all 4 ledger rules
# declare an enforcement class" over a ledger holding five, one of them
# untagged. A guard against claims wider than their code, keyed on the
# spelling of a list marker. Taking a file as an argument is what lets
# the cases below plant a rule and observe the domain.
ledger_rules() {
    awk '/^#[[:space:]]*Rules[.,[:space:]]/ { b = 1 }
         b && /^[^#]/                       { exit }
         b' "$1" | grep -E '^#[[:space:]]+[-*+][[:space:]]'
}
ledger_block() {
    awk '/^#[[:space:]]*Rules[.,[:space:]]/ { b = 1 }
         b && /^[^#]/                       { exit }
         b' "$1"
}
ledger_untagged() {
    ledger_rules "$1" | grep -vE '\[(enforced|unenforced): [a-z0-9-]+\]'
}
ledger_tags() {
    ledger_rules "$1" | sed -n "s/.*\[$2: \([a-z0-9-]*\)\].*/\1/p" \
        | sort -u | tr '\n' ' ' | sed 's/ $//'
}
norm() { printf '%s' "$1" | tr ' ' '\n' | grep -v '^$' | sort -u | tr '\n' ' ' | sed 's/ $//'; }

LEDGER="$(cd "$(dirname "$CHECK")/.." && pwd)/.github/dispatch-pending.txt"

# NON-VACUITY. Every assertion below is universally quantified over the
# rules, so an empty rule set satisfies all of them while checking
# nothing -- the way a universal gate is satisfied by emptying its
# domain. A moved header or a renamed marker must be loud here.
if [ -z "$(ledger_block "$LEDGER")" ] || [ -z "$(ledger_rules "$LEDGER")" ]; then
    echo "FAIL: no rules block or no rule lines found in $LEDGER —"
    echo "      the class correspondence below would pass having read nothing"
    fails=1
else
    n_rules=$(ledger_rules "$LEDGER" | grep -c .)
    untagged=$(ledger_untagged "$LEDGER")
    if [ -z "$untagged" ]; then
        echo "PASS: all $n_rules ledger rules declare an enforcement class"
    else
        echo "FAIL: a ledger rule declares no enforcement class:"
        printf '      %s\n' "$untagged"
        fails=1
    fi

    check "the ledger's [enforced:] rules are exactly the ones demonstrated to fail" \
        "$(ledger_tags "$LEDGER" enforced)" "$(norm "$DEMO_ENFORCED")"
    check "the ledger's [unenforced:] rules are exactly the ones demonstrated to pass" \
        "$(ledger_tags "$LEDGER" unenforced)" "$(norm "$DEMO_UNENFORCED")"
fi

# --- the DOMAIN of those three checks, driven (#849 review 4) -----------
#
# Ground 1 of the fourth hold: the ledger claimed, unqualified, that "a
# new rule cannot be added without declaring which class it is in". It
# could. Both halves are driven here against a planted copy of the real
# ledger, one rule at a time.
plant_rule() {   # <marker-or-empty> -> path to a ledger copy with an untagged rule in it
    local marker="$1" out="$TMP/planted-ledger.txt" ins
    if [ -n "$marker" ]; then
        ins="#   $marker A planted rule with no enforcement class."
    else
        ins="#     A planted rule with no enforcement class."
    fi
    # INSIDE the Rules block, not merely before the file's first `- `
    # line. The retraction block higher up this ledger is also written
    # as `- ` bullets, so a plant keyed on "first list line in the file"
    # lands outside the domain and every case below goes red for a
    # reason that is not the thing under test. It did, on the first run.
    awk -v ins="$ins" '
        /^#[[:space:]]*Rules[.,[:space:]]/                    { inb = 1 }
        inb && !done && /^#[[:space:]]+[-*+][[:space:]]/      { print ins; done = 1 }
        { print }
    ' "$LEDGER" > "$out"
    printf '%s' "$out"
}

# REMEDY A: every list marker is in the domain. Driven per marker, so a
# narrowing back to any single one goes red on the markers it drops.
for m in '-' '*' '+'; do
    planted=$(plant_rule "$m")
    if [ -n "$(ledger_untagged "$planted")" ]; then
        echo "PASS: an untagged rule written with '$m' is caught"
    else
        echo "FAIL: an untagged rule written with '$m' is invisible to the class check"
        fails=1
    fi
done

# THE BOUNDARY, and it is a real one rather than a miss to be patched.
# A rule written with NO list marker is indistinguishable from a
# CONTINUATION LINE of the rule above it -- and every rule in that block
# has continuations, which is why the domain cannot simply be "every
# line". This case asserts the miss on purpose, exactly as the
# docs-say-unavailable boundary above does: if anyone ever makes an
# unmarked rule detectable, this goes red and the paragraph in the
# ledger that states the escape has to change with it.
planted=$(plant_rule "")
if [ -z "$(ledger_untagged "$planted")" ]; then
    echo "PASS: an unmarked rule is NOT caught — the stated boundary, pinned"
else
    echo "FAIL: an unmarked rule is now caught; the ledger still states it as an escape"
    fails=1
fi

# ...and the planted-rule fixture must be able to fail, or all four
# cases above are asserting over a copy that never had a rule planted
# in it. One control, driven the other way: the SAME planted file with
# a class tag added is clean.
planted=$(plant_rule '-')
sed -i 's/A planted rule with no enforcement class./[enforced: reason-and-clears] planted./' "$planted"
if [ -z "$(ledger_untagged "$planted")" ]; then
    echo "PASS: and a planted rule that DOES declare a class is clean (control)"
else
    echo "FAIL: the planted-rule control is dirty, so the cases above prove nothing"
    fails=1
fi

# --- A PULL REQUEST INTO THE DEFAULT BRANCH (#977) -----------------------
#
# The release PR could not be green and leave the default branch green.
# The workflow is not on the default branch yet, so its entry is
# required; the merge puts it there, so the same entry is stale one
# commit later. The v2.1.0 release merge turned this gate red on the
# default branch for exactly that reason.
#
# On a pull request whose base IS the default branch the merge is what
# makes the workflow reachable, so no entry is required — and an entry
# that IS there is stale, which is what keeps the default branch green
# after the merge and puts the pruning in the release PR where the
# runbook says it is.
#
# Its own fixture, and a second one that is a REAL CLONE. The two
# derivations of "which branch is the default" are available in
# different places — a hosted runner has the event payload and no
# `origin/HEAD`, a clone has `origin/HEAD` and no event payload — so
# each is driven where it exists rather than through a test-only
# override. The clone also runs with NO `BASE_REF` set at all, which is
# how the gate runs in CI.
REPO6="$TMP/repo6"
mkdir -p "$REPO6/.github/workflows"
git -C "$REPO6" init -q -b main
git -C "$REPO6" config user.email t@example.com
git -C "$REPO6" config user.name t
git -C "$REPO6" config commit.gpgsign false
dispatchable onmain6 > "$REPO6/.github/workflows/onmain6.yml"
git -C "$REPO6" add -A && git -C "$REPO6" commit -qm base
git -C "$REPO6" checkout -q -b work
dispatchable new6 > "$REPO6/.github/workflows/new6.yml"

EVENT_MAIN="$TMP/event-main.json"
printf '{"repository":{"default_branch":"main"}}\n' > "$EVENT_MAIN"
EVENT_OTHER="$TMP/event-other.json"
printf '{"repository":{"default_branch":"release"}}\n' > "$EVENT_OTHER"

# <event-name> <pr base> [event payload] [BASE_REF]
verdict6() {
    ( cd "$REPO6" \
      && GITHUB_EVENT_NAME="$1" GITHUB_BASE_REF="$2" \
         GITHUB_EVENT_PATH="${3:-}" BASE_REF="${4:-main}" \
         bash "$CHECK" >"$TMP/out6" 2>&1 ) \
        && echo pass || echo "rc$?"
}

# The pre-#977 verdict, with no event environment at all. Every case
# below is one variable away from this line, so it is the control that
# says what the exemption actually changed.
check "with no event environment an absent workflow still fails" \
    rc1 "$(verdict6 '' '')"

check "a pull request into the default branch does not need an entry (#977)" \
    pass "$(verdict6 pull_request main "$EVENT_MAIN")"
grep -F 'merging this pull request into main' "$TMP/out6" >/dev/null \
    && echo "PASS: and the PASS line says the merge is what makes it reachable" \
    || { echo "FAIL: the exemption's PASS line does not name the merge"; fails=1; }
grep -F 'are on main' "$TMP/out6" >/dev/null \
    && { echo "FAIL: the PASS line claims the workflow is already on the default branch"
         fails=1; } \
    || echo "PASS: and it does not claim the workflow is already there"

# ORTHOGONALITY, the same bargain the *.yml scan and the comment strip
# above make: a case that merely passes proves nothing unless the gate
# WITHOUT the exemption is shown to reject this fixture. `cmp` is what
# says the sed matched — an edit that matched nothing would leave a copy
# that passes its own assertion while testing the unmodified gate.
noexempt="$TMP/noexempt.sh"
sed -e 's/ || \[ "\$MERGES_INTO_DEFAULT" -eq 1 \]//' "$CHECK" > "$noexempt"
if cmp -s "$CHECK" "$noexempt"; then
    echo "FAIL: the exemption could not be removed from the copy, so the case above"
    echo "      would be green whether or not the exemption exists"
    fails=1
elif ( cd "$REPO6" && GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main \
        GITHUB_EVENT_PATH="$EVENT_MAIN" BASE_REF=main bash "$noexempt" \
        >/dev/null 2>&1 ); then
    echo "FAIL: the gate without the exemption also passed the release-PR fixture"
    fails=1
else
    echo "PASS: the gate without the exemption rejects the same fixture (orthogonality confirmed)"
fi
rm -f "$TMP/noexempt.sh"

# THE CONTROLS. Each one is the exemption's own condition, removed.
check "a pull request into dev keeps today's verdict" \
    rc1 "$(verdict6 pull_request dev "$EVENT_MAIN")"
check "a push keeps today's verdict even with a base ref in the environment" \
    rc1 "$(verdict6 push main "$EVENT_MAIN")"

# ...and the base is compared against a DERIVED default branch, never
# the string `main`. Here the repository's default branch is `release`,
# so a pull request into `main` is an ordinary pull request.
check "a pull request into main is NOT exempt when main is not the default branch" \
    rc1 "$(verdict6 pull_request main "$EVENT_OTHER")"

# AN ENTRY IS STALE ON THE PULL REQUEST, and that is the half that keeps
# the default branch green after the merge. Without it the release PR
# passes while carrying the entry that turns the gate red one commit
# later, which is #977 in the other direction.
{ printf '# pending\n'; entry .github/workflows/new6.yml; } \
    > "$REPO6/.github/dispatch-pending.txt"
check "an entry for a workflow this pull request merges to the default branch fails" \
    rc1 "$(verdict6 pull_request main "$EVENT_MAIN")"
grep -F 'merging this pull request puts it on main' "$TMP/out6" >/dev/null \
    && echo "PASS: and the message says the merge is what makes the entry stale" \
    || { echo "FAIL: the stale-on-merge message is missing"; fails=1; }

# The same entry on a pull request into dev is the legitimate case it
# has always been. One variable moved: the base.
check "the same entry on a pull request into dev passes (control)" \
    pass "$(verdict6 pull_request dev "$EVENT_MAIN")"

# The pre-existing stale rule is not suspended by the exemption: a
# workflow that IS on the default branch and declared still fails, with
# the message it has always had.
{ printf '# pending\n'; entry .github/workflows/onmain6.yml; } \
    > "$REPO6/.github/dispatch-pending.txt"
check "an entry for a workflow already on the default branch still fails under the exemption" \
    rc1 "$(verdict6 pull_request main "$EVENT_MAIN")"
grep -F 'stopped meaning anything' "$TMP/out6" >/dev/null \
    && echo "PASS: and it is the stale message, not the merge one" \
    || { echo "FAIL: the stale-entry message was replaced under the exemption"; fails=1; }
rm -f "$REPO6/.github/dispatch-pending.txt"

# THE COMPARISON REF IS THE THIRD CONDITION. Pointed at a branch that is
# not the default one, the gate must not answer "reachable" about a
# branch nobody asked about -- and it must say why it declined, because
# a fix that silently does nothing on the one run it was written for
# looks exactly like a fix that works.
check "the exemption is refused when the gate is pointed at another branch" \
    rc1 "$(verdict6 pull_request main "$EVENT_MAIN" work)"
grep -F 'was NOT applied' "$TMP/out6" >/dev/null \
    && echo "PASS: and declining is printed rather than silent" \
    || { echo "FAIL: the exemption was declined silently"; fails=1; }

# --- the second derivation, in a REAL CLONE, with no BASE_REF ----------
#
# A clone has `origin/HEAD` and no event payload; a hosted runner has the
# event payload and no `origin/HEAD`. Driving only the first would leave
# the route CI actually takes unmeasured, and vice versa. This fixture
# also runs with no `BASE_REF` set at all, so the default spelling
# `origin/main` is the one being compared with the derived branch name --
# the comparison that makes the exemption inert if it is written against
# the wrong spelling.
REPO7="$TMP/repo7"
git clone -q "$REPO6" "$REPO7"
# repo6 is checked out on its work branch, so the clone's origin/HEAD
# follows THAT, not the default branch. Set it to what a clone of a
# repository sitting on its default branch would have -- otherwise this
# fixture derives `work` as the default branch and the case below fails
# for a reason that has nothing to do with the gate.
git -C "$REPO7" remote set-head origin main
git -C "$REPO7" config user.email t@example.com
git -C "$REPO7" config user.name t
git -C "$REPO7" config commit.gpgsign false
git -C "$REPO7" checkout -q -b work7
dispatchable new7 > "$REPO7/.github/workflows/new7.yml"

verdict7() {
    ( cd "$REPO7" && GITHUB_EVENT_NAME="$1" GITHUB_BASE_REF="$2" \
        GITHUB_EVENT_PATH="${3:-}" bash "$CHECK" >"$TMP/out7" 2>&1 ) \
        && echo pass || echo "rc$?"
}

check "a clone with no event payload still fails an absent workflow" \
    rc1 "$(verdict7 '' '')"
check "origin/HEAD is enough to derive the default branch (#977)" \
    pass "$(verdict7 pull_request main)"
check "and a pull request into dev is still refused in the clone" \
    rc1 "$(verdict7 pull_request dev)"

# The control FIRST: the same clone with an event payload that AGREES
# with origin/HEAD is exempt. Without it the disagreement case below
# could be failing on the mere presence of a payload.
check "an event payload that agrees with origin/HEAD is exempt (control)" \
    pass "$(verdict7 pull_request main "$EVENT_MAIN")"

# BOTH SOURCES PRESENT AND DISAGREEING. One fact derived twice with two
# answers: the exemption is refused rather than letting whichever
# derivation answers first decide. Driven where it CHANGES THE VERDICT
# rather than only the message -- origin/HEAD is moved off the branch
# the payload names, and everything else in the run is left exactly as
# the passing control above. The dangling target is deliberate: this is
# about which answer the gate trusts, not about what the ref resolves
# to.
git -C "$REPO7" symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/elsewhere
check "two sources that disagree about the default branch refuse the exemption" \
    rc1 "$(verdict7 pull_request main "$EVENT_MAIN")"
grep -F 'origin/HEAD says' "$TMP/out7" >/dev/null \
    && echo "PASS: and both answers are named" \
    || { echo "FAIL: the disagreement is not reported"; fails=1; }

# ...and putting it back restores the control, so the case above turned
# on the disagreement and nothing else.
git -C "$REPO7" symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/main
check "restoring origin/HEAD restores the exemption (control)" \
    pass "$(verdict7 pull_request main "$EVENT_MAIN")"

# --- THE SHAPE A HOSTED RUNNER IS IN (#977) -----------------------------
#
# Neither fixture above is what CI looks like. actions/checkout fetches
# ONE ref, so `origin/main` is not present and `origin/HEAD` does not
# exist; the gate's own fallback then fetches the default branch and
# REWRITES `BASE_REF` to `FETCH_HEAD` before any of this is decided. An
# exemption written against the rewritten value is inert on every hosted
# run — green suite, green fixtures, and the release PR red exactly as
# before, discovered at the next release.
#
# So: a clone with `origin/main` and `origin/HEAD` removed, the event
# payload as the only derivation, and no `BASE_REF` override. The remote
# is a path, so the gate's fetch succeeds the way it does in CI.
REPO8="$TMP/repo8"
git clone -q "$REPO6" "$REPO8"
git -C "$REPO8" config user.email t@example.com
git -C "$REPO8" config user.name t
git -C "$REPO8" config commit.gpgsign false
git -C "$REPO8" checkout -q -b work8
git -C "$REPO8" symbolic-ref --delete refs/remotes/origin/HEAD
git -C "$REPO8" update-ref -d refs/remotes/origin/main
dispatchable new8 > "$REPO8/.github/workflows/new8.yml"

# THE PROPERTY SURVIVES EXACTLY ONE RUN UNLESS IT IS RESTORED. The
# gate's fallback fetches the branch from a configured remote, and git
# opportunistically re-creates `refs/remotes/origin/main` while doing
# it. The first case below therefore left the fixture identical to the
# two above, and the mutant that reads BASE_REF after the rewrite
# SURVIVED against it -- measured, not foreseen. So the ref is removed
# before every case and its absence is a verdict of its own.
verdict8() {
    git -C "$REPO8" update-ref -d refs/remotes/origin/main
    if ( cd "$REPO8" && git rev-parse --verify --quiet origin/main >/dev/null ); then
        echo "fixture-has-origin-main"
        return
    fi
    ( cd "$REPO8" && GITHUB_EVENT_NAME="$1" GITHUB_BASE_REF="$2" \
        GITHUB_EVENT_PATH="${3:-}" bash "$CHECK" >"$TMP/out8" 2>&1 ) \
        && echo pass || echo "rc$?"
}

check "the fetched default branch still fails an absent workflow on a push" \
    rc1 "$(verdict8 push '')"
# ...and that run is the proof the rewrite happened: the gate names the
# ref it ended up comparing against. Without this the whole block could
# be running against `origin/main` and testing nothing new.
grep -F 'is not on FETCH_HEAD' "$TMP/out8" >/dev/null \
    && echo "PASS: and the comparison ref was rewritten to FETCH_HEAD, as on a runner" \
    || { echo "FAIL: BASE_REF was not rewritten, so this fixture is not the CI shape"
         fails=1; }

check "a release PR is exempt on a runner that has to fetch the default branch" \
    pass "$(verdict8 pull_request main "$EVENT_MAIN")"
check "and a pull request into dev is still refused there" \
    rc1 "$(verdict8 pull_request dev "$EVENT_MAIN")"

# --- THE TWO ARMS THE #977 CASES LEFT UNDRIVEN --------------------------
#
# Both were correct when driven by hand and asserted by nothing, so
# deleting either left the suite green.
#
# ARM ONE: a pull request into the default branch where NEITHER
# derivation answers. REPO8 has no `origin/HEAD`; run it with no event
# payload either and the gate has nothing to derive the default branch
# from. It must decline in words and fall back to the push verdict.
check "a pull request with neither derivation available is not exempted" \
    rc1 "$(verdict8 pull_request main '')"
grep -F 'could not be derived' "$TMP/out8" >/dev/null \
    && echo "PASS: and the reason names the missing derivations" \
    || { echo "FAIL: the no-derivation decline printed no reason"; fails=1; }

# ARM TWO: `pull_request_target` is in the exemption's domain beside
# `pull_request`. Inert in this repository today, and a second event
# class that nothing drove: removing it from the case list left the
# suite green.
check "pull_request_target into the default branch is exempt too" \
    pass "$(verdict6 pull_request_target main "$EVENT_MAIN")"
check "pull_request_target into dev keeps today's verdict (control)" \
    rc1 "$(verdict6 pull_request_target dev "$EVENT_MAIN")"

# --- THE RELEASE ROUTE, COMPOSED ON ONE LEDGER (#977 round 1) -----------
#
# Each half of the release route passed in isolation while the route as
# a whole was impassable. The exemption is keyed on a pull request into
# the DEFAULT branch, but a commit only reaches the release pull request
# by being on `dev` first, and the runbook's route onto `dev` is step 5,
# `release/vX.Y.Z` -> `dev`. That pull request removes the entry, gets
# no exemption, and goes red; so does every pull request into `dev`
# until the release merges, because they are tested as the merge product
# and `dev` already carries the removal.
#
# So this walks ONE ledger through the whole runbook route and asserts
# the verdict at every hop. A fixture per hop cannot see this: the
# failure is the composition.
REPO9="$TMP/repo9"
mkdir -p "$REPO9/.github/workflows"
git -C "$REPO9" init -q -b main
git -C "$REPO9" config user.email t@example.com
git -C "$REPO9" config user.name t
git -C "$REPO9" config commit.gpgsign false

# The published-image pin is the one fact that says which version a tree
# is, read the same way on both sides (scripts/bump-version.sh rewrites
# it at runbook step 2).
printf 'docker plugin install ghcr.io/claymore666/docker-net-dhcp:v9.8.0\n' \
    > "$REPO9/README.md"
dispatchable onmain9 > "$REPO9/.github/workflows/onmain9.yml"
git -C "$REPO9" add -A && git -C "$REPO9" commit -qm "v9.8.0, released"

# Mid-cycle: a new dispatchable workflow merges to dev, declared.
git -C "$REPO9" checkout -q -b dev
dispatchable new9 > "$REPO9/.github/workflows/new9.yml"
{ printf '# pending\n'; entry .github/workflows/new9.yml; } \
    > "$REPO9/.github/dispatch-pending.txt"
git -C "$REPO9" add -A && git -C "$REPO9" commit -qm "new9, declared"

# <event-name> <pr base>
verdict9() {
    ( cd "$REPO9" \
      && GITHUB_EVENT_NAME="$1" GITHUB_BASE_REF="$2" \
         GITHUB_EVENT_PATH="$EVENT_MAIN" BASE_REF=main \
         bash "$CHECK" >"$TMP/out9" 2>&1 ) \
        && echo pass || echo "rc$?"
}

check "hop 1: mid-cycle push to dev with the entry present passes" \
    pass "$(verdict9 push '')"

check "hop 2: the release PR into main with the entry still present is stale" \
    rc1 "$(verdict9 pull_request main)"
grep -F 'Remove it here' "$TMP/out9" >/dev/null \
    && echo "PASS: and it says to remove the entry in that pull request" \
    || { echo "FAIL: hop 2 did not name the pull request as the place to remove it"
         fails=1; }

# THE CONTROL FOR HOP 3, TAKEN FIRST. The removal alone, with no version
# bump, is an ordinary mid-cycle prune of a live entry and must still
# fail. Without this line hop 3 would only measure "a PR into dev with
# no entry passes", which would be the gate deleted.
git -C "$REPO9" checkout -q -b nobump9 dev
printf '# pending\n' > "$REPO9/.github/dispatch-pending.txt"
check "hop 3 control: removing the entry with no release in flight still fails" \
    rc1 "$(verdict9 pull_request dev)"
git -C "$REPO9" checkout -q -- .github/dispatch-pending.txt

# HOP 3, runbook step 5: `release/v9.9.0` -> `dev`, carrying the version
# bump and the removal. This is the pull request that was red.
git -C "$REPO9" checkout -q -b release/v9.9.0 dev
sed -i 's/v9\.8\.0/v9.9.0/' "$REPO9/README.md"
printf '# pending\n' > "$REPO9/.github/dispatch-pending.txt"
git -C "$REPO9" add -A && git -C "$REPO9" commit -qm "release v9.9.0: bump pins, prune the ledger"
check "hop 3: the release branch into dev, carrying the removal, passes" \
    pass "$(verdict9 pull_request dev)"
grep -F 'one release step behind it' "$TMP/out9" >/dev/null \
    && echo "PASS: and it says what it compared, naming the step" \
    || { echo "FAIL: hop 3 passed without naming the pin comparison"; fails=1; }
grep -F 'v9.9.0' "$TMP/out9" >/dev/null && grep -F 'v9.8.0' "$TMP/out9" >/dev/null \
    && echo "PASS: and both pinned versions are printed" \
    || { echo "FAIL: the pin note does not name the two versions"; fails=1; }

# EVERY LINE THE RUN PRINTS MUST BE TRUE OF WHAT IT DERIVED. The same
# bargain as the merging line's negative assertion above: the summary
# must not describe a suspended workflow with the sentence written for a
# declared one, and the ledger here holds no entry at all.
grep -F 'declared in .github/dispatch-pending.txt' "$TMP/out9" >/dev/null \
    && { echo "FAIL: a suspended workflow is reported as declared, with an empty ledger"
         fails=1; } \
    || echo "PASS: and a suspended workflow is not reported as declared"
grep -F 'reachable on' "$TMP/out9" >/dev/null \
    && { echo "FAIL: the summary claims a suspended workflow is reachable on the default branch"
         fails=1; } \
    || echo "PASS: and it does not claim the suspended workflow is reachable"
grep -F 'suspended by the pin comparison above' "$TMP/out9" >/dev/null \
    && echo "PASS: and the summary names the suspension as the reason" \
    || { echo "FAIL: the summary does not say why the workflow is not a finding"; fails=1; }

# HOP 4: that merges to dev. Nothing is a pull request now, and the push
# lane on dev has to stay green for the length of the release.
git -C "$REPO9" checkout -q dev
git -C "$REPO9" merge -q --ff-only release/v9.9.0
check "hop 4: the push lane on dev stays green while the release is in flight" \
    pass "$(verdict9 push '')"

# HOP 5: an ordinary pull request into dev during the release window. It
# is tested as the merge product, so it carries dev's removal without
# having made it. This is the one that blocks other people's work.
git -C "$REPO9" checkout -q -b feat9 dev
printf 'unrelated\n' > "$REPO9/feature.txt"
git -C "$REPO9" add -A && git -C "$REPO9" commit -qm "an unrelated change"
check "hop 5: an unrelated pull request into dev during the window passes" \
    pass "$(verdict9 pull_request dev)"
git -C "$REPO9" checkout -q dev

# TWO PINS ARE NOT A VERSION. The suspension turns on ONE fact read the
# same way on both sides, so a tree that pins two different versions has
# not said which it is, and cannot-tell is not in flight. Without this
# case the uniqueness requirement can be dropped and the suite stays
# green, which would let a half-bumped tree buy the suspension.
git -C "$REPO9" checkout -q -b twopins9 dev
printf 'docker plugin install ghcr.io/claymore666/docker-net-dhcp:v9.9.0\nand ghcr.io/other/docker-net-dhcp:v9.7.0\n' \
    > "$REPO9/README.md"
check "a tree pinning two different versions gets no suspension" \
    rc1 "$(verdict9 pull_request dev)"
git -C "$REPO9" checkout -q dev
git -C "$REPO9" checkout -q -- README.md

# HOP 6: runbook step 6, the release pull request itself, at that head.
check "hop 6: the release pull request into main passes with the entry gone" \
    pass "$(verdict9 pull_request main)"

# HOP 7: it merges. The default branch now has the workflow and the two
# versions agree, so the suspension is OVER and the gate is live again.
git -C "$REPO9" checkout -q main
git -C "$REPO9" merge -q --ff-only dev
check "hop 7: the default branch is green one commit after the merge" \
    pass "$(verdict9 push '')"
grep -F 'is suspended for' "$TMP/out9" >/dev/null \
    && { echo "FAIL: the suspension is still on after the release merged"; fails=1; } \
    || echo "PASS: and the suspension is over, because the two versions agree again"

# HOP 7b AND 7c: THE GAP BETWEEN RUNBOOK STEPS 8 AND 11 (#977 round 2,
# F5). The release pull request merging is step 8; `dev` is
# fast-forwarded to `main` at step 11, and the two are not the same
# moment. In between, the default branch carries the merge commit and
# `dev` does not, with identical trees. Collapsing the two into one
# fast-forward, as the route above did, means no run is ever measured in
# that gap.
git -C "$REPO9" checkout -q main
git -C "$REPO9" commit -q --allow-empty -m "Merge pull request #999 from claymore666/dev"
git -C "$REPO9" checkout -q dev
check "hop 7b: a push on dev in the gap between the merge and the fast-forward" \
    pass "$(verdict9 push '')"
git -C "$REPO9" merge -q --ff-only main
check "hop 7c: and after the fast-forward at step 11" \
    pass "$(verdict9 push '')"
git -C "$REPO9" checkout -q main

# BEHIND IS NOT IN FLIGHT. The comparison is strictly newer, not
# different: a branch that has not been back-merged pins an OLDER
# version than the default branch, and reading that as a release in
# flight would hand the suspension to every stale branch in the
# repository, permanently.
git -C "$REPO9" checkout -q -b behind9 main
sed -i 's/v9\.9\.0/v9.8.0/' "$REPO9/README.md"
dispatchable behindnew9 > "$REPO9/.github/workflows/behindnew9.yml"
check "a branch pinning an OLDER version than the default branch is not in flight" \
    rc1 "$(verdict9 pull_request dev)"
git -C "$REPO9" checkout -q main
git -C "$REPO9" checkout -q -- README.md
rm -f "$REPO9/.github/workflows/behindnew9.yml"

# ...and the gate is not merely quiet: a new undeclared workflow on the
# post-release tree fails again. A suspension that never lifts is the
# gate deleted, and nothing above would have noticed.
git -C "$REPO9" checkout -q -b after9 main
dispatchable later9 > "$REPO9/.github/workflows/later9.yml"
check "hop 8: an undeclared workflow after the release fails again" \
    rc1 "$(verdict9 pull_request dev)"

# CANNOT TELL ON THE OTHER SIDE EITHER. The uniqueness requirement is
# read on BOTH sides, and only the default-branch side discriminates:
# with two pins in the tree the newest-of-both comparison already fails,
# so a fixture that half-bumps the TREE cannot tell the requirement from
# its absence. Half-bump the DEFAULT BRANCH instead and the difference
# is visible -- the tree pins one newer version, and without the
# requirement that reads as a release in flight.
git -C "$REPO9" checkout -q main
printf 'docker plugin install ghcr.io/claymore666/docker-net-dhcp:v9.9.0\nand ghcr.io/other/docker-net-dhcp:v9.7.0\n' \
    > "$REPO9/README.md"
git -C "$REPO9" commit -q -am "main: two disagreeing pins"
git -C "$REPO9" checkout -q -b halfbumped9 main
printf 'docker plugin install ghcr.io/claymore666/docker-net-dhcp:v10.0.0\n' \
    > "$REPO9/README.md"
dispatchable later10 > "$REPO9/.github/workflows/later10.yml"
check "a default branch pinning two different versions gives no suspension either" \
    rc1 "$(verdict9 pull_request dev)"

# --- WHAT THE PIN COMPARISON DOES NOT KNOW (#977 round 2) --------------
#
# The suspension is derived from two version pins and nothing else. Three
# things follow, and each of them was true and unobserved: the gate
# cannot tell a release from a bare pin bump (F2), a workflow merged
# undeclared while the pins differ is missed for as long as they differ
# (F3), and a release that is parked leaves the pins differing forever
# (F4). Its own fixture, because each of the three has to be driven with
# the window held in a state the composed route never sits in.
REPO10="$TMP/repo10"
mkdir -p "$REPO10/.github/workflows"
git -C "$REPO10" init -q -b main
git -C "$REPO10" config user.email t@example.com
git -C "$REPO10" config user.name t
git -C "$REPO10" config commit.gpgsign false
printf 'docker plugin install ghcr.io/claymore666/docker-net-dhcp:v9.8.0\n' \
    > "$REPO10/README.md"
dispatchable onmain10 > "$REPO10/.github/workflows/onmain10.yml"
printf '# pending\n' > "$REPO10/.github/dispatch-pending.txt"
git -C "$REPO10" add -A && git -C "$REPO10" commit -qm "v9.8.0, released"

verdict10() {
    ( cd "$REPO10" \
      && GITHUB_EVENT_NAME="$1" GITHUB_BASE_REF="$2" \
         GITHUB_EVENT_PATH="$EVENT_MAIN" BASE_REF=main \
         bash "$CHECK" >"$TMP/out10" 2>&1 ) \
        && echo pass || echo "rc$?"
}

# F2. A BARE PIN BUMP IS NOT A RELEASE, and this run has no way to know
# the difference: no release branch, no ledger entry was ever removed
# here, and no release pull request exists. The suspension still applies,
# because the pins are what it reads. What must NOT happen is the run
# asserting the rest of that story as though it had checked it.
git -C "$REPO10" checkout -q -b bump10 main
sed -i 's/v9\.8\.0/v9.9.0/' "$REPO10/README.md"
dispatchable bumped10 > "$REPO10/.github/workflows/bumped10.yml"
check "a bare pin bump with no release suspends the finding" \
    pass "$(verdict10 pull_request dev)"
grep -F 'WHAT THIS RUN CHECKED IS THE TWO PINS, AND NOTHING ELSE' "$TMP/out10" >/dev/null \
    && echo "PASS: and the run says the pins are all it checked" \
    || { echo "FAIL: the suspension does not say what it was derived from"; fails=1; }
for claim in \
    'Their ledger entries were removed on the release branch' \
    'a release is in flight'
do
    grep -F "$claim" "$TMP/out10" >/dev/null \
        && { echo "FAIL: the run asserts '$claim', which it never derived"; fails=1; } \
        || echo "PASS: and it does not assert '$claim'"
done
grep -F 'cannot tell a release from a bare pin bump' "$TMP/out10" >/dev/null \
    && echo "PASS: and the false-positive direction is named in the output" \
    || { echo "FAIL: the bump-without-a-release direction is not stated"; fails=1; }

# F3. THE STATED BOUND, CARRIED BY A REAL WORKFLOW. `bumped10` is a
# dispatchable workflow that was never declared, and while the pins
# differ it is not a finding. The bound says it is missed until they
# agree again, so the same workflow is put through the closing of the
# window.
git -C "$REPO10" checkout -q main
git -C "$REPO10" merge -q --no-ff -m "the release lands without bumped10" \
    --strategy=ours bump10
sed -i 's/v9\.8\.0/v9.9.0/' "$REPO10/README.md"
git -C "$REPO10" commit -q -am "v9.9.0, released"
git -C "$REPO10" checkout -q bump10
check "the same undeclared workflow is caught once the pins agree again" \
    rc1 "$(verdict10 pull_request dev)"
grep -F 'bumped10.yml' "$TMP/out10" >/dev/null \
    && echo "PASS: and the finding names the workflow the window was hiding" \
    || { echo "FAIL: the workflow carried through the window is not named"; fails=1; }

# F4. THE SUSPENSION IS BOUNDED BY THE DISTANCE BETWEEN THE TWO PINS.
# Two steps apart is refused, and the run says so where it bites: the
# refusal above otherwise reads as "add an entry" to someone who has a
# bumped pin and believes the suspension covers it.
git -C "$REPO10" checkout -q -b parked10 main
dispatchable parked10wf > "$REPO10/.github/workflows/parked10wf.yml"
sed -i 's/v9\.9\.0/v9.11.0/' "$REPO10/README.md"
check "a tree two release steps ahead is not suspended" \
    rc1 "$(verdict10 pull_request dev)"
grep -F 'more than one release step' "$TMP/out10" >/dev/null \
    && echo "PASS: and the run says the suspension did not apply, and by how much" \
    || { echo "FAIL: the refusal is not explained where it bites"; fails=1; }

# AND IT NAMES BOTH READINGS, NOT ONE (#977 round 3). Two pins this far
# apart are left by a release that never landed AND by a release that
# skips a version -- this project has dropped a planned version before --
# and the gate cannot tell those apart. Asserting the stale one as the
# reason is the sentence wider than the code that this whole PR is about,
# and on the skipped-release reading the way through is the ledger entry,
# which the run has to name.
grep -F 'a release that skips a version' "$TMP/out10" >/dev/null \
    && echo "PASS: and the skipped-release reading is named beside the stale one" \
    || { echo "FAIL: the run offers only the stale-tree reading of two pins"; fails=1; }
grep -F '.github/dispatch-pending.txt as any other pending workflow' "$TMP/out10" >/dev/null \
    && echo "PASS: and the ledger entry is named as the way through" \
    || { echo "FAIL: the run does not say what to do on the reading it cannot exclude"
         fails=1; }

# AND THE OTHER HALF OF THAT SENTENCE: a run with nothing to REPORT
# stays quiet, pins or no pins. The four texts say the refusal is loud
# on the runs that can act on it, which is narrower than "every run
# where the pins differ" -- the pins are not news on their own, and a
# green run that announced a lapsed suspension would be the same kind
# of sentence this whole change is about. Same two-step tree, nothing
# undeclared left in it.
rm -f "$REPO10/.github/workflows/parked10wf.yml" \
      "$REPO10/.github/workflows/bumped10.yml"
check "the same two-step tree with nothing to report passes" \
    pass "$(verdict10 pull_request dev)"
grep -F 'pin suspension did NOT apply' "$TMP/out10" >/dev/null \
    && { echo "FAIL: a run with no finding still announces a suspension"; fails=1; } \
    || echo "PASS: and it says nothing about a suspension nothing needed"
dispatchable parked10wf > "$REPO10/.github/workflows/parked10wf.yml"
check "restoring the undeclared workflow restores the refusal (control)" \
    rc1 "$(verdict10 pull_request dev)"

# ...and the preservation control, because a red that only measures
# "hard" proves nothing: ONE step ahead on the same fixture, same
# workflow, same ledger, passes. Without it the case above would be
# satisfied by a gate that had stopped suspending anything.
sed -i 's/v9\.11\.0/v9.10.0/' "$REPO10/README.md"
check "one step ahead on the same fixture is still suspended (control)" \
    pass "$(verdict10 pull_request dev)"

# F4b. THE BOUND IS A DISTANCE, NOT A DURATION (#977 round 3), and this
# case pins that escape rather than leaving it to be discovered. A
# release parked after runbook step 5 sits exactly ONE step ahead, which
# is the shape the suspension accepts, so it stays suspended for as long
# as nobody finishes or unwinds it. The script header, the ledger, the
# runbook and the pull request body all say so now; if anyone gives the
# suspension a time bound, this case goes red and those four texts have
# to change with it.
git -C "$REPO10" add -A
git -C "$REPO10" commit -qm "release/v9.10.0 reaches dev at step 5, and is then parked"
check "a parked release one step ahead is suspended on the day it parks" \
    pass "$(verdict10 pull_request dev)"

# ORTHOGONALITY, taken before the commits pile up: a copy of the gate
# WITH a duration bound accepts this same fixture today. Without that,
# the assertion after the commits is satisfied by any copy that refuses
# nothing, and an inert edit would pass its own case.
timed="$TMP/timed.sh"
sed -e 's@^        RELEASE_IN_FLIGHT=1$@        RELEASE_IN_FLIGHT=1\n        [ "$(git rev-list --count "${BASE_REF}"..HEAD 2>/dev/null || echo 0)" -le 10 ] || RELEASE_IN_FLIGHT=0@' \
    "$CHECK" > "$timed"
# The copy is required to be a WORKING gate with exactly one injected
# bound, not merely a file that differs: an empty file differs too, and
# it would pass every assertion below by doing nothing at all.
if [ "$(grep -c -- '-le 10 \] || RELEASE_IN_FLIGHT=0' "$timed")" = 1 ] \
   && bash -n "$timed" 2>/dev/null; then
    echo "PASS: the duration-bounded copy carries exactly one time bound and parses"
else
    echo "FAIL: the duration-bounded copy did not land, so the two assertions below"
    echo "      measure nothing"
    fails=1
fi
timed_verdict() {
    ( cd "$REPO10" \
      && GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=dev \
         GITHUB_EVENT_PATH="$EVENT_MAIN" BASE_REF=main \
         bash "$timed" >/dev/null 2>&1 ) \
        && echo pass || echo "rc$?"
}
check "the duration-bounded copy accepts the parked release on day one (control)" \
    pass "$(timed_verdict)"

for i in $(seq 1 40); do
    printf 'while the release sits parked: %s\n' "$i" >> "$REPO10/parked.log"
    git -C "$REPO10" add -A
    git -C "$REPO10" commit -qm "ordinary work, release still parked ($i)"
done
check "and still suspended 40 commits later: the bound is a distance, not a duration" \
    pass "$(verdict10 pull_request dev)"
grep -F 'pin suspension did NOT apply' "$TMP/out10" >/dev/null \
    && { echo "FAIL: the parked one-step window reports the suspension as not applied"
         fails=1; } \
    || echo "PASS: and nothing in that run reports the suspension as lapsed"
check "the duration-bounded copy refuses it, so the case above can go red" \
    rc1 "$(timed_verdict)"
rm -f "$timed"

# F4c. ONLY AHEAD, AND THE OTHER ARM IS LOUD TOO (#977 round 3). A
# release cut on an older line -- a hotfix off an older tag while the
# default branch has moved on -- pins a version BEHIND the default
# branch. It is not suspended, which is correct, and the run used to say
# nothing at all about why: the reason was keyed on the one mechanism
# that sets the newer arm. The readings it prints are this arm's own;
# a release that never landed cannot produce it.
git -C "$REPO10" checkout -q main
git -C "$REPO10" checkout -q -b hotfix10 main
rm -f "$REPO10/.github/workflows/parked10wf.yml" "$REPO10/.github/workflows/bumped10.yml"
dispatchable hotfix10wf > "$REPO10/.github/workflows/hotfix10wf.yml"
sed -i 's/v9\.9\.0/v9.8.1/' "$REPO10/README.md"
check "a release cut on an older line is not suspended" \
    rc1 "$(verdict10 pull_request dev)"
grep -F 'pin suspension did NOT apply' "$TMP/out10" >/dev/null \
    && echo "PASS: and this arm says the suspension did not apply as well" \
    || { echo "FAIL: the behind arm is silent about why the suspension missed it"
         fails=1; }
grep -F 'a release cut on an older line' "$TMP/out10" >/dev/null \
    && echo "PASS: and it names the reading this arm actually has" \
    || { echo "FAIL: the behind arm does not name the older-line reading"; fails=1; }
grep -F 'a release that skips a version' "$TMP/out10" >/dev/null \
    && { echo "FAIL: the behind arm offers a reading only the ahead arm can have"
         fails=1; } \
    || echo "PASS: and it does not offer the ahead arm's readings"

# F4d. AND THE NOTE IS ABSENT WHEN THE PINS AGREE (#977 round 3).
# Deleting the equality guard left the whole suite green: the suspension
# still never applied, because a version is not its own successor, but
# the newest-of-the-two fallback is true for two EQUAL pins, so every
# ordinary mid-cycle failure printed "this tree pins v9.9.0, main pins
# v9.9.0, and that is more than one release step ahead of it". A false
# sentence in the evidence trail, on the most common failure there is.
git -C "$REPO10" checkout -q main
git -C "$REPO10" checkout -q -- README.md
git -C "$REPO10" checkout -q -b equal10 main
rm -f "$REPO10/.github/workflows/hotfix10wf.yml"
dispatchable equal10wf > "$REPO10/.github/workflows/equal10wf.yml"
check "an ordinary mid-cycle failure with the pins equal still fails" \
    rc1 "$(verdict10 pull_request dev)"
grep -F 'pin suspension did NOT apply' "$TMP/out10" >/dev/null \
    && { echo "FAIL: equal pins are reported as a suspension that did not apply"
         fails=1; } \
    || echo "PASS: and no suspension is reported on a run whose pins agree"

# ORTHOGONALITY: the guard-deleted copy prints it on this same fixture,
# so the absence asserted above is the guard's doing and not the
# fixture's.
noeq="$TMP/noeq.sh"
guard='\[ "\$TREE_VERSION" != "\$BASE_VERSION" \]'
sed -e 's@^   && \[ "\$TREE_VERSION" != "\$BASE_VERSION" \]; then$@   ; then@' "$CHECK" > "$noeq"
# Same rule as the copy above: the guard has to have been there once and
# be gone exactly once, and what is left has to still run.
if [ "$(grep -c -- "$guard" "$CHECK")" = 1 ] \
   && [ "$(grep -c -- "$guard" "$noeq")" = 0 ] \
   && bash -n "$noeq" 2>/dev/null; then
    echo "PASS: the guard-deleted copy has the one equality guard removed and parses"
else
    echo "FAIL: the guard-deleted copy did not land, so the assertion below measures"
    echo "      nothing"
    fails=1
fi
( cd "$REPO10" \
  && GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=dev \
     GITHUB_EVENT_PATH="$EVENT_MAIN" BASE_REF=main \
     bash "$noeq" >"$TMP/outnoeq" 2>&1 )
grep -F 'pin suspension did NOT apply' "$TMP/outnoeq" >/dev/null \
    && echo "PASS: without the equality guard the same run prints the false sentence" \
    || { echo "FAIL: the guard-deleted copy prints nothing, so the case above would"
         echo "      pass against a gate with no guard at all"; fails=1; }
rm -f "$noeq"

# --- the real repository ------------------------------------------------
# The shipped state must satisfy its own gate.
real=$( cd "$(dirname "$CHECK")/.." && bash "$CHECK" >/dev/null 2>&1 && echo pass || echo "rc$?" )
check "this repository passes its own dispatch-reachability gate" pass "$real"

exit "$fails"
