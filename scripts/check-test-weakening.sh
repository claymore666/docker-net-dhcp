#!/usr/bin/env bash
# Test-weakening gate (#413).
#
# An escape hatch was once built to silence a failing restart test — a
# helper that made containers stop slowly so the test passed — with an
# honest comment explaining exactly what it did. That sentence was
# correct and understood at the time. It still hid #402 and #408, a
# user-facing `docker restart` failure, for months.
#
# The test had already found the bug. Making it pass destroyed the
# finding at the moment it was produced.
#
# The lesson applies to itself: writing "don't do that" into CLAUDE.md
# is the same move that failed the first time — prose nobody re-reads,
# enforced by nothing. It is in CLAUDE.md, and this is the fix.
#
# Usage: check-test-weakening.sh <commit-range> [pr-body-file]
#   <commit-range>: any git range, e.g. origin/dev..HEAD
#   [pr-body-file]: optional file holding the PR description
#
# Exit: 0 clean or waived, 1 unwaived weakening found, 2 cannot check.
#
# THE ESCAPE HATCH IS DELIBERATE AND CHEAP, BUT IT MUST BE DELIBERATE.
# A finding is waived only by an explicit trailer naming an issue:
#
#     Test-weakening: #123
#
# in the PR body or a commit message in the range.
#
# The first version of this gate accepted any `#123` anywhere, which is
# what #413 asks for literally. Running it against a2b3ac2 — the commit
# that actually introduced the opt-out helper and hid #402 and #408 —
# showed why that is not enough: the gate found the helper and then
# waived itself, because the commit cited the issue it was implementing.
# Practically every commit in this repo cites an issue, so that waiver
# fires always and the gate reports without ever preventing anything.
#
# The trailer cannot be produced by accident. Citing the issue you are
# fixing is not a statement about weakening a test; writing this line
# is. That is the difference between "it cannot happen silently" and
# "it cannot happen while you happen to be working on a ticket".
set -u

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    echo "usage: $0 <commit-range> [pr-body-file]" >&2
    exit 2
fi

RANGE="$1"
BODY="${2-}"

if ! git rev-list "$RANGE" >/dev/null 2>&1; then
    echo "FAIL  cannot resolve commit range '$RANGE'" >&2
    exit 2
fi

# `git diff A..B` is a TREE comparison — two dots mean nothing to it that
# a space would not. But CI passes the base branch's tip *at event time*,
# not the fork point, so the moment dev moves ahead of a branch the diff
# renders everything landed in the meantime as a revert on that branch.
# #461 — a docs PR touching no test file at all — was told it changed a
# timing budget in failure_test.go, because #356 had merged while it was
# open (#463).
#
# `A...B` is exactly `git diff $(git merge-base A B) B`, which judges the
# branch on its own commits. `git log`/`git rev-list` keep the two-dot
# range: there it already means "commits on B and not A", which is what
# the waiver scan wants.
case "$RANGE" in
    *...*) DIFF_RANGE="$RANGE" ;;
    *..*)  DIFF_RANGE="${RANGE%%..*}...${RANGE#*..}" ;;
    *)     DIFF_RANGE="$RANGE" ;;
esac

if ! git diff --name-only "$DIFF_RANGE" -- >/dev/null 2>&1; then
    echo "FAIL  cannot diff '$DIFF_RANGE' (no common ancestor?)" >&2
    exit 2
fi

# Only test-bearing files. Weakening production code is a different
# problem with different reviewers; this gate is about destroying a
# finding a test just produced.
mapfile -t FILES < <(git diff --name-only --diff-filter=d "$DIFF_RANGE" -- \
    '*_test.go' 'test/integration/harness/*.go' 2>/dev/null || true)

if [ "${#FILES[@]}" -eq 0 ]; then
    echo "test-weakening gate: no test files changed"
    exit 0
fi

# A waiver anywhere in the PR body or the range's commit messages covers
# the whole change. Reviewers read those; that is the point.
# Matched case-insensitively with flexible spacing so a reasonable
# person writing it by hand gets it right first time, but it still has
# to be this line and not an incidental issue mention.
TRAILER='[Tt]est-weakening:[[:space:]]*#[0-9]+'
waiver=""
if [ -n "$BODY" ] && [ -f "$BODY" ] && grep -qE "$TRAILER" "$BODY"; then
    waiver="the PR body"
elif git log --format='%B' "$RANGE" | grep -qE "$TRAILER"; then
    waiver="a commit message"
fi

# Assertion accounting is diff-WIDE, not per file.
#
# Counting per file flags any refactor that moves checks into a shared
# helper: they read as deleted in every file they left. That is not a
# weakened test, and a gate that fires on ordinary consolidation gets
# waived by reflex, which is how a gate stops being read at all.
# Verified against 5b6f94c, which moved ~30 assertions into the harness
# and tripped the per-file version.
total_added_asserts=0
total_removed_asserts=0

findings=0
report() {
    findings=$((findings + 1))
    printf '  %-22s %s\n' "$1" "$2"
    printf '      %s\n' "$3"
}

# --- timing-budget values (#450) -------------------------------------
#
# Encoded as <kind>:<n> so two readings are only ever compared when they
# are the same kind of thing:
#
#   ns:120000000000   a Go duration, normalised to nanoseconds
#   raw:60            a bare integer, comparable only to another bare one
#   unknown:0         matched a budget assignment, could not read a value
#
# `unknown` never compares equal or less, so an expression this cannot
# read still reports. Guessing at it is the one outcome worth avoiding:
# a gate that silently passes what it does not understand is the gate
# that was not there.
budget_encode() {
    local expr="$1" n
    # Held in variables because an unbalanced ')' inside [[ =~ ]] is
    # fine for bash and unparseable for shellcheck.
    local dur_re='^[[:space:]]*([0-9]+)[[:space:]]*\*[[:space:]]*time\.([A-Za-z]+)'
    local bare_re='^[[:space:]]*([0-9]+)[[:space:]]*[,)]?[[:space:]]*(//.*)?$'
    if [[ "$expr" =~ $dur_re ]]; then
        n="${BASH_REMATCH[1]}"
        case "${BASH_REMATCH[2]}" in
            Nanosecond)  printf 'ns:%d\n' "$n" ;;
            Microsecond) printf 'ns:%d\n' "$((n * 1000))" ;;
            Millisecond) printf 'ns:%d\n' "$((n * 1000000))" ;;
            Second)      printf 'ns:%d\n' "$((n * 1000000000))" ;;
            Minute)      printf 'ns:%d\n' "$((n * 60000000000))" ;;
            Hour)        printf 'ns:%d\n' "$((n * 3600000000000))" ;;
            *)           printf 'unknown:0\n' ;;
        esac
    elif [[ "$expr" =~ $bare_re ]]; then
        printf 'raw:%d\n' "${BASH_REMATCH[1]}"
    else
        printf 'unknown:0\n'
    fi
}

# Emit "<name> <encoded>" for each timing-budget assignment in the given
# diff lines. The leading [^A-Za-z0-9_] anchors the identifier so the
# whole name is captured, not just the keyword inside it.
budget_values() {
    printf '%s\n' "$1" \
      | sed -nE 's/^.*[^A-Za-z0-9_]([A-Za-z0-9_]*(Timeout|Budget|Grace|Deadline|Interval)[A-Za-z0-9_]*)[[:space:]]*:?=[[:space:]]*([0-9].*)$/\1\t\3/p' \
      | while IFS=$'\t' read -r bn bexpr; do
            printf '%s %s\n' "$bn" "$(budget_encode "$bexpr")"
        done
}

budget_human() {
    local n="${1#*:}"
    case "${1%%:*}" in
        ns)
            if   [ "$n" -ge 60000000000 ] && [ $((n % 60000000000)) -eq 0 ]; then printf '%dm\n' "$((n / 60000000000))"
            elif [ $((n % 1000000000)) -eq 0 ]; then printf '%ds\n' "$((n / 1000000000))"
            elif [ $((n % 1000000)) -eq 0 ];    then printf '%dms\n' "$((n / 1000000))"
            else printf '%dns\n' "$n"; fi ;;
        raw) printf '%s\n' "$n" ;;
        *)   printf 'unparseable\n' ;;
    esac
}

for f in "${FILES[@]}"; do
    diff=$(git diff -U0 "$DIFF_RANGE" -- "$f" 2>/dev/null) || continue
    added=$(printf '%s\n' "$diff" | grep -E '^\+' | grep -v '^+++' || true)
    removed=$(printf '%s\n' "$diff" | grep -E '^-' | grep -v '^---' || true)

    # 1. A skip is the highest-confidence signal there is. A test that
    #    is skipped is a test that found nothing.
    if printf '%s\n' "$added" | grep -qE '\bt\.Skipf?\('; then
        report "$f" "adds t.Skip" \
            "A skipped test cannot fail, so it cannot report. If the condition is genuinely unsupported here, say which issue tracks it."
    fi

    # 2. A BARE sleep in a test — one that is not the interval of a
    #    deadline-bounded poll — is a race being waited out rather than
    #    fixed.
    #
    #    The qualifier is the whole signal. Sweeping 20 commits of real
    #    history, every single time.Sleep flagged without it was the
    #    interval inside a `for time.Now().Before(deadline)` loop: the
    #    deadline-bounded poll this message recommends. A gate that
    #    fires on the recommended fix gets waived by reflex, and then it
    #    is not a gate.
    #
    #    So the sleep is judged with context: if its neighbourhood in
    #    the diff mentions a deadline, a budget, or a bounded loop, it
    #    is a poll and not a smell.
    if [[ "$f" == *_test.go ]]; then
        ctx=$(git diff -U8 "$DIFF_RANGE" -- "$f" 2>/dev/null | grep -E '^\+' | grep -v '^+++' || true)
        if printf '%s\n' "$added" | grep -qE '\btime\.Sleep\('; then
            if ! printf '%s\n' "$ctx" | grep -qE 'deadline|Deadline|time\.Now\(\)\.Before|[Bb]udget|for .*range|Await\('; then
                report "$f" "adds a bare time.Sleep" \
                    "A sleep that is not the interval of a bounded poll waits a race out instead of removing it: it passes on a fast machine and fails on a loaded one. Poll against a deadline, or say which issue tracks the race."
            fi
        fi
    fi

    # 4. A budget that GREW. The first version of this fired on any
    #    change to such a constant, because reading the numbers needs
    #    duration parsing in shell. #449 then moved every budget in
    #    failure_test.go down — 240s to 120s, 4m to 90s — once #356
    #    removed the dnsmasq lease floor that had forced them up, and
    #    had to be waived for tightening the suite. A waiver earned by
    #    doing the right thing is where waiver-by-reflex starts (#450).
    #
    #    So the values are parsed and paired by name. Silence is bought
    #    only by a proven decrease: anything unparseable, or a unit
    #    swap that makes the two incomparable, still reports.
    unset budget_old budget_new
    declare -A budget_old budget_new
    while read -r bname bval; do
        [ -n "$bname" ] && budget_old["$bname"]="$bval"
    done < <(budget_values "$removed")
    while read -r bname bval; do
        [ -n "$bname" ] && budget_new["$bname"]="$bval"
    done < <(budget_values "$added")

    for bname in "${!budget_new[@]}"; do
        old="${budget_old[$bname]-}"
        # No counterpart is a new constant, not a retune. A renamed one
        # reads the same way; the assertion-count signal is what covers
        # wholesale removal.
        [ -n "$old" ] || continue
        new="${budget_new[$bname]}"

        # Two unreadable expressions are not evidence of equality, so
        # the equal-values shortcut comes after the unknown check.
        if [ "${old%%:*}" != "unknown" ] && [ "${new%%:*}" != "unknown" ]; then
            [ "$old" = "$new" ] && continue
            if [ "${old%%:*}" = "${new%%:*}" ] && [ "${new#*:}" -lt "${old#*:}" ]; then
                continue  # tightened — the test now fails sooner, not later
            fi
        fi
        if [ "${old%%:*}" = "unknown" ] || [ "${new%%:*}" = "unknown" ]; then
            report "$f" "changes $bname" \
                "$(budget_human "$old") -> $(budget_human "$new"): this gate cannot read one of those, so it cannot tell whether the budget grew. Raising one turns a reproducible failure into an intermittent one."
        else
            report "$f" "raises $bname" \
                "$(budget_human "$old") -> $(budget_human "$new"). Raising a budget turns a reproducible failure into an intermittent one. If the old value was genuinely wrong, the issue is where that argument lives."
        fi
    done

    # Error/Errorf only, NOT Fatal. In this suite t.Fatalf is
    # overwhelmingly error propagation — `if err != nil { t.Fatalf(...) }`
    # — while t.Errorf is where a value is actually asserted. Measured
    # on 5b6f94c, which centralised 27 t.Fatalf error checks into a
    # helper and removed exactly zero t.Errorf: counting Fatal made a
    # pure consolidation look like 15 deleted checks. On a2b3ac2, the
    # commit this gate exists for, no assertion was removed either — it
    # is caught by the opt-out signal, which is the one that matters
    # there.
    n=$(printf '%s\n' "$added" | grep -cE '\bt\.Errorf?\(' || true)
    total_added_asserts=$((total_added_asserts + n))
    n=$(printf '%s\n' "$removed" | grep -cE '\bt\.Errorf?\(' || true)
    total_removed_asserts=$((total_removed_asserts + n))

    # 5. The exact shape that caused this: a harness helper whose
    #    purpose is to switch a check off.
    if printf '%s\n' "$added" | grep -qE '\bfunc\s+[A-Za-z]*(NoInit|NoWait|NoCheck|SkipCheck|OptOut|Unchecked|Disable[A-Z])[A-Za-z]*\s*\('; then
        report "$f" "adds an opt-out helper" \
            "A helper that exists to bypass a check is the shape that hid #402 and #408. An assertion that the bypassed condition holds is almost always what was wanted instead."
    fi
done

# 3. A net drop in assertions across the whole change means checks were
#    deleted rather than relocated. The strongest signal there is, and
#    the one that most needs the cross-file view.
if [ "$total_removed_asserts" -gt "$total_added_asserts" ]; then
    report "(whole diff)" "removes $((total_removed_asserts - total_added_asserts)) assertion(s)" \
        "Deleting a check is how a finding gets destroyed. Moving one into a helper nets out here, so this is a real drop. If the assertion was wrong, the issue explaining why is what makes that reviewable."
fi

if [ "$findings" -eq 0 ]; then
    echo "test-weakening gate: clean (${#FILES[@]} test file(s) inspected)"
    exit 0
fi

echo
if [ -n "$waiver" ]; then
    echo "test-weakening gate: $findings finding(s), WAIVED by an issue reference in $waiver."
    echo "  Recorded rather than blocked — the rule is that this cannot happen silently,"
    echo "  not that it can never happen (#413)."
    exit 0
fi

cat >&2 <<'EOF'

test-weakening gate FAILED.

A test that only passes once you weaken it is a bug report. That exact
move — an opt-out helper added to silence a failing restart test, with
an honest comment explaining it — hid #402 and #408, a user-facing
`docker restart` failure, for months. The test had already found the
bug; making it pass destroyed the finding.

If the harness really is at fault here, that is fine and it happens.
File an issue saying so, then add this line to the PR body or a commit
message in the range:

    Test-weakening: #<issue>

That is all it takes. It has to be that line rather than any mention of
an issue, because almost every commit here cites one — a waiver that
loose fires on everything, and a gate that always waives itself
prevents nothing.
EOF
exit 1
