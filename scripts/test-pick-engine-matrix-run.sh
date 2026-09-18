#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for pick-engine-matrix-run.sh (#1014).
#
# The release gate reads another workflow's recorded verdict instead of
# rebuilding the tag, so CHOOSING the run is the step where a wrong
# answer is invisible: picking a stale run answers for the wrong tree,
# and picking any run at all when none measured the commit turns an
# absence into a pass. Every case below differs from its opposite by one
# field.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

SCRIPT="$(cd "$(dirname "$0")" && pwd)/pick-engine-matrix-run.sh"
guarded_tmpdir TMP

failures=0
n=0

SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
OTHER=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

# picks WHAT JSON WANT
picks() {
    local what="$1" json="$2" want="$3"
    n=$((n + 1))
    local got
    got="$(printf '%s' "$json" | bash "$SCRIPT" "$SHA" 2>"$TMP/err")"
    local rc=$?
    if [ "$rc" -eq 0 ] && [ "$got" = "$want" ]; then
        echo "PASS: $what"
    else
        echo "FAIL: $what: want '$want' (exit 0), got '$got' (exit $rc)"
        sed 's/^/    /' "$TMP/err"
        failures=$((failures + 1))
    fi
}

# refuses WHAT JSON
refuses() {
    local what="$1" json="$2"
    n=$((n + 1))
    local got
    got="$(printf '%s' "$json" | bash "$SCRIPT" "$SHA" 2>"$TMP/err")"
    local rc=$?
    if [ "$rc" -eq 1 ] && [ -z "$got" ]; then
        echo "PASS: $what"
    else
        echo "FAIL: $what should be refused (exit 1), got '$got' (exit $rc)"
        failures=$((failures + 1))
    fi
}

# cannot_check WHAT JSON ARGS...
cannot_check() {
    local what="$1" json="$2"; shift 2
    n=$((n + 1))
    local got
    got="$(printf '%s' "$json" | bash "$SCRIPT" "$@" 2>"$TMP/err")"
    local rc=$?
    if [ "$rc" -eq 2 ] && [ -z "$got" ]; then
        echo "PASS: $what"
    else
        echo "FAIL: $what should be a cannot-check (exit 2), got '$got' (exit $rc)"
        failures=$((failures + 1))
    fi
}

run() { # id sha status conclusion created
    printf '{"id":%s,"headSha":"%s","status":"%s","conclusion":"%s","createdAt":"%s"}' \
        "$1" "$2" "$3" "$4" "$5"
}

picks "one completed run for the commit is chosen" \
      "[$(run 11 "$SHA" completed success 2026-09-18T10:00:00Z)]" \
      "11 completed success"

picks "a run still in flight is chosen, and its status says so" \
      "[$(run 12 "$SHA" in_progress "" 2026-09-18T10:00:00Z)]" \
      "12 in_progress "

picks "a failed run is still the run to read, because the colour is not the verdict" \
      "[$(run 13 "$SHA" completed failure 2026-09-18T10:00:00Z)]" \
      "13 completed failure"

# THE STALE-GREEN CASE. A re-run must answer for the commit, never the
# older run it replaced.
picks "the newest run for the commit wins over an older one" \
      "[$(run 20 "$SHA" completed success 2026-09-18T09:00:00Z),\
$(run 21 "$SHA" completed failure 2026-09-18T11:00:00Z)]" \
      "21 completed failure"

picks "order in the list does not decide it" \
      "[$(run 31 "$SHA" completed failure 2026-09-18T11:00:00Z),\
$(run 30 "$SHA" completed success 2026-09-18T09:00:00Z)]" \
      "31 completed failure"

picks "another commit's run is never chosen" \
      "[$(run 40 "$OTHER" completed success 2026-09-18T12:00:00Z),\
$(run 41 "$SHA" completed success 2026-09-18T10:00:00Z)]" \
      "41 completed success"

refuses "no run at all is refused" "[]"
refuses "only another commit's runs is refused" \
        "[$(run 50 "$OTHER" completed success 2026-09-18T10:00:00Z)]"
refuses "a cancelled run is not evidence" \
        "[$(run 51 "$SHA" completed cancelled 2026-09-18T10:00:00Z)]"
refuses "a skipped run is not evidence" \
        "[$(run 52 "$SHA" completed skipped 2026-09-18T10:00:00Z)]"

picks "a cancelled run does not hide a good one for the same commit" \
      "[$(run 60 "$SHA" completed cancelled 2026-09-18T12:00:00Z),\
$(run 61 "$SHA" completed success 2026-09-18T10:00:00Z)]" \
      "61 completed success"

cannot_check "an empty read is a cannot-check, never a pass" "" "$SHA"
cannot_check "a non-array body is a cannot-check" '{"id":1}' "$SHA"
cannot_check "unparseable input is a cannot-check" 'not json' "$SHA"
cannot_check "no argument is a cannot-check" "[]"
cannot_check "an empty sha is a cannot-check" "[]" ""
cannot_check "two arguments is a cannot-check" "[]" "$SHA" extra

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
