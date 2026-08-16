#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Keep the "good first issue" promise honest (#537, follow-up to #455).
#
# Two public artifacts make claims about live tracker state:
#
#   * .bestpractices.json answers `small_tasks` as Met, and justifies it
#     by naming a label filter and specific issue numbers.
#   * README.md's Contributing section links the same filter.
#
# Both stop being true silently. When the seeded starter tasks are
# picked up and nothing replaces them, the filter returns an empty list,
# the README sends a newcomer to an empty page, and the badge answer
# asserts something false. Nothing builds, nothing goes red, and the
# first person to find out is the contributor the criterion exists to
# serve — at the exact moment we succeeded at attracting one.
#
# TWO CHECKS, DELIBERATELY SPLIT BY WHAT THEY NEED (see #537, which asks
# for this decision to be made before the code is written).
#
#   --static  Needs no network. The README's filter URL and the badge
#             justification's filter URL must be byte-identical, so a
#             label rename cannot leave one of them pointing at a query
#             that silently matches nothing. Safe at PR time: it is a
#             property of the tree, so it can only be broken by the
#             commit being tested.
#
#   --live    Needs the tracker API. The label exists, at least one OPEN
#             issue carries it, and every issue number cited in the
#             justification is still open and still labelled. This is
#             NOT safe at PR time — an unrelated PR would go red because
#             somebody closed the last starter issue, charging the cost
#             to whoever pushed next. It runs on a schedule instead.
#
# EXIT CODES ARE PART OF THE CONTRACT:
#   0  the claims hold
#   1  a claim is FALSE — the artifacts and the tracker disagree
#   2  the check COULD NOT SEE (missing file, unparseable, API
#      unreachable). Never silently 0: a gate that cannot look must say
#      so, or its green means nothing.
#
# Env seams, for the self-test:
#   GFI_README  path to README.md
#   GFI_BADGE   path to .bestpractices.json
#   GFI_GH      the gh command (default: gh), so --live can be driven
#               against a stub without touching the network.

set -uo pipefail

MODE="${1:-}"
case "$MODE" in
    --static|--live) ;;
    *) echo "usage: $0 --static|--live" >&2; exit 2 ;;
esac

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
README="${GFI_README:-$ROOT/README.md}"
BADGE="${GFI_BADGE:-$ROOT/.bestpractices.json}"
GH="${GFI_GH:-gh}"

for f in "$README" "$BADGE"; do
    [ -r "$f" ] || { echo "::error title=Cannot see::$f is missing or unreadable" >&2; exit 2; }
done

# The filter URL, wherever it appears.
#
# MATCH THE SUPERSET, THEN JUDGE. The obvious implementation greps for a
# URL mentioning "good first issue" — and is then blind to the single
# case this check exists for, because a renamed label produces a URL
# that no longer contains that string. The gate would report "cannot
# see" for the one input it is supposed to fail on. Same shape as #487.
#
# So: any tracker issue-filter URL carrying a label term qualifies, and
# the comparison decides whether they agree.
url_re='https://github\.com/[^ )"]*issues\?q=[^ )"]*label(%3A|:)[^ )"]*'

readme_url=$(grep -oE "$url_re" "$README" | head -1)
badge_url=$(python3 - "$BADGE" <<'PY'
import json, re, sys
try:
    d = json.load(open(sys.argv[1], encoding="utf-8"))
except Exception as e:
    print(f"UNPARSEABLE {e}", file=sys.stderr)
    sys.exit(2)
j = d.get("small_tasks_justification", "") or ""
m = re.findall(r'https://github\.com/[^ )"]*issues\?q=[^ )"]*label(?:%3A|:)[^ )"]*', j)
print(m[0] if m else "")
PY
) || exit 2

if [ -z "$readme_url" ]; then
    echo "::error title=Cannot see::no good-first-issue filter URL found in $README." \
         "Either the Contributing section lost its link, or this check's pattern no longer matches it." >&2
    exit 2
fi
if [ -z "$badge_url" ]; then
    echo "::error title=Cannot see::no good-first-issue filter URL found in small_tasks_justification." >&2
    exit 2
fi

# The label name the two artifacts agree on, decoded from the query.
label=$(printf '%s' "$badge_url" | sed -n 's/.*label%3A%22\([^%&]*\)%22.*/\1/p' | tr '+' ' ')
[ -n "$label" ] || label=$(printf '%s' "$badge_url" | sed -n 's/.*label:"\([^"&]*\)".*/\1/p')

if [ "$MODE" = "--static" ]; then
    if [ "$readme_url" != "$badge_url" ]; then
        echo "::error title=Starter-task links disagree::the README and the badge" \
             "justification point at different filters, so a label rename has left one" \
             "of them matching nothing:" >&2
        echo "  README: $readme_url" >&2
        echo "  badge : $badge_url" >&2
        exit 1
    fi
    if [ -z "$label" ]; then
        echo "::error title=Cannot see::could not decode a label name from $badge_url" >&2
        exit 2
    fi
    echo "static: README and badge justification agree on label \"$label\""
    exit 0
fi

# ---- --live ----------------------------------------------------------
command -v "$GH" >/dev/null 2>&1 || {
    echo "::error title=Cannot see::$GH is not available, so the tracker claims were not checked" >&2
    exit 2
}

open_json=$("$GH" issue list --label "$label" --state open --limit 100 --json number 2>/dev/null) || {
    echo "::error title=Cannot see::could not list open issues labelled \"$label\"" >&2
    exit 2
}
# An API that answers with nothing at all is unreachable, not empty. An
# empty RESULT is "[]"; an empty STRING is a failure we must not read as
# zero (absent data is not a zero).
[ -n "$open_json" ] || {
    echo "::error title=Cannot see::empty response listing issues labelled \"$label\"" >&2
    exit 2
}

mapfile -t open_nums < <(printf '%s' "$open_json" | python3 -c 'import json,sys;[print(i["number"]) for i in json.load(sys.stdin)]' 2>/dev/null)
rc=$?
[ "$rc" -eq 0 ] || { echo "::error title=Cannot see::unparseable issue list" >&2; exit 2; }

fail=0
if [ "${#open_nums[@]}" -eq 0 ]; then
    echo "::error title=No starter tasks left::the label \"$label\" has no OPEN issues." \
         "README.md and .bestpractices.json both promise a newcomer somewhere to start," \
         "and that promise is currently false. Seed a real one, or change both claims." >&2
    fail=1
else
    echo "live: $(printf '%s' "${#open_nums[@]}") open issue(s) carry \"$label\": ${open_nums[*]}"
fi

# Every issue the justification names by number must still be open and
# still labelled — a justification citing closed work is a stale claim
# even when other starter tasks exist.
cited=$(python3 - "$BADGE" <<'PY'
import json, re, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
j = d.get("small_tasks_justification", "") or ""
# Strip the URL first: it contains no issue numbers, but a future one might.
j = re.sub(r'https://\S+', ' ', j)
print(" ".join(sorted({m for m in re.findall(r'#(\d{1,7})\b', j)}, key=int)))
PY
) || exit 2

if [ -n "$cited" ]; then
    for n in $cited; do
        hit=0
        for o in "${open_nums[@]}"; do [ "$o" = "$n" ] && hit=1 && break; done
        if [ "$hit" -eq 0 ]; then
            echo "::error title=Stale badge justification::small_tasks_justification cites #$n" \
                 "as a starter task, but it is not an open issue labelled \"$label\" any more." \
                 "Update the justification to name tasks that actually exist." >&2
            fail=1
        fi
    done
    [ "$fail" -eq 0 ] && echo "live: every cited issue ($cited) is still open and labelled"
fi

exit "$fail"
