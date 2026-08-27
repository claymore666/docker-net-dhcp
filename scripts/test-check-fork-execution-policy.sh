#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-fork-execution-policy.sh (#830).
#
# THERE IS NO SEAM IN THE GATE, ON PURPOSE. `gh` is stubbed on PATH, so
# every line of the checker -- the API call, the rc test, the shape
# guard, the drift comparison -- runs here exactly as it runs in CI.
# #827 is why: check-attestation-parity took an env-var seam that
# returned the CLASSIFIED verdict, so the classification it existed to
# perform had never executed while the gate scored clean on every axis.
#
# THE STUB IS PREPENDED, NEVER ASSIGNED. The checker shells out to
# mktemp, tr and cut, and this suite invokes it with `bash`; replacing
# PATH exits 127 before the gate runs a line.
#
# THE STUB IS WITNESSED. Every case asserts how many times `gh` was
# called. Without that, a case where PATH did not take reaches the real
# `gh`, makes a live network call, and -- for the refusal cases -- exits
# with precisely the code the test wanted. Measured on #827: three of
# seven cases returned the right exit code having invoked nothing.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-fork-execution-policy.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

STUB="$TMP/bin"; mkdir -p "$STUB"
export GH_CALLS="$TMP/calls" GH_MODE="$TMP/mode"

cat > "$STUB/gh" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GH_CALLS"
field=""
for a in "$@"; do case "$a" in .*) field="${a#.}" ;; esac; done
# fallback-safe: `cat` on a missing path writes nothing to stdout, so the
# fallback REPLACES the value rather than appending a second line to it.
mode="$(cat "$GH_MODE" 2>/dev/null || echo ok)"
case "$mode" in
    # A 403 from a token without Administration: read -- the shape a
    # lapsed or under-scoped SCORECARD_TOKEN actually produces.
    forbidden) echo "gh: Resource not accessible by integration (HTTP 403)" >&2; exit 1 ;;
    # rc 0 with a JSON error object on STDOUT and STDERR EMPTY. Without
    # the shape guard this is reported as DRIFT rather than refused.
    body)      printf '{"message":"Not Found"}\n'; exit 0 ;;
    empty)     exit 0 ;;
    silent)    exit 7 ;;
    *)         v="$(cat "$GH_MODE.$field" 2>/dev/null)"
               [ -n "$v" ] || v="$(printf '%s' "$mode")"
               printf '%s\n' "$v"; exit 0 ;;
esac
STUBEOF
chmod +x "$STUB/gh"

failures=0
n=0

set_values() {
    printf 'ok' > "$GH_MODE"
    printf '%s' "${1:-all_external_contributors}" > "$GH_MODE.approval_policy"
    printf '%s' "${2:-read}"                     > "$GH_MODE.default_workflow_permissions"
    printf '%s' "${3:-false}"                    > "$GH_MODE.can_approve_pull_request_reviews"
}

# run NAME WANT_EXIT WANT_CALLS [SUBSTR...]
run() {
    local name="$1" want="$2" wantcalls="$3"; shift 3
    : > "$GH_CALLS"
    n=$((n + 1))
    PATH="$STUB:$PATH" REPO="owner/name" bash "$CHECK" > "$TMP/out" 2>&1
    local got=$? calls
    # `grep -c` PRINTS 0 AND EXITS 1 on no match, so `|| echo 0` appended
    # a SECOND zero and `calls` became two lines. `[ "0\n0" -ne 0 ]` is
    # not false -- it is an ERROR, exit 2, and inside an `||` chain that
    # reads exactly like the assertion holding. Every zero-call case
    # would print PASS with its call count unchecked. Nothing expected
    # zero calls until the population observer landed, so the defect
    # shipped unreachable. The guard below is kept because an assertion
    # that cannot be evaluated must not be indistinguishable from one
    # that passed.
    calls=$(grep -c . "$GH_CALLS" 2>/dev/null); calls=${calls:-0}
    case "$calls" in
        ''|*[!0-9]*)
            echo "FAIL: $name -- the call counter read '$calls', which is not a number."
            echo "    An assertion that cannot be evaluated is not an assertion that passed."
            failures=$((failures + 1)); return ;;
    esac
    if [ "$got" -ne "$want" ] || [ "$calls" -ne "$wantcalls" ]; then
        echo "FAIL: $name -- want exit $want / $wantcalls call(s), got $got / $calls"
        [ "$calls" -eq 0 ] && echo "    the stub was never invoked -- PATH did not take, so this case tested nothing"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    local missing=""
    for s in "$@"; do grep -F -- "$s" "$TMP/out" >/dev/null || missing="$missing '$s'"; done
    if [ -n "$missing" ]; then
        echo "FAIL: $name -- exit and calls as wanted, output lacks:$missing"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    echo "PASS: $name (exit $got, $calls gh call(s))"
}

# --- the documented state ---------------------------------------------
set_values
run "all three as documented" 0 3 "all 3 settings are as documented"

# --- the enumeration this gate's argument rests on ---------------------
# The header names the workflows that expose the pool to pull requests.
# Until the observer landed, that sentence was checked by nobody -- and
# it was WRONG, claiming five where the tree has two. These cases drive
# the watcher that now stands behind it.
#
# Every case here expects ZERO gh calls: the population is judged before
# any setting is queried, because a header whose premise has moved
# misinforms whatever the settings say.
mkwf() { # DIR -- the two really-exposed workflows
    mkdir -p "$1"
    printf 'name: coverage\non:\n  pull_request:\njobs:\n  c:\n    runs-on: [self-hosted, dhcp-ci]\n    steps: [{run: "true"}]\n' > "$1/coverage.yml"
    printf 'name: integration\non:\n  pull_request:\njobs:\n  i:\n    runs-on: [self-hosted, dhcp-ci]\n    steps: [{run: "true"}]\n' > "$1/integration.yml"
}

# runwf NAME WANT_EXIT WANT_CALLS DIR [SUBSTR...]
runwf() {
    local name="$1" want="$2" wantcalls="$3" dir="$4"; shift 4
    : > "$GH_CALLS"
    n=$((n + 1))
    PATH="$STUB:$PATH" REPO="owner/name" WF_DIR="$dir" bash "$CHECK" > "$TMP/out" 2>&1
    local got=$? calls
    # `grep -c` PRINTS 0 AND EXITS 1 on no match, so `|| echo 0` appended
    # a SECOND zero and `calls` became two lines. `[ "0\n0" -ne 0 ]` is
    # not false -- it is an ERROR, exit 2, and inside an `||` chain that
    # reads exactly like the assertion holding. Every zero-call case
    # would print PASS with its call count unchecked. Nothing expected
    # zero calls until the population observer landed, so the defect
    # shipped unreachable. The guard below is kept because an assertion
    # that cannot be evaluated must not be indistinguishable from one
    # that passed.
    calls=$(grep -c . "$GH_CALLS" 2>/dev/null); calls=${calls:-0}
    case "$calls" in
        ''|*[!0-9]*)
            echo "FAIL: $name -- the call counter read '$calls', which is not a number."
            echo "    An assertion that cannot be evaluated is not an assertion that passed."
            failures=$((failures + 1)); return ;;
    esac
    if [ "$got" -ne "$want" ] || [ "$calls" -ne "$wantcalls" ]; then
        echo "FAIL: $name -- want exit $want / $wantcalls call(s), got $got / $calls"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    local missing=""
    for s in "$@"; do grep -F -- "$s" "$TMP/out" >/dev/null || missing="$missing '$s'"; done
    if [ -n "$missing" ]; then
        echo "FAIL: $name -- exit and calls as wanted, output lacks:$missing"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    echo "PASS: $name (exit $got, $calls gh call(s))"
}

set_values

WFOK="$TMP/wf-ok"; mkwf "$WFOK"
runwf "the declared population is the derived one" 0 3 "$WFOK"

# The failure that was invisible: a THIRD workflow gains a self-hosted
# job on pull_request and the header keeps claiming two.
WF3="$TMP/wf-three"; mkwf "$WF3"
printf 'name: newlane\non:\n  pull_request:\njobs:\n  n:\n    runs-on: [self-hosted, dhcp-ci]\n    steps: [{run: "true"}]\n' > "$WF3/newlane.yml"
runwf "a third exposed workflow refuses before any setting is read" 2 0 "$WF3" \
    "newlane.yml" "has CHANGED"

# And the other direction: the exposure disappearing entirely. That is
# this gate's reason evaporating, which must be said out loud rather
# than passing quietly.
WF0="$TMP/wf-none"; mkdir -p "$WF0"
printf 'name: hosted\non:\n  pull_request:\njobs:\n  h:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' > "$WF0/hosted.yml"
runwf "zero exposed workflows refuses rather than passing" 2 0 "$WF0" "ZERO workflows"

# --- the two false positives that produced the wrong number ------------
# A `pull_request` trigger with every job hosted is NOT exposure. This
# is exactly what coverage-presence, release-backmerge and test are, and
# counting them is how the header came to say five.
WFH="$TMP/wf-hosted"; mkwf "$WFH"
printf 'name: hostedonly\non:\n  pull_request:\njobs:\n  h:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' > "$WFH/hostedonly.yml"
runwf "a pull_request workflow with only hosted jobs is not exposure" 0 3 "$WFH"

# A self-hosted job in a workflow that merely MENTIONS pull_request --
# in an `if:` expression, or in prose -- does not trigger on it. A
# file-wide grep counts it; reading the `on:` block does not.
WFM="$TMP/wf-mention"; mkwf "$WFM"
printf 'name: mentions\n# guarded on github.event.pull_request.head.repo.full_name\non:\n  push:\n    branches: [main]\njobs:\n  m:\n    if: github.event.pull_request.head.repo.full_name == github.repository\n    runs-on: [self-hosted, dhcp-ci]\n    steps: [{run: "true"}]\n' > "$WFM/mentions.yml"
runwf "mentioning pull_request in an if: is not a pull_request trigger" 0 3 "$WFM"

set_values

# --- each setting drifts on its own, and the message names the stakes --
set_values "first_time_contributors"
run "the approval policy is relaxed" 1 3 \
    "approval_policy is 'first_time_contributors'" "runs as root" "1 of 3 settings"

set_values "" "write"
run "the default token becomes writable" 1 3 \
    "default_workflow_permissions is 'write'" "push access"

set_values "" "" "true"
run "Actions may approve pull requests" 1 3 \
    "can_approve_pull_request_reviews is 'true'" "branch protection"

set_values "none" "write" "true"
run "all three drift at once" 1 3 "3 of 3 settings"

# --- CANNOT JUDGE is not a pass and not a failure ---------------------
# A token that cannot read administration settings is the likely
# real-world fault: SCORECARD_TOKEN is a PAT and PATs expire.
printf 'forbidden' > "$GH_MODE"
run "a token without Administration: read refuses" 2 1 \
    "could not read approval_policy" "Nothing was measured"

# THE CASE THE SHAPE GUARD EXISTS FOR. `gh api --jq` prints a 4xx body
# on stdout with stderr empty. Reported as drift, this sends the reader
# to the settings page to fix a setting that was never read.
printf 'body' > "$GH_MODE"
run "a JSON error body refuses rather than reporting drift" 2 1 \
    "not one of the values this setting can hold" "Nothing was measured"

printf 'empty' > "$GH_MODE"
run "an empty answer refuses" 2 1 "could not read approval_policy"

printf 'silent' > "$GH_MODE"
run "gh failing silently refuses and reports rc" 2 1 "printed nothing on either stream"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
