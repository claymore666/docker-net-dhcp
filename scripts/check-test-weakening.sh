#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

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

# The end of the range is the tree the added lines belong to. A bare rev
# compares against the working tree, so there is no post-image commit and
# the file on disk is the right thing to read.
case "$DIFF_RANGE" in
    *...*) POST="${DIFF_RANGE##*...}" ;;
    *)     POST="" ;;
esac

if ! git diff --name-only "$DIFF_RANGE" -- >/dev/null 2>&1; then
    echo "FAIL  cannot diff '$DIFF_RANGE' (no common ancestor?)" >&2
    exit 2
fi

# Only test-bearing files. Weakening production code is a different
# problem with different reviewers; this gate is about destroying a
# finding a test just produced.
#
# THE CI'S OWN TESTS ARE TESTS (#828). `scripts/test-*.sh` was outside
# this list until then, so the rule CLAUDE.md calls binding was enforced
# mechanically over the tool and not over the tooling — and the CI's test
# corpus is the larger, less-validated one. Measured: a gate self-test
# with an assertion deleted and `return 0  # temporarily skipped` added
# exited 0 with "no test files changed", byte-identical to the output for
# an unrelated edit. Nothing distinguished them.
#
# Widening the list ALONE would have been worse than the gap. Against
# that same gutted commit it turns
#
#     rc=0  "test-weakening gate: no test files changed"
#
# into
#
#     rc=0  "test-weakening gate: clean (1 test file(s) inspected)"
#
# — an honest silence becoming a claim of inspection over a gutted file,
# which is the exact class this script's header is written about. So the
# domain and the shell signals land together, never separately.
TEST_PATHS=('*_test.go' 'test/integration/harness/*.go' 'scripts/test-*.sh')

# --- code, not data (#828) -------------------------------------------
#
# THE GATE FIRED ON ITS OWN SELF-TEST the moment the domain widened, and
# it was right to on the text: `scripts/test-check-test-weakening.sh`
# contains `return 0  # temporarily skipped` eight times, as FIXTURE
# TEXT handed to the gate under test.
#
# That is not a quirk of one file. A gate that matches on CONTENT will
# always find its own triggers quoted inside its own self-test, so
# admitting `scripts/test-*.sh` to any content-matching signal
# guarantees a false positive on the commit that writes the tests — the
# file most likely to be edited next. The same shape as the Go signals
# leaking onto shell fixtures, one layer down.
#
# The property that separates them is not spelling, it is whether the
# shell would EXECUTE the line. So the added lines are classified
# against the post-image: inside a here-document or inside a quoted
# string that opened on an earlier line is data, everything else is
# code, and only code is judged.
#
# The classifier is deliberately small and it says when it is out of its
# depth. If it does not end a file OUTSIDE every construct it opened, it
# has lost track and the file is judged whole, exactly as it would be
# without it — wrong in the direction of reporting, never of silence.
# Measured over all 160 shell scripts in scripts/, including all 81 in
# the domain: 0 files end unbalanced. Both counts are of the tree at the
# time of writing; the domain is what `scripts/test-*.sh` matches, so it
# moves whenever a self-test is added.
post_image() { # <file>
    if [ -n "${POST:-}" ] && git show "$POST:$1" >/dev/null 2>&1; then
        git show "$POST:$1" 2>/dev/null
    else
        cat "$1" 2>/dev/null
    fi
}

# The lines this change ADDED that the shell will run, rendered as diff
# `+` lines so the signals below read them the same way they read Go.
added_code_lines() { # <file>
    local f="$1" nums
    nums=$(git diff -U0 "$DIFF_RANGE" -- "$f" 2>/dev/null | awk '
        /^@@/ { h = $3; sub(/^\+/, "", h); split(h, p, ",")
                s = p[1] + 0; n = (p[2] == "" ? 1 : p[2] + 0)
                for (k = 0; k < n; k++) print s + k }')
    [ -n "$nums" ] || return 0
    post_image "$f" | awk -v nums="$nums" '
        # \047 is a single quote, written as an escape so this program
        # survives living inside a shell single-quoted string.
        BEGIN { split(nums, a, "\n"); for (i in a) want[a[i] + 0] = 1 }
        {
            state = "CODE"
            if (hd != "") {
                state = "DATA"
                s = $0; sub(/^[ \t]+/, "", s)
                if (s == hd) hd = ""
            } else if (inq) {
                state = "DATA"
                p = index($0, "\047")
                if (p > 0) { inq = 0; scan(substr($0, p + 1)) }
            } else {
                scan($0)
            }
            if (want[NR] && state == "CODE") print "+" $0
            lines[NR] = $0
        }
        function scan(line,   i, c, dq, q, rest, depth) {
            dq = 0; depth = 0
            for (i = 1; i <= length(line); i++) {
                c = substr(line, i, 1)
                if (c == "\\") { i++; continue }
                # A command substitution restores normal quoting inside
                # double quotes, so `"$(awk \047...` opens a quote a flat
                # toggle never sees. Two gates in scripts/ are written
                # that way, and both went unbalanced without this.
                if (c == "$" && substr(line, i + 1, 1) == "(") {
                    saved[++depth] = dq; dq = 0; i++; continue
                }
                if (c == ")" && depth > 0) { dq = saved[depth--]; continue }
                if (c == "\"") { dq = !dq; continue }
                if (dq) continue
                if (c == "#" && (i == 1 || substr(line, i-1, 1) ~ /[ \t;&|(]/)) return
                if (c == "\047") {
                    q = index(substr(line, i + 1), "\047")
                    if (q == 0) { inq = 1; return }
                    i = i + q
                    continue
                }
                if (c == "<" && substr(line, i+1, 1) == "<" && substr(line, i+2, 1) != "<") {
                    rest = substr(line, i + 2); sub(/^-/, "", rest)
                    if (match(rest, /^[ \t]*[\047"]?[A-Za-z_][A-Za-z0-9_]*/)) {
                        hd = substr(rest, RSTART, RLENGTH)
                        gsub(/^[ \t]*/, "", hd); gsub(/[\047"]/, "", hd)
                    }
                    return
                }
            }
        }
        END {
            # Lost track: say nothing was data rather than pretend.
            if (hd != "" || inq)
                for (n = 1; n <= NR; n++) if (want[n]) print "+" lines[n]
        }'
}

mapfile -t FILES < <(git diff --name-only --diff-filter=d "$DIFF_RANGE" -- \
    "${TEST_PATHS[@]}" 2>/dev/null || true)

# --- work the range cannot see (#569) --------------------------------
#
# This gate judges a COMMIT RANGE. Run by hand with test changes written
# but not yet committed — which is exactly when someone runs it, to find
# out whether what they just wrote is about to be flagged — the range
# holds no test file, and the gate printed
#
#     test-weakening gate: no test files changed
#
# and exited 0. A clean pass over work it never opened.
#
# That is the third gate here with the same shape: check-version-pins
# matched only well-formed pins, so a broken one was invisible (#487);
# check-license-headers walked only tracked files, so a file being
# written was invisible (#564). Each was green in CI and blind in a
# working checkout, and each reported SUCCESS rather than "nothing to
# compare" — which is what makes this worse than silence.
#
# The fix is not to judge uncommitted work. Inferring intent from a
# half-written tree is its own trap, and a gate that fires on work in
# progress gets ignored. The fix is to stop implying that it did.
# Staged and unstaged changes to tracked files, plus untracked test
# files, all count: each is a test file the verdict below does not cover.
#
# Exit codes are unchanged. This is a refusal to claim a verdict, not a
# failure.
mapfile -t DIRTY < <(
    {
        git diff --name-only HEAD -- "${TEST_PATHS[@]}" 2>/dev/null || true
        git ls-files --others --exclude-standard -- "${TEST_PATHS[@]}" 2>/dev/null || true
    } | sort -u
)

# Printed alongside EVERY verdict, not just the empty-range one. "clean
# (4 test file(s) inspected)" with three more sitting uncommitted makes
# the same claim the empty range did, just less obviously.
note_dirty() {
    [ "${#DIRTY[@]}" -eq 0 ] && return 0
    echo "  NOT INSPECTED — ${#DIRTY[@]} test file(s) have uncommitted changes, which are"
    echo "  outside the range '$RANGE'. This gate judges commits; commit them and run"
    echo "  it again for a verdict on them:"
    printf '    %s\n' "${DIRTY[@]}"
}

if [ "${#FILES[@]}" -eq 0 ]; then
    if [ "${#DIRTY[@]}" -ne 0 ]; then
        echo "test-weakening gate: NO VERDICT — the range '$RANGE' changes no test file."
        note_dirty
        exit 0
    fi
    echo "test-weakening gate: no test files changed"
    exit 0
fi

# A waiver anywhere in the PR body or the range's commit messages covers
# the whole change. Reviewers read those; that is the point.
# Matched case-insensitively with flexible spacing so a reasonable
# person writing it by hand gets it right first time, but it still has
# to be this line and not an incidental issue mention.
# Anchored at column 0. A trailer is a trailer — its own line, no
# indent — so a commit or PR body that QUOTES the trailer while
# explaining it does not thereby waive anything. Unanchored, the gate
# could be switched off by any text describing it, which is how the
# sibling gate in #735 was first caught waiving itself.
TRAILER='^[Tt]est-weakening:[[:space:]]*#[0-9]+'
waiver=""
if [ -n "$BODY" ] && [ -f "$BODY" ] && grep -qE "$TRAILER" "$BODY"; then
    waiver="the PR body"
elif git log --format='%B' "$RANGE" | grep -E "$TRAILER" >/dev/null; then
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

    # EVERY GO SIGNAL IS GUARDED ON A GO FILE (#828).
    #
    # Until the domain widened, `*.go` was the only thing in it and the
    # guard was implicit. It is not any more, and the shell self-tests
    # are full of Go source: they build fixture files to hand to the
    # gate, so `t.Skip(`, `func ...OptOut(` and `t.Errorf(` all appear in
    # them as DATA. Measured over the last 400 non-merge commits, an
    # unguarded widening moves three verdicts from clean to FAILED —
    # 57a2232, f9cbf7c and 4530045 — and every one of them is this file's
    # own self-test being accused of adding a t.Skip it merely quotes.
    # 57a2232 is accused a SECOND time over that same file, of adding an
    # opt-out helper it also only quotes. Both signals need the guard: a
    # reader who guards t.Skip alone re-breaks that commit through the
    # other one.
    #
    # A gate that fires on the commit that writes its own tests is a
    # cry-wolf on the file most likely to be edited next, and a gate
    # waived by reflex is not a gate. The widening only holds if each
    # signal is judged in the language it was priced against.
    if [[ "$f" == *.go ]]; then
        # 1. A skip is the highest-confidence signal there is. A test
        #    that is skipped is a test that found nothing.
        if printf '%s\n' "$added" | grep -E '\bt\.Skipf?\(' >/dev/null; then
            report "$f" "adds t.Skip" \
                "A skipped test cannot fail, so it cannot report. If the condition is genuinely unsupported here, say which issue tracks it."
        fi
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
        if printf '%s\n' "$added" | grep -E '\btime\.Sleep\(' >/dev/null; then
            if ! printf '%s\n' "$ctx" | grep -E 'deadline|Deadline|time\.Now\(\)\.Before|[Bb]udget|for .*range|Await\(' >/dev/null; then
                report "$f" "adds a bare time.Sleep" \
                    "A sleep that is not the interval of a bounded poll waits a race out instead of removing it: it passes on a fast machine and fails on a loaded one. Poll against a deadline, or say which issue tracks the race."
            fi
        fi
    fi

    # --- the same two moves, in shell (#828) -------------------------
    #
    # THE GO SIGNALS DO NOT TRANSFER, and the assertion count is the one
    # that matters. Go calls t.Errorf inline, so counting call sites
    # works. The shell self-tests call a NAMED helper and the corpus has
    # no shared name for it: `check` 558 times, `run_case` 126, `run`
    # 122. There is no analogue without deriving a dominant helper per
    # file, and the obvious substitute — counting removed FAIL-bearing
    # lines — was priced at zero false positives AND zero true positives.
    # A signal set with no false alarms that also catches nothing is a
    # check with one possible verdict, which is not a check.
    #
    # THAT PRICING COVERED 60 COMMITS, and the population is 116 at the
    # time of writing. The number is #828's, taken on an older tree; it
    # is quoted here because it is what was actually measured, and the
    # boundary is stated because a pricing that covered roughly half a
    # population does not license a claim about the whole of it. The
    # first draft of this comment said "all 60 commits that have EVER
    # touched scripts/test-*.sh" — a completeness claim, and one that
    # was already false when it was written.
    #
    # So two signals, both priced on that same history, and both unable
    # to perturb anything that existed: they live inside the `*.sh`
    # branch below, which is the ONLY caller of added_code_lines, so no
    # .go file can reach either of them. That is preservation by
    # construction, not by sampling — and it is why the earlier draft's
    # "silent across 60 Go commits" was both unsourceable and
    # unnecessary. (60 is the shell population above, six lines up; one
    # number was doing duty for two different sets.)
    if [[ "$f" == *.sh ]]; then
        # Judged on what the shell would run, not on what the file
        # quotes. See added_code_lines() above.
        sh_added=$(added_code_lines "$f")
        ctx=$(git diff -U8 "$DIFF_RANGE" -- "$f" 2>/dev/null | grep -E '^\+' | grep -v '^+++' || true)

        # 6. A case switched off, and SAYING SO.
        #
        # A bare added early `return`/`exit 0` alone fires on 4 of the 60
        # — every one of them inside a generated stub heredoc, which is
        # a fixture and not a weakening. Requiring the line, or the added
        # line above it, to also carry a skip/temporary/disabled comment
        # takes that to 0 while still killing the mutant. That is not a
        # loophole: switching a case off is a thing people announce, and
        # the alternative signal anyone reaches for first — a
        # commented-out assertion call — fires on 19 of 60 (32%) and
        # would be a cry-wolf inside a month.
        if printf '%s\n' "$sh_added" | awk '
            {
                prev = mark
                mark = (tolower($0) ~ /#.*(skip|temporar|disabl|for now|fixme)/)
                if ($0 ~ /^\+[ \t]*(return|exit)([ \t]+0)?[ \t]*(#.*)?$/ && (mark || prev)) found = 1
            }
            END { exit found ? 0 : 1 }
        '; then
            report "$f" "disables a case" \
                "An early return or exit that its own comment calls temporary, skipped or disabled is a test being switched off. A switched-off case cannot report. Say which issue tracks it."
        fi

        # 7. A bare sleep, judged with the same context rule as the Go
        #    one: the interval of a bounded poll is the recommended fix,
        #    not the smell. Fires on 0 of the 60 — correctly silent, and
        #    kept because the move is available in shell and this is the
        #    only place that would see it.
        if printf '%s\n' "$sh_added" | grep -E '^\+[[:space:]]*sleep[[:space:]]+[0-9]' >/dev/null; then
            if ! printf '%s\n' "$ctx" | grep -Ei 'deadline|timeout|until |while |retry|attempt|elapsed|budget' >/dev/null; then
                report "$f" "adds a bare sleep" \
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
    if [[ "$f" == *.go ]]; then   # Go signals, Go files (#828)
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
    fi

    # 5. The exact shape that caused this: a harness helper whose
    #    purpose is to switch a check off.
    if [[ "$f" == *.go ]] \
       && printf '%s\n' "$added" | grep -E '\bfunc\s+[A-Za-z]*(NoInit|NoWait|NoCheck|SkipCheck|OptOut|Unchecked|Disable[A-Z])[A-Za-z]*\s*\(' >/dev/null; then
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
    note_dirty
    exit 0
fi

echo
note_dirty
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
