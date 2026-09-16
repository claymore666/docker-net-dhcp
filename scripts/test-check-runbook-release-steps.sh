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

# --- 1. the declaration, which is no longer in the page ----------------
#
# It used to be a `<!-- release-walkthrough: ... -->` comment in the
# runbook, and the three cases here drove that comment: absent, empty,
# naming a job the workflow does not have. They are replaced, not
# dropped, because the thing they drove is gone: the set now lives on
# the jobs in the workflow.
#
# WHY IT MOVED, measured. Narrow the page's declaration from `release,
# promote-latest` to `release` and delete the alias promotion from the
# prose, and the gate printed "walks release step for step" and exited
# 0. Rules 3 and 4 are unconditional, so the proofs and the counts
# stayed covered; the step lists did not. The page decided what it
# would be judged on.
drop_markers() { sed -i '/^    # runbook-walkthrough:/d' "$1"; }
run "a workflow where no job carries the marker is a refusal" \
    2 none drop_markers "having compared no step lists"

# A marker on a job with no named steps is the ghost case's successor:
# a job cannot be declared that does not exist, because the marker
# lives inside the job, but it can be put on one there is nothing to
# walk. That has to be a finding and not a silent pass.
mark_a_stepless_job() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]
lines = open(p, encoding="utf-8").read().split("\n")
out = []
for ln in lines:
    out.append(ln)
    if ln == "  resolve:":
        out.append("    # runbook-walkthrough: marker on a job with nothing to walk")
open(p, "w", encoding="utf-8").write("\n".join(out))
PY
    # `resolve` has steps, so strip them: what this case is about is a
    # marked job the gate can read no step list from.
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p, encoding="utf-8").read()
s = re.sub(r"\n  resolve:.*?(?=\n  [a-z0-9_-]+:\n)",
           "\n  resolve:\n    # runbook-walkthrough: marker on a job with nothing to walk\n"
           "    runs-on: ubuntu-latest\n    steps:\n"
           "      - run: echo nothing named here\n", s, count=1, flags=re.S)
open(p, "w", encoding="utf-8").write(s)
PY
}
run "a marker on a job with no named steps fails" \
    1 none mark_a_stepless_job "has no named steps"

# And the page may not carry a second declaration of the same set. Two
# declarations disagree the day one of them is edited, and the one in
# the page is the one a page edit can reach.
stale_page_declaration() {
    sed -i '1a <!-- release-walkthrough: release -->' "$1"
}
run "a page that still declares the walked set is a refusal" \
    2 stale_page_declaration none "two declarations of one set"

# 1b. THE MUTANT THIS MOVE EXISTS FOR. A step of a walked job deleted
#     from the page, with no declaration in the page to narrow. Under
#     the old mechanism this passed as long as the page also stopped
#     declaring the job.
drop_promotion_step_from_page() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]
s = open(p, encoding="utf-8").read()
old = "*Promote the Hub alias floating tags*"
assert s.count(old) >= 1, "the alias promotion is named in the page"
open(p, "w", encoding="utf-8").write(s.replace(old, "*Promote the floating tags*"))
PY
}
run "a walked job's step deleted from the page fails, with no way to narrow" \
    1 drop_promotion_step_from_page none "Promote the Hub alias floating tags"

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
