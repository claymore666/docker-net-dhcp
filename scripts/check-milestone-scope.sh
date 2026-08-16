#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Keep the release PR's `Closes` list from closing work that is not done.
#
# docs/release-runbook.md step 6 builds that list from MILESTONE
# MEMBERSHIP — "a `Closes #N` line for every issue in the milestone" —
# and merging the release PR closes every issue it names. Milestone
# membership is therefore not a planning convenience; it is the input
# to an irreversible batch close.
#
# The label taxonomy says `backlog` never sits on a milestoned issue,
# precisely because the two mean opposite things: `backlog` says "not
# this release", the milestone says "closed by this release". Nothing
# enforced that. Five open `backlog` issues were sitting in the v1.6.0
# milestone when this was written, and the tag would have closed all
# five as delivered.
#
# WHAT THE PAIRING MEANS, AND WHY THE FIX DIFFERS. `in-dev` (maintained
# by scripts/sync-issue-state-labels.sh) marks an issue whose work has
# merged into `dev`. Against a `backlog` label it splits the cases:
#
#   backlog + milestone + in-dev  -> the work SHIPPED and the `backlog`
#                                    label is stale. The milestone is
#                                    right; drop the label. (#486, #537)
#   backlog + milestone, no in-dev -> genuinely not done, and the tag
#                                    would close it as delivered. Move
#                                    it off the milestone. (#396, #403,
#                                    #480)
#
# Both are reported, separately, with the fix named — a gate that says
# only "these disagree" leaves the reader to re-derive which way.
#
# WHY THIS IS NOT IN test.yaml. The claim can become false with nobody
# touching the tree: someone adds a milestone in the web UI and an
# unrelated PR goes red, charging the cost to whoever pushed next. It
# would also fail on a tracker API blip. Same reasoning, and the same
# accepted cost, as scripts/check-good-first-issues.sh --live: a red
# schedule is easier to ignore than a red PR, and that is the trade.
#
# The risk window is long — a milestone is wrong for days or weeks
# before a release PR is opened — so a daily check catches it with room
# to spare, which is what makes the trade affordable here.
#
# EXIT CODES ARE PART OF THE CONTRACT:
#   0  every milestoned issue is genuinely in scope
#   1  a `backlog` issue carries a milestone — the release would close it
#   2  the check COULD NOT SEE (no gh, API unreachable, unparseable).
#      Never silently 0: a gate that cannot look must say so, or its
#      green means nothing.
#
# Env seams, for the self-test:
#   MS_GH        the gh command (default: gh), so this can be driven
#                against a stub without touching the network
#   MS_BACKLOG   the "not this release" label (default: backlog)
#   MS_INDEV     the "merged into dev" label (default: in-dev)
#
# Usage: bash scripts/check-milestone-scope.sh

set -uo pipefail

GH="${MS_GH:-gh}"
BACKLOG="${MS_BACKLOG:-backlog}"
INDEV="${MS_INDEV:-in-dev}"

command -v "$GH" >/dev/null 2>&1 || {
    echo "::error title=Cannot see::$GH is not available, so milestone scope was not checked" >&2
    exit 2
}

# Ask for the superset — every OPEN issue carrying `backlog` — and judge
# here, rather than asking the tracker for "backlog AND has-milestone".
# Match the superset, then judge (#487): a server-side filter that stops
# matching returns an empty list, which is indistinguishable from a
# clean tracker, and this gate would report success for the one input it
# exists to fail on.
raw=$("$GH" issue list --label "$BACKLOG" --state open --limit 200 \
        --json number,title,milestone,labels 2>/dev/null) || {
    echo "::error title=Cannot see::could not list open issues labelled \"$BACKLOG\"" >&2
    exit 2
}

# An API that answers with nothing at all is unreachable, not empty. An
# empty RESULT is "[]"; an empty STRING is a failure we must not read as
# zero — absent data is not a zero.
[ -n "$raw" ] || {
    echo "::error title=Cannot see::empty response listing issues labelled \"$BACKLOG\"" >&2
    exit 2
}

report=$(printf '%s' "$raw" | python3 -c '
import json, sys

indev = sys.argv[1]
try:
    issues = json.load(sys.stdin)
except Exception as e:
    print(f"UNPARSEABLE {e}", file=sys.stderr)
    sys.exit(2)
if not isinstance(issues, list):
    print("UNPARSEABLE not a list", file=sys.stderr)
    sys.exit(2)

stale, notdone = [], []
for i in issues:
    ms = i.get("milestone") or {}
    title = (ms or {}).get("title")
    if not title:
        continue
    labels = {l.get("name") for l in (i.get("labels") or [])}
    row = (i.get("number"), title, i.get("title", ""))
    (stale if indev in labels else notdone).append(row)

for kind, rows in (("STALE", stale), ("NOTDONE", notdone)):
    for n, ms, t in rows:
        print(f"{kind}\t{n}\t{ms}\t{t}")
' "$INDEV") || {
    echo "::error title=Cannot see::unparseable issue list from $GH" >&2
    exit 2
}

if [ -z "$report" ]; then
    echo "milestone scope: no open \"$BACKLOG\" issue carries a milestone"
    exit 0
fi

fail=0

notdone=$(printf '%s\n' "$report" | grep '^NOTDONE' || true)
if [ -n "$notdone" ]; then
    fail=1
    echo "::error title=Release would close unfinished work::these open issues are labelled" \
         "\"$BACKLOG\" and carry a milestone, and none has \"$INDEV\", so no work for them has" \
         "merged. The release PR's Closes list is built from milestone membership" \
         "(docs/release-runbook.md step 6), so tagging would close them as delivered." \
         "Move them off the milestone:" >&2
    printf '%s\n' "$notdone" | while IFS=$'\t' read -r _ n ms t; do
        echo "  #$n [$ms] $t" >&2
    done
fi

stale=$(printf '%s\n' "$report" | grep '^STALE' || true)
if [ -n "$stale" ]; then
    fail=1
    echo "::error title=Stale backlog label::these open issues carry \"$BACKLOG\" AND \"$INDEV\"," \
         "which contradict each other: their work has already merged into dev. The milestone is" \
         "right and the label is stale. Drop \"$BACKLOG\":" >&2
    printf '%s\n' "$stale" | while IFS=$'\t' read -r _ n ms t; do
        echo "  #$n [$ms] $t" >&2
    done
fi

exit "$fail"
