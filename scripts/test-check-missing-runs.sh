#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-missing-runs.sh (#418).
#
# `gh` is stubbed via PATH. The cases that matter are the ones where the
# detector cannot see: this exists because a silence was mistaken for
# health, so every unreadable answer must be loud rather than clean.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-missing-runs.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

OLD=$(date -u -d '3 hours ago' +%Y-%m-%dT%H:%M:%SZ)
NEW=$(date -u -d '2 minutes ago' +%Y-%m-%dT%H:%M:%SZ)

# make_gh <dir> <pulls-json> <commit-date> <run-count>
# A run count of "ERR" makes the runs query fail.
make_gh() {
    local dir="$1" pulls="$2" cdate="$3" runs="$4"
    mkdir -p "$dir/bin"
    cat > "$dir/bin/gh" <<EOF
#!/usr/bin/env bash
args="\$*"
case "\$args" in
  *"pulls?state=open"*) cat <<'J'
$pulls
J
  ;;
  *"/commits/"*)
     [ "$cdate" = "ERR" ] && exit 1
     echo "$cdate" ;;
  *"actions/runs?head_sha"*)
     [ "$runs" = "ERR" ] && exit 1
     echo "$runs" ;;
  *"repo view"*) echo "o/r" ;;
  *) echo "" ;;
esac
EOF
    chmod +x "$dir/bin/gh"
}

run_it() {
    local dir; dir=$(mktemp -d)
    make_gh "$dir" "$1" "$2" "$3"
    PATH="$dir/bin:$PATH" GATE_REPO=o/r bash "$CHECK" 20 >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

ONE_PR='[{"number":7,"head":"abc123def456","branch":"feature/x","updated":"2026-08-01T10:00:00Z","draft":false}]'

# --- the happy path ---------------------------------------------------
out=$(run_it "$ONE_PR" "$OLD" "3"); rc=$?
[ "$rc" = 0 ] && ok "a head with runs passes" || no "a head with runs failed (exit $rc): $out"

# --- the incident shape ----------------------------------------------
out=$(run_it "$ONE_PR" "$OLD" "0"); rc=$?
if [ "$rc" = 1 ]; then ok "a head with zero runs fails"; else no "zero runs did not fail (exit $rc)"; fi
case "$out" in *"#7"*) ok "the failure names the PR" ;; *) no "PR not named: $out" ;; esac

# --- inside the grace -------------------------------------------------
# A push a moment ago has legitimately not been picked up yet. Flagging
# it would make the detector cry wolf on every push and get muted.
out=$(run_it "$ONE_PR" "$NEW" "0"); rc=$?
[ "$rc" = 0 ] && ok "a head inside the grace window is not flagged" || no "a fresh push was flagged (exit $rc)"

# --- cannot see: every one of these must be loud ---------------------
out=$(run_it "$ONE_PR" "ERR" "3"); rc=$?
if [ "$rc" = 1 ]; then ok "an unresolvable head commit is reported, not skipped"; else
  no "unresolvable head returned $rc — silence is what this tool exists to catch"; fi

out=$(run_it "$ONE_PR" "$OLD" "ERR"); rc=$?
if [ "$rc" = 1 ]; then ok "an unreadable runs query is reported, not skipped"; else
  no "unreadable runs query returned $rc"; fi

# --- no PRs -----------------------------------------------------------
out=$(run_it '[]' "$OLD" "3"); rc=$?
[ "$rc" = 0 ] && ok "no open PRs is clean" || no "no open PRs returned $rc"

# --- missing tooling --------------------------------------------------
BASH_BIN=$(command -v bash)
PATH="" "$BASH_BIN" "$CHECK" >/dev/null 2>&1
[ "$?" = 2 ] && ok "missing gh/jq exits 2 rather than reporting clean" || no "missing tooling did not exit 2"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
