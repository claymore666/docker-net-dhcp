#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-runbook-release-steps.sh (#972).
#
# The real gate is run against mutated copies of the real runbook and
# the real release.yml. Every mutator is checked for having changed its
# file, so a rotted anchor goes red rather than quiet.
#
# THE CASES THAT CARRY THE WEIGHT are the four that reproduce what the
# page actually said before this change: a step the workflow runs and
# the page never mentions, an install proof the page never names, and
# the two "waits on six jobs" sentences after the answer became eight.
# Each is asserted to FAIL, which is what nothing did at the time.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(cd "$(dirname "$0")" && pwd)/check-runbook-release-steps.sh"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RB="$ROOT/docs/release-runbook.md"
WF="$ROOT/.github/workflows/release.yml"
guarded_tmpdir TMP

pass=0; fail=0

[ -f "$RB" ] || { echo "FAIL: no runbook at $RB"; exit 1; }
[ -f "$WF" ] || { echo "FAIL: no workflow at $WF"; exit 1; }

# run NAME WANT_RC RB_MUTATOR WF_MUTATOR [NEEDLE]
run() {
    local name="$1" want="$2" rbmut="$3" wfmut="$4" needle="${5:-}" out got
    cp "$RB" "$TMP/rb.md"
    cp "$WF" "$TMP/wf.yml"
    [ "$rbmut" = "none" ] || "$rbmut" "$TMP/rb.md"
    [ "$wfmut" = "none" ] || "$wfmut" "$TMP/wf.yml"
    if [ "$rbmut" != "none" ] && cmp -s "$RB" "$TMP/rb.md"; then
        echo "FAIL: $name — \`$rbmut\` left the runbook byte-identical; its anchor has rotted"
        fail=$((fail + 1)); return
    fi
    if [ "$wfmut" != "none" ] && cmp -s "$WF" "$TMP/wf.yml"; then
        echo "FAIL: $name — \`$wfmut\` left the workflow byte-identical; its anchor has rotted"
        fail=$((fail + 1)); return
    fi
    out=$(bash "$GATE" "$TMP/rb.md" "$TMP/wf.yml" 2>&1); got=$?
    if [ "$got" -ne "$want" ]; then
        echo "FAIL: $name — want exit $want, got $got"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    elif [ -n "$needle" ] && ! printf '%s\n' "$out" | grep -F -- "$needle" >/dev/null; then
        echo "FAIL: $name — exit $got as expected, but output never mentions '$needle'"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    else
        echo "ok: $name"
        pass=$((pass + 1))
    fi
}

# --- the control -------------------------------------------------------
run "the shipped runbook matches the shipped workflow" 0 none none "walks release promote-latest"

# --- 1. the declaration ------------------------------------------------
drop_declaration() { sed -i '/^<!-- release-walkthrough:/d' "$1"; }
run "a runbook with no walkthrough declaration is a refusal" \
    2 drop_declaration none "having compared nothing"

empty_declaration() {
    sed -i 's|^<!-- release-walkthrough:.*|<!-- release-walkthrough: -->|' "$1"
}
run "an empty walkthrough declaration is a refusal" \
    2 empty_declaration none "declaration in"

declare_a_ghost() {
    sed -i 's|^<!-- release-walkthrough: release, promote-latest -->|<!-- release-walkthrough: release, promote-latest, publish-to-the-moon -->|' "$1"
}
run "a declared job the workflow does not have fails" \
    1 declare_a_ghost none "publish-to-the-moon"

# --- 2. a step the page never mentions ---------------------------------
# THE SHAPE THIS GATE EXISTS FOR. This is the alias copy step, removed
# from the page exactly as it was absent from it before this change.
drop_alias_step_from_page() {
    sed -i 's|Install oras →\n||; s|→ \*\*Publish the same manifest under the Hub alias\*\* (or skip) →|→|' "$1"
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = s.replace("Install oras\n   → **Publish the same manifest under the Hub alias** (or skip) →\n   Install syft", "Install syft")
s = re.sub(r"\*Publish the same manifest under the Hub alias\* runs", "*A step that no longer names itself* runs", s)
open(p, "w").write(s)
PY
}
page_lost_the_step() {
    ! grep -q 'Publish the same manifest under the Hub alias' "$1"
}
cp "$RB" "$TMP/probe.md"; drop_alias_step_from_page "$TMP/probe.md"
if page_lost_the_step "$TMP/probe.md"; then
    echo "ok: the step-removal mutator really removes the step name from the page"
    pass=$((pass + 1))
else
    echo "FAIL: the step-removal mutator left the step name in the page; the case below measures nothing"
    fail=$((fail + 1))
fi
run "a workflow step the page never mentions fails" \
    1 drop_alias_step_from_page none "Publish the same manifest under the Hub alias"

# The reverse: a step ADDED to the workflow and not to the page. This is
# the direction that bites on every future change, and it needs no
# runbook edit at all.
add_step_to_workflow() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]; lines = open(p).read().split("\n")
for i, l in enumerate(lines):
    if l == "      - name: Install syft":
        lines.insert(i, "      - name: Polish the rootfs to a shine")
        lines.insert(i + 1, "        run: true")
        break
open(p, "w").write("\n".join(lines))
PY
}
run "a step added to the workflow and not to the page fails" \
    1 none add_step_to_workflow "Polish the rootfs to a shine"

# --- 3. the install proofs, both directions ----------------------------
drop_alias_proof_from_page() {
    sed -i 's|verify-install-hub-alias|verify-install-hub-ALIAS-RENAMED|g' "$1"
}
run "an install proof the page never names fails" \
    1 drop_alias_proof_from_page none "verify-install-hub-alias"

run "a proof the page names that is not a job fails" \
    1 drop_alias_proof_from_page none "which is not a job in"

# --- 4. the counts -----------------------------------------------------
# "all six of the above are green" outlived two added proofs. Both
# sentences are driven, because they are two transcriptions of two
# different needs: lists and one can rot without the other.
stale_promote_count() {
    sed -i 's|only after all eight of the above are green|only after all six of the above are green|' "$1"
}
run "a stale promote-latest count fails" \
    1 stale_promote_count none "has 8"

stale_release_count() {
    sed -i 's|needs the same eight jobs|needs the same six jobs|' "$1"
}
run "a stale github-release count fails" \
    1 stale_release_count none "has 8"

# A page that states NO count is not a page that agrees with the
# workflow. Deleting the sentence is the obvious way to answer a stale
# count, and it takes with it the one thing that tells a releaser when
# the run is complete.
drop_promote_count() {
    sed -i 's|only after all eight of the above are green|only after the jobs above are green|' "$1"
}
run "a page that states no promote-latest count fails" \
    1 drop_promote_count none "states no count"

drop_release_count() {
    sed -i 's|needs the same eight jobs|needs the same jobs|' "$1"
}
run "a page that states no github-release count fails" \
    1 drop_release_count none "states no count"

# The count moves with the workflow, not with the page: adding a proof
# to promote-latest's needs: makes the page's correct-today number
# wrong, with no runbook edit.
widen_promote_needs() {
    sed -i 's|^\(    needs: \[release, release-arm64, verify-install,\)|\1 verify-install-on-a-tuesday,|' "$1"
}
run "a proof added to needs: without a page edit fails" \
    1 none widen_promote_needs "has 9"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
