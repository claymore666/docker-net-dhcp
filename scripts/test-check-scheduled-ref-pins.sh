#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-scheduled-ref-pins.sh (#839).
#
# The gate makes six separable claims, and this file is arranged so
# that exactly one of them fails per case:
#
#   1. every scheduled workflow carries a subject marker
#   2. the marker agrees with the permission block
#   3. `tracker` pins every checkout to dev
#   4. `tree` pins none of them to dev
#   5. the SHAPE of the file does not decide the verdict
#   6. a workflow it cannot read is a refusal, never an absence
#
# EVERY CASE ASSERTS THE ERROR, NOT ONLY THE EXIT CODE, and that is not
# tidiness. An exit-code-only oracle cannot tell a guard from anything
# else that exits with the same number, and two of this suite's guards
# were surviving deletion because of it:
#
#   * deleting the missing-marker guard makes `markers[0]` trip `set -u`.
#     The shell aborts, the exit code is 1, and 1 is exactly what the
#     case asserted. The case could not tell its guard from a crash.
#   * deleting the two-markers guard left `c_two_markers` red anyway,
#     because that fixture carried `noissues` as well, so `markers[0]`
#     read `tracker` and the CONTRADICTION fired instead. A mutant
#     refused by a different guard is not a kill; the fixture is re-cut
#     below so the double marker is the only thing wrong with it.
#
# So run_case takes an expected error: a `clean` run must print no
# `::error` at all, and every other case names text that has to appear
# in one. Swapping the two pin-error titles -- a mutation that changes
# no verdict anywhere -- used to leave the suite fully green.
#
# Fixtures are GENERATED per case rather than mutated in place, which is
# how the sibling gate meta-tests do it and which sidesteps the
# restore-a-mutated-fixture hazard entirely (a backup written inside the
# fixture directory is not a backup — the setup that replaces the
# directory eats it, and later cases then run against an already-broken
# subject).
#
# GENERATION HAS A SECOND FAILURE MODE, and it is the one this file
# shipped with. A generator's UN-VARIED PARAMETERS are the gate's blind
# spot, and a green case count says nothing about them. The first form
# of this file ran 21 cases over four axes -- marker, trigger,
# permission, ref -- and every one of them came out of an emit_wf that
# hardcoded `on:` and `permissions:` in BLOCK style. Two of the gate's
# detectors parsed those keys, both failed on the flow spelling, and no
# case could reach either: 21 passing cases and a clean real corpus were
# entirely consistent with the headline universal being false, because
# the universal's domain was decided by the same un-varied parameter.
#
# The remedy is not more cases. It is to enumerate the generator's
# constants and ask, for each, whether the subject can legitimately
# differ there. YAML offers a flow spelling for every block one, so the
# SHAPE of the file is such a constant -- hence WF_SHAPE below. Be most
# suspicious of a gate that parses a format tested only against the one
# way the current tree happens to write that format: the tree is not the
# format.
#
# Generation has its own failure mode in exchange: a "mutant" whose
# generator arguments happen to produce the control's bytes would pass
# for the wrong reason and read as a surviving mutant. So every case
# that claims to differ from the control is diffed against it, and an
# identical fixture is reported as a defect in this file rather than a
# verdict about the gate.
set -uo pipefail

GATE="$(cd "$(dirname "$0")" && pwd)/check-scheduled-ref-pins.sh"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# wkey NAME writes NAME as a YAML key, honouring the KEY-SPELLING axis
# (KQ, KSP -- set by emit_wf, read here through bash dynamic scope).
# YAML lets a key be quoted with either quote character and lets a space
# stand before the colon, so `on:`, `"on":`, 'on':` and `on :` are FOUR
# SPELLINGS OF ONE KEY. Measured 2026-08-28, the gate read only the
# first two, and a scheduled workflow spelling it either of the other
# two never entered the domain at all -- not reported as unclassified,
# not counted, exit 0 clean while holding `issues: write` (#839). The
# generator could not reach that because it hardcoded one spelling,
# which is the SAME un-varied-parameter defect WF_SHAPE was introduced
# for, one axis along: the key is a constant of this generator exactly
# as the block/flow shape was.
wkey() { printf '%s%s%s%s:' "$KQ" "$1" "$KQ" "$KSP"; }

# emit_wf FILE MARKER TRIGGER PERMS [REF...]
#
#   MARKER   tracker | tree | none | <anything else, written verbatim>
#            `both` writes two markers, which is its own error case.
#   TRIGGER  sched | nosched
#   PERMS    issues | noissues | scalar:<value> | openflow
#            `scalar:` writes `permissions: <value>` on one line, which
#            is the third KIND of value. For a scalar the content
#            and the spelling are the same thing, so it belongs on this
#            axis rather than on WF_SHAPE, and it overrides `flow-perms`.
#            `openflow` writes a flow mapping that is never closed --
#            the residue case for a value the gate cannot finish
#            reading.
#   REF...   one token per actions/checkout step: none | dev | main |
#            refs/heads/dev. No tokens means no checkout step at all.
#
# WF_SHAPE is the fifth axis, and it is set through `shaped` rather than
# a positional because REF... is variadic. It varies how the file is
# WRITTEN, not what it says:
#
#   block            (default) block mappings throughout, marker at column 0
#   flow-on          `on: {schedule: [{cron: ...}]}` on one line
#   flow-perms       `permissions: {contents: read, issues: read}` on one line
#   flow-perms-multi the same flow mapping spread over three lines, so it
#                    does not end on the line that opens it
#   indent           the marker indented, beside the step it governs
#   dq-keys          every key double-quoted
#   sq-keys          every key single-quoted
#   spaced           a space before every colon, which YAML permits
#
# Every value must produce a file with the SAME MEANING as `block`.
# That is what makes these cases evidence: if the gate's verdict moves
# when only the spelling moves, the gate is reading the spelling.
emit_wf() {
    local file="$1" marker="$2" trigger="$3" perms="$4"; shift 4
    local shape="${WF_SHAPE:-block}" mark_pfx=''
    local KQ='' KSP=''
    [ "$shape" = indent ] && mark_pfx='      '
    case "$shape" in
        dq-keys) KQ='"' ;;
        sq-keys) KQ="'" ;;
        spaced)  KSP=' ' ;;
    esac
    {
        printf '%s %s\n' "$(wkey name)" "$(basename "$file")"
        case "$marker" in
            none) ;;
            both) printf '%s# scheduled-subject: tracker\n%s# scheduled-subject: tree\n' \
                         "$mark_pfx" "$mark_pfx" ;;
            *)    printf '%s# scheduled-subject: %s\n' "$mark_pfx" "$marker" ;;
        esac
        if [ "$shape" = flow-on ]; then
            if [ "$trigger" = sched ]; then
                printf "%s {%s [{%s '0 0 * * *'}]}\n" "$(wkey on)" "$(wkey schedule)" "$(wkey cron)"
            else
                printf '%s {%s {%s [dev]}}\n' "$(wkey on)" "$(wkey push)" "$(wkey branches)"
            fi
        else
            printf '%s\n' "$(wkey on)"
            if [ "$trigger" = sched ]; then
                printf "  %s\n    - %s '0 0 * * *'\n" "$(wkey schedule)" "$(wkey cron)"
            else
                printf '  %s\n    %s [dev]\n' "$(wkey push)" "$(wkey branches)"
            fi
        fi
        if [ "$perms" = openflow ]; then
            printf '%s {\n  %s read\n' "$(wkey permissions)" "$(wkey contents)"
        elif [ "${perms#scalar:}" != "$perms" ]; then
            printf '%s %s\n' "$(wkey permissions)" "${perms#scalar:}"
        elif [ "$shape" = flow-perms ]; then
            if [ "$perms" = issues ]; then
                printf '%s {%s read, %s read}\n' \
                       "$(wkey permissions)" "$(wkey contents)" "$(wkey issues)"
            else
                printf '%s {%s read}\n' "$(wkey permissions)" "$(wkey contents)"
            fi
        elif [ "$shape" = flow-perms-multi ]; then
            printf '%s {\n  %s read%s\n}\n' "$(wkey permissions)" "$(wkey contents)" \
                   "$([ "$perms" = issues ] && printf ',\n  %s read' "$(wkey issues)")"
        else
            printf '%s\n  %s read\n' "$(wkey permissions)" "$(wkey contents)"
            [ "$perms" = issues ] && printf '  %s read\n' "$(wkey issues)"
        fi
        printf '%s\n  %s\n    %s ubuntu-latest\n    %s\n' \
               "$(wkey jobs)" "$(wkey j)" "$(wkey runs-on)" "$(wkey steps)"
        local r
        for r in "$@"; do
            printf '      - %s actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' \
                   "$(wkey uses)"
            if [ "$r" != none ]; then
                printf '        %s\n          %s %s\n' "$(wkey with)" "$(wkey ref)" "$r"
            fi
        done
        printf '      - %s work\n        %s echo hi\n' "$(wkey name)" "$(wkey run)"
    } > "$file"
}

# shaped SHAPE emit_wf-args...
# `local` here is what keeps WF_SHAPE from leaking into the next call:
# a bare `WF_SHAPE=x emit_wf ...` prefix assignment persists after a
# shell FUNCTION returns, which would silently reshape every later
# fixture in the same case.
shaped() {
    local WF_SHAPE="$1"; shift
    emit_wf "$@"
}

# A valid corpus: one of each class, plus a non-scheduled workflow that
# would violate BOTH rules if the population were selected wrongly — it
# asks for issues:, pins dev, and carries no marker at all. It is the
# control for "the gate looks only at scheduled workflows".
emit_control() {
    local d="$1"
    emit_wf "$d/tracker.yml" tracker sched issues dev
    emit_wf "$d/tree.yml"    tree    sched noissues none
    emit_wf "$d/pushonly.yml" none   nosched issues dev
}

CONTROL_DIR=$(mktemp -d)
emit_control "$CONTROL_DIR"

# verdict NAME WANT-RC WANT-ERR RC OUT
#
#   WANT-ERR   `clean`  the run must print no `::error` line at all
#              <text>   that text must appear inside a printed `::error`
#
# Both halves are asserted, and a case fails naming every half that
# missed. The exit code alone cannot distinguish a guard from a crash,
# from a different guard, or from the same guard reporting the wrong
# thing; the error text is what pins WHICH claim failed.
verdict() {
    local name="$1" want="$2" wanterr="$3" rc="$4" out="$5"
    local why='' errs
    [ "$rc" = "$want" ] || why="exit $rc, want $want"
    errs=$(printf '%s\n' "$out" | grep '::error')
    # Both greps here read to EOF. A piped `grep -q` exits at the first
    # match and SIGPIPEs its producer, which under pipefail reports
    # failure on success (check-pipefail-consumers.sh).
    if [ "$wanterr" = clean ]; then
        [ -z "$errs" ] || why="${why:+$why; }printed an ::error and should not have"
    elif ! printf '%s\n' "$errs" | grep -F -- "$wanterr" >/dev/null; then
        why="${why:+$why; }no ::error matching \"$wanterr\""
    fi
    if [ -z "$why" ]; then
        ok "$name"
    else
        no "$name ($why)"
        printf '      %s\n' "$out" >&2
    fi
}

# run_case NAME WANT-RC DIFFERS-FROM-CONTROL WANT-ERR BUILDER
run_case() {
    local name="$1" want="$2" differs="$3" wanterr="$4" builder="$5"
    local dir rc out
    dir=$(mktemp -d)
    "$builder" "$dir"

    if [ "$differs" = differs ]; then
        if diff -r -q "$CONTROL_DIR" "$dir" >/dev/null 2>&1; then
            no "$name — FIXTURE IS IDENTICAL TO THE CONTROL, so this case proves nothing" \
               "about the gate. Fix the generator, not the gate."
            rm -rf "$dir"
            return
        fi
    fi

    out=$(bash "$GATE" "$dir" 2>&1)
    rc=$?
    rm -rf "$dir"
    verdict "$name" "$want" "$wanterr" "$rc" "$out"
}

# --- the control: green has to be reachable before any mutant means -----
# anything. A meta-test whose control fails is measuring a broken gate,
# and every "mutant died" below would be the same failure repeated.
c_control() { emit_control "$1"; }
run_case "a classified, correctly pinned corpus is clean" 0 same clean c_control

# --- claim 3: tracker pins every checkout to dev ------------------------
c_unpinned() {
    emit_wf "$1/tracker.yml" tracker sched issues none
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker workflow with an unpinned checkout is reported" 1 differs \
    "Tracker workflow not pinned to dev" c_unpinned

c_pinned_elsewhere() {
    emit_wf "$1/tracker.yml" tracker sched issues main
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker pinned to a branch that is not dev is reported" 1 differs \
    "Tracker workflow not pinned to dev" c_pinned_elsewhere

# Two checkouts, one pinned. The rule is EVERY checkout, and a gate that
# checked "at least one" would pass this — the second, unpinned one is
# the checkout that actually judges the tracker.
c_half_pinned() {
    emit_wf "$1/tracker.yml" tracker sched issues dev none
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker with one pinned and one unpinned checkout is reported" 1 differs \
    "Tracker workflow not pinned to dev" c_half_pinned

# `refs/heads/dev` is the same instruction spelled longer. Accepting only
# the short form would fail a workflow with nothing wrong with it, and a
# gate that cries wolf gets discharged.
c_long_ref() {
    emit_wf "$1/tracker.yml" tracker sched issues refs/heads/dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "refs/heads/dev counts as pinned to dev" 0 differs clean c_long_ref

# A tracker workflow judges the tracker AGAINST THE TREE. With no
# checkout there is no tree, and "0 of 0 checkouts unpinned" would
# otherwise satisfy the pin rule vacuously.
c_no_checkout() {
    emit_wf "$1/tracker.yml" tracker sched issues
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker workflow with no checkout at all is reported" 1 differs \
    "Tracker workflow checks out nothing" c_no_checkout

# --- claim 4: tree pins none of them to dev -----------------------------
# The opposite direction. #839 says pinning everything would be wrong,
# so the gate has to be able to say so; without this case the gate
# states half its rule.
c_tree_pinned() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues dev
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tree workflow pinned to dev is reported" 1 differs "Tree workflow pinned to dev" c_tree_pinned

# A tag or sha pin on a tree workflow is a different question and the
# gate says so in its header. Reporting it would be the cry-wolf case.
c_tree_tag() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues main
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tree workflow pinned to something other than dev is left alone" 0 differs \
    clean c_tree_tag

# --- claim 1: every scheduled workflow carries a marker -----------------
# This is the case that closes #839 rather than patching its instances.
c_no_marker() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/newcomer.yml" none   sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a new scheduled workflow with no marker is reported" 1 differs \
    "Unclassified scheduled workflow" c_no_marker

c_unknown_marker() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    banana  sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a marker naming an unknown class is reported" 1 differs "Unknown subject" c_unknown_marker

# The double-marker file is a THIRD workflow, and it is otherwise
# faultless: it asks for `issues:` and it pins `ref: dev`, so whichever
# of its two markers is believed, no other rule has anything to say about
# it. The corpus keeps a real `tree.yml` beside it so no class empties.
#
# The first form put `both` on the tree.yml itself, with `noissues`. That
# made `markers[0]` read `tracker`, so deleting the two-markers guard
# handed the file straight to the tracker-without-issues contradiction
# and the case stayed red for a reason that said nothing about the guard
# it was named after. A mutant refused by a DIFFERENT guard is not a kill.
c_two_markers() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/dbl.yml"     both    sched issues dev
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "two markers in one file are reported" 1 differs "Two subject markers" c_two_markers

# --- claim 2: the marker is cross-checked, not trusted ------------------
# The `gate-selftest-runs-in:` precedent: a declaration is verified
# against something the author cannot restate. Both directions, because
# either alone would let one of the two statements be the only witness.
c_tree_with_issues() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched issues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tree marker on a workflow requesting issues: is reported" 1 differs \
    'but requests the `issues:` permission' c_tree_with_issues

c_tracker_without_issues() {
    emit_wf "$1/tracker.yml" tracker sched noissues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker marker on a workflow requesting no issues: is reported" 1 differs \
    'but requests no `issues:` permission' c_tracker_without_issues

# --- claim 2, continued: the THIRD spelling of `permissions:` ----------
# The block rule matches a shape and the flow rule matches a shape; a
# SCALAR value matches neither, and both used to read it as `issues = 0`
# in silence. `permissions: write-all` is the dangerous half -- full
# tracker write access declaring `tree`, skipping the pin, and exiting 0
# clean, which is #839's opening hazard walking through the gate built
# to stop it. Every case here is paired with a preservation control,
# because a rule that answered "contradiction" to every scalar would
# satisfy the defect cases by refusing correct workflows.
c_scalar_write_all_tree() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched scalar:write-all none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "permissions: write-all contradicts a tree marker" 1 differs \
    'but requests the `issues:` permission' c_scalar_write_all_tree

# Preservation. `write-all` does grant `issues: write`, so it SATISFIES a
# tracker marker; the widening must not have turned every scalar red.
c_scalar_write_all_tracker() {
    emit_wf "$1/tracker.yml" tracker sched scalar:write-all dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "permissions: write-all satisfies a tracker marker" 0 differs \
    clean c_scalar_write_all_tracker

# `read-all` IS the same KIND of statement as `write-all`, and these two
# cases used to assert the opposite -- in their NAMES, which is why the
# false claim had to be corrected rather than left to decay. A wrong
# claim with a green test defending it does not decay quietly; it gets
# defended.
#
# The claim was: this repository's default_workflow_permissions is
# `read` (measured 2026-08-28, and still true), therefore `read-all`
# grants exactly what a workflow with no `permissions:` block already
# has. The premise does not carry the conclusion. The restricted default
# grants read on `contents` and `packages` ONLY -- every other scope,
# `issues:` included, is `none` -- while `read-all` grants read across
# ALL scopes (GitHub docs: "Managing GitHub Actions settings for a
# repository"; workflow-syntax `permissions`). So `read-all` grants
# `issues: read` where the default grants `issues: none`: an escalation
# above the default, by exactly the argument that closes `write-all`.
# Measured 2026-08-28 against the gate that shipped: a scheduled
# workflow with `permissions: read-all` and a `tree` marker exited 0
# clean while holding read on every tracker scope there is.
c_scalar_read_all_tree() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched scalar:read-all none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "permissions: read-all contradicts a tree marker" 1 differs \
    'but requests the `issues:` permission' c_scalar_read_all_tree

# Preservation, and the pair that stops the rule above from being
# "every scalar is a contradiction": `read-all` does grant `issues:
# read`, which is the access a tracker workflow needs, so it SATISFIES
# a tracker marker. This case asserted the opposite until the review
# of #839 checked the premise against the GitHub docs.
c_scalar_read_all_tracker() {
    emit_wf "$1/tracker.yml" tracker sched scalar:read-all dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "permissions: read-all satisfies a tracker marker" 0 differs \
    clean c_scalar_read_all_tracker

# A QUOTED scalar is the same scalar. YAML offers a quoted spelling for
# every plain one, and this is the same un-varied-parameter trap as
# WF_SHAPE: without this case, stripping the quotes could be deleted and
# `permissions: "write-all"` would fall out of the enumeration into the
# residue -- a refusal where the answer is a contradiction, and the gate
# reporting "I cannot see" about a file it can see perfectly well.
c_scalar_quoted() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched "scalar:\"write-all\"" none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a quoted scalar is read as the scalar it quotes" 1 differs \
    'but requests the `issues:` permission' c_scalar_quoted

# THE RESIDUE, which is what keeps the enumeration from being the next
# silence. Three known spellings and no fourth branch would read an
# unrecognised value as absent -- the identical defect, one spelling
# along. An unclassifiable value is a REFUSAL: the cross-check cannot be
# made, and a marker believed alone is what the cross-check exists to
# prevent.
c_scalar_residue() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched scalar:read_all none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "an unrecognised permissions value is refused, not read as absent" 2 differs \
    "Unclassifiable permissions value" c_scalar_residue

# The refusal disables the CROSS-CHECK and nothing else. This file is
# unreadable on the permission side AND pins a tree workflow to dev; the
# pin rule reads checkouts, not permissions, so it still fires -- and a
# violation outranks a refusal, so the exit code is 1, not 2. Without
# this, "refuse the residue" could have meant "stop judging the file".
c_scalar_residue_still_pin_checked() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/odd.yml"     tree    sched scalar:read_all dev
}
run_case "a refused permission value still leaves the pin rule judging the file" 1 differs \
    "Tree workflow pinned to dev" c_scalar_residue_still_pin_checked

# --- claim 6: a workflow it cannot read is a refusal, not an absence ---
# `facts_of` producing nothing must never read as "not scheduled". The
# guard says so in the gate and had no case: deleting it left the suite
# 30/0 green while an unreadable workflow dropped out of the domain
# without a word.
#
# The fixture is a DIRECTORY named *.yml rather than a file at mode 000,
# and deliberately. Mode 000 depends on who runs the suite -- as root it
# is readable, the case would pass having exercised nothing, and a case
# that quietly stops testing is the failure this whole file is about.
#
# THE JUSTIFICATION THAT USED TO STAND HERE WAS FALSE, and the case it
# defended was the red this branch went out on. It said: "read(2) on a
# directory is EISDIR for every uid, so this fixture reaches the guard
# on any runner." gawk never issues read(2) on it. It stats the
# argument, prints `warning: command line argument ... is a directory:
# skipped`, and carries on with status 0 -- so its END rule runs and
# emits a complete all-zero fact line. Measured 2026-08-28:
#
#   mawk 1.3.4 (this box)  cannot open it, no output, status 2
#   gawk 5.2.1 (the lane)  skips it, full all-zero output, status 0
#
# The fixture was right and the gate was wrong: keyed on empty stdout,
# the refusal fired under one awk and not the other, and under the
# other the gate read a file it never opened as `scheduled=0`. The
# guard is now made in the SHELL, with `-f` and `-r`, which are the
# same on every awk and every uid; that is what makes this fixture
# reach it on any runner, and the sentence above is the reason the
# claim is now about the GATE rather than about read(2).
c_unreadable() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
    mkdir "$1/isadir.yml"
}
run_case "a workflow whose facts cannot be extracted is a refusal, not an absence" 2 differs \
    "Cannot read workflow" c_unreadable

# The composed attack. Each guard above dies to a single mutation; this
# moves the marker AND the permission together so the cross-check is
# satisfied, and requires the pin rule to catch it on its own. Without
# this, a gate that only ever fired via the cross-check would pass every
# case above.
c_consistent_but_unpinned() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tracker sched issues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a consistently relabelled workflow is still caught by the pin rule" 1 differs \
    "Tracker workflow not pinned to dev" c_consistent_but_unpinned

# The defect this gate shipped with in its first form, pinned so it
# cannot come back: when the only workflow of a class is the broken one,
# that class is empty, and the non-vacuity guard reported a definite
# violation as "cannot check" — sending the reader to look for a broken
# discriminator instead of the workflow the error had just named. A
# verdict must not be decided by the thing it is judging.
c_violation_empties_a_class() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    none    sched noissues none
}
run_case "a violation that empties its own class still exits 1, not 2" 1 differs \
    "Unclassified scheduled workflow" c_violation_empties_a_class

# --- non-vacuity: green having decided nothing --------------------------
c_no_schedules() {
    emit_wf "$1/pushonly.yml" none nosched issues dev
    emit_wf "$1/other.yml"    none nosched noissues none
}
run_case "a corpus with no scheduled workflow cannot check" 2 differs \
    "No scheduled workflows" c_no_schedules

c_all_tree() {
    emit_wf "$1/a.yml" tree sched noissues none
    emit_wf "$1/b.yml" tree sched noissues none
}
run_case "every scheduled workflow classified tree cannot check" 2 differs \
    "No tracker workflows" c_all_tree

c_all_tracker() {
    emit_wf "$1/a.yml" tracker sched issues dev
    emit_wf "$1/b.yml" tracker sched issues dev
}
run_case "every scheduled workflow classified tracker cannot check" 2 differs \
    "No tree workflows" c_all_tracker

c_empty() { :; }
run_case "an empty directory cannot check" 2 differs "No workflows found" c_empty

# A missing directory is not an empty one: the empty case proves the
# glob guard, this proves the gate does not treat an absent subject as a
# clean one.
missing=$(mktemp -d); rmdir "$missing"
out=$(bash "$GATE" "$missing" 2>&1); rc=$?
verdict "a missing directory cannot check" 2 "No workflow directory" "$rc" "$out"

# --- population selection ----------------------------------------------
# The control already carries a non-scheduled workflow that would break
# both rules; this makes the claim explicitly, by removing the scheduled
# members' faults and leaving only the push-only offender.
c_pushonly_ignored() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly1.yml" none  nosched issues dev
    emit_wf "$1/pushonly2.yml" banana nosched issues dev
}
run_case "workflows without a schedule are outside the domain" 0 differs clean c_pushonly_ignored

# The one route OUT of the domain, pinned so the next author does not
# have to rediscover it. A reusable workflow declares no `schedule:`, so
# a scheduled caller that delegates with `uses:` puts the checkout in a
# file the domain never examines. These two cases assert the gate's
# CURRENT behaviour in both directions, which is the honest state and
# not a defect being hidden: the header names this escape, and a header
# sentence decays where a case does not. If the domain is ever taught to
# follow the `uses:` edge, the first case goes red and points at the
# paragraph that explains the decision.
emit_delegating_pair() {
    local d="$1" marker="$2" perms="$3"
    {
        printf 'name: caller.yml\n# scheduled-subject: %s\n' "$marker"
        printf "on:\n  schedule:\n    - cron: '0 0 * * *'\n"
        printf 'permissions:\n  contents: read\n'
        [ "$perms" = issues ] && printf '  issues: read\n'
        printf 'jobs:\n  j:\n    uses: ./.github/workflows/callee.yml\n'
    } > "$d/caller.yml"
    {
        printf 'name: callee.yml\non:\n  workflow_call:\n'
        printf 'permissions:\n  contents: read\n  issues: write\n'
        printf 'jobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n'
        printf '      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'
        printf '      - name: work\n        run: echo hi\n'
    } > "$d/callee.yml"
}

# `tree` is the open direction: zero checkouts is legitimate for a tree
# workflow, so the caller passes on its own terms while the callee holds
# an unexamined checkout of `main` and `issues: write`.
c_uses_tree() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_delegating_pair "$1" tree noissues
}
run_case "a scheduled tree caller delegating with uses: passes; the callee is outside the domain" \
    0 differs clean c_uses_tree

# The same delegation declared `tracker` is CLOSED, by the rule that a
# tracker workflow must check something out. This is the boundary of the
# escape, and without it the header's claim that it is tree-only would be
# prose nobody had driven.
c_uses_tracker() {
    emit_wf "$1/tree.yml" tree sched noissues none
    emit_delegating_pair "$1" tracker issues
}
run_case "the same delegation declared tracker is caught by the no-checkout rule" \
    1 differs "Tracker workflow checks out nothing" c_uses_tracker

# --- claim 5: the SHAPE of the file does not decide the verdict ---------
# The axis emit_wf held constant for 21 cases. Both of the gate's key
# detectors parsed a block mapping, both were blind to the flow spelling
# of the same key, and no fixture could reach either. Each defect below
# is paired with a PRESERVATION CONTROL: a widening that only ever says
# "1" would satisfy these cases by refusing every flow-style file, which
# is a different gate, not a fixed one.

# The domain. A flow-style `on:` was never counted as scheduled, so an
# unclassified workflow holding issues: and an unpinned checkout printed
# as "scheduled workflows: 2" and exited 0 -- outside the population the
# gate counted, which is the failure its own header names.
c_flow_on_unclassified() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    shaped flow-on "$1/newcomer.yml" none sched issues none
}
run_case "a flow-style on: does not put a workflow outside the domain" 1 differs \
    "Unclassified scheduled workflow" c_flow_on_unclassified

c_flow_on_clean() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    shaped flow-on "$1/tree.yml" tree sched noissues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "a flow-style on: on a correct workflow is still clean" 0 differs clean c_flow_on_clean

# The cross-check, and this is the dangerous direction. An inline
# permissions mapping made `issues` read as absent, so the marker became
# the only witness -- and the marker is exactly what the cross-check
# exists not to trust. A `tree` declaration on a workflow holding real
# tracker access is #839's defect passing through the gate built to stop
# it.
c_flow_perms_contradiction() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    shaped flow-perms "$1/tree.yml" tree sched issues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "an inline permissions mapping is still cross-checked" 1 differs \
    'but requests the `issues:` permission' c_flow_perms_contradiction

c_flow_perms_clean() {
    shaped flow-perms "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml" tree sched noissues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "an inline permissions mapping satisfies the tracker cross-check" 0 differs \
    clean c_flow_perms_clean

# The marker anchored at column 0, so a correctly classified, correctly
# pinned workflow whose marker sat beside the step it governs was
# rejected as unclassified -- a red naming the wrong remedy, telling the
# author to add the line they had just added.
c_indented_marker() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    shaped indent "$1/tree.yml" tree sched noissues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "a marker indented beside its step is read" 0 differs clean c_indented_marker

# --- prose cannot answer for the file ----------------------------------
# Both flow rules read a whole line, so both could be satisfied by a
# trailing comment. That is the shape check-python-deps.sh shipped
# (#743): a gate reading its own description instead of the thing it
# describes. Hand-written, because the point is bytes emit_wf will not
# produce.
raw_case() {
    local name="$1" want="$2" wanterr="$3" body="$4"
    local dir rc out
    dir=$(mktemp -d)
    printf '%s' "$body" > "$dir/a.yml"
    emit_wf "$dir/tracker.yml" tracker sched issues dev
    emit_wf "$dir/tree.yml"    tree    sched noissues none
    out=$(bash "$GATE" "$dir" 2>&1); rc=$?
    rm -rf "$dir"
    verdict "$name" "$want" "$wanterr" "$rc" "$out"
}

raw_case "a comment on the on: line does not declare a schedule" 0 clean \
'name: a
on:  # nightly, on the same schedule: as the others
  push:
    branches: [dev]
permissions:
  contents: read
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        with:
          ref: dev
'

raw_case "a comment mentioning issues: does not satisfy the cross-check" 1 \
'but requests no `issues:` permission' \
'name: a
# scheduled-subject: tracker
on:
  schedule:
    - cron: 0 0 * * *
permissions: {contents: read}  # issues: read is not requested here
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        with:
          ref: dev
'

# A STEP ENDS WHERE ITS INDENTATION ENDS, and the rule that closes it at
# the first shallower line survived every case above -- because in every
# generated fixture the next `- ` closes the step anyway, and so does
# END. It is not dead: a job-level `outputs:` mapping is real Actions
# YAML, a job output called `ref` is an ordinary thing to write, and with
# the step left open that key is attributed to the checkout above it. An
# UNPINNED tracker checkout then reads as pinned and the gate exits 0 --
# the dangerous direction, and the exact defect the gate exists to
# report. Hand-written, because emit_wf emits no job-level keys after the
# step list; that constant is one more un-varied parameter.
raw_case "a job-level key after the step list is not the checkout's ref" 1 \
'Tracker workflow not pinned to dev' \
'name: a
# scheduled-subject: tracker
on:
  schedule:
    - cron: 0 0 * * *
permissions:
  contents: read
  issues: read
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    outputs:
      ref: dev
'

# --- claim 7: the KEY has spellings too, and they are normalised -------
#
# Everything above varies the SHAPE of a value. The KEY was the
# generator constant nobody had varied: every fixture in this file wrote
# `on:` and `permissions:` bare and unquoted, and the gate matched them
# with `/^"?on"?:/` and `/^[[:space:]]*permissions:/`. YAML permits a key
# to be quoted with either quote character and permits a space before the
# colon, so those are four spellings of one key, and the gate read two.
#
# Measured 2026-08-28 against the shipped gate, on both mawk 1.3.4 and
# gawk 5.2.1, with actionlint 1.7.12 accepting every file and
# python3 yaml.safe_load parsing each to the same document as the bare
# spelling: 'on': and `on :` NEVER ENTERED THE DOMAIN -- not reported as
# unclassified, not counted -- while holding `issues: write`; and
# "permissions", 'permissions', "issues" and "ref" were each read as
# absent.
#
# The domain half is the worse one, and it is why the fix is
# normalisation rather than one more branch: the VALUE enumeration has a
# residue to fall into, and the domain rule cannot have one. Its
# complement is every push-triggered workflow in the repository, which is
# not an error, so an unrecognised spelling of `on:` is indistinguishable
# from a workflow that simply is not scheduled.
#
# Each shape is driven four ways: the whole corpus written that way must
# still be CLEAN (the preservation control -- a rule that answered
# "unreadable" to every quoted key would satisfy the three defect cases
# by refusing correct workflows), and then one workflow written that way
# is planted in an otherwise ordinary corpus to attack the domain rule,
# the cross-check and the pin rule in turn. Planted rather than
# corpus-wide on purpose: a corpus written entirely in an unreadable
# spelling makes the gate refuse for non-vacuity, which is a different
# verdict from the silent pass an attacker gets by planting ONE file.
for KEYSHAPE in dq-keys sq-keys spaced; do
    c_keyshape_control() {
        shaped "$KEYSHAPE" "$1/tracker.yml"  tracker sched issues dev
        shaped "$KEYSHAPE" "$1/tree.yml"     tree    sched noissues none
        shaped "$KEYSHAPE" "$1/pushonly.yml" none    nosched issues dev
    }
    run_case "$KEYSHAPE: a correct corpus written this way is still clean" 0 differs \
        clean c_keyshape_control

    c_keyshape_domain() {
        emit_wf "$1/tracker.yml" tracker sched issues dev
        emit_wf "$1/tree.yml"    tree    sched noissues none
        shaped "$KEYSHAPE" "$1/sneak.yml" none sched issues none
    }
    run_case "$KEYSHAPE: an unmarked scheduled workflow still enters the domain" 1 differs \
        "Unclassified scheduled workflow" c_keyshape_domain

    c_keyshape_crosscheck() {
        emit_wf "$1/tracker.yml" tracker sched issues dev
        shaped "$KEYSHAPE" "$1/tree.yml" tree sched issues none
        emit_wf "$1/pushonly.yml" none   nosched issues dev
    }
    run_case "$KEYSHAPE: a tree marker beside issues: written this way is caught" 1 differs \
        'but requests the `issues:` permission' c_keyshape_crosscheck

    c_keyshape_pin() {
        emit_wf "$1/tracker.yml" tracker sched issues dev
        shaped "$KEYSHAPE" "$1/tree.yml" tree sched noissues dev
        emit_wf "$1/pushonly.yml" none   nosched issues dev
    }
    run_case "$KEYSHAPE: a dev pin written this way is still seen" 1 differs \
        "Tree workflow pinned to dev" c_keyshape_pin
done

# The quoted SCALAR with a quoted KEY. `"permissions": write-all` is the
# author's own precedent turned around: the domain rule already handled
# a quoted `on:` because a quoted key is a real spelling, and the
# permission rules were never given the same treatment. Measured
# 2026-08-28 against the shipped gate: exit 0, clean, holding write on
# every scope.
c_quoted_key_scalar() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    shaped dq-keys "$1/tree.yml" tree sched scalar:write-all none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "a quoted permissions key carrying write-all is still read" 1 differs \
    'but requests the `issues:` permission' c_quoted_key_scalar

# --- claim 8: a flow mapping need not end on the line that opens it ----
# The single-line flow rule reads the opening line and nothing else, so
# `permissions: {` with `issues: write` beneath it opened no block for
# the block rule and closed no mapping for the flow rule: BOTH read
# `issues = 0` for a workflow holding full tracker access. Measured
# 2026-08-28 against the shipped gate: exit 0, clean.
c_flow_multiline() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    shaped flow-perms-multi "$1/tree.yml" tree sched issues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "a flow mapping spread over several lines is still cross-checked" 1 differs \
    'but requests the `issues:` permission' c_flow_multiline

# Preservation for the rule above. A multi-line flow mapping that names
# no `issues:` is a correct tree workflow and must stay clean; a brace
# counter that never left the mapping would report every later line of
# the file as permission text.
c_flow_multiline_clean() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    shaped flow-perms-multi "$1/tree.yml" tree sched noissues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "a multi-line flow mapping without issues: stays clean" 0 differs \
    clean c_flow_multiline_clean

# And its residue. A flow mapping still open at end of file is a value
# this gate did not finish reading; reporting it as `issues = 0` is
# precisely the absence the residue exists to refuse.
c_flow_unterminated() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/odd.yml"     tree    sched openflow none
}
run_case "an unterminated flow mapping is refused, not read as absent" 2 differs \
    "Unclassifiable permissions value" c_flow_unterminated

# --- claim 6, continued: the OTHER half of the same refusal ------------
# The refusal has two halves -- empty stdout, and a non-zero status --
# and the fixture above can only drive the first: with `-f` and `-r`
# asked in the shell first, no workflow file reaches awk unless awk can
# open it. So the status half was a branch with no case, which is the
# same defect one level down (a guard whose test cannot reach it reports
# exactly what a working guard reports). SCHEDULED_REF_PINS_AWK is the
# seam; the stub prints a COMPLETE, plausible fact line and then exits
# non-zero, so only the status can tell the gate that something went
# wrong. An awk that dies part way through a file is the real shape of
# this: partial output is not empty output.
c_awk_fails_loudly() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
}
seam_case() {
    local name="$1" want="$2" wanterr="$3" builder="$4" awkstub="$5"
    local dir rc out
    dir=$(mktemp -d)
    "$builder" "$dir"
    out=$(SCHEDULED_REF_PINS_AWK="$awkstub" bash "$GATE" "$dir" 2>&1)
    rc=$?
    rm -rf "$dir"
    verdict "$name" "$want" "$wanterr" "$rc" "$out"
}

AWK_STUB=$(mktemp)
cat > "$AWK_STUB" <<'STUB'
#!/bin/sh
printf 'scheduled=1\nissues=1\ncheckouts=1\npinned_dev=1\nother_ref=0\nunknown_perm=0\n'
exit 2
STUB
chmod +x "$AWK_STUB"
seam_case "an awk that prints a full fact line and then fails is a refusal" 2 \
    "Cannot read workflow" c_awk_fails_loudly "$AWK_STUB"

# Preservation for the seam itself: the same stub exiting 0 must be
# BELIEVED, or the case above would pass because the seam is broken
# rather than because the status is read. A case that cannot tell its
# subject from its harness is not evidence.
cat > "$AWK_STUB" <<'STUB'
#!/bin/sh
printf 'scheduled=1\nissues=1\ncheckouts=1\npinned_dev=1\nother_ref=0\nunknown_perm=0\n'
STUB
chmod +x "$AWK_STUB"
seam_case "the same fact line with status 0 is believed, not refused" 1 \
    'but requests the `issues:` permission' c_awk_fails_loudly "$AWK_STUB"
rm -f "$AWK_STUB"

rm -rf "$CONTROL_DIR"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
