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
    calls=$(grep -c . "$GH_CALLS" 2>/dev/null || echo 0)
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
