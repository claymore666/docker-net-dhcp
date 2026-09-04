#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for runner-image-tags.sh (#812).
#
# This drives the SHIPPED script — the same file .github/workflows/
# runner-image.yml calls — rather than a transcription of its logic. A
# self-test of a copied block proves the copy.
#
# The subject is which tags a Runner image build publishes, and the one
# that matters is `latest`: it is what the runner orchestrator launches,
# so moving it repoints every self-hosted job in this repo. The cases
# below are therefore mostly about the ways `latest` must NOT appear.
#
# The wiring assertions at the end exist because everything above them
# would keep passing against a workflow that stopped calling the script.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
SUBJECT="$HERE/runner-image-tags.sh"
WORKFLOW="$(cd "$HERE/.." && pwd)/.github/workflows/runner-image.yml"

failures=0
n=0

ok()   { printf 'ok   %2d  %s\n' "$n" "$1"; }
bad()  { printf 'FAIL %2d  %s\n' "$n" "$1"; failures=$((failures + 1)); }

# run <name> <want_exit> <want_tags_space_separated> <event> <ref> <promote>
# want_tags is compared as the exact ordered list, so "latest is last"
# is asserted by every passing case rather than by a separate check.
run() {
    n=$((n + 1))
    local name="$1" want_exit="$2" want_tags="$3" ev="$4" ref="$5" prom="$6"
    local out got_exit got_tags
    out="$(EVENT_NAME="$ev" REF="$ref" PROMOTE_LATEST="$prom" SHA7="${SHA7_OVERRIDE-abc1234}" \
           bash "$SUBJECT" 2>/dev/null)" && got_exit=0 || got_exit=$?
    got_tags="$(printf '%s' "$out" | tr '\n' ' ' | sed 's/ *$//')"
    if [ "$got_exit" != "$want_exit" ]; then
        bad "$name — exit $got_exit, wanted $want_exit"
    elif [ "$got_tags" != "$want_tags" ]; then
        bad "$name — tags [$got_tags], wanted [$want_tags]"
    else
        ok "$name"
    fi
}

# --- the promoting cases -------------------------------------------------
# `latest` last in both: a floating tag must not move before the image it
# will point at is fully published (#736).
run "push to dev promotes"                0 "abc1234 latest" push refs/heads/dev ""
run "dispatch with promote_latest=true"   0 "abc1234 latest" workflow_dispatch refs/heads/anything true

# --- the non-promoting cases --------------------------------------------
run "push to a non-dev branch"            0 "abc1234" push refs/heads/other ""
# The 2.x integration branch reaches this workflow's push trigger through
# the `2.*` pattern in runner-image.yml, and it is RENAMED at each
# milestone boundary (D27) -- so these cases drive the shape rather than
# whatever the branch is called this month. Whichever name it carries, the
# pool's :latest must keep pointing at dev's image while the 2.x line
# builds its own, and neither a 1.x maintenance branch nor a tag whose
# name starts `2.` may promote it either.
run "push to the 2.x branch, alpha name" 0 "abc1234" push refs/heads/2.0.0-alpha.1 ""
run "push to the 2.x branch, 2.0.0"      0 "abc1234" push refs/heads/2.0.0 ""
run "push to a 1.9.x branch"             0 "abc1234" push refs/heads/1.9.x ""
run "push to a tag ref"                  0 "abc1234" push refs/tags/v1.9.0 ""
run "push to a 2.x tag ref"              0 "abc1234" push refs/tags/v2.0.0 ""
run "dispatch, input unset"               0 "abc1234" workflow_dispatch refs/heads/b ""
run "dispatch, input false"               0 "abc1234" workflow_dispatch refs/heads/b false
run "dispatch on dev without the input"   0 "abc1234" workflow_dispatch refs/heads/dev ""

# Exact equality, not truthiness. Each of these is a value a boolean
# input or a hand-edited dispatch can actually carry.
run "dispatch, TRUE"                      0 "abc1234" workflow_dispatch refs/heads/b TRUE
run "dispatch, True"                      0 "abc1234" workflow_dispatch refs/heads/b True
run "dispatch, 1"                         0 "abc1234" workflow_dispatch refs/heads/b 1
run "dispatch, yes"                       0 "abc1234" workflow_dispatch refs/heads/b yes
run "dispatch, leading space"             0 "abc1234" workflow_dispatch refs/heads/b " true"
run "dispatch, trailing space"            0 "abc1234" workflow_dispatch refs/heads/b "true "
run "dispatch, truex"                     0 "abc1234" workflow_dispatch refs/heads/b truex

# promote_latest is a dispatch input; it must not promote on other events.
run "push to non-dev with promote=true"   0 "abc1234" push refs/heads/other true
run "unknown event with promote=true"     0 "abc1234" schedule refs/heads/dev true

# A branch whose name merely contains dev is not dev.
run "refs/heads/dev-thing"                0 "abc1234" push refs/heads/dev-thing ""
run "refs/heads/my-dev"                   0 "abc1234" push refs/heads/my-dev ""

# --- refusing to publish without an immutable tag -----------------------
n=$((n + 1))
if SHA7="" EVENT_NAME=push REF=refs/heads/dev PROMOTE_LATEST="" \
       bash "$SUBJECT" >/dev/null 2>&1; then
    bad "empty SHA7 must fail, not publish a bare :latest"
else
    ok "empty SHA7 refuses to publish"
fi

# --- wiring: the workflow must actually use all of this -----------------
wire() { # wire <description> <grep-args...>
    n=$((n + 1))
    local desc="$1"; shift
    if grep -q "$@" "$WORKFLOW"; then ok "wiring — $desc"
    else bad "wiring — $desc"; fi
}

wire "manifest calls runner-image-tags.sh" -- 'bash scripts/runner-image-tags.sh'
wire "promote_latest input is declared"    -- 'promote_latest:'
wire "the input defaults to false"         -Pzo -- 'promote_latest:(.|\n)*?default: false'
wire "EVENT_NAME is passed"                -- 'EVENT_NAME: ${{ github.event_name }}'
wire "REF is passed"                       -- 'REF: ${{ github.ref }}'
wire "PROMOTE_LATEST is passed"            -- 'PROMOTE_LATEST: ${{ inputs.promote_latest }}'
wire "manifest checks out the repo"        -- 'actions/checkout@'

# The step must not reintroduce an unconditional `latest`. This is the
# one textual assertion here, and it is aimed at the exact pre-#812
# shape rather than at the guard's spelling.
n=$((n + 1))
if grep -q 'for tag in "latest"' "$WORKFLOW"; then
    bad "manifest hardcodes latest in the publish loop (pre-#812 shape)"
else
    ok "no hardcoded latest in the publish loop"
fi

# A process substitution's exit status is not checked, so `mapfile <
# <(script)` would turn a failing script into an empty tag list and a
# green job that published nothing.
n=$((n + 1))
if grep -q 'mapfile -t tags < <(' "$WORKFLOW"; then
    bad "tag list read through process substitution — script failure would go green"
else
    ok "tag list read so that a script failure fails the job"
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n failed"
    exit 1
fi
echo "all $n passed"
