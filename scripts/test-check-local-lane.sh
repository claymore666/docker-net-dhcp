#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-local-lane.sh (#636).
#
# Both inputs are generated — a fake workflow and a fake lane script that
# answers --list / --list-exempt — so the cases keep their meaning as the
# real lane grows. Pointing them at the repo's own files would make every
# case restate today's gate list.
#
# The cases that keep the rest honest are the ones where the gate must
# NOT fire: a `test-*.sh` is out of scope by design (#542 discovers those,
# listing them here would maintain the same set twice), a script named
# only in a COMMENT is not an invocation, and a lane that checks MORE than
# CI is allowed. Without those, a gate that demanded an entry for every
# string matching `scripts/*.sh` would still pass every failure case here.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-local-lane.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

# mkwf <file> <run-line>...
mkwf() {
    local f="$1"; shift
    {
        printf 'jobs:\n  test:\n    steps:\n'
        local r
        for r in "$@"; do printf '      - name: step\n        run: %s\n' "$r"; done
    } > "$f"
}

# mklane <file> <lane-csv> <exempt-csv "script:reason,...">
mklane() {
    local f="$1" lane="$2" ex="$3"
    {
        echo '#!/usr/bin/env bash'
        echo 'case "${1:-}" in'
        echo '  --list)'
        local s
        for s in ${lane//,/ }; do [ -n "$s" ] && echo "    echo $s"; done
        echo '    ;;'
        echo '  --list-exempt)'
        local e
        IFS=',' read -ra parts <<< "$ex"
        for e in "${parts[@]}"; do
            [ -n "$e" ] || continue
            printf '    printf "%%s\\t%%s\\n" "%s" "%s"\n' "${e%%:*}" "${e#*:}"
        done
        echo '    ;;'
        echo 'esac'
    } > "$f"
}

WF="$DIR/wf.yaml"; LANE="$DIR/lane.sh"

# --- in sync -----------------------------------------------------------
mkwf "$WF" "bash scripts/check-a.sh" "bash scripts/check-b.sh"
mklane "$LANE" "scripts/check-a.sh,scripts/check-b.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a lane covering every invoked script passes" || no "in-sync failed (rc=$rc: $out)"

# --- the #636 shape: a gate added to CI with no lane entry -------------
mkwf "$WF" "bash scripts/check-a.sh" "bash scripts/check-new.sh"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a workflow gate with no lane entry fails" || no "the drift shape returned $rc (: $out)"
case "$out" in *check-new.sh*) ok "the failure names the uncovered script" ;;
  *) no "the failure does not name the script: $out" ;; esac

# --- a declaration is an acceptable answer ----------------------------
mklane "$LANE" "scripts/check-a.sh" "scripts/check-new.sh:needs a pull-request body"
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "declaring it out of lane with a reason satisfies the gate" \
               || no "a declared exemption still failed (rc=$rc: $out)"

# --- but not an empty one ---------------------------------------------
mklane "$LANE" "scripts/check-a.sh" "scripts/check-new.sh: "
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "an exemption with no reason fails" || no "an empty reason returned $rc (: $out)"

# --- contradiction -----------------------------------------------------
mkwf "$WF" "bash scripts/check-a.sh"
mklane "$LANE" "scripts/check-a.sh" "scripts/check-a.sh:cannot run locally"
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a script both run and declared out of lane fails" || no "the contradiction returned $rc (: $out)"

# --- stale declaration -------------------------------------------------
mkwf "$WF" "bash scripts/check-a.sh"
mklane "$LANE" "scripts/check-a.sh" "scripts/check-gone.sh:needed the network"
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "an exemption for a script the workflow no longer runs fails" \
               || no "a stale exemption returned $rc (: $out)"

# --- the lane may check MORE than CI ----------------------------------
mkwf "$WF" "bash scripts/check-a.sh"
mklane "$LANE" "scripts/check-a.sh,scripts/check-extra.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a lane entry CI does not run is allowed, not an error" \
               || no "the lane was blocked from checking more than CI (rc=$rc: $out)"
case "$out" in *NOTE*check-extra.sh*) ok "the extra entry is reported rather than hidden" ;;
  *) no "the extra lane entry was silent: $out" ;; esac

# --- scope: self-tests are #542's, not this gate's --------------------
mkwf "$WF" "bash scripts/check-a.sh" "bash scripts/test-something.sh"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "an invoked test-*.sh needs no lane entry (discovered, not listed)" \
               || no "a test-*.sh was demanded (rc=$rc: $out)"

# --- scope: a comment is not an invocation ----------------------------
mkwf "$WF" "bash scripts/check-a.sh"
printf '      # see scripts/check-mentioned-only.sh for why\n' >> "$WF"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a script named only in a comment needs no lane entry" \
               || no "a commented mention was treated as an invocation (rc=$rc: $out)"

# --- cannot see: every one of these must be loud ----------------------
printf 'jobs:\n  test:\n    steps: []\n' > "$DIR/empty.yaml"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$DIR/empty.yaml" "$LANE" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a workflow with no script invocations exits 2, not clean" \
               || no "an empty workflow returned $rc — comparing two empty sets is not a result"

mkwf "$WF" "bash scripts/check-a.sh"
printf '#!/usr/bin/env bash\nexit 0\n' > "$DIR/silent.sh"
out=$(bash "$CHECK" "$WF" "$DIR/silent.sh" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a lane script that lists nothing exits 2" || no "a silent lane returned $rc"

out=$(bash "$CHECK" "$DIR/nope.yaml" "$LANE" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing workflow exits 2" || no "a missing workflow returned $rc"

# --- the repository itself ---------------------------------------------
out=$(bash "$CHECK" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "the repository's own lane is in sync with test.yaml" \
               || no "the repo's lane has drifted (rc=$rc: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
