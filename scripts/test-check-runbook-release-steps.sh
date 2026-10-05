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

# --- 5. every job is named, emphasised (#799) -------------------------
# `resolve` went unnamed for a cycle and `production-shape` (#1014) after
# it; rule 3 only looked at `verify-install*`.
add_job_to_workflow() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]; s = open(p).read()
s = s.replace("\n  promote-latest:\n",
              "\n  publish-to-a-third-registry:\n    needs: [release]\n"
              "    runs-on: ubuntu-latest\n    steps:\n"
              "      - name: Push somewhere new\n        run: true\n\n"
              "  promote-latest:\n", 1)
open(p, "w").write(s)
PY
}
run "a job added to the workflow and not to the page fails" \
    1 none add_job_to_workflow "publish-to-a-third-registry"

# shellcheck disable=SC2016 # the backticks are literal markdown
unname_production_shape() { sed -i 's/\*\*production-shape\*\*/the engine gate/g; s/`production-shape`/the engine gate/g' "$1"; }
run "the engine gate job unnamed on the page fails" \
    1 unname_production_shape none "production-shape"

# Prose use is not naming: `resolve` and `release` are ordinary words.
# shellcheck disable=SC2016 # the backticks are literal markdown
unemphasise_resolve() { sed -i 's/\*\*resolve\*\*/resolve/g; s/`resolve`/resolve/g' "$1"; }
run "a job named only as a plain word fails" \
    1 unemphasise_resolve none "'resolve'"

# --- 6. the page's step chains name real steps, in order (#799) --------
ghost_step_in_chain() {
    sed -i 's|Log in to Docker Hub → \*\*Both registries|Log in to Docker Hub → **Warn if Docker Hub credentials missing** → **Both registries|' "$1"
}
run "a chain naming a step the job does not run fails" \
    1 ghost_step_in_chain none "Warn if Docker Hub credentials missing"

foreign_step_opens_chain() {
    sed -i 's|in this order: checkout → setup-go|in this order: Resolve release tag → checkout → setup-go|' "$1"
}
run "a chain opening with another job's step fails" \
    1 foreign_step_opens_chain none "Resolve release tag"

swap_chain_order() {
    sed -i 's|Install syft → \*\*Generate SBOM (SPDX + CycloneDX)\*\*|**Generate SBOM (SPDX + CycloneDX)** → Install syft|' "$1"
}
run "a chain out of workflow order fails" \
    1 swap_chain_order none "out of order"

# Preservation: prose with an arrow and one step name in it is not a chain
# of that job, so it is not judged as one.
prose_arrow_with_one_step() {
    sed -i '1a\\nIf it hangs, read in turn: Install cosign → the runner logs.' "$1"
}
run "a prose arrow sharing one step name is not judged as a chain" \
    0 prose_arrow_with_one_step none

# Triggers sit at a job's indent under `on:`; they are not jobs.
add_unnamed_trigger() {
    sed -i 's/^  workflow_dispatch:$/  repository_dispatch:\n    types: [rebuild]\n  workflow_dispatch:/' "$1"
}
run "a trigger the page never names is not demanded as a job" \
    0 none add_unnamed_trigger

drop_promote_chain() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]; s = open(p).read()
a = s.index("Steps: *Refuse to")
b = s.index(":latest*.", a)
s = s[:a] + "Steps: see the run." + s[b + len(":latest*."):]
open(p, "w").write(s)
PY
}
run "a walked job with no chain on the page fails" \
    1 drop_promote_chain none "no step chain"

# --- 7. the arm64 chain does not wait on the amd64 build (#799) --------
arm_proof_needs_release() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s, n = re.subn(r"(\n  verify-install-hub-alias-arm64:\n    needs: )\[[^\]]*\]",
               r"\1[release, release-arm64]", s)
assert n == 1
open(p, "w").write(s)
PY
}
run "an arm64 install proof that names release fails" \
    1 none arm_proof_needs_release "verify-install-hub-alias-arm64"

# Through an amd64 proof, in the scalar form `verify-install` itself uses.
arm_proof_reaches_release() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s, n = re.subn(r"(\n  verify-install-arm64:\n    needs: )\[[^\]]*\]",
               r"\1verify-install", s)
assert n == 1
open(p, "w").write(s)
PY
}
run "an arm64 job reaching release through needs fails" \
    1 none arm_proof_reaches_release "'verify-install-arm64' is in the arm64 chain and reaches 'release'"

arm_job_without_suffix() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]; s = open(p).read()
s = s.replace("\n  promote-latest:\n",
              "\n  smoke:\n    needs: [release]\n"
              "    runs-on: ubuntu-24.04-arm\n    steps:\n"
              "      - name: Smoke\n        run: true\n\n"
              "  promote-latest:\n", 1)
open(p, "w").write(s)
PY
}
run "an arm64 job without the suffix that waits on release fails" \
    1 none arm_job_without_suffix "'smoke' is in the arm64 chain"

# The other key: an arm64-tag job on an amd64 runner is still in the chain.
arm_suffix_on_amd64_runner() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s, n = re.subn(r"(\n  verify-install-hub-alias-arm64:\n)    needs: \[[^\]]*\]\n    runs-on: [^\n]*\n",
               r"\1    needs: [release, release-arm64]\n    runs-on: ubuntu-latest\n", s)
assert n == 1
open(p, "w").write(s)
PY
}
run "an -arm64 job on an amd64 runner that waits on release fails" \
    1 none arm_suffix_on_amd64_runner "'verify-install-hub-alias-arm64' is in the arm64 chain"

block_needs() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]; s = open(p).read()
old = "\n  verify-install:\n    needs: release\n"
assert s.count(old) == 1
s = s.replace(old, "\n  verify-install:\n    needs:\n      - release\n")
open(p, "w").write(s)
PY
}
run "a block-form needs: is a refusal" \
    2 none block_needs "block form"

no_arm_jobs() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r"\n  [a-z0-9-]*-arm64:\n.*?(?=\n  [a-z0-9_-]+:\n)", "\n", s, flags=re.S)
s = s.replace("ubuntu-24.04-arm", "ubuntu-24.04")
open(p, "w").write(s)
PY
}
run "a workflow with no arm64 job is a refusal" \
    2 none no_arm_jobs "no arm64 job"

rename_release_job() { sed -i 's/^  release:$/  release-amd64:/' "$1"; }
run "a workflow with no job named release fails" \
    1 none rename_release_job "no job named 'release'"

# The other half: a proof that stops waiting on the build it installs.
arm_proof_skips_its_build() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s, n = re.subn(r"(\n  verify-install-arm64:\n    needs: )\[[^\]]*\]", r"\1[resolve]", s)
assert n == 1
open(p, "w").write(s)
PY
}
run "an arm64 proof that does not wait on release-arm64 fails" \
    1 none arm_proof_skips_its_build "can run before the arm64 tag exists"

rename_arm_build() { sed -i 's/^  release-arm64:$/  build-arm64:/' "$1"; }
run "a workflow with no job named release-arm64 fails" \
    1 none rename_arm_build "no job named 'release-arm64'"

# Two spellings the line reader once skipped (#799 review): a quoted
# entry, and a scalar with a trailing comment one hop away.
quoted_release_in_arm_needs() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s, n = re.subn(r"(\n  verify-install-arm64:\n    needs: )\[[^\]]*\]",
               r"\1['release', release-arm64]", s)
assert n == 1
open(p, "w").write(s)
PY
}
run "an arm64 proof naming release in quotes fails" \
    1 none quoted_release_in_arm_needs "'verify-install-arm64' is in the arm64 chain and reaches 'release'"

commented_scalar_on_the_path() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s, n = re.subn(r"(\n  verify-install-hub-arm64:\n    needs: )\[[^\]]*\]",
               r"\1[resolve, verify-install, release-arm64]", s)
assert n == 1
old = "\n  verify-install:\n    needs: release\n"
assert s.count(old) == 1
s = s.replace(old, "\n  verify-install:\n    needs: release  # amd64 proof\n")
open(p, "w").write(s)
PY
}
run "an arm64 proof reaching release through a commented needs: fails" \
    1 none commented_scalar_on_the_path "'verify-install-hub-arm64' is in the arm64 chain and reaches 'release'"

anchor_needs() {
    sed -i 's/^    needs: release$/    needs: *amd64/' "$1"
}
run "a needs: that is an anchor is a refusal" \
    2 none anchor_needs "cannot read"

multiline_flow_needs() {
    sed -i 's/^    needs: \[resolve, release-arm64\]$/    needs: [resolve,\n            release-arm64]/' "$1"
}
run "a needs: list over several lines is a refusal" \
    2 none multiline_flow_needs "cannot read"

# The chain threshold: two real steps make a chain, so its ghost is seen.
ghost_after_two_steps() {
    sed -i '1a\\nOn a retry, read in turn: Install crane → Log in to GHCR → Wave a flag.' "$1"
}
run "a chain of two real steps and a ghost fails" \
    1 ghost_after_two_steps none "'Wave a flag'"

# The rc watch list says every job; github-release was left out of it.
watch_list_drops_a_job() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]; s = open(p).read()
old = ", and\n**github-release**, which publishes an rc as a draft (#469)."
assert s.count(old) == 1
s = s.replace(old, ".")
open(p, "w").write(s)
PY
}
run "an rc watch list leaving out a job fails" \
    1 watch_list_drops_a_job none "leaves out 'github-release'"

no_watch_list() { sed -i 's/every job must be green:/watch these:/' "$1"; }
run "a page with no rc watch list fails" \
    1 no_watch_list none "has 0 paragraphs saying 'every job must be green'"

two_watch_lists() { sed -i '1a\\nOn a re-run, every job must be green as well.' "$1"; }
run "a page with two rc watch lists fails" \
    1 two_watch_lists none "has 2 paragraphs saying 'every job must be green'"

# In the watch list too a job counts only emphasised: plain `release`
# is also a word, and inside `release-arm64`.
watch_list_plain_release() {
    sed -i 's/^\*\*production-shape\*\* (both builds wait on it), \*\*release\*\* and$/**production-shape** (both builds wait on it), release and/' "$1"
}
run "an rc watch list naming a job only as a plain word fails" \
    1 watch_list_plain_release none "leaves out 'release'"

# Preservation: the claim is found across a line wrap.
watch_list_wrapped() { sed -i 's/every job must be green:/every job must\nbe green:/' "$1"; }
run "an rc watch list wrapped mid-claim is still read" \
    0 watch_list_wrapped none

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
