#!/usr/bin/env bash
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

# fixture DIR SUBJECTS ISSUES_JSON PRS_JSON
fixture() {
    local dir="$TMP/$1"
    mkdir -p "$dir"
    printf '%s\n' "$2" > "$dir/subjects.txt"
    printf '%s\n' "$3" > "$dir/issues.json"
    printf '%s\n' "$4" > "$dir/prs.json"
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
