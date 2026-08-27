#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-scheduled-ref-pins.sh (#839).
#
# The gate makes four separable claims, and this file is arranged so
# that exactly one of them fails per case:
#
#   1. every scheduled workflow carries a subject marker
#   2. the marker agrees with the permission block
#   3. `tracker` pins every checkout to dev
#   4. `tree` pins none of them to dev
#
# Fixtures are GENERATED per case rather than mutated in place, which is
# how the sibling gate meta-tests do it and which sidesteps the
# restore-a-mutated-fixture hazard entirely (a backup written inside the
# fixture directory is not a backup — the setup that replaces the
# directory eats it, and later cases then run against an already-broken
# subject).
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

# emit_wf FILE MARKER TRIGGER PERMS [REF...]
#
#   MARKER   tracker | tree | none | <anything else, written verbatim>
#            `both` writes two markers, which is its own error case.
#   TRIGGER  sched | nosched
#   PERMS    issues | noissues
#   REF...   one token per actions/checkout step: none | dev | main |
#            refs/heads/dev. No tokens means no checkout step at all.
emit_wf() {
    local file="$1" marker="$2" trigger="$3" perms="$4"; shift 4
    {
        printf 'name: %s\n' "$(basename "$file")"
        case "$marker" in
            none) ;;
            both) printf '# scheduled-subject: tracker\n# scheduled-subject: tree\n' ;;
            *)    printf '# scheduled-subject: %s\n' "$marker" ;;
        esac
        printf 'on:\n'
        if [ "$trigger" = sched ]; then
            printf "  schedule:\n    - cron: '0 0 * * *'\n"
        else
            printf '  push:\n    branches: [dev]\n'
        fi
        printf 'permissions:\n  contents: read\n'
        [ "$perms" = issues ] && printf '  issues: read\n'
        printf 'jobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n'
        local r
        for r in "$@"; do
            printf '      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'
            if [ "$r" != none ]; then
                printf '        with:\n          ref: %s\n' "$r"
            fi
        done
        printf '      - name: work\n        run: echo hi\n'
    } > "$file"
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

# run_case NAME WANT-RC DIFFERS-FROM-CONTROL BUILDER
run_case() {
    local name="$1" want="$2" differs="$3" builder="$4"
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
    if [ "$rc" = "$want" ]; then
        ok "$name"
    else
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" >&2
    fi
}

# --- the control: green has to be reachable before any mutant means -----
# anything. A meta-test whose control fails is measuring a broken gate,
# and every "mutant died" below would be the same failure repeated.
c_control() { emit_control "$1"; }
run_case "a classified, correctly pinned corpus is clean" 0 same c_control

# --- claim 3: tracker pins every checkout to dev ------------------------
c_unpinned() {
    emit_wf "$1/tracker.yml" tracker sched issues none
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker workflow with an unpinned checkout is reported" 1 differs c_unpinned

c_pinned_elsewhere() {
    emit_wf "$1/tracker.yml" tracker sched issues main
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker pinned to a branch that is not dev is reported" 1 differs c_pinned_elsewhere

# Two checkouts, one pinned. The rule is EVERY checkout, and a gate that
# checked "at least one" would pass this — the second, unpinned one is
# the checkout that actually judges the tracker.
c_half_pinned() {
    emit_wf "$1/tracker.yml" tracker sched issues dev none
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker with one pinned and one unpinned checkout is reported" 1 differs c_half_pinned

# `refs/heads/dev` is the same instruction spelled longer. Accepting only
# the short form would fail a workflow with nothing wrong with it, and a
# gate that cries wolf gets discharged.
c_long_ref() {
    emit_wf "$1/tracker.yml" tracker sched issues refs/heads/dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "refs/heads/dev counts as pinned to dev" 0 differs c_long_ref

# A tracker workflow judges the tracker AGAINST THE TREE. With no
# checkout there is no tree, and "0 of 0 checkouts unpinned" would
# otherwise satisfy the pin rule vacuously.
c_no_checkout() {
    emit_wf "$1/tracker.yml" tracker sched issues
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker workflow with no checkout at all is reported" 1 differs c_no_checkout

# --- claim 4: tree pins none of them to dev -----------------------------
# The opposite direction. #839 says pinning everything would be wrong,
# so the gate has to be able to say so; without this case the gate
# states half its rule.
c_tree_pinned() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues dev
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tree workflow pinned to dev is reported" 1 differs c_tree_pinned

# A tag or sha pin on a tree workflow is a different question and the
# gate says so in its header. Reporting it would be the cry-wolf case.
c_tree_tag() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues main
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tree workflow pinned to something other than dev is left alone" 0 differs c_tree_tag

# --- claim 1: every scheduled workflow carries a marker -----------------
# This is the case that closes #839 rather than patching its instances.
c_no_marker() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/newcomer.yml" none   sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a new scheduled workflow with no marker is reported" 1 differs c_no_marker

c_unknown_marker() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    banana  sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a marker naming an unknown class is reported" 1 differs c_unknown_marker

c_two_markers() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    both    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "two markers in one file are reported" 1 differs c_two_markers

# --- claim 2: the marker is cross-checked, not trusted ------------------
# The `gate-selftest-runs-in:` precedent: a declaration is verified
# against something the author cannot restate. Both directions, because
# either alone would let one of the two statements be the only witness.
c_tree_with_issues() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml"    tree    sched issues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tree marker on a workflow requesting issues: is reported" 1 differs c_tree_with_issues

c_tracker_without_issues() {
    emit_wf "$1/tracker.yml" tracker sched noissues dev
    emit_wf "$1/tree.yml"    tree    sched noissues none
    emit_wf "$1/pushonly.yml" none   nosched issues dev
}
run_case "a tracker marker on a workflow requesting no issues: is reported" 1 differs c_tracker_without_issues

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
    c_consistent_but_unpinned

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
    c_violation_empties_a_class

# --- non-vacuity: green having decided nothing --------------------------
c_no_schedules() {
    emit_wf "$1/pushonly.yml" none nosched issues dev
    emit_wf "$1/other.yml"    none nosched noissues none
}
run_case "a corpus with no scheduled workflow cannot check" 2 differs c_no_schedules

c_all_tree() {
    emit_wf "$1/a.yml" tree sched noissues none
    emit_wf "$1/b.yml" tree sched noissues none
}
run_case "every scheduled workflow classified tree cannot check" 2 differs c_all_tree

c_all_tracker() {
    emit_wf "$1/a.yml" tracker sched issues dev
    emit_wf "$1/b.yml" tracker sched issues dev
}
run_case "every scheduled workflow classified tracker cannot check" 2 differs c_all_tracker

c_empty() { :; }
run_case "an empty directory cannot check" 2 differs c_empty

# A missing directory is not an empty one: the empty case proves the
# glob guard, this proves the gate does not treat an absent subject as a
# clean one.
missing=$(mktemp -d); rmdir "$missing"
out=$(bash "$GATE" "$missing" 2>&1); rc=$?
if [ "$rc" = 2 ]; then ok "a missing directory cannot check"
else no "a missing directory cannot check (exit $rc, want 2)"; printf '      %s\n' "$out" >&2; fi

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
run_case "workflows without a schedule are outside the domain" 0 differs c_pushonly_ignored

rm -rf "$CONTROL_DIR"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
