#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for run-evidence.sh (#432).
#
# `gh` is stubbed via PATH so the overlap arithmetic is exercised
# against real JSON shapes without touching the API.
#
# The case that matters most is the last one. The first version of this
# script reported "none — ran alone" for runs older than everything its
# concurrent-run query could see, and it did that against the actual
# v1.4.0 release tree. The answer was reassuring and unsupported, which
# is the precise failure #432 exists to prevent.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_UNDER_TEST="$HERE/run-evidence.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

TREE=aaaa000000000000000000000000000000000000

# make_gh writes a stub whose responses depend on the path requested.
make_gh() {
    local dir="$1" wf_runs="$2" all_runs="$3"
    mkdir -p "$dir/bin"
    cat > "$dir/bin/gh" <<EOF
#!/usr/bin/env bash
args="\$*"
case "\$args" in
  *"workflows/integration.yml/runs"*) cat <<'J'
$wf_runs
J
  ;;
  *"actions/runs?"*) cat <<'J'
$all_runs
J
  ;;
  *"/commits/"*) echo "$TREE" ;;
  *"repo view"*) echo "o/r" ;;
  *) echo "" ;;
esac
EOF
    chmod +x "$dir/bin/gh"
}

run_it() {
    local wf="$1" all="$2"
    local dir; dir=$(mktemp -d)
    make_gh "$dir" "$wf" "$all"
    PATH="$dir/bin:$PATH" GATE_REPO=o/r bash "$SCRIPT_UNDER_TEST" "$TREE" 2>&1
    rm -rf "$dir"
}

# The workflow-runs query is jq-filtered inside the script, so the stub
# returns the already-projected shape.
WF='[{"id":1,"sha":"c1","branch":"dev","status":"completed","conclusion":"success","started":"2026-08-01T12:00:00Z","ended":"2026-08-01T12:10:00Z","attempt":1}]'

# --- genuinely alone --------------------------------------------------
out=$(run_it "$WF" '[{"id":9,"name":"Integration","branch":"other","started":"2026-08-01T11:00:00Z","ended":"2026-08-01T11:05:00Z"}]')
case "$out" in *"none — ran alone"*) ok "a run with no concurrent window reports 'ran alone'" ;;
  *) no "expected 'ran alone', got: $out" ;; esac

# ANCHOR is an old entry whose only job is to push the concurrent-list
# horizon back past the run under test, so the cases below exercise the
# overlap arithmetic rather than the horizon guard. Without it every
# case correctly answers "unknown" — which is how this file first
# failed, and is the guard doing its job.
ANCHOR='{"id":99,"name":"Coverage","branch":"anchor","started":"2026-08-01T09:00:00Z","ended":"2026-08-01T09:01:00Z"}'

# --- genuinely overlapping -------------------------------------------
out=$(run_it "$WF" "[$ANCHOR,{\"id\":9,\"name\":\"Integration\",\"branch\":\"feature/x\",\"started\":\"2026-08-01T12:05:00Z\",\"ended\":\"2026-08-01T12:20:00Z\"}]")
case "$out" in *"Integration[feature/x]"*) ok "an overlapping run is named" ;;
  *) no "expected the overlapping run to be named, got: $out" ;; esac

# --- adjacent is NOT overlapping -------------------------------------
# The v1.4.0 write-up called two adjacent runs concurrent. They were six
# minutes apart. Touching endpoints must not count.
out=$(run_it "$WF" "[$ANCHOR,{\"id\":9,\"name\":\"Integration\",\"branch\":\"adj\",\"started\":\"2026-08-01T12:10:00Z\",\"ended\":\"2026-08-01T12:20:00Z\"}]")
case "$out" in *"none — ran alone"*) ok "a run starting exactly when this one ends is not an overlap" ;;
  *) no "adjacency was counted as overlap: $out" ;; esac

# --- THE ONE THAT MATTERS: horizon shorter than the run --------------
out=$(run_it "$WF" '[{"id":9,"name":"Integration","branch":"later","started":"2026-08-01T18:00:00Z","ended":"2026-08-01T18:10:00Z"}]')
case "$out" in
  *"unknown"*) ok "a run older than the concurrent list reports unknown, not 'ran alone'" ;;
  *"ran alone"*) no "reported 'ran alone' for a run the query could not see — the #432 bug" ;;
  *) no "unexpected: $out" ;;
esac

# --- no runs for the tree --------------------------------------------
out=$(run_it '[]' '[]')
case "$out" in *"no evidence"*) ok "an untested tree says 'no evidence', not 'no failures'" ;;
  *) no "expected the no-evidence wording, got: $out" ;; esac

# --- usage ------------------------------------------------------------
bash "$SCRIPT_UNDER_TEST" >/dev/null 2>&1
[ "$?" = 2 ] && ok "missing tree sha exits 2" || no "missing tree sha did not exit 2"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
