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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(cd "$(dirname "$0")" && pwd)/check-publish-verify-parity.sh"
SRC="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/release.yml"
guarded_tmpdir TMP

pass=0; fail=0

[ -f "$SRC" ] || { echo "FAIL: no release.yml at $SRC"; exit 1; }

# cmds is the workflow with its comments removed. Every claim below is
# about what the release lane RUNS; a sentence in a comment that happens
# to spell `crane tag` or `docker plugin install` is prose, and a
# mutator or an assertion that counted it would be measuring the
# documentation.
cmds() { grep -v '^[[:space:]]*#' "$1"; }

# A COPY LINE IS KEYED ON ITS SHAPE, NOT ON ITS TOOL. The alias copy was
# `oras cp -r SRC DST` and is now `scripts/publish-hub-alias.sh
# --expect-digest D SRC DST`; the gate reads neither tool name, only
# that the line ENDS in a quoted registry reference it is writing to.
# Every mutator and census below keys on the same shape, so changing the
# tool again moves the gate and this suite together instead of leaving
# one of them anchored on a word that no longer appears.
copy_lines() {
    cmds "$1" \
        | grep -E '[[:space:]]"[^"]*\$\{[A-Z_]+\}:\$\{[A-Z_]+\}"[[:space:]]*$' \
        | grep -Ev '^[[:space:]]*(echo|printf)[[:space:]]'
}

# run NAME WANT_RC MUTATOR [NEEDLE [POSTCONDITION]]
#
# THE MUTATION IS CHECKED, NOT ASSUMED. Four mutators here are `sed`
# expressions anchored on text from release.yml. An edit to that text --
# a flag added to an install, a variable renamed, a line rewrapped --
# leaves the sed matching nothing, and a case whose mutant is the
# unmutated file asserts only what the control already asserts: it goes
# quiet instead of red, which is the failure mode this whole script
# exists to prevent in the gate.
#
# So: any mutator that leaves the file byte-identical fails the case,
# loudly, by name. That covers every present mutator and every future
# one without anyone remembering to add a check -- the repair `sed` and
# `python3` mutators need equally, applied once at the call site rather
# than four times inside the mutators.
#
# It is not sufficient on its own. A mutator can change SOMETHING and
# still miss the site the case is about (three install verifiers
# rewritten and a fourth left alone, and the gate still refuses -- for
# the three). POSTCONDITION, where given, is a function handed the
# mutated file that says what the mutation was supposed to achieve.
run() {
    local name="$1" want="$2" mut="$3" needle="${4:-}" post="${5:-}" f out got
    f="$TMP/wf.yml"
    cp "$SRC" "$f"
    [ "$mut" = "none" ] || "$mut" "$f"
    if [ "$mut" != "none" ] && cmp -s "$SRC" "$f"; then
        echo "FAIL: $name — the mutator \`$mut\` left release.yml byte-identical"
        echo "      Its anchor no longer matches the workflow, so this case has been"
        echo "      running the gate over an UNMUTATED file and reporting the control's"
        echo "      verdict. Re-anchor it on the property, not on the line."
        fail=$((fail + 1))
        return
    fi
    if [ -n "$post" ] && ! "$post" "$f"; then
        echo "FAIL: $name — \`$mut\` changed the file, but \`$post\` says it did not"
        echo "      reach what this case is about. The mutation is partial: some sites"
        echo "      moved and at least one did not."
        fail=$((fail + 1))
        return
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

# --- what each mutation has to have achieved ---------------------------
# Each is a property of the MUTATED file, derived from it rather than
# transcribed as a count: "no real install command survives", not "four
# lines changed". A count would go red the day the release lane grows a
# fifth cell, which is a true change and not a defect; these go red only
# when a site the mutator was supposed to reach is still standing.

# Every `docker plugin install` left in the file is inside an echo. If
# one real invocation survives, the gate can still see a verified cell
# and the case's refusal is about the others.
installs_all_echoed() {
    [ "$(cmds "$1" | grep -c 'docker plugin install' || true)" -gt 0 ] &&
        [ "$(cmds "$1" | grep 'docker plugin install' | grep -vc 'echo' || true)" -eq 0 ]
}

# ... and for the two echoed-with-the-flag cases, the flag has to still
# be there as text. Without this they decay into the case above, whose
# refusal #858 showed is produced by the missing flag and not by the
# echo.
echoed_flag_survives() {
    installs_all_echoed "$1" &&
        cmds "$1" | grep -F -- '--grant-all-permissions' >/dev/null
}

# The Hub retags are gone and the GHCR retags are NOT: this case is
# about one cell losing its promotion, so a mutation that removed every
# retag would be proving `break_promote`'s claim instead.
hub_retag_only_dropped() {
    ! cmds "$1" | grep -F 'crane tag "${HUB_NAME}' >/dev/null &&
        cmds "$1" | grep -F 'crane tag "${GHCR_NAME}' >/dev/null
}

no_crane_tag() { ! cmds "$1" | grep -E 'crane tag ' >/dev/null; }

# No publish invocation is still recognisable, and the `make ... push`
# lines are all still there: the domain was emptied by renaming the
# variable, not by deleting the jobs.
no_publish_name() {
    ! cmds "$1" | grep -F 'PLUGIN_NAME=' >/dev/null &&
        [ "$(copy_lines "$1" | wc -l)" -eq 0 ] &&
        [ "$(cmds "$1" | grep -Ec 'make .*push' || true)" -eq "$(cmds "$SRC" | grep -Ec 'make .*push' || true)" ] &&
        [ "$(cmds "$1" | grep -Ec 'publish-hub-alias\.sh' || true)" -eq "$(cmds "$SRC" | grep -Ec 'publish-hub-alias\.sh' || true)" ]
}

# --- the anchors cover the whole population ----------------------------
# The mutators above are spellings. These three assertions say the
# spellings are the WHOLE of what they claim to mutate, so a new cell
# written a new way is reported here rather than silently escaping every
# mutation. This is the census the `cmp` check cannot do: it sees that a
# mutator changed something, not that it changed everything it should.
census() {
    local name="$1" got="$2" want="$3"
    if [ "$got" -eq "$want" ]; then
        echo "ok: $name"; pass=$((pass + 1))
    else
        echo "FAIL: $name — $got, want $want"; fail=$((fail + 1))
    fi
}

census "every real plugin install carries the flag the mutators anchor on" \
    "$(cmds "$SRC" | grep 'docker plugin install' | grep -vc 'echo' || true)" \
    "$(cmds "$SRC" | grep -Fc 'docker plugin install --grant-all-permissions "$REF"' || true)"

census "every make push invocation carries PLUGIN_NAME=" \
    "$(cmds "$SRC" | grep -Ec 'make .*push' || true)" \
    "$(cmds "$SRC" | grep -E 'make .*push' | grep -Fc 'PLUGIN_NAME=' || true)"

# Keyed on "a crane retag names a VARIABLE", not on which two names
# exist today. The literal alternation was two names when it was
# written; the alias made it three (#972), and a census that has to be
# edited every time a registry is added is a census that will be edited
# to match rather than consulted.
census "every crane retag names a registry variable" \
    "$(cmds "$SRC" | grep -Ec 'crane tag ' || true)" \
    "$(cmds "$SRC" | grep -E 'crane tag ' | grep -Ec 'crane tag "\$\{[A-Z_]+\}' || true)"

# The copy form is a publish too, and it has to be present for the
# cases below to mean anything. Both halves, both derived: every
# invocation of the alias publisher ends in its destination reference,
# which is the shape the gate reads and the argument order the
# publisher's own header says is load-bearing; and at least one copy
# line exists, or every case below mutates nothing.
census "every alias publish invocation ends in its destination reference" \
    "$(cmds "$SRC" | grep -Ec 'publish-hub-alias\.sh' || true)" \
    "$(cmds "$SRC" | grep -E 'publish-hub-alias\.sh' | grep -Ec '[[:space:]]"[^"]*\$\{[A-Z_]+\}:\$\{[A-Z_]+\}"[[:space:]]*$' || true)"

census "the workflow carries at least one copy publish for the cases below" \
    "$(if [ "$(copy_lines "$SRC" | wc -l)" -gt 0 ]; then echo 1; else echo 0; fi)" "1"

# --- the control -------------------------------------------------------
# If this fails every mutant below is noise: a gate that refuses the real
# workflow would "catch" every mutation for the wrong reason.
run "the release workflow as it stands is in parity" 0 none "6 published cell(s)"

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
run "a published cell that never reaches :latest fails" 1 drop_hub_promote "HUB_NAME" hub_retag_only_dropped

# --- a NEW registry, which is the thing this gate is for ---------------
# The whole point is that adding a registry cannot ship unverified. A
# transcribed list of four job names would pass this.
# Anchored on the NAME variable and not on the whole invocation: the
# push line carries whatever else the release needs to pass through it
# (VERSION since 2.0-alpha.1), and a mutator that has to be re-spelled
# every time one is added is a mutator that silently stops mutating --
# which is what happened here, and the case then "passed" by doing
# nothing at all.
add_third_registry() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
re_push = r'^( *)run: (make PLUGIN_NAME="\$\{GHCR_NAME\}".*)$'
m = re.search(re_push, s, flags=re.M)
if m is None:
    raise SystemExit("no GHCR push invocation to duplicate")
line = "%srun: %s\n%srun: %s" % (
    m.group(1), m.group(2),
    m.group(1), m.group(2).replace("${GHCR_NAME}", "${QUAY_NAME}"))
open(p, "w").write(s[:m.start()] + line + s[m.end():])
PY
}
run "a newly published registry with no verifier fails" 1 add_third_registry "QUAY_NAME"

# --- the advertisement is not the command ------------------------------
# The release job PRINTS `docker plugin install ...` into the step
# summary as instructions. If that counted, a workflow that verified
# nothing would look fully verified.
#
# THE CASE BELOW DID NOT MEASURE THAT, AND #858 SHIPPED BECAUSE OF IT.
# Its mutator echoes the command AND deletes `--grant-all-permissions`,
# so the refusal it asserts is produced by the missing flag, not by the
# echo. Driven: the same mutation with NO echo at all -- a real command
# with the flag removed -- gives the identical rc and the identical
# message. The word `echo` contributed nothing to the verdict.
#
# It is kept, because a verify step that loses the flag really must
# refuse, and relabelled to say which property it holds.
echo_only_verify() {
    sed -i 's|docker plugin install --grant-all-permissions "$REF"|echo "docker plugin install $REF"|g' "$1"
}
run "an install stripped of --grant-all-permissions is a refusal" 2 echo_only_verify "no longer matches" installs_all_echoed

# The mutation the case above described but never ran: the command is
# echoed with the flag INTACT. Before #858 this returned rc=0 and the
# gate's strongest sentence -- "4 published cell(s), each install-
# verified and promoted" -- over a release that installed nothing.
#
# A gate keyed on the flag cannot distinguish this from a real install,
# because an advertisement is text and text may carry any flag. The
# discriminator is POSITION: the token sits inside shell quoting.
echoed_with_flag() {
    sed -i 's|docker plugin install --grant-all-permissions "$REF"|echo "docker plugin install --grant-all-permissions $REF"|g' "$1"
}
run "an echoed install carrying the flag is not verification" 2 echoed_with_flag "no longer matches" echoed_flag_survives

# The other quoting form, because the position test is the whole fix and
# a single-quote-only or double-quote-only implementation would pass the
# case above while leaving half the hole open.
echoed_single_quoted() {
    sed -i "s|docker plugin install --grant-all-permissions \"\$REF\"|echo 'docker plugin install --grant-all-permissions ref'|g" "$1"
}
run "a single-quoted echoed install is not verification" 2 echoed_single_quoted "no longer matches" echoed_flag_survives

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
run "the promote runner's arch is not the cell's arch" 0 promote_on_arm "6 published cell(s)"

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
# BOTH PUBLISH FORMS, or this case stops being about an empty publish
# set: with only `make ... push` broken, the copied cells remain and the
# gate renders an ordinary verdict over two cells instead of refusing.
# --- THE COPY IS A PUBLISH, DRIVEN THREE WAYS (#972) --------------------
#
# The Hub alias is published by copying the signed manifest, not by a
# second build. Three cases, because "the gate sees the copy" and "the
# gate sees it as a publish" and "the gate does not see an advertisement
# of one" are three different claims.

# 1. Remove the copy and the alias verifiers and promotions are left
#    covering a cell nothing publishes. This is what a dropped publish
#    step looks like from here, and before this change it read as a
#    clean pass: the gate only ever compared in the other direction.
drop_alias_copy() { sed -i '/publish-hub-alias\.sh/d' "$1"; }
no_copy_left() {
    [ "$(copy_lines "$1" | wc -l)" -eq 0 ] &&
        cmds "$1" | grep -E 'make .*push' >/dev/null
}
#    TWO REPORTS, ASSERTED SEPARATELY. The same fixture orphans an
#    install verifier AND a promotion, so a single case naming only
#    "HUB_ALIAS" is satisfied by either one: disabling either reverse
#    comparison left this case green, measured with both mutants. Each
#    direction is now named in its own assertion.
run "dropping the copy leaves an install verifier for a cell nothing publishes" \
    1 drop_alias_copy "has an install verifier, but nothing publishes it" no_copy_left
run "dropping the copy leaves a promotion for a cell nothing publishes" \
    1 drop_alias_copy "has a promotion to :latest, but nothing publishes it" no_copy_left

# 2. An ADVERTISED copy is not a copy. The line still carries the words
#    and still ends in a quoted destination, so the pattern matches; the
#    command sits inside quotes, so nothing runs. Same discrimination as
#    the echoed install above (#858), keyed on position and not on
#    vocabulary.
echoed_copy() {
    sed -i 's|^\( *\)\(scripts/publish-hub-alias\.sh .*\) \("[^"]*"\)$|\1echo "\2" \3|' "$1"
}
# The destination is still the last operand on the line -- that is the
# whole point of the case. `copy_lines` excludes a printer-led line the
# way the gate does, so the shape is asserted on the raw text here: what
# has to survive is the REFERENCE, and what has to change is who runs.
copy_only_echoed() {
    cmds "$1" | grep -F 'echo "scripts/publish-hub-alias.sh' >/dev/null &&
        ! cmds "$1" | grep -E '^[[:space:]]*scripts/publish-hub-alias\.sh ' >/dev/null &&
        cmds "$1" | grep -E '[[:space:]]"[^"]*\$\{HUB_ALIAS\}:\$\{[A-Z_]+\}"[[:space:]]*$' >/dev/null
}
run "an echoed copy publishes nothing" \
    1 echoed_copy "HUB_ALIAS" copy_only_echoed

# 2b. THE ARGUMENT ORDER IS THE CLAIM. `scripts/publish-hub-alias.sh`
#     takes the destination LAST because that is what this gate reads,
#     and its own header says so. Move the destination out of the final
#     position and the alias leaves the published set while every
#     verifier and promotion for it stays -- the same reverse-direction
#     report as a deleted copy, which is the point: a reordering is a
#     dropped publish as far as anything downstream can tell.
reorder_copy_args() {
    sed -i 's|^\( *\)\(scripts/publish-hub-alias\.sh --expect-digest "[^"]*"\) \("[^"]*"\) \("[^"]*"\)$|\1\2 \4 \3|' "$1"
}
copy_dest_not_last() {
    cmds "$1" | grep -E '^[[:space:]]*scripts/publish-hub-alias\.sh ' >/dev/null &&
        [ "$(copy_lines "$1" | grep -Ec '\$\{HUB_ALIAS\}:\$\{[A-Z_]+\}"[[:space:]]*$' || true)" -eq 0 ]
}
run "a copy whose destination is not the last operand publishes nothing" \
    1 reorder_copy_args "has an install verifier, but nothing publishes it" copy_dest_not_last

# 2c. PRESERVATION. The rule is keyed on the shape of the line and not
#     on the tool, so the tool this used to be -- a bare `oras cp -r` --
#     must still read as a copy. Without this, re-keying the gate could
#     have narrowed it to one script name and nobody would notice until
#     someone wrote a copy some other way.
back_to_oras() {
    sed -i 's|^\( *\)scripts/publish-hub-alias\.sh --expect-digest "[^"]*" \("[^"]*"\) \("[^"]*"\)$|\1oras cp -r \2 \3|' "$1"
}
oras_form_only() {
    cmds "$1" | grep -E '^[[:space:]]*oras cp -r ' >/dev/null &&
        ! cmds "$1" | grep -E '^[[:space:]]*scripts/publish-hub-alias\.sh ' >/dev/null
}
run "a bare oras copy is still a publish" 0 back_to_oras "6 published cell(s)" oras_form_only

# 3. The other side of the same claim: the copied cells ARE in the
#    published set, so removing their install proofs fails. Without the
#    copy being read as a publish this case would pass, because an
#    unpublished cell with no verifier is nothing to report.
drop_alias_verify() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
for _ in range(2):
    s = re.sub(r"\n  verify-install-hub-alias(?:-arm64)?:.*?(?=\n  [a-z0-9_-]+:\n)",
               "\n", s, count=1, flags=re.S)
open(p, "w").write(s)
PY
}
no_alias_verify() {
    ! cmds "$1" | grep -F 'REF="${HUB_ALIAS}' >/dev/null &&
        cmds "$1" | grep -F 'REF="${HUB_NAME}' >/dev/null &&
        [ "$(copy_lines "$1" | wc -l)" -gt 0 ]
}
run "a copied cell with no install verifier fails" \
    1 drop_alias_verify "HUB_ALIAS" no_alias_verify

break_publish() {
    sed -i -e 's/PLUGIN_NAME=/PLUGIN_NOM=/g' \
           -e 's|^\( *\)\(scripts/publish-hub-alias\.sh .*\) \("[^"]*"\)$|\1\2 \3 --then-some|' "$1"
}
run "zero derived publish cells is a refusal" 2 break_publish "ZERO published cells" no_publish_name

break_promote() { sed -i 's/crane tag/crane retag/g' "$1"; }
run "zero derived promote cells is a refusal" 2 break_promote "ZERO promoted cells" no_crane_tag

# --- an unresolvable tag is a refusal, not a guess ---------------------
# Treating an unbound name as amd64 would merge an arm64 cell into its
# neighbour and report a parity nobody checked.
unbind_tag() {
    python3 - "$1" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r'PLUGIN_TAG="\$\{TAG\}"', 'PLUGIN_TAG="${NOSUCH}"', s, count=1)
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
