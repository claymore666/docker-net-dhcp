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
#
# Rule 4 (no orphan gates) carries the same pairs: a gate wired into a
# DIFFERENT workflow is not an orphan, and one invoked as
# `./scripts/check-x.sh` is invoked. Both must pass, or the rule would
# force every gate into one lane and one spelling.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-local-lane.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

# mkgates <name>...
#   Plant fixture gate scripts so rule 4 (no orphan gates) has a
#   directory to read. It is judged by BASENAME, so these need no
#   contents -- what is being asked is whether anything invokes the
#   file, not what the file does.
SDIR=""
mkgates() {
    rm -rf "$DIR/scripts"; mkdir -p "$DIR/scripts"
    local n
    for n in "$@"; do : > "$DIR/scripts/$n"; done
    SDIR="$DIR/scripts"
}

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
mkgates check-a.sh check-b.sh
mkwf "$WF" "bash scripts/check-a.sh" "bash scripts/check-b.sh"
mklane "$LANE" "scripts/check-a.sh,scripts/check-b.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a lane covering every invoked script passes" || no "in-sync failed (rc=$rc: $out)"

# --- the #636 shape: a gate added to CI with no lane entry -------------
mkgates check-a.sh check-new.sh
mkwf "$WF" "bash scripts/check-a.sh" "bash scripts/check-new.sh"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a workflow gate with no lane entry fails" || no "the drift shape returned $rc (: $out)"
case "$out" in *check-new.sh*) ok "the failure names the uncovered script" ;;
  *) no "the failure does not name the script: $out" ;; esac

# --- a declaration is an acceptable answer ----------------------------
mklane "$LANE" "scripts/check-a.sh" "scripts/check-new.sh:needs a pull-request body"
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "declaring it out of lane with a reason satisfies the gate" \
               || no "a declared exemption still failed (rc=$rc: $out)"

# --- but not an empty one ---------------------------------------------
mklane "$LANE" "scripts/check-a.sh" "scripts/check-new.sh: "
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "an exemption with no reason fails" || no "an empty reason returned $rc (: $out)"

# --- contradiction -----------------------------------------------------
mkgates check-a.sh
mkwf "$WF" "bash scripts/check-a.sh"
mklane "$LANE" "scripts/check-a.sh" "scripts/check-a.sh:cannot run locally"
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a script both run and declared out of lane fails" || no "the contradiction returned $rc (: $out)"

# --- stale declaration -------------------------------------------------
mkgates check-a.sh
mkwf "$WF" "bash scripts/check-a.sh"
mklane "$LANE" "scripts/check-a.sh" "scripts/check-gone.sh:needed the network"
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "an exemption for a script the workflow no longer runs fails" \
               || no "a stale exemption returned $rc (: $out)"

# --- the lane may check MORE than CI ----------------------------------
mkgates check-a.sh
mkwf "$WF" "bash scripts/check-a.sh"
mklane "$LANE" "scripts/check-a.sh,scripts/check-extra.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a lane entry CI does not run is allowed, not an error" \
               || no "the lane was blocked from checking more than CI (rc=$rc: $out)"
case "$out" in *NOTE*check-extra.sh*) ok "the extra entry is reported rather than hidden" ;;
  *) no "the extra lane entry was silent: $out" ;; esac

# --- scope: self-tests are #542's, not this gate's --------------------
mkgates check-a.sh
mkwf "$WF" "bash scripts/check-a.sh" "bash scripts/test-something.sh"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "an invoked test-*.sh needs no lane entry (discovered, not listed)" \
               || no "a test-*.sh was demanded (rc=$rc: $out)"

# --- scope: a comment is not an invocation ----------------------------
mkgates check-a.sh
mkwf "$WF" "bash scripts/check-a.sh"
printf '      # see scripts/check-mentioned-only.sh for why\n' >> "$WF"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a script named only in a comment needs no lane entry" \
               || no "a commented mention was treated as an invocation (rc=$rc: $out)"

# --- 4. orphan gates ---------------------------------------------------
# The direction rules 1-3 structurally cannot see. All three start from
# what the workflow invokes, so a gate NO workflow invokes is in
# neither list and nothing reports anything: it exists, it passes when
# run by hand, and it protects nothing. check-plugin-set-order.sh
# shipped in exactly that state, and check-fuzz-budget.sh had been in
# it for longer -- named only in a COMMENT, which rule 1 excludes by
# design (#724).
mkgates check-a.sh check-orphan.sh
mkwf "$WF" "bash scripts/check-a.sh"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a gate no workflow runs fails" \
               || no "an orphan gate returned $rc (: $out)"
case "$out" in *check-orphan.sh*) ok "the failure names the orphan" ;;
  *) no "the failure does not name the orphan: $out" ;; esac

# A comment is not an invocation here either — that is the state
# check-fuzz-budget.sh was actually in, and reading it as covered would
# make this rule agree with the bug.
mkgates check-a.sh check-orphan.sh
mkwf "$WF" "bash scripts/check-a.sh"
printf '      # scripts/check-orphan.sh keeps this shape from regressing\n' >> "$WF"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a gate named only in a comment is still an orphan" \
               || no "a commented mention counted as wiring (rc=$rc: $out)"

# Declaring it out of lane is an acceptable answer here too: the
# declaration is a decision, and a decision with a reason is what this
# gate asks for everywhere else.
mkgates check-a.sh check-orphan.sh
mkwf "$WF" "bash scripts/check-a.sh" "bash scripts/check-orphan.sh"
mklane "$LANE" "scripts/check-a.sh" "scripts/check-orphan.sh:needs a live registry"
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "an orphan declared out of lane with a reason passes" \
               || no "a declared gate was still called an orphan (rc=$rc: $out)"

# Wired into a DIFFERENT workflow is wired. A gate that runs only in
# release.yml or on a schedule is not an orphan, and demanding it be in
# test.yaml would push every gate into one lane.
mkgates check-a.sh check-elsewhere.sh
mkwf "$WF" "bash scripts/check-a.sh"
mkwf "$DIR/other.yaml" "bash scripts/check-elsewhere.sh"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a gate wired into another workflow is not an orphan" \
               || no "a gate in a sibling workflow was called an orphan (rc=$rc: $out)"
rm -f "$DIR/other.yaml"

# Judged by basename, so the prefix a caller happens to write is not
# part of the question.
mkgates check-a.sh check-pathy.sh
mkwf "$WF" "bash scripts/check-a.sh" './scripts/check-pathy.sh --static'
mklane "$LANE" "scripts/check-a.sh,scripts/check-pathy.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a gate invoked as ./scripts/... counts as invoked" \
               || no "the invocation form changed the verdict (rc=$rc: $out)"

# An empty scripts directory reads nothing, and nothing must not
# compare clean.
mkgates
mkwf "$WF" "bash scripts/check-a.sh"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$WF" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a scripts directory with no check-*.sh exits 2, not clean" \
               || no "an empty scripts dir returned $rc (: $out)"

# --- cannot see: every one of these must be loud ----------------------
printf 'jobs:\n  test:\n    steps: []\n' > "$DIR/empty.yaml"
mklane "$LANE" "scripts/check-a.sh" ""
out=$(bash "$CHECK" "$DIR/empty.yaml" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a workflow with no script invocations exits 2, not clean" \
               || no "an empty workflow returned $rc — comparing two empty sets is not a result"

mkgates check-a.sh
mkwf "$WF" "bash scripts/check-a.sh"
printf '#!/usr/bin/env bash\nexit 0\n' > "$DIR/silent.sh"
out=$(bash "$CHECK" "$WF" "$DIR/silent.sh" "$SDIR" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a lane script that lists nothing exits 2" || no "a silent lane returned $rc"

out=$(bash "$CHECK" "$DIR/nope.yaml" "$LANE" "$SDIR" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing workflow exits 2" || no "a missing workflow returned $rc"

# --- the repository itself ---------------------------------------------
out=$(bash "$CHECK" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "the repository's own lane is in sync with test.yaml" \
               || no "the repo's lane has drifted (rc=$rc: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
