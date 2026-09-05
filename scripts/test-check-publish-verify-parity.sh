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

# cmds is the workflow with its comments removed. Every claim below is
# about what the release lane RUNS; a sentence in a comment that happens
# to spell `crane tag` or `docker plugin install` is prose, and a
# mutator or an assertion that counted it would be measuring the
# documentation.
cmds() { grep -v '^[[:space:]]*#' "$1"; }

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
        [ "$(cmds "$1" | grep -Ec 'make .*push' || true)" -eq "$(cmds "$SRC" | grep -Ec 'make .*push' || true)" ]
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

census "every crane retag names HUB_NAME or GHCR_NAME" \
    "$(cmds "$SRC" | grep -Ec 'crane tag ' || true)" \
    "$(cmds "$SRC" | grep -E 'crane tag ' | grep -Ec 'crane tag "\$\{(HUB|GHCR)_NAME\}' || true)"

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
