#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-issue-ref.sh (#718).
#
# The gate's whole value is that it fails, so every carrier is driven from
# both sides: present and reachable, absent and red. The template case is the
# one that would otherwise embarrass us — .github/PULL_REQUEST_TEMPLATE.md
# ships the line "<!-- e.g. Closes #123 -->", so a gate that counted commented
# text would pass every PR that had not touched the template at all, while
# reporting the issue number as found.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-issue-ref.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

# A throwaway repository, so the range is real rather than mocked.
REPO="$TMP/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q
git -C "$REPO" config user.email t@example.invalid
git -C "$REPO" config user.name Test
git -C "$REPO" config commit.gpgsign false
echo base > "$REPO/f"; git -C "$REPO" add f; git -C "$REPO" commit -qm 'chore: base'
BASE=$(git -C "$REPO" rev-parse HEAD)

commit_on() {  # commit_on <subject>
    echo "$RANDOM" > "$REPO/f"
    git -C "$REPO" add f
    git -C "$REPO" commit -qm "$1"
}

# run NAME WANT_EXIT WANT_GREP RANGE TITLE BODY
run() {
    local name="$1" want_exit="$2" want_grep="$3" range="$4" title="$5" body="$6"
    local targ="" barg=""
    if [ "$title" != "-" ]; then printf '%s' "$title" > "$TMP/title.txt"; targ="$TMP/title.txt"; fi
    if [ "$body" != "-" ]; then printf '%s' "$body" > "$TMP/body.md"; barg="$TMP/body.md"; fi
    ( cd "$REPO" && bash "$CHECK" "$range" "$targ" "$barg" ) > "$TMP/out" 2>&1
    local got=$?
    local ok=1
    [ "$got" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ]; then command grep -q -- "$want_grep" "$TMP/out" || ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit / grep '$want_grep', got exit $got)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# --- usage and inputs --------------------------------------------------
( bash "$CHECK" ) >/dev/null 2>&1
[ $? -eq 2 ] && echo "PASS: no range is a usage error" || {
    echo "FAIL: no range is a usage error"; failures=$((failures + 1)); }

run "an unresolvable range cannot check" 2 "cannot resolve" \
    "nosuchref..HEAD" - -

# Absent data is not a zero: an empty range means the caller passed the wrong
# base, and "reachable" would be a green check over no commits at all.
run "an empty range cannot check" 2 "cannot check" \
    "$BASE..$BASE" - -

# --- the three carriers ------------------------------------------------
commit_on 'fix(plugin): a thing (#123)'
run "a ref in a commit subject passes" 0 "a commit subject" \
    "$BASE..HEAD" - -

git -C "$REPO" reset -q --hard "$BASE"
commit_on 'fix(plugin): a thing with no reference'
run "a ref in the PR title passes" 0 "the PR title" \
    "$BASE..HEAD" 'fix(plugin): a thing with no reference (#123)' -

run "a closing keyword in the PR body passes" 0 "the PR body" \
    "$BASE..HEAD" 'fix(plugin): a thing with no reference' 'Closes #123.'

# The #718 shape exactly: subjects silent, title silent, body carries it.
run "the #718 shape is reachable" 0 "#123" \
    "$BASE..HEAD" 'fix: tighten file modes, bound both HTTP servers' \
    'Closes #123, closes #124.

Some prose about the change.'

# --- the title is read the way the RECONCILER reads it (#742) ----------
# The gate used to parse titles with `--parse`, i.e. commit_refs(), the
# merge-aware variant. The reconciler parses titles with refs(). So a
# merge-form title satisfied the gate while giving the reconciler
# nothing to read — a green check for a PR whose reference no downstream
# consumer can see, which is the false green this gate exists to
# prevent. It is also the direction commit_refs()'s own docstring warns
# about: a PR title is attacker-controlled, and the merge form names a
# PR, never an issue.
run "a merge-form PR title is NOT a reference" 1 "references an issue" \
    "$BASE..HEAD" 'Merge pull request #500 from someone/branch' 'Just some prose.'

# ...and the ordinary trailing-group title still is, or the fix would
# just be a gate nobody can satisfy from a title.
run "an ordinary trailing-group title still passes" 0 "the PR title" \
    "$BASE..HEAD" 'fix(plugin): a thing with no reference (#123)' 'Just some prose.'

# --- the failure the gate exists for -----------------------------------
run "nothing anywhere is red" 1 "references an issue" \
    "$BASE..HEAD" 'fix(plugin): a thing with no reference' 'Just some prose.'

# A bare number is not a reference. The reconciler will not read it, so
# neither may this.
run "a bare number in prose is not a reference" 1 "references an issue" \
    "$BASE..HEAD" 'fix(plugin): about #123' 'It relates to #123 somehow.'

# THE TEMPLATE CASE. .github/PULL_REQUEST_TEMPLATE.md ships this comment.
# A PR that deleted nothing from the template must still be judged on its
# own content.
run "a commented-out template example does not count" 1 "references an issue" \
    "$BASE..HEAD" 'fix(plugin): a thing with no reference' \
    '## Related issue

<!-- e.g. Closes #123 -->
'

# --- the waiver --------------------------------------------------------
run "an explicit waiver with a reason passes" 0 "waived" \
    "$BASE..HEAD" 'chore: fix a typo in a comment' \
    'No issue: a one-word typo in a comment, nothing to track.'

run "a waiver with no reason does not pass" 1 "references an issue" \
    "$BASE..HEAD" 'chore: fix a typo' 'No issue:'

# A PR carrying both is reported on its reference, not on the waiver — so a
# stale waiver line cannot mask which carrier actually worked.
run "a reference wins over a waiver" 0 "the PR body" \
    "$BASE..HEAD" 'fix(plugin): a thing' 'Closes #123.
No issue: left over from an earlier draft.'

# --- the parser is shared, not copied ----------------------------------
# If check-issue-ref.sh ever grew its own regex, this would drift silently.
# Assert the two agree on a subject the shared parser deliberately refuses.
shared=$(printf '%s\n' 'docs: see (#12) for the rationale' \
    | bash "$HERE/sync-issue-state-labels.sh" --parse)
git -C "$REPO" reset -q --hard "$BASE"
commit_on 'docs: see (#12) for the rationale'
if [ -z "$shared" ]; then
    run "the gate refuses what the shared parser refuses" 1 "references an issue" \
        "$BASE..HEAD" 'docs: see (#12) for the rationale' 'Just prose.'
else
    echo "FAIL: the shared parser accepted a mid-subject group — fixture is stale"
    failures=$((failures + 1))
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "failed: $failures"
    exit 1
fi
echo "all check-issue-ref.sh tests passed"
