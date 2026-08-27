#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-publish-verify-parity.sh (#833).
#
# Runs the real shipped gate against mutated copies of the real
# release.yml. The gate is copied, never reimplemented, so this cannot
# pass over a rewritten check.
#
# THE CASE THAT CARRIES THE WEIGHT is `runner-arch-is-not-cell-arch`.
# The first version of this gate keyed each cell's architecture on its
# job's `runs-on`, and reported the two arm64 cells unpromoted --
# because `promote-latest` retags both architectures from one amd64
# runner. That was a false failure manufactured by the instrument. The
# case below moves the runner and asserts the verdict does NOT move,
# so the fix cannot be undone by someone reaching for the obvious key.
set -uo pipefail

GATE="$(cd "$(dirname "$0")" && pwd)/check-publish-verify-parity.sh"
SRC="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/release.yml"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0

[ -f "$SRC" ] || { echo "FAIL: no release.yml at $SRC"; exit 1; }

# run NAME WANT_RC MUTATOR [NEEDLE]
run() {
    local name="$1" want="$2" mut="$3" needle="${4:-}" f out got
    f="$TMP/wf.yml"
    cp "$SRC" "$f"
    [ "$mut" = "none" ] || "$mut" "$f"
    out=$(bash "$GATE" "$f" 2>&1); got=$?
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
# If this fails every mutant below is noise: a gate that refuses the real
# workflow would "catch" every mutation for the wrong reason.
run "the release workflow as it stands is in parity" 0 none "4 published cell(s)"

# --- the defect the issue is about -------------------------------------
# 20 tags shipped a Hub artifact nothing proved installable. Drop the Hub
# arm64 verifier and the gate must name exactly that cell.
drop_hub_arm_verify() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r"\n  verify-install-hub-arm64:.*?(?=\n  [a-z0-9_-]+:\n)", "\n", s, flags=re.S)
open(p, "w").write(s)
PY
}
run "a published cell with no install verifier fails" 1 drop_hub_arm_verify "arm64/HUB_NAME"

drop_hub_promote() {
    sed -i '/crane tag "${HUB_NAME}/d' "$1"
}
run "a published cell that never reaches :latest fails" 1 drop_hub_promote "HUB_NAME"

# --- a NEW registry, which is the thing this gate is for ---------------
# The whole point is that adding a registry cannot ship unverified. A
# transcribed list of four job names would pass this.
add_third_registry() {
    sed -i 's|^\( *\)run: make PLUGIN_NAME="${GHCR_NAME}" PLUGIN_TAG="${TAG}" push|\1run: make PLUGIN_NAME="${GHCR_NAME}" PLUGIN_TAG="${TAG}" push\n\1run: make PLUGIN_NAME="${QUAY_NAME}" PLUGIN_TAG="${TAG}" push|' "$1"
}
run "a newly published registry with no verifier fails" 1 add_third_registry "QUAY_NAME"

# --- the advertisement is not the command ------------------------------
# The release job PRINTS `docker plugin install ...` into the step
# summary as instructions. If that counted, a workflow that verified
# nothing would look fully verified.
echo_only_verify() {
    sed -i 's|docker plugin install --grant-all-permissions "$REF"|echo "docker plugin install $REF"|g' "$1"
}
run "an echoed install does not count as verification" 2 echo_only_verify "no longer matches"

# --- the instrument's own failure mode (regression control) ------------
# `promote-latest` retags BOTH architectures from ONE amd64 runner. A
# gate keying arch on `runs-on` calls the arm64 cells unpromoted. Moving
# the runner must not move the verdict.
promote_on_arm() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r"(\n  promote-latest:\n(?:.*\n)*?    runs-on: )ubuntu-latest",
           r"\1ubuntu-24.04-arm", s)
open(p, "w").write(s)
PY
}
run "the promote runner's arch is not the cell's arch" 0 promote_on_arm "4 published cell(s)"

# And the same claim from the other side: keying on the runner is what
# the gate must NOT do, so prove a runner-keyed reading disagrees here.
# Without this, the case above passes for any gate that ignores runners
# entirely -- including one that ignores architecture altogether.
f="$TMP/orth.yml"; cp "$SRC" "$f"; promote_on_arm "$f"
# `grep -F ... >/dev/null` and not `grep -q`: a piped -q exits at the
# first match and SIGPIPEs the producer, so under pipefail the pipeline
# reports failure on success. Redirecting reads to EOF, so the status is
# the real one.
if grep -A20 '^  promote-latest:' "$f" | grep -F 'runs-on: ubuntu-24.04-arm' >/dev/null; then
    echo "ok: the fixture really did move promote-latest onto an arm runner"
    pass=$((pass + 1))
else
    echo "FAIL: the fixture did not move the runner, so the case above proves nothing"
    fail=$((fail + 1))
fi

# --- non-vacuity: a universal is true over an empty domain -------------
# Each of these breaks one detector. The gate must refuse, not report
# the strongest possible pass.
break_publish() { sed -i 's/PLUGIN_NAME=/PLUGIN_NOM=/g' "$1"; }
run "zero derived publish cells is a refusal" 2 break_publish "ZERO published cells"

break_promote() { sed -i 's/crane tag/crane retag/g' "$1"; }
run "zero derived promote cells is a refusal" 2 break_promote "ZERO promoted cells"

# --- an unresolvable tag is a refusal, not a guess ---------------------
# Treating an unbound name as amd64 would merge an arm64 cell into its
# neighbour and report a parity nobody checked.
unbind_tag() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r'PLUGIN_TAG="\$\{TAG\}" push', 'PLUGIN_TAG="${NOSUCH}" push', s, count=1)
open(p, "w").write(s)
PY
}
run "a tag variable the job never binds is a refusal" 2 unbind_tag "never binds"

# --- unreadable input --------------------------------------------------
missing_rc=$(bash "$GATE" "$TMP/does-not-exist.yml" >/dev/null 2>&1; echo $?)
if [ "$missing_rc" -eq 2 ]; then
    echo "ok: a missing workflow is a refusal"; pass=$((pass + 1))
else
    echo "FAIL: a missing workflow — want exit 2, got $missing_rc"; fail=$((fail + 1))
fi

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
