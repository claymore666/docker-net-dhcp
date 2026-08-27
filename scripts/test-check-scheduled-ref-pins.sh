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

# emit_wf FILE MARKER TRIGGER PERMS [REF...]
#
#   MARKER   tracker | tree | none | <anything else, written verbatim>
#            `both` writes two markers, which is its own error case.
#   TRIGGER  sched | nosched
#   PERMS    issues | noissues
#   REF...   one token per actions/checkout step: none | dev | main |
#            refs/heads/dev. No tokens means no checkout step at all.
#
# WF_SHAPE is the fifth axis, and it is set through `shaped` rather than
# a positional because REF... is variadic. It varies how the file is
# WRITTEN, not what it says:
#
#   block        (default) block mappings throughout, marker at column 0
#   flow-on      `on: {schedule: [{cron: ...}]}` on one line
#   flow-perms   `permissions: {contents: read, issues: read}` on one line
#   indent       the marker indented, beside the step it governs
#
# Every value must produce a file with the SAME MEANING as `block`.
# That is what makes these cases evidence: if the gate's verdict moves
# when only the spelling moves, the gate is reading the spelling.
emit_wf() {
    local file="$1" marker="$2" trigger="$3" perms="$4"; shift 4
    local shape="${WF_SHAPE:-block}" mark_pfx=''
    [ "$shape" = indent ] && mark_pfx='      '
    {
        printf 'name: %s\n' "$(basename "$file")"
        case "$marker" in
            none) ;;
            both) printf '%s# scheduled-subject: tracker\n%s# scheduled-subject: tree\n' \
                         "$mark_pfx" "$mark_pfx" ;;
            *)    printf '%s# scheduled-subject: %s\n' "$mark_pfx" "$marker" ;;
        esac
        if [ "$shape" = flow-on ]; then
            if [ "$trigger" = sched ]; then
                printf "on: {schedule: [{cron: '0 0 * * *'}]}\n"
            else
                printf 'on: {push: {branches: [dev]}}\n'
            fi
        else
            printf 'on:\n'
            if [ "$trigger" = sched ]; then
                printf "  schedule:\n    - cron: '0 0 * * *'\n"
            else
                printf '  push:\n    branches: [dev]\n'
            fi
        fi
        if [ "$shape" = flow-perms ]; then
            if [ "$perms" = issues ]; then
                printf 'permissions: {contents: read, issues: read}\n'
            else
                printf 'permissions: {contents: read}\n'
            fi
        else
            printf 'permissions:\n  contents: read\n'
            [ "$perms" = issues ] && printf '  issues: read\n'
        fi
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
    0 differs c_uses_tree

# The same delegation declared `tracker` is CLOSED, by the rule that a
# tracker workflow must check something out. This is the boundary of the
# escape, and without it the header's claim that it is tree-only would be
# prose nobody had driven.
c_uses_tracker() {
    emit_wf "$1/tree.yml" tree sched noissues none
    emit_delegating_pair "$1" tracker issues
}
run_case "the same delegation declared tracker is caught by the no-checkout rule" \
    1 differs c_uses_tracker

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
    c_flow_on_unclassified

c_flow_on_clean() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    shaped flow-on "$1/tree.yml" tree sched noissues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "a flow-style on: on a correct workflow is still clean" 0 differs c_flow_on_clean

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
    c_flow_perms_contradiction

c_flow_perms_clean() {
    shaped flow-perms "$1/tracker.yml" tracker sched issues dev
    emit_wf "$1/tree.yml" tree sched noissues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "an inline permissions mapping satisfies the tracker cross-check" 0 differs \
    c_flow_perms_clean

# The marker anchored at column 0, so a correctly classified, correctly
# pinned workflow whose marker sat beside the step it governs was
# rejected as unclassified -- a red naming the wrong remedy, telling the
# author to add the line they had just added.
c_indented_marker() {
    emit_wf "$1/tracker.yml" tracker sched issues dev
    shaped indent "$1/tree.yml" tree sched noissues none
    emit_wf "$1/pushonly.yml" none nosched issues dev
}
run_case "a marker indented beside its step is read" 0 differs c_indented_marker

# --- prose cannot answer for the file ----------------------------------
# Both flow rules read a whole line, so both could be satisfied by a
# trailing comment. That is the shape check-python-deps.sh shipped
# (#743): a gate reading its own description instead of the thing it
# describes. Hand-written, because the point is bytes emit_wf will not
# produce.
raw_case() {
    local name="$1" want="$2" body="$3"
    local dir rc out
    dir=$(mktemp -d)
    printf '%s' "$body" > "$dir/a.yml"
    emit_wf "$dir/tracker.yml" tracker sched issues dev
    emit_wf "$dir/tree.yml"    tree    sched noissues none
    out=$(bash "$GATE" "$dir" 2>&1); rc=$?
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then ok "$name"
    else no "$name (exit $rc, want $want)"; printf '      %s\n' "$out" >&2; fi
}

raw_case "a comment on the on: line does not declare a schedule" 0 \
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

rm -rf "$CONTROL_DIR"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
