#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Assert the privileged lanes share ONE concurrency key expression (#742).
#
# THE FAILURE THIS CATCHES IS A COPY THAT WENT STALE. #390 put
# integration.yml, coverage.yml and capture-fixtures.yml in a single
# concurrency group so two privileged jobs — each installing a plugin,
# each driving the integration suite — could not run against the same
# ref at once. #617 then changed the key so branch pushes bucket on the
# COMMIT rather than the ref, because a merge burst into dev was
# evicting queued runs and leaving commits nothing ever tested.
#
# 7e06f06 made that change in two of the three files. 76d0b7f added the
# third two days later, copying the pre-#617 spelling. Nothing noticed
# for months, and nothing could have: a concurrency group is not
# evaluated by any test, it is evaluated by GitHub, and two workflows
# that disagree about their key simply do not exclude each other. There
# is no error, no annotation, and no red check — the only symptom is two
# jobs running that were supposed to take turns.
#
# So the property is asserted as TEXT, which is the only place it is
# visible before the fact. Byte-identical is a deliberately strict bar:
# these are expressions evaluated by GitHub, not by us, and "obviously
# equivalent" is exactly the judgement that produced the drift. If a
# lane genuinely needs a different key it stops being in this group and
# comes out of the list below, in a commit that says why.
#
# WHAT THIS DOES NOT ASSERT, so the next reader does not over-read it.
# It does not claim the three lanes always exclude each other. They
# cannot: #617's per-commit keying means a push-triggered run and a
# dispatch-triggered run of two of these workflows key on different
# values by design, and no single group can span both. This gate covers
# the one thing that is in our hands — that where they DO share a key,
# they share the same one.
#
# It also asserts that every declared lane has a concurrency block at
# all — see MUST_HAVE_GROUP below for why that list is named and not
# discovered.
#
# Usage: check-concurrency-parity.sh [workflow-dir]
# Exit:  0 all lanes agree, 1 a lane has drifted or has no group,
#        2 cannot check.
set -uo pipefail

WF_DIR="${1:-.github/workflows}"

# The privileged lanes, by the group they are all meant to be in. Adding
# a workflow that installs a plugin on the self-hosted pool means adding
# it here too — that is the point of naming them rather than discovering
# them, since a discovery rule would silently drop a lane that lost its
# `concurrency:` block entirely.
LANES=(integration.yml coverage.yml capture-fixtures.yml capability-matrix.yml)
GROUP_PREFIX='selfhosted-privileged-'

# Lanes that must have SOME concurrency group, whether or not they share
# the privileged one. test.yaml is here rather than in LANES because it
# is hosted and has no business excluding the self-hosted lanes — but it
# was the only CI-heavy workflow in the tree that had never had a group
# at all (#742), so three fixups in five minutes left fifteen runners
# live and ten of them testing dead commits.
#
# Named rather than discovered, deliberately. "Every workflow triggered
# by push or pull_request" is the rule a discovery pass would use, and
# nine workflows here do not satisfy it — codeql, trivy, scorecard,
# dependency-review, release and the rest. Whether they should is a real
# question and not this one; a gate that answers it by accident would
# either fail the tree or have to be weakened until it meant nothing.
MUST_HAVE_GROUP=("${LANES[@]}" test.yaml)

[ -d "$WF_DIR" ] || {
    echo "::error title=Nothing to inspect::no workflow directory '$WF_DIR'." >&2
    exit 2
}

# Every named lane must exist. A renamed or deleted workflow is a real
# change to this group and has to be made deliberately; silently
# skipping it is how a list like this rots into naming nothing.
missing=""
for lane in "${MUST_HAVE_GROUP[@]}"; do
    [ -r "$WF_DIR/$lane" ] || missing="$missing $lane"
done
if [ -n "$missing" ]; then
    echo "::error title=Nothing to inspect::declared privileged lane(s) not found in" \
         "$WF_DIR:$missing. Either the workflow was renamed — in which case update" \
         "the LANES list in this gate — or it was removed, in which case say so." >&2
    exit 2
fi

# The group line, comments stripped so the prose in these workflows
# (which quotes the expression at length, deliberately) cannot satisfy
# or trip this check.
group_of() {
    grep -vE '^[[:space:]]*#' "$1" \
        | grep -E "^[[:space:]]*group:[[:space:]]*${GROUP_PREFIX}" \
        | head -1 \
        | sed -E 's/^[[:space:]]*group:[[:space:]]*//; s/[[:space:]]+$//'
}

fail=0

# --- every declared lane has a group at all ---------------------------
for lane in "${MUST_HAVE_GROUP[@]}"; do
    if ! grep -vE '^[[:space:]]*#' "$WF_DIR/$lane" \
           | grep -E '^concurrency:' >/dev/null; then
        echo "FAIL  $lane has no top-level 'concurrency:' block." >&2
        echo "      Without one, a burst of fixups leaves every superseded commit" >&2
        echo "      still occupying a runner. Key branch pushes on the COMMIT and" >&2
        echo "      not the ref (#617): an evicted run produces NO check run at" >&2
        echo "      all, which reads as absent rather than red." >&2
        fail=1
    fi
done

# --- and the privileged ones agree on which group -----------------------
declare -A SEEN=()
for lane in "${LANES[@]}"; do
    g=$(group_of "$WF_DIR/$lane")
    if [ -z "$g" ]; then
        echo "FAIL  $lane has no 'group: ${GROUP_PREFIX}...' line." >&2
        echo "      A privileged lane with no concurrency group excludes nothing," >&2
        echo "      and GitHub reports that as two jobs running, never as an error." >&2
        fail=1
        continue
    fi
    SEEN["$g"]="${SEEN[$g]:-}$lane "
done

if [ "${#SEEN[@]}" -gt 1 ]; then
    echo "FAIL  the privileged lanes do not share one concurrency key:" >&2
    for g in "${!SEEN[@]}"; do
        printf '        %-40s %s\n' "${SEEN[$g]}" "$g" >&2
    done
    echo "      Two workflows that disagree about their key do not exclude each" >&2
    echo "      other, and nothing goes red when they stop — the only symptom is" >&2
    echo "      two privileged jobs running that were meant to take turns." >&2
    echo "      Make them byte-identical, or take the odd one out of the LANES" >&2
    echo "      list in this gate with a commit that says why." >&2
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo >&2
    echo "::error title=Concurrency key drift::the ${#LANES[@]} privileged lanes" \
         "must share one group expression." >&2
    exit 1
fi

echo "PASS  ${#MUST_HAVE_GROUP[@]} lane(s) have a concurrency group;" \
     "the ${#LANES[@]} privileged ones share one key: ${!SEEN[*]}"
