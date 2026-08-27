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
check "a declaration for a workflow now on the default branch fails" rc1 "$(verdict)"

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
check "an entry with no Reason fails" rc1 "$(verdict3)"

led <<'EOF'
.github/workflows/cron.yml
    Reason:  new this cycle.
    Triggers: workflow_dispatch schedule
EOF
check "an entry with no Clears fails" rc1 "$(verdict3)"

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
check "omitting a trigger the workflow declares fails, however fluent the reason" rc1 "$(verdict3)"
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

rm -f "$REPO3/.github/workflows/cron.yml" "$REPO3/.github/workflows/manual3.yml" \
      "$REPO3/.github/dispatch-pending.txt"

# --- the real repository ------------------------------------------------
# The shipped state must satisfy its own gate.
real=$( cd "$(dirname "$CHECK")/.." && bash "$CHECK" >/dev/null 2>&1 && echo pass || echo "rc$?" )
check "this repository passes its own dispatch-reachability gate" pass "$real"

exit "$fails"
