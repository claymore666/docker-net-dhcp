#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every `uses:` in every workflow must name a 40-hex commit SHA (#831).
#
# WHAT THIS IS. Pinning is at 100% today and was measured to be enforced
# by nothing: `actions/checkout@v7` was planted in a workflow and all 60
# gates were run, and the red set came back BYTE-IDENTICAL to the
# clean-tree baseline -- zero new reds. `allowed_actions` is `all` at the
# API level too. The property is real and is held entirely by whoever is
# reviewing that day.
#
# THE CONTROL IS WHY THE ZERO IS BELIEVABLE, and it belongs in the record
# rather than in someone's memory: FIFTEEN of the 60 gates are red when
# invoked bare, because they need CI context. Without running the
# clean-tree baseline first, the mutant run reads as "15 gates caught it"
# -- a false confirmation of exactly the thing being tested for.
#
# STATED PLAINLY: THIS IS PROPHYLACTIC. No incident sits behind it. The
# corpus ran at 1 precautionary gate in 60 before this one. That cost is
# named here rather than buried, because the finding of the CI review is
# that gates get added faster than the reason for them gets recorded --
# so a gate whose reason is "no incident yet" has to say so in its own
# header, where the person deciding whether to delete it will look.
#
# A tag is not a pin. `@v7` and `@main` are mutable: whoever controls the
# action repository can repoint them at any commit, and the next run of
# every workflow executes it. That is the whole reason the ecosystem
# pins, and it is why a MOVED tag and a NEW tag are the same risk.
#
# Inputs (environment):
#   WORKFLOW_DIR  directory scanned instead of .github/workflows. Moves
#                 DISCOVERY ONLY -- every judgement below runs on whatever
#                 it finds, so the self-test drives this same code and not
#                 a stub of it.
#
# Exit: 0 every `uses:` is SHA-pinned
#       1 at least one is not, and each is named with file:line
#       2 CANNOT JUDGE -- no workflow files, or no `uses:` at all. A
#         universal over an empty set is true and worthless, so this
#         refuses instead of passing.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW_DIR="${WORKFLOW_DIR:-$ROOT/.github/workflows}"

refuse() {
    echo "::error title=Action pinning cannot be judged::$*" >&2
    exit 2
}

[ -d "$WORKFLOW_DIR" ] || refuse "no workflow directory at $WORKFLOW_DIR."

# BOTH EXTENSIONS, DELIBERATELY. GitHub honours .yml and .yaml alike, and
# this tree contains 23 of one and 1 of the other. A gate matching only
# the common extension would pass over the odd file forever, which is the
# defect class this whole family exists to stop.
mapfile -t FILES < <(find "$WORKFLOW_DIR" -maxdepth 1 -type f \
    \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)

[ "${#FILES[@]}" -gt 0 ] || refuse "no .yml or .yaml files under $WORKFLOW_DIR; there is nothing to judge and 'all of nothing is pinned' is not an answer."

violations=0
seen=0

for f in "${FILES[@]}"; do
    while IFS= read -r hit; do
        lineno="${hit%%:*}"
        line="${hit#*:}"
        ref="$(printf '%s' "$line" | sed -E 's/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*//' | awk '{print $1}' | tr -d '"'"'")"
        [ -n "$ref" ] || continue
        seen=$((seen + 1))

        case "$ref" in
            # A local action or reusable workflow is this repository's own
            # tree at this repository's own commit. There is no third
            # party and nothing to pin to.
            ./*|.\\*) continue ;;
            docker://*)
                case "$ref" in
                    *@sha256:*) continue ;;
                    *) echo "  ${f#"$WORKFLOW_DIR"/}:$lineno  $ref" >&2
                       echo "      a docker:// action must be pinned by @sha256: digest, not by tag." >&2
                       violations=$((violations + 1)) ;;
                esac
                continue ;;
        esac

        # owner/repo[/path]@ref -- the ref after the LAST @ must be 40 hex.
        case "$ref" in
            *@*)
                after="${ref##*@}"
                if [[ "$after" =~ ^[0-9a-f]{40}$ ]]; then
                    continue
                fi
                echo "  ${f#"$WORKFLOW_DIR"/}:$lineno  $ref" >&2
                echo "      '@$after' is a tag or branch, not a commit SHA. Whoever controls that repository can repoint it at any commit, and the next run executes it." >&2
                violations=$((violations + 1)) ;;
            *)
                echo "  ${f#"$WORKFLOW_DIR"/}:$lineno  $ref" >&2
                echo "      no '@ref' at all, so this resolves to the action's default branch and changes without warning." >&2
                violations=$((violations + 1)) ;;
        esac
    done < <(grep -nE '^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*[^[:space:]]' "$f" 2>/dev/null || true)
done

# THE SECOND HALF OF THE NON-VACUITY PREMISE. Files can exist and contain
# no `uses:` at all -- a tree of workflows that only run `run:` steps, or
# a discovery expression that silently matched the wrong thing. "Every
# `uses:` is pinned" is then true over an empty set.
[ "$seen" -gt 0 ] || refuse "found ${#FILES[@]} workflow file(s) under $WORKFLOW_DIR but not one 'uses:' line. Either these are not workflows or the match is wrong; either way nothing was actually checked."

if [ "$violations" -gt 0 ]; then
    echo "::error title=Unpinned action reference::$violations of $seen 'uses:' reference(s) are not pinned to a 40-hex commit SHA. Each is named above with file and line." >&2
    exit 1
fi

echo "action pins: all $seen 'uses:' reference(s) across ${#FILES[@]} workflow file(s) are SHA-pinned."
exit 0
