#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-release-registries.sh.
#
# The branch that matters is the one that can never fire in practice:
# the canonical repository publishing without Hub credentials. On the
# real repository the credentials are set, so that branch is dead code
# until the day it is not -- and a conditional that has never taken its
# failing path is a check with one possible verdict.
#
# So both branches are DRIVEN here, by substituting the repository name
# rather than by reasoning about what the workflow would do. The
# substitution is asserted: a case that silently ran against the wrong
# repository would pass for the wrong reason and look identical.
set -uo pipefail

GATE="$(cd "$(dirname "$0")" && pwd)/check-release-registries.sh"
CANON=claymore666/docker-net-dhcp

failures=0
LAST_OUT=""
check() {
    local name="$1" want="$2"; shift 2
    local out got
    out="$(env "$@" bash "$GATE" 2>&1)"; got=$?
    LAST_OUT="$out"
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name (exit $got)"
    else
        echo "FAIL: $name -- want $want, got $got"
        printf '%s\n' "$out" | sed 's/^/    /'
        failures=$((failures + 1))
    fi
}
want_in() {
    # grep -q exits at the first match and SIGPIPEs the producer, which
    # under pipefail reports failure on a SUCCESSFUL match. Read to EOF.
    printf '%s' "$LAST_OUT" | grep -F -- "$1" >/dev/null && return 0
    echo "FAIL: the last case's output did not contain: $1"
    failures=$((failures + 1))
}

# --- the case the whole script exists for -----------------------------
check "the canonical repo without Hub credentials FAILS" 1 \
    "REPO=$CANON" HAS_HUB_CREDS=false
want_in "::error::"
want_in "$CANON"
want_in "BOTH registries"

# --- the leniency that is deliberate and stays ------------------------
check "a fork without Hub credentials warns and proceeds" 0 \
    REPO=someone/docker-net-dhcp HAS_HUB_CREDS=false
want_in "::warning::"
want_in "documented behaviour for a fork"
# The warning must say where the same absence is fatal, or a fork
# maintainer reading it learns nothing about the rule they are outside.
want_in "$CANON"

# --- credentials present: silent success on both ----------------------
check "the canonical repo with Hub credentials passes" 0 \
    "REPO=$CANON" HAS_HUB_CREDS=true
want_in "both registries"
check "a fork with Hub credentials passes" 0 \
    REPO=someone/docker-net-dhcp HAS_HUB_CREDS=true

# --- the substitution is real -----------------------------------------
#
# Every case above turns on REPO. If the gate ignored REPO entirely --
# hardcoding the canonical name, or reading it from the git remote --
# the fork cases would pass by accident and the failing case would pass
# for the wrong reason. Drive the same inputs against a DIFFERENT
# canonical name and require the verdicts to swap.
check "CANONICAL_REPO steers the verdict: the named repo fails" 1 \
    REPO=someone/docker-net-dhcp HAS_HUB_CREDS=false CANONICAL_REPO=someone/docker-net-dhcp
want_in "::error::"
check "CANONICAL_REPO steers the verdict: the real repo is then a fork" 0 \
    "REPO=$CANON" HAS_HUB_CREDS=false CANONICAL_REPO=someone/docker-net-dhcp
want_in "::warning::"

# --- inputs it cannot read REFUSE, never pass -------------------------
#
# An unreadable input must not resolve to either verdict. Read as
# "false" it fails every fork; read as "true" it passes the exact case
# this exists to catch. Both are worse than stopping.
check "an unset REPO refuses" 2 HAS_HUB_CREDS=false
want_in "REPO is unset"
check "an unset HAS_HUB_CREDS refuses" 2 "REPO=$CANON"
want_in "did not reach this step"
check "a HAS_HUB_CREDS that is neither true nor false refuses" 2 \
    "REPO=$CANON" HAS_HUB_CREDS=1
want_in "has changed shape"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures case(s) failed"
    exit 1
fi
echo "all cases passed"
