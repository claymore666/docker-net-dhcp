#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-milestone-scope.sh, driven through the
# MS_GH seam against a stub `gh`. Nothing here touches the network.
#
# The cases that matter most are the 2s. This gate exists because a
# claim about live tracker state decays silently, so a version of it
# that answers 0 when it cannot actually look would reproduce the
# original bug with extra steps. "The API returned nothing" must never
# be read as "no issue is mislabelled" — absent data is not a zero.
#
# The second group that matters is the STALE/NOTDONE split. The two have
# opposite fixes — drop the label vs. move the milestone — so a gate
# that merely detected "backlog and a milestone" would send half its
# readers the wrong way. Each case therefore asserts on the message, not
# only the exit code.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-milestone-scope.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# A stub gh. $STUB_OUT is what `gh issue list` prints; $STUB_RC its exit.
#
# `${STUB_OUT-[]}` and NOT `${STUB_OUT:-[]}`: the colon form substitutes
# the default for an EMPTY value as well as an unset one, which would
# make the "the API answered with nothing" case silently test "[]" — the
# precise distinction that case exists to check.
cat > "$TMP/gh" <<'EOF'
#!/usr/bin/env bash
[ "${STUB_RC:-0}" -ne 0 ] && exit "${STUB_RC}"
printf '%s' "${STUB_OUT-[]}"
EOF
chmod +x "$TMP/gh"

# issue <number> <milestone|null> <label>... -> one JSON object
issue() {
    local n="$1" ms="$2"; shift 2
    python3 - "$n" "$ms" "$@" <<'PY'
import json, sys
n, ms, *labels = sys.argv[1:]
print(json.dumps({
    "number": int(n),
    "title": f"issue {n}",
    "milestone": None if ms == "null" else {"title": ms},
    "labels": [{"name": l} for l in labels],
}))
PY
}
arr() { printf '[%s]' "$(printf '%s,' "$@" | sed 's/,$//')"; }

# run <STUB_OUT> <STUB_RC> -> sets RC and OUT (stdout+stderr merged)
run() {
    OUT=$(STUB_OUT="$1" STUB_RC="$2" MS_GH="$TMP/gh" bash "$GATE" 2>&1)
    RC=$?
}

# --- clean -----------------------------------------------------------

run "$(arr)" 0
[ "$RC" -eq 0 ] && ok "no backlog issues at all -> 0" || no "empty list should pass (rc=$RC)"

run "$(arr "$(issue 1 null backlog)" "$(issue 2 null backlog ci)")" 0
[ "$RC" -eq 0 ] && ok "backlog without a milestone -> 0" || no "unmilestoned backlog should pass (rc=$RC)"

# --- NOTDONE: the release would close unfinished work -----------------

run "$(arr "$(issue 396 v1.6.0 backlog ci)")" 0
if [ "$RC" -eq 1 ] && grep -q '#396' <<<"$OUT" && grep -qi 'close them as delivered' <<<"$OUT"; then
    ok "backlog + milestone, no in-dev -> 1, names the issue and the consequence"
else
    no "unfinished milestoned issue not reported correctly (rc=$RC): $OUT"
fi

if grep -qi 'Move them off the milestone' <<<"$OUT"; then
    ok "NOTDONE names the right fix (move the milestone)"
else
    no "NOTDONE did not name the fix: $OUT"
fi

if grep -qi 'stale' <<<"$OUT"; then
    no "NOTDONE wrongly reported as a stale label: $OUT"
else
    ok "NOTDONE is not reported as a stale label"
fi

# --- STALE: the work shipped, the label did not ----------------------

run "$(arr "$(issue 486 v1.6.0 backlog testing in-dev)")" 0
if [ "$RC" -eq 1 ] && grep -q '#486' <<<"$OUT" && grep -qi 'Drop "backlog"' <<<"$OUT"; then
    ok "backlog + milestone + in-dev -> 1, names the right fix (drop the label)"
else
    no "stale-label case not reported correctly (rc=$RC): $OUT"
fi

if grep -qi 'Move them off the milestone' <<<"$OUT"; then
    no "STALE wrongly told the reader to move the milestone: $OUT"
else
    ok "STALE does not tell the reader to move the milestone"
fi

# --- both at once, each in its own bucket ----------------------------

run "$(arr "$(issue 396 v1.6.0 backlog ci)" "$(issue 486 v1.6.0 backlog in-dev)")" 0
if [ "$RC" -eq 1 ] \
   && grep -qi 'close them as delivered' <<<"$OUT" \
   && grep -qi 'Drop "backlog"' <<<"$OUT"; then
    ok "mixed input reports both kinds"
else
    no "mixed input did not report both kinds (rc=$RC): $OUT"
fi

# The bucketing must be per-issue, not per-run: #396 under NOTDONE and
# #486 under STALE. A gate that classified the whole batch by its first
# row would pass every assertion above and still mislead.
notdone_block=$(sed -n '/close them as delivered/,/Drop "backlog"/p' <<<"$OUT")
if grep -q '#396' <<<"$notdone_block" && ! grep -q '#486' <<<"$notdone_block"; then
    ok "each issue lands in its own bucket"
else
    no "issues were not bucketed individually: $OUT"
fi

# --- an in-dev backlog issue with NO milestone is not this gate's ----

run "$(arr "$(issue 500 null backlog in-dev)")" 0
[ "$RC" -eq 0 ] && ok "backlog + in-dev without a milestone -> 0" \
                || no "unmilestoned in-dev issue should not fail this gate (rc=$RC): $OUT"

# --- cannot see ------------------------------------------------------

run "" 1
[ "$RC" -eq 2 ] && ok "gh failure -> 2" || no "gh failure should be 2, got $RC"

run "" 0
if [ "$RC" -eq 2 ]; then
    ok "empty response -> 2 (absent data is not a zero)"
else
    no "an empty API response must not read as a clean tracker (rc=$RC): $OUT"
fi

run "not json at all" 0
[ "$RC" -eq 2 ] && ok "unparseable response -> 2" || no "unparseable should be 2, got $RC"

run '{"number":1}' 0
[ "$RC" -eq 2 ] && ok "JSON that is not a list -> 2" || no "non-list JSON should be 2, got $RC"

OUT=$(MS_GH="$TMP/definitely-not-here" bash "$GATE" 2>&1); RC=$?
[ "$RC" -eq 2 ] && ok "missing gh -> 2" || no "missing gh should be 2, got $RC"

# --- the label names are seams, and must actually be honoured --------

OUT=$(STUB_OUT="$(arr "$(issue 7 v9.9.9 later shipped)")" STUB_RC=0 \
      MS_GH="$TMP/gh" MS_BACKLOG=later MS_INDEV=shipped bash "$GATE" 2>&1); RC=$?
if [ "$RC" -eq 1 ] && grep -qi 'Drop "later"' <<<"$OUT"; then
    ok "MS_BACKLOG/MS_INDEV are honoured, including in the message"
else
    no "renamed labels not honoured (rc=$RC): $OUT"
fi

# --- the gate must actually be RUN by something ----------------------
#
# run-gate-selftests.sh discovers this file, so the self-test wires
# itself. The GATE does not: it is only ever executed because a workflow
# names it. A gate that nothing invokes is indistinguishable from a
# passing one, which is the failure this whole file is about.
wired=$(grep -rl 'check-milestone-scope\.sh' "$HERE/../.github/workflows" 2>/dev/null)
if [ -n "$wired" ]; then
    ok "a workflow runs the gate ($(basename "$wired" | tr '\n' ' '))"
else
    no "no workflow in .github/workflows runs check-milestone-scope.sh — the gate would never execute"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
