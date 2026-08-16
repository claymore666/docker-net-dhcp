#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for sync-issue-state-labels.sh (#482).
#
# Two things in that script can be wrong in ways nothing else would
# notice, because its output is a label rather than a failing build:
#
#   1. the reference parser — too greedy and it labels issues that were
#      merely mentioned in passing; too strict and `in-dev` silently
#      stops appearing and the milestone goes back to lying;
#   2. the reconciliation — which label wins, and whether a label that
#      no longer applies is actually taken back off. A sync that only
#      ever adds looks perfect on the first run and rots from then on.
#
# Both are exercised offline through --parse and --plan; no network, no
# gh, no repository state.
set -u

SYNC="$(dirname "$0")/sync-issue-state-labels.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

# parse NAME SUBJECT WANT   ("-" for no refs)
parse() {
    local name="$1" subject="$2" want="$3"
    local got
    got=$(printf '%s\n' "$subject" | bash "$SYNC" --parse | tr '\n' ' ' | sed 's/ *$//')
    [ -z "$got" ] && got="-"
    if [ "$got" = "$want" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name"
        echo "    subject: $subject"
        echo "    want [$want]  got [$got]"
        failures=$((failures + 1))
    fi
}

# --- the real shapes on dev today -------------------------------------
parse "issue then PR" \
    'test(integration): say which path preserved the address (#386) (#481)' \
    '#386 #481'
parse "two issues in one group" \
    'ci: shard the main suite three ways, and retire two premises (#468, #430) (#471)' \
    '#468 #430 #471'
parse "a lone trailing group" \
    'build(deps): bump debian (#475)' \
    '#475'

# The regression that makes this parser worth having: an issue number
# quoted in the prose of the subject is NOT a reference.
parse "number in prose is not a ref" \
    'fix(plugin): make the #408 restart wait observable (#422) (#437)' \
    '#422 #437'
parse "prose-only number, no trailing group" \
    'fix(plugin): make the #408 restart wait observable' \
    '-'

# The documented limit, pinned so it changes deliberately or not at all.
parse "prose inside a group stops the walk" \
    'ci(runner-image): add kea alongside dnsmasq (#356, step 1 of 2) (#448)' \
    '#448'

# --- shapes that must not parse ---------------------------------------
parse "no refs at all" 'chore: tidy the Makefile' '-'
parse "group not at the end" 'docs: see (#12) for the rationale' '-'
parse "trailing junk after the group" 'docs: something (#12)x' '-'
parse "empty ref" 'docs: something (#)' '-'
parse "hash without parens" 'docs: closes #12' '-'

# Titles are attacker-controlled on a public repo. Nothing but digits
# may survive, and an unbounded number must not.
parse "shell metacharacters in a group" 'evil: title (#1; rm -rf /)' '-'
parse "command substitution in a group" 'evil: title ($(id)) (#7)' '#7'
parse "absurdly long number is refused" 'evil: title (#123456789012345)' '-'
parse "backticks in a group" 'evil: title (#`whoami`)' '-'

# Formatting slack that should still parse.
parse "no space after the comma" 'feat: a thing (#1,#2) (#3)' '#1 #2 #3'
parse "three separate groups" 'feat: a thing (#1) (#2) (#3)' '#1 #2 #3'
parse "the same number twice is emitted once" 'feat: a thing (#5) (#5)' '#5'
parse "leading zeros normalise" 'feat: a thing (#0012)' '#12'
parse "issue zero is not an issue" 'feat: a thing (#0)' '-'

# --- reconciliation ----------------------------------------------------
# plan NAME WANT_EXIT DIR GREP  (GREP "" = only the exit code matters)
plan() {
    local name="$1" want_exit="$2" dir="$3" want_grep="$4"
    bash "$SYNC" --plan "$dir" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -qF "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit / grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# fixture DIR SUBJECTS ISSUES_JSON PRS_JSON [PR_TITLES_JSON]
#
# pr_titles.json is optional on purpose: absent means the one hop
# contributed nothing, which is what makes it usable as a negative
# control for every test that depends on the hop.
fixture() {
    local dir="$TMP/$1"
    rm -rf "$dir"
    mkdir -p "$dir"
    printf '%s\n' "$2" > "$dir/subjects.txt"
    printf '%s\n' "$3" > "$dir/issues.json"
    printf '%s\n' "$4" > "$dir/prs.json"
    if [ "$#" -ge 5 ]; then
        printf '%s\n' "$5" > "$dir/pr_titles.json"
    fi
    if [ "$#" -ge 6 ]; then
        printf '%s\n' "$6" > "$dir/pr_bodies.json"
    fi
    echo "$dir"
}

# 100 has merged work, 101 has an open PR, 102 has neither.
D=$(fixture basic \
    'feat: done (#100) (#900)' \
    '[{"number":100,"labels":[]},{"number":101,"labels":[]},{"number":102,"labels":[]}]' \
    '[{"number":901,"title":"wip: on it (#101)"}]')
plan "merged work earns in-dev" 0 "$D" $'ADD\t100\tin-dev'
plan "an open PR earns has-pr" 0 "$D" $'ADD\t101\thas-pr'
if grep -q '	102	' "$TMP/out"; then
    echo "FAIL: an untouched issue is left alone"
    failures=$((failures + 1))
else
    echo "PASS: an untouched issue is left alone"
fi

# PR body parses 'Closes #N', 'Fixes #N, #M', and case variations (#548).
D=$(fixture pr_body \
    'chore: nothing' \
    '[{"number":103,"labels":[]},{"number":104,"labels":[]},{"number":105,"labels":[]},{"number":106,"labels":[]},{"number":107,"labels":[]},{"number":108,"labels":[]}]' \
    '[{"number":903,"title":"wip: work","body":"## Related issue\n\nCloses #103\n"},{"number":904,"title":"fix: bug","body":"Fixes #104, #105\n"},{"number":905,"title":"chore: clean","body":"Resolves: #106\n"},{"number":906,"title":"feat: new","body":"Closes nothing on dev\n"},{"number":907,"title":"fix: lower","body":"closes #107\n"},{"number":908,"title":"fix: upper","body":"FIXES #108\n"}]')
plan "a PR body with Closes #N earns has-pr" 0 "$D" $'ADD\t103\thas-pr'
plan "a PR body with Fixes #N, #M earns has-pr for first" 0 "$D" $'ADD\t104\thas-pr'
plan "a PR body with Fixes #N, #M earns has-pr for second" 0 "$D" $'ADD\t105\thas-pr'
plan "a PR body with Resolves: #N earns has-pr" 0 "$D" $'ADD\t106\thas-pr'
plan "a PR body with lowercase closes #N earns has-pr" 0 "$D" $'ADD\t107\thas-pr'
plan "a PR body with uppercase FIXES #N earns has-pr" 0 "$D" $'ADD\t108\thas-pr'

# PR template HTML comments and overlong digit strings must not parse as refs.
D=$(fixture pr_body_comments \
    'chore: nothing' \
    '[{"number":123,"labels":[]},{"number":1234567,"labels":[]}]' \
    '[{"number":909,"title":"feat: template untouched","body":"## Related issue\n\n<!-- e.g. Closes #123 -->\n"},{"number":910,"title":"feat: overlong ref","body":"Closes #12345678\n"}]')
plan "HTML template comment is not parsed as closing ref" 0 "$D" 'SUMMARY'
if grep -qE '^(ADD|REMOVE)' "$TMP/out"; then
    echo "FAIL: template HTML comments or overlong numbers produced false labels"
    failures=$((failures + 1))
else
    echo "PASS: template HTML comments and overlong numbers are ignored"
fi

# The number that is a PR, not an issue. 900 is referenced by the
# subject but is not an open issue, so it must never be labelled — this
# is the whole reason the script intersects instead of classifying.
if grep -q '	900	' "$TMP/out"; then
    echo "FAIL: a PR number is not treated as an issue"
    failures=$((failures + 1))
else
    echo "PASS: a PR number is not treated as an issue"
fi

# in-dev outranks has-pr: a follow-up PR must not un-finish an issue.
D=$(fixture both \
    'feat: done (#100) (#900)' \
    '[{"number":100,"labels":[]}]' \
    '[{"number":902,"title":"followup: more (#100)"}]')
plan "in-dev wins over has-pr" 0 "$D" $'ADD\t100\tin-dev'
if grep -q 'has-pr' "$TMP/out"; then
    echo "FAIL: has-pr is not also applied"
    failures=$((failures + 1))
else
    echo "PASS: has-pr is not also applied"
fi

# The rot case. A label that no longer applies must come back off —
# a PR that closed unmerged, or work that shipped and left dev.
D=$(fixture stale \
    'chore: nothing relevant' \
    '[{"number":100,"labels":[{"name":"has-pr"},{"name":"ci"}]}]' \
    '[]')
plan "a stale has-pr is removed" 0 "$D" $'REMOVE\t100\thas-pr'
if grep -q 'ci' "$TMP/out"; then
    echo "FAIL: labels outside the state axis are untouched"
    failures=$((failures + 1))
else
    echo "PASS: labels outside the state axis are untouched"
fi

D=$(fixture shipped \
    'chore: nothing relevant' \
    '[{"number":100,"labels":[{"name":"in-dev"}]}]' \
    '[]')
plan "a shipped in-dev is removed" 0 "$D" $'REMOVE\t100\tin-dev'

# Already correct: a converged state must produce no churn.
D=$(fixture converged \
    'feat: done (#100) (#900)' \
    '[{"number":100,"labels":[{"name":"in-dev"}]}]' \
    '[]')
plan "a converged state plans nothing" 0 "$D" 'SUMMARY'
if grep -qE '^(ADD|REMOVE)' "$TMP/out"; then
    echo "FAIL: re-running changes nothing"
    failures=$((failures + 1))
else
    echo "PASS: re-running changes nothing"
fi

# --- the one hop (#487) ------------------------------------------------
# A squash subject that names only the PR. #472 shipped this way and read
# as untouched: the subject is GitHub's default (the PR title as it stood
# at merge) and is immutable afterwards, so the only way to reach the
# issue is to ask what PR #900 was called.

# unresolved NAME DIR WANT  ("-" for none)
unresolved() {
    local name="$1" dir="$2" want="$3" got
    got=$(bash "$SYNC" --unresolved "$dir" 2>&1 | tr '\n' ' ' | sed 's/ *$//')
    [ -z "$got" ] && got="-"
    if [ "$got" = "$want" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name"
        echo "    want [$want]  got [$got]"
        failures=$((failures + 1))
    fi
}

HOP_SUBJECT='test(harness): verify the lease kea granted (#900)'
HOP_ISSUES='[{"number":100,"labels":[]}]'

# The lookup list is what the online path spends API calls on. It must
# hold the PR and nothing that is already an open issue.
D=$(fixture unres "$HOP_SUBJECT"$'\nfeat: done (#100) (#901)' "$HOP_ISSUES" '[]')
unresolved "an unresolved ref is listed for lookup" "$D" '900 901'

D=$(fixture unres_none 'feat: done (#100)' "$HOP_ISSUES" '[]')
unresolved "an open issue is never looked up" "$D" '-'

# NEGATIVE CONTROL, and the reason the rest of this section means
# anything: without the hop the issue is invisible. If this ever starts
# planning ADD 100, the tests below stopped testing the hop.
D=$(fixture hop_off "$HOP_SUBJECT" "$HOP_ISSUES" '[]')
plan "without the hop the issue stays invisible" 0 "$D" 'SUMMARY'
if grep -qE '^(ADD|REMOVE)' "$TMP/out"; then
    echo "FAIL: the hop is what makes the case below pass"
    failures=$((failures + 1))
else
    echo "PASS: the hop is what makes the case below pass"
fi

D=$(fixture hop_on "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"test(harness): verify the lease kea granted (#100)"}')
plan "a PR title resolves the subject to its issue" 0 "$D" $'ADD\t100\tin-dev'

# The intersection still classifies. A title may not introduce a number
# the repo does not list as open, however it got there.
D=$(fixture hop_closed "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"feat: a thing (#404)"}')
plan "a title naming a closed issue adds nothing" 0 "$D" 'SUMMARY'
if grep -qE '^(ADD|REMOVE)' "$TMP/out"; then
    echo "FAIL: a title cannot introduce a number that is not open"
    failures=$((failures + 1))
else
    echo "PASS: a title cannot introduce a number that is not open"
fi

# One hop, not a chase. A title naming another PR does not get looked up
# in turn — otherwise a ref cycle would run until the API said stop.
D=$(fixture hop_no_recurse "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"feat: a thing (#901)","901":"feat: a thing (#100)"}')
plan "the hop does not recurse" 0 "$D" 'SUMMARY'
if grep -q 'ADD' "$TMP/out"; then
    echo "FAIL: only refs from the subjects are looked up"
    failures=$((failures + 1))
else
    echo "PASS: only refs from the subjects are looked up"
fi

# Titles are attacker-controlled, and now they are read by the planner
# rather than merely listed. Nothing but digits may survive.
D=$(fixture hop_evil "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"evil: $(touch '"$TMP"'/pwned) (#100; rm -rf /)"}')
plan "a hostile title contributes no refs" 0 "$D" 'SUMMARY'
if [ -e "$TMP/pwned" ] || grep -qE '^(ADD|REMOVE)' "$TMP/out"; then
    echo "FAIL: a hostile title is inert"
    failures=$((failures + 1))
else
    echo "PASS: a hostile title is inert"
fi

# An unreadable answer must not degrade to "the hop found nothing" —
# that is the silent-underlabelling failure this issue was about.
D=$(fixture hop_broken "$HOP_SUBJECT" "$HOP_ISSUES" '[]' 'not json at all')
plan "an unreadable pr_titles.json fails loudly" 1 "$D" ""

# --- the hop reads PR BODIES too (#548) --------------------------------
# The live case, and the ordinary one rather than an edge case: a squash
# subject carries the PR's own number and nothing else, so an issue named
# only in the body is reachable through the body or not at all. PR 532's
# subject is "(#532)" while its body says "Closes #530", and #530 read as
# untouched for days after its work had merged.
D=$(fixture hop_body "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"chore: no number here"}' \
    '{"900":"## Related issue\n\nCloses #100\n"}')
plan "a merged PR naming its issue only in the body earns in-dev" 0 "$D" $'ADD\t100\tin-dev'

# The body must not drag in anything it did not close. Same trust model
# as the title: digits, then intersected with the open-issue list.
D=$(fixture hop_body_scope "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"chore: no number here"}' \
    '{"900":"Closes #100\n\nSee also #101 for context.\n"}')
plan "an issue merely mentioned in the body is not dragged in" 0 "$D" $'ADD\t100\tin-dev'
if grep -qE '^ADD\t101\t' "$TMP/out"; then
    echo "FAIL: only closing keywords in a body earn in-dev"
    failures=$((failures + 1))
else
    echo "PASS: only closing keywords in a body earn in-dev"
fi

# "Refs #N" means "related to" in this repo, not "delivered". PR #550
# refs #549 while fixing one of its two findings — counting that would
# make the label lie in the other direction, which is worse than the
# under-labelling this change fixes.
D=$(fixture hop_body_refs "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"chore: no number here"}' \
    '{"900":"Refs #100\n"}')
plan "a body that only says Refs #N earns nothing" 0 "$D" 'SUMMARY'
if grep -qE '^(ADD|REMOVE)' "$TMP/out"; then
    echo "FAIL: Refs in a body does not earn in-dev"
    failures=$((failures + 1))
else
    echo "PASS: Refs in a body does not earn in-dev"
fi

# Bodies are attacker-controlled on a public repo, exactly as titles are,
# and are now read by the planner rather than merely fetched.
D=$(fixture hop_body_evil "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"chore: no number here"}' \
    '{"900":"Closes $(touch '"$TMP"'/pwned2) #100; rm -rf /\n"}')
plan "a hostile body is inert" 0 "$D" 'SUMMARY'
if [ -e "$TMP/pwned2" ]; then
    echo "FAIL: a hostile body is inert"
    failures=$((failures + 1))
else
    echo "PASS: a hostile body is inert"
fi

# The negative control that keeps every fixture written before this
# change honest: no pr_bodies.json means no body contribution, and must
# never be an error.
D=$(fixture hop_no_bodies "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"chore: no number here"}')
plan "an absent pr_bodies.json is a no-op, not a failure" 0 "$D" 'SUMMARY'

# An unreadable one fails loudly, for the same reason its title
# counterpart does: a hop that silently found nothing strips labels.
D=$(fixture hop_bodies_broken "$HOP_SUBJECT" "$HOP_ISSUES" '[]' \
    '{"900":"chore: no number here"}' 'not json at all')
plan "an unreadable pr_bodies.json fails loudly" 1 "$D" ""

# --- the workflow must run ONE version of this script (#490) -----------
# Not a property of the script, but the property that decides which
# script runs at all — so it is asserted here rather than left to a
# comment. Without an explicit ref, cron checks out the default branch
# (`main`) while a push checks out dev. Every run is a reconciler that
# also REMOVES labels, so two versions of the parser undo each other on
# a loop for the whole window between merging to dev and shipping.
WF="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/issue-state-labels.yml"
if [ ! -f "$WF" ]; then
    echo "FAIL: the workflow is where this expects it ($WF)"
    failures=$((failures + 1))
else
    # Comments stripped first: a `# ref: dev` must not satisfy this.
    wf_body=$(sed 's/[[:space:]]*#.*$//' "$WF")
    if printf '%s\n' "$wf_body" | grep -qE '^[[:space:]]+ref:[[:space:]]*dev[[:space:]]*$'; then
        echo "PASS: the workflow pins its checkout to dev"
    else
        echo "FAIL: the workflow pins its checkout to dev"
        echo "    without 'ref: dev', cron runs main's copy of the script"
        failures=$((failures + 1))
    fi

    # The pin above is only meaningful if there is one checkout to pin.
    checkouts=$(printf '%s\n' "$wf_body" | grep -cE 'uses:[[:space:]]*actions/checkout@')
    if [ "$checkouts" -eq 1 ]; then
        echo "PASS: the workflow has exactly one checkout"
    else
        echo "FAIL: the workflow has exactly one checkout (found $checkouts)"
        failures=$((failures + 1))
    fi
fi

# --- guard the guard ---------------------------------------------------
# If --plan could not fail, every assertion above would be vacuous.
D=$(fixture broken \
    'feat: done (#100) (#900)' \
    'not json at all' \
    '[]')
plan "unreadable input fails rather than planning nothing" 1 "$D" ""

plan "a directory missing its files is a usage error" 2 "$TMP" ""

bash "$SYNC" --plan "$TMP/does-not-exist" >/dev/null 2>&1
[ $? -eq 2 ] && echo "PASS: a missing directory is a usage error" || {
    echo "FAIL: a missing directory is a usage error"
    failures=$((failures + 1))
}

bash "$SYNC" --nonsense >/dev/null 2>&1
[ $? -eq 2 ] && echo "PASS: an unknown flag is a usage error" || {
    echo "FAIL: an unknown flag is a usage error"
    failures=$((failures + 1))
}

if [ "$failures" -ne 0 ]; then
    echo "$failures test(s) failed" >&2
    exit 1
fi
echo "All sync-issue-state-labels.sh tests passed."
