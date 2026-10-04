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

# #1205: two runs of one commit, the branch push's finished and the tag
# push's still going. The newest still answers, so the gate waits for it.
# Taking the finished older run instead would let a green from earlier
# answer for a run still measuring a moving engine tag (`29-dind`).
picks "a newer run still in flight wins over an older completed one" \
      "[$(run 14 "$SHA" completed success 2026-09-18T09:00:00Z),\
$(run 15 "$SHA" in_progress "" 2026-09-18T09:00:20Z)]" \
      "15 in_progress "

picks "an older run still in flight does not hide a newer completed one" \
      "[$(run 16 "$SHA" in_progress "" 2026-09-18T09:00:00Z),\
$(run 17 "$SHA" completed success 2026-09-18T09:00:20Z)]" \
      "17 completed success"

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

# THE GATE'S WAIT BOUND (#1205). The tag's engine-matrix run queues behind
# the branch push's run of the same commit, so the gate may wait for two
# lanes in a row. The slowest lane measured is 31 min (run 37142752845,
# 40 runs read 2026-10-04). The bound lives in release.yml, so it is read
# from there: reverting to the old 1800 s must turn this red.
SLOWEST_LANE_SECONDS=1860
RELEASE_YML="${RELEASE_YML:-$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/release.yml}"

bound_case() { # what file want(0|1)
    local what="$1" file="$2" want="$3"
    n=$((n + 1))
    local out rc
    out="$(python3 - "$file" "$SLOWEST_LANE_SECONDS" <<'PY' 2>&1
import re, sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
lane = int(sys.argv[2])
job = doc["jobs"]["production-shape"]
wait = int(job["env"]["ENGINE_WAIT_SECONDS"])
ceiling = int(job["timeout-minutes"]) * 60
find = [s for s in job["steps"] if s.get("id") == "run"][0]["run"]
bad = []
if not re.search(r"deadline=.*\+ *\$?\{?ENGINE_WAIT_SECONDS", find):
    bad.append("the wait step does not derive its deadline from ENGINE_WAIT_SECONDS")
if wait < 2 * lane:
    bad.append("wait %d s is under two serialised lanes (%d s)" % (wait, 2 * lane))
if wait + 300 > ceiling:
    bad.append("wait %d s leaves under 300 s of the %d s job ceiling" % (wait, ceiling))
print("; ".join(bad))
sys.exit(1 if bad else 0)
PY
)"
    rc=$?
    if [ "$rc" -eq "$want" ]; then
        echo "PASS: $what"
    else
        echo "FAIL: $what: want exit $want, got $rc: $out"
        failures=$((failures + 1))
    fi
}

bound_case "the shipped gate waits for two serialised lanes inside its job ceiling" "$RELEASE_YML" 0

sed 's/ENGINE_WAIT_SECONDS: .3900./ENGINE_WAIT_SECONDS: '"'1800'"'/' "$RELEASE_YML" > "$TMP/release-old-bound.yml"
bound_case "the old 1800 s bound is refused" "$TMP/release-old-bound.yml" 1

sed 's/timeout-minutes: 75/timeout-minutes: 45/' "$RELEASE_YML" > "$TMP/release-low-ceiling.yml"
bound_case "a wait that fills the job ceiling is refused" "$TMP/release-low-ceiling.yml" 1

sed 's/+ ENGINE_WAIT_SECONDS ))/+ 3900 ))/' "$RELEASE_YML" > "$TMP/release-hardcoded.yml"
bound_case "a wait step that ignores the declared bound is refused" "$TMP/release-hardcoded.yml" 1

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
