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

# run NAME WANT_EXIT WANT_GREP RANGE TITLE BODY [AUTHOR]
#
# AUTHOR is optional and defaults to absent, so every case written before
# the bot exemption keeps calling the gate with three arguments — which is
# also the control that an absent author exempts nobody.
run() {
    local name="$1" want_exit="$2" want_grep="$3" range="$4" title="$5" body="$6"
    local author="${7-}"
    local targ="" barg=""
    if [ "$title" != "-" ]; then printf '%s' "$title" > "$TMP/title.txt"; targ="$TMP/title.txt"; fi
    if [ "$body" != "-" ]; then printf '%s' "$body" > "$TMP/body.md"; barg="$TMP/body.md"; fi
    if [ "$#" -ge 7 ]; then
        ( cd "$REPO" && bash "$CHECK" "$range" "$targ" "$barg" "$author" ) > "$TMP/out" 2>&1
    else
        ( cd "$REPO" && bash "$CHECK" "$range" "$targ" "$barg" ) > "$TMP/out" 2>&1
    fi
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

# --- the waiver must survive the gate's OWN failure message ------------
#
# THE CASE THAT CARRIES THE WEIGHT. The waiver used to allow leading
# whitespace, and the FAIL text below teaches the waiver string indented by
# four spaces. The most natural reaction to a failing required check is to
# paste its output into the pull request to talk about it — and that paste
# matched, so the check went green and printed its own help text back as the
# reason it passed.
#
# The fixture is GENERATED from the script, never transcribed. A copy typed
# out here would drift from the message the moment anyone reworded it, and
# this case would keep passing while testing nothing.
git -C "$REPO" reset -q --hard "$BASE"
commit_on 'chore: a change that references nothing'

printf '%s' 'chore: a change that references nothing' > "$TMP/gen-title.txt"
: > "$TMP/gen-body.md"
( cd "$REPO" && bash "$CHECK" "$BASE..HEAD" "$TMP/gen-title.txt" "$TMP/gen-body.md" ) \
    > "$TMP/failure.txt" 2>&1

# If the message stops teaching the waiver string, the case below is
# vacuous — it would be asserting that text containing no waiver fails to
# waive. Say so loudly rather than keep a green case that proves nothing.
if command grep -q 'No issue:' "$TMP/failure.txt"; then
    echo "PASS: the failure text still teaches the waiver string (fixture is live)"
else
    echo "FAIL: the failure text no longer contains 'No issue:' — the self-paste case now tests nothing"
    failures=$((failures + 1))
fi

run "the gate's own failure output pasted into the body does NOT waive" 1 \
    "references an issue" "$BASE..HEAD" 'chore: a change that references nothing' \
    "$(cat "$TMP/failure.txt")"

# The same property stated directly, so the rule survives a rewording of
# the message that the generated case above is pinned to.
run "an indented waiver does not pass" 1 "references an issue" \
    "$BASE..HEAD" 'chore: fix a typo' '    No issue: indented, so inert.'

run "a tab-indented waiver does not pass" 1 "references an issue" \
    "$BASE..HEAD" 'chore: fix a typo' '	No issue: indented, so inert.'

# ...and the other direction, so anchoring cannot be "fixed" by making the
# waiver unreachable altogether.
run "a column-0 waiver still passes" 0 "waived" \
    "$BASE..HEAD" 'chore: fix a typo' 'No issue: a typo in a comment, nothing to track.'

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

# --- the bot exemption -------------------------------------------------
#
# Dependabot cannot reference an issue and cannot write a waiver, so the
# gate was unsatisfiable for it. Every case here is driven from both
# sides, because a widening that accepts everything would pass the one
# case it was written for while quietly retiring the gate.
git -C "$REPO" reset -q --hard "$BASE"
commit_on 'build(deps): bump github.com/sirupsen/logrus from 1.10.0 to 1.10.1'

BOT='dependabot[bot]'

run "the bot with no reference anywhere passes" 0 "exempt" \
    "$BASE..HEAD" 'build(deps): bump the actions group with 3 updates' \
    'Bumps the actions group with 3 updates.' "$BOT"

# THE PRESERVATION CONTROL. The whole risk of this change is that it
# widens into a gate nobody can fail. Identical inputs, human author, and
# the gate must still go red — otherwise the exemption is not an exemption,
# it is a switch that turned the check off.
run "a HUMAN with the same empty PR is still red" 1 "references an issue" \
    "$BASE..HEAD" 'build(deps): bump the actions group with 3 updates' \
    'Bumps the actions group with 3 updates.' 'claymore666'

# ...and the same inputs with no author argument at all, which is how
# every caller before this change invoked the gate. An absent author must
# exempt nobody, so a workflow that stops passing it fails loudly rather
# than waiving everything.
run "an ABSENT author exempts nobody" 1 "references an issue" \
    "$BASE..HEAD" 'build(deps): bump the actions group with 3 updates' \
    'Bumps the actions group with 3 updates.'

run "an empty author string exempts nobody" 1 "references an issue" \
    "$BASE..HEAD" 'build(deps): bump the actions group with 3 updates' \
    'Bumps the actions group with 3 updates.' ''

# THE NAME IS NOT A PATTERN. Each of these is a login somebody could
# actually hold, and every one of them would be exempt under a substring,
# prefix or suffix test. Equality is what makes the exemption a name
# rather than a shape.
for impostor in 'dependabot' 'Dependabot[bot]' 'DEPENDABOT[BOT]' \
                'mydependabot[bot]' 'dependabot[bot]x' 'x-dependabot[bot]' \
                'dependabot[bot] ' ' dependabot[bot]' 'dependabot-preview[bot]'; do
    run "a lookalike login '$impostor' is NOT exempt" 1 "references an issue" \
        "$BASE..HEAD" 'build(deps): bump something' 'No reference here.' "$impostor"
done

# The exemption is checked last, so a bump that does carry a reference is
# still reported on the reference. Otherwise the message would lie about
# which carrier worked, and a bot PR that referenced a real issue would
# look indistinguishable from one that referenced nothing.
git -C "$REPO" reset -q --hard "$BASE"
commit_on 'build(deps): bump the actions group (#807)'
run "a bot PR that DOES reference is reported on the reference" 0 "a commit subject" \
    "$BASE..HEAD" 'build(deps): bump the actions group with 3 updates' \
    'Bumps the actions group.' "$BOT"

# --- the wiring, one level up ------------------------------------------
#
# Everything above proves the SCRIPT exempts the bot. None of it proves
# the WORKFLOW passes the author, and a gate whose premise is supplied by
# its caller fails silently at the caller (#790 is the same shape: a
# presence check that stopped one level short of the wrapper). If the
# argument is dropped, every case above still passes and dependency bumps
# go red again in CI with nothing here to say why.
WF="$HERE/../.github/workflows/test.yaml"
if [ ! -f "$WF" ]; then
    echo "FAIL: cannot find .github/workflows/test.yaml — the wiring case tests nothing"
    failures=$((failures + 1))
else
    inv=$(command grep -A6 'bash scripts/check-issue-ref.sh' "$WF")
    if [ -z "$inv" ]; then
        echo "FAIL: test.yaml no longer invokes check-issue-ref.sh — wiring case is vacuous"
        failures=$((failures + 1))
    elif printf '%s' "$inv" | command grep -q 'PR_AUTHOR'; then
        echo "PASS: test.yaml passes the PR author to the gate"
    else
        echo "FAIL: test.yaml invokes check-issue-ref.sh without the author argument —"
        echo "      the bot exemption is dead in CI and every dependency bump goes red"
        failures=$((failures + 1))
    fi
    if command grep -q 'PR_AUTHOR: ${{ github.event.pull_request.user.login }}' "$WF"; then
        echo "PASS: PR_AUTHOR is sourced from the event payload, not computed"
    else
        echo "FAIL: PR_AUTHOR is not sourced from github.event.pull_request.user.login"
        failures=$((failures + 1))
    fi
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "failed: $failures"
    exit 1
fi
echo "all check-issue-ref.sh tests passed"
