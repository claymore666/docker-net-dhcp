#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Assert that every workflow_dispatch workflow is actually DISPATCHABLE.
#
# THE RULE GITHUB ENFORCES AND NOTHING HERE DID. A workflow_dispatch
# workflow can only be triggered if the file exists on the repository's
# DEFAULT branch. Not the branch you pass to `--ref` — the default one.
# Until then the workflow is not merely unrunnable, it is not in the
# workflows API at all, and `gh workflow run <file>` answers 404, which
# reads like a typo or a missing scope rather than "this does not exist
# yet".
#
# This project develops on `dev` and releases to `main`, and `main` is
# the default branch. So a new dispatch-only workflow merged to `dev` is
# undispatchable until the next release ships — which is a whole release
# cycle in which its documentation is wrong.
#
# That is not hypothetical. capture-fixtures.yml (#665) was merged to
# dev, docs/internals.md was changed to make `gh workflow run
# capture-fixtures.yml` the PRIMARY route for re-recording the request
# fixtures, and the first dispatch 404'd. The remedy a gate points at
# has to exist — the same rule check-capture-lane.sh applies to the
# script a workflow names, one level up, applied to the workflow itself.
#
# WHY AN ALLOWLIST RATHER THAN A HARD FAILURE. "Not on the default
# branch yet" is the normal, correct state of a new workflow for one
# release cycle. Failing on it outright would just block adding one. So
# a pending workflow is DECLARED, with the reason and what clears it —
# the same bargain as .github/vuln-allowlist.txt, and for the same
# reason: an accepted condition with a written expiry, never a bare
# entry.
#
# A stale declaration fails too. Once the workflow reaches the default
# branch the entry has stopped meaning anything, and an allowlist nobody
# prunes is how a temporary exception becomes permanent.
#
# Usage: bash scripts/check-dispatch-reachable.sh [workflow-dir] [allowlist]
# Env:   BASE_REF (default origin/main) — the default branch to test against.
# Exit:  0 reachable or declared (also when the default branch cannot be
#          read — reported as NOT INSPECTED, never a silent pass),
#        1 an undeclared or stale entry,
#        2 cannot check at all.
set -uo pipefail

WF_DIR="${1:-.github/workflows}"
ALLOWLIST="${2:-.github/dispatch-pending.txt}"
BASE_REF="${BASE_REF:-origin/main}"

[ -d "$WF_DIR" ] || { echo "FAIL  no workflow directory '$WF_DIR'" >&2; exit 2; }

# The comparison target is a branch, not this checkout. CI clones one
# ref; fetch the default branch rather than assuming it is present.
if ! git rev-parse --verify --quiet "$BASE_REF" >/dev/null; then
    git fetch --no-tags --quiet origin "${BASE_REF#origin/}" 2>/dev/null || true
    git rev-parse --verify --quiet "FETCH_HEAD" >/dev/null && BASE_REF=FETCH_HEAD
fi

if ! git rev-parse --verify --quiet "$BASE_REF" >/dev/null; then
    # Deliberately not a pass and not a failure. A workstation with no
    # network cannot answer this, and answering it wrong in the green
    # direction is how a gate stops meaning anything.
    echo "NOT INSPECTED  cannot read '$BASE_REF' — no default branch to compare against."
    echo "  This is the one thing this check needs; CI fetches it, so the"
    echo "  verdict there is the authoritative one."
    exit 0
fi

declared=""
if [ -r "$ALLOWLIST" ]; then
    declared=$(grep -vE '^[[:space:]]*(#|$)' "$ALLOWLIST" | awk '{print $1}' | sort -u)
fi

fail=0
note() { echo "FAIL  $*" >&2; fail=1; }

pending=""
for f in "$WF_DIR"/*.yml "$WF_DIR"/*.yaml; do
    [ -e "$f" ] || continue
    # `on:` may be block or inline; match the key wherever it sits.
    grep -E '^[[:space:]]*workflow_dispatch:' "$f" >/dev/null || continue

    rel="${f#./}"
    if git cat-file -e "${BASE_REF}:${rel}" 2>/dev/null; then
        # On the default branch: dispatchable. A declaration for it is stale.
        if printf '%s\n' "$declared" | grep -Fx "$rel" >/dev/null; then
            note "'$rel' is on ${BASE_REF} but still declared in ${ALLOWLIST}."
            echo "  It is dispatchable now; the entry has stopped meaning anything." >&2
            echo "  Remove it." >&2
        fi
        continue
    fi

    pending="$pending $rel"
    if ! printf '%s\n' "$declared" | grep -Fx "$rel" >/dev/null; then
        note "'$rel' declares workflow_dispatch but is not on ${BASE_REF}."
        echo "  GitHub only exposes a dispatchable workflow from the DEFAULT" >&2
        echo "  branch, so 'gh workflow run $(basename "$rel")' answers 404 today —" >&2
        echo "  and any documentation telling a reader to run it is wrong until" >&2
        echo "  the next release ships." >&2
        echo "  Either say so where it is documented and add an entry to" >&2
        echo "  ${ALLOWLIST} with the reason and what clears it, or do not" >&2
        echo "  present it as a route yet." >&2
    fi
done

if [ "$fail" -ne 0 ]; then
    exit 1
fi

if [ -n "$pending" ]; then
    echo "PASS  dispatch targets reachable on ${BASE_REF}; declared pending:$pending"
else
    echo "PASS  every workflow_dispatch workflow is on ${BASE_REF}"
fi
