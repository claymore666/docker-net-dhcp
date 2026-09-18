#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Pick the engine-matrix run that measured a given commit (#1014).
#
# WHY THIS IS A PURE FUNCTION. The release gate has to answer "did the
# documented shapes run on the deployed engine's row, for THIS commit",
# and the answer lives in another workflow's run. Fetching that run is
# one `gh api` call; CHOOSING among what comes back is the part that can
# be wrong in ways nobody notices, so it is here, reading JSON on stdin,
# with cases either side of every rule.
#
# WHAT IT REFUSES, and each one is a release that must not proceed:
#   - no run at all for the commit: nothing measured this tree;
#   - every run for it cancelled or skipped: same, with a reason;
#   - a run that is not the newest for that commit is never chosen, so a
#     stale green cannot answer for a re-run that went red.
#
# It deliberately does NOT decide on `conclusion`. A matrix cell runs
# under `continue-on-error`, so the run's colour is not the verdict and
# reading it as one is the mistake #1013 already recorded. The verdict
# is the row line, and scripts/production-shape-verdict.sh reads that.
# This script only says WHICH run to read and whether it has finished.
#
# Usage: the caller fetches one commit's engine-matrix runs and pipes
#        the JSON array in:
#          gh api "repos/$REPO/actions/workflows/engine-matrix.yml/runs?head_sha=$SHA" \
#            --jq '[.workflow_runs[] | {id, headSha: .head_sha, status,
#                    conclusion, createdAt: .created_at}]' \
#            | bash scripts/pick-engine-matrix-run.sh "$SHA"
# Input: a JSON array of runs, each with id, headSha, status, conclusion
#        and createdAt. The caller asks the API for one commit's runs, so
#        this script is normally choosing among re-runs of one commit;
#        it still matches on headSha, because a caller that widened its
#        query must not silently start answering from another commit.
# Output: "<id> <status> <conclusion>" for the newest matching run
# Exit:  0 a run was chosen
#        1 refused (no usable run for that commit)
#        2 cannot check (wrong arguments, or unreadable input)

set -uo pipefail

if [ "$#" -ne 1 ]; then
    echo "::error title=pick-engine-matrix-run::exactly one argument is" \
         "required, got $#." >&2
    echo "usage: ... | bash scripts/pick-engine-matrix-run.sh <commit-sha>" >&2
    exit 2
fi

SHA="$1"

if [ -z "$SHA" ]; then
    echo "::error title=pick-engine-matrix-run::the commit sha is empty." >&2
    exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
    echo "::error title=pick-engine-matrix-run::jq is not available, so the" \
         "run list cannot be read. This is a cannot-check, never a pass." >&2
    exit 2
fi

INPUT="$(cat)"

if [ -z "${INPUT//[[:space:]]/}" ]; then
    echo "::error title=pick-engine-matrix-run::the run list was empty on" \
         "stdin. An empty read is a cannot-check, never a pass." >&2
    exit 2
fi

if ! printf '%s' "$INPUT" | jq -e 'type == "array"' >/dev/null 2>&1; then
    echo "::error title=pick-engine-matrix-run::the run list is not a JSON" \
         "array. This is a cannot-check, never a pass." >&2
    exit 2
fi

# Newest first by createdAt, then by id, so a re-run of the same commit
# answers for it and an older green cannot.
CHOSEN="$(printf '%s' "$INPUT" | jq -r --arg sha "$SHA" '
    [ .[]
      | select(.headSha == $sha)
      | select((.status // "") != "")
      | select((.conclusion // "") != "cancelled")
      | select((.conclusion // "") != "skipped")
    ]
    | sort_by(.createdAt, (.id | tostring))
    | reverse
    | .[0]
    | if . == null then empty
      else "\(.id) \(.status) \(.conclusion // "")"
      end
')"

if [ -z "$CHOSEN" ]; then
    echo "::error title=No engine-matrix run for this commit::Nothing has" \
         "driven the documented network shapes on the deployed engine for" \
         "commit ${SHA}. The matrix runs on every v* tag push, so a tag cut" \
         "before that lane existed, a run that was cancelled, and a commit" \
         "that was never pushed as a tag all arrive here. The release is" \
         "refused because the evidence is absent, which is not the same as" \
         "the evidence being good." >&2
    exit 1
fi

echo "$CHOSEN"
