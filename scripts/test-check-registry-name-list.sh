#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-registry-name-list.sh (#972).
#
# Same bargain as the parity suite next door: the real gate is run
# against mutated copies of the real release.yml, never a reimplementation
# of either. Each mutator is checked for having changed the file at all,
# and most carry a postcondition saying what the change had to achieve --
# a `sed` whose anchor has rotted leaves a case asserting the control's
# verdict, which is a case that has gone quiet rather than red.
#
# THE CASE THAT CARRIES THE WEIGHT is `rebind-in-a-job`: a job-level
# `env:` binding HUB_ALIAS to a different repository. That is the exact
# shape this gate exists for, it is what the workflow looked like before
# this change, and every other gate in the tree reads it as one cell
# because they all key on the variable and not on its value.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(cd "$(dirname "$0")" && pwd)/check-registry-name-list.sh"
SRC="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/release.yml"
guarded_tmpdir TMP

pass=0; fail=0

[ -f "$SRC" ] || { echo "FAIL: no release.yml at $SRC"; exit 1; }

run() {
    local name="$1" want="$2" mut="$3" needle="${4:-}" post="${5:-}" f out got
    f="$TMP/wf.yml"
    cp "$SRC" "$f"
    [ "$mut" = "none" ] || "$mut" "$f"
    if [ "$mut" != "none" ] && cmp -s "$SRC" "$f"; then
        echo "FAIL: $name — the mutator \`$mut\` left release.yml byte-identical"
        echo "      Its anchor no longer matches the workflow, so this case has been"
        echo "      running the gate over an UNMUTATED file."
        fail=$((fail + 1)); return
    fi
    if [ -n "$post" ] && ! "$post" "$f"; then
        echo "FAIL: $name — \`$mut\` changed the file, but \`$post\` says it did not"
        echo "      reach what this case is about."
        fail=$((fail + 1)); return
    fi
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

nocomment() { grep -v '^[[:space:]]*#' "$1"; }

# --- the control -------------------------------------------------------
run "the release workflow as it stands binds each name once" 0 none "one binding for each published name"

# --- 1. the list itself ------------------------------------------------
# An empty list makes every other rule true of nothing. The block is
# emptied by renaming the variables, not by deleting the file, so what
# is measured is the derivation and not the absence of a workflow.
# A plausible refactor, not a deletion: the three names move to
# repository-level `vars`, so the workflow still has a list and the jobs
# still work, and this gate can no longer read a single value out of the
# file. That has to refuse, because "every name passes" over a list it
# could not derive is the strongest possible false pass.
empty_list() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
for v in ("GHCR_NAME", "HUB_NAME", "HUB_ALIAS"):
    s = re.sub(r"\n  %s: \S+" % v, "\n  %s: ${{ vars.%s }}" % (v, v), s, count=1)
open(p, "w").write(s)
PY
}
list_is_empty() {
    [ "$(nocomment "$1" | grep -Ec '^  (GHCR_NAME|HUB_NAME|HUB_ALIAS): \$\{\{ vars\.' || true)" -eq 3 ]
}
run "an empty name list is a refusal, not a pass over nothing" \
    2 empty_list "measured no names at all" list_is_empty

# Hub names gone, GHCR name still bound: rule 4 has nothing to range
# over and says so, instead of reporting that every Hub name passed.
ghcr_only() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r"\n  HUB_NAME: \S+", "", s, count=1)
s = re.sub(r"\n  HUB_ALIAS: \S+", "", s, count=1)
open(p, "w").write(s)
PY
}
run "a list with no Hub repository is a refusal" \
    2 ghcr_only "binds no Docker Hub repository" ""

# --- 2. a rebinding ----------------------------------------------------
# THE SHAPE THIS GATE EXISTS FOR. A verifying job binds HUB_ALIAS to a
# different repository; every cell-keyed gate in the tree still reports
# parity, because they all key on the variable.
rebind_in_a_job() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]; lines = open(p).read().split("\n")
for i, l in enumerate(lines):
    if l == "  verify-install-hub-alias:":
        for j in range(i, len(lines)):
            if lines[j] == "    env:":
                lines.insert(j + 1, "      HUB_ALIAS: claymore666/some-other-name")
                break
        break
open(p, "w").write("\n".join(lines))
PY
}
rebinding_present() {
    nocomment "$1" | grep -E '^      HUB_ALIAS: claymore666/some-other-name$' >/dev/null
}
run "a job that rebinds a listed name fails" \
    1 rebind_in_a_job "rebinds HUB_ALIAS" rebinding_present

# The SAME rebinding, to the SAME value the list already holds, is still
# a finding: an identical copy today is a second place to edit tomorrow,
# and that is how the five transcriptions were justified each time.
rebind_same_value() {
    python3 - "$1" <<'PY'
import sys
p = sys.argv[1]; lines = open(p).read().split("\n")
for i, l in enumerate(lines):
    if l == "  verify-install-hub:":
        for j in range(i, len(lines)):
            if lines[j] == "    env:":
                lines.insert(j + 1, "      HUB_NAME: claymore666/net-dhcp")
                break
        break
open(p, "w").write("\n".join(lines))
PY
}
run "a job that rebinds a listed name to the same string still fails" \
    1 rebind_same_value "rebinds HUB_NAME" ""

# --- 3. a stray literal ------------------------------------------------
literal_in_a_run() {
    sed -i 's|"docker\.io/\${HUB_ALIAS}:\${TAG}"|"docker.io/claymore666/docker-net-dhcp:${TAG}"|' "$1"
}
literal_present() {
    nocomment "$1" | grep -F 'docker.io/claymore666/docker-net-dhcp:${TAG}' >/dev/null
}
run "a repository literal written into a run line fails" \
    1 literal_in_a_run "instead of \${HUB_ALIAS}" literal_present

# A literal in a COMMENT is prose and is not a finding. Without this the
# gate would be answered by rewriting sentences, and the next author
# would learn to keep the documentation vague.
literal_in_a_comment() {
    sed -i '0,/^# THE PUBLISHED NAMES, ONCE (#972)\./s||# THE PUBLISHED NAMES, ONCE (#972). One is claymore666/net-dhcp.|' "$1"
}
comment_carries_literal() {
    grep -F '# THE PUBLISHED NAMES, ONCE (#972). One is claymore666/net-dhcp.' "$1" >/dev/null
}
run "a repository name inside a comment is prose, not a binding" \
    0 literal_in_a_comment "one binding for each published name" comment_carries_literal

# ...and the same thing OUTSIDE the name list, which is the case that
# actually drives the comment guard. The fixture above puts its literal
# in the block's own header comment, and rule 3 skips that whole region
# by line number, so deleting the comment guard leaves it passing for a
# reason that has nothing to do with comments. A mutant answered by a
# different guard measures the other guard.
literal_in_a_job_comment() {
    sed -i '0,/^      # OUR copy of the resolver, checked out before the tag the input$/s||      # OUR copy of the resolver. It publishes claymore666/net-dhcp one day|' "$1"
}
job_comment_carries_literal() {
    nocomment "$1" | grep -F 'It publishes claymore666/net-dhcp one day' >/dev/null && return 1
    grep -F '# OUR copy of the resolver. It publishes claymore666/net-dhcp one day' "$1" >/dev/null
}
run "a repository name in a job comment is prose too" \
    0 literal_in_a_job_comment "one binding for each published name" \
    job_comment_carries_literal

# --- 4. the consumers with no other observer ---------------------------
drop_alias_description() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r"\n      - name: Sync the Hub alias description from README.*?readme-filepath: \./README\.md\n",
           "\n", s, count=1, flags=re.S)
open(p, "w").write(s)
PY
}
no_alias_description() {
    ! nocomment "$1" | grep -F 'repository: ${{ env.HUB_ALIAS }}' >/dev/null &&
        nocomment "$1" | grep -F 'repository: ${{ env.HUB_NAME }}' >/dev/null
}
run "a Hub name whose description is never synced fails" \
    1 drop_alias_description "a Docker Hub description sync" no_alias_description

drop_alias_from_notes() {
    sed -i 's| and \\`\${HUB_ALIAS}\\`, the same digest under both names||' "$1"
}
alias_out_of_notes() {
    ! nocomment "$1" | grep -F 'mirrored to Docker Hub as' | grep -F 'HUB_ALIAS' >/dev/null
}
run "a Hub name the release notes never mention fails" \
    1 drop_alias_from_notes "the step that writes the release notes" alias_out_of_notes

drop_hub_signature() {
    sed -i 's|cosign sign --yes "\${HUB_NAME}@\${HUB_DIGEST}"|true # signing removed|g' "$1"
}
no_hub_sign() {
    ! nocomment "$1" | grep -F 'cosign sign --yes "${HUB_NAME}' >/dev/null &&
        nocomment "$1" | grep -F 'cosign sign --yes "${GHCR_NAME}' >/dev/null
}
run "a Hub name nothing signs fails" \
    1 drop_hub_signature "a cosign signature" no_hub_sign

# THE OTHER DIRECTION, so the four rules are not measured as "hard".
# GHCR has no description sync and no mirror line of its own, and it
# must not be reported for either: a rule invented so every name could
# be held to it would fail the control on the day it was written.
run "the GHCR name is not held to the Hub-only roles" 0 none "GHCR_NAME=ghcr.io/"

# --- the gate's own inputs ---------------------------------------------
out=$(bash "$GATE" "$TMP/does-not-exist.yml" 2>&1); got=$?
if [ "$got" -eq 2 ] && printf '%s\n' "$out" | grep -F 'there is no name list' >/dev/null; then
    echo "ok: a missing workflow is a refusal"; pass=$((pass + 1))
else
    echo "FAIL: a missing workflow is a refusal — got exit $got"; fail=$((fail + 1))
fi

# --- 5. A NAME CAN LEAVE THE LIST (#972 round 3) -----------------------
#
# Every rule up to here ranges over the list, so every one of them is
# satisfied by taking a name OUT of it. That is not a contrived shape:
# it is exactly what this workflow looked like before this change, and
# it was measured to pass the gate written to forbid it.

# 5a. The round-1 shape, restored: HUB_ALIAS leaves the workflow-level
#     block and is transcribed into the jobs that used it. Measured
#     against the gate as it stood: exit 0, printing OK over the two
#     names left, as if nothing were missing.
delist_into_jobs() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p, encoding="utf-8").read()
s = s.replace("  HUB_ALIAS: claymore666/docker-net-dhcp\n", "", 1)
lines, cur, uses = s.split("\n"), None, set()
for ln in lines:
    m = re.match(r"^  ([A-Za-z0-9_-]+):\s*$", ln)
    if m:
        cur = m.group(1)
    if cur and "HUB_ALIAS" in ln:
        uses.add(cur)
out, cur = [], None
for ln in lines:
    m = re.match(r"^  ([A-Za-z0-9_-]+):\s*$", ln)
    if m:
        cur = m.group(1)
    out.append(ln)
    if ln == "    env:" and cur in uses:
        out.append("      HUB_ALIAS: claymore666/docker-net-dhcp")
open(p, "w", encoding="utf-8").write("\n".join(out))
PY
}
alias_left_the_list() {
    ! grep -q '^  HUB_ALIAS:' "$1" && grep -q '^      HUB_ALIAS:' "$1"
}
run "a listed name transcribed into the jobs that used it fails" \
    1 delist_into_jobs "outside the workflow-level list" alias_left_the_list

# 5b. The reason it matters, one step further along: with the name out
#     of the list, one job's copy can point somewhere else entirely and
#     the install proof proves a different repository. The finding has
#     to NAME the repository, because the whole defect is that two
#     strings are wearing one variable.
delist_and_diverge() {
    delist_into_jobs "$1"
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]
lines, cur = open(p, encoding="utf-8").read().split("\n"), None
for i, ln in enumerate(lines):
    m = re.match(r"^  ([A-Za-z0-9_-]+):\s*$", ln)
    if m:
        cur = m.group(1)
    if cur == "verify-install-hub-alias" and ln.strip() == "HUB_ALIAS: claymore666/docker-net-dhcp":
        lines[i] = "      HUB_ALIAS: claymore666/some-other-repo"
open(p, "w", encoding="utf-8").write("\n".join(lines))
PY
}
diverged() { grep -q 'claymore666/some-other-repo' "$1"; }
run "a de-listed name bound to two repositories fails, naming both" \
    1 delist_and_diverge "claymore666/some-other-repo" diverged

# 5c. THE POPULATION RULE ON ITS OWN. 5a and 5b are also caught by the
#     binding rule, and a case that two rules answer cannot say which
#     one is alive. Here the name simply leaves the list and reaches
#     the steps from outside the file, so there is no second binding to
#     find: only the reconciliation against the step lines is left.
delist_only() { sed -i '/^  HUB_ALIAS: claymore666\/docker-net-dhcp$/d' "$1"; }
no_alias_binding() { ! grep -qE '^\s+HUB_ALIAS:' "$1"; }
run "a name used by the steps and bound nowhere in the file fails" \
    1 delist_only "uses \${HUB_ALIAS} as a registry name" no_alias_binding

# 5d. And its non-vacuity guard, driven the way every other one here is:
#     a workflow whose step lines use no variable as a registry
#     reference gives rule 3b nothing to range over, which is a refusal
#     and not a pass.
no_reference_uses() {
    sed -i 's/\${\(GHCR_NAME\|HUB_NAME\|HUB_ALIAS\)}[:@]/PLACEHOLDER/g' "$1"
}
# `${REF}` and `${reg}` are LOCAL shell variables the file assigns, so
# they are not in the population either way; what this case has to
# achieve is that none of the LISTED names is used as a reference.
no_refs_left() { ! grep -qE '\$\{(GHCR_NAME|HUB_NAME|HUB_ALIAS)\}[:@]' "$1"; }
run "a workflow whose steps use no name as a reference refuses" \
    2 no_reference_uses "has nothing to be the list OF" no_refs_left

# --- 6. THE LITERAL RULE MUST NOT MISDIRECT (#972 round 3) -------------
#
# It was a plain substring test, and a substring test on a repository
# name is wrong twice over.

# 6a. A project URL contains the repository name and is not a registry
#     reference. The advice the old rule printed here -- write
#     ${HUB_ALIAS} instead -- would have broken the link.
project_url() {
    sed -i '0,/^          set -euo pipefail$/s||          set -euo pipefail\n          echo "Source: https://github.com/claymore666/docker-net-dhcp/releases"|' "$1"
}
url_present() { grep -q 'github.com/claymore666/docker-net-dhcp/releases' "$1"; }
run "a project URL carrying the repository name is not a transcription" \
    0 project_url "one binding for each published name" url_present

# 6b. GHCR_NAME's value CONTAINS HUB_ALIAS's value, so one legitimate
#     GHCR literal used to produce TWO findings, the second of them
#     telling the author to write the Hub variable into a GHCR
#     reference. The occurrence belongs to the longest name that
#     matches, and it is reported once.
ghcr_literal() {
    sed -i '0,/^          set -euo pipefail$/s||          set -euo pipefail\n          echo "ghcr.io/claymore666/docker-net-dhcp:v1.0.0"|' "$1"
}
ghcr_literal_present() { grep -q 'echo "ghcr.io/claymore666/docker-net-dhcp:v1.0.0"' "$1"; }
run "a GHCR literal is one finding, against the GHCR name" \
    1 ghcr_literal "instead of \${GHCR_NAME}" ghcr_literal_present

literal_finding_count() {
    bash "$GATE" "$1" 2>&1 | grep -c "^FAIL: .*writes the literal"
}
f="$TMP/one.yml"; cp "$SRC" "$f"; ghcr_literal "$f"
n=$(literal_finding_count "$f")
if [ "$n" -eq 1 ]; then
    echo "ok: and exactly one, not one per name whose value is a substring"
    pass=$((pass + 1))
else
    echo "FAIL: a single GHCR literal produced $n literal findings, want 1"
    fail=$((fail + 1))
fi

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
