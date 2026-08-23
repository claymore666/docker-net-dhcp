#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-label-taxonomy.sh (#715).
#
# Every rule the gate claims is driven RED here on a fixture that breaks it.
# The three cases marked REGRESSION are the actual defects found on the
# tracker on 2026-08-22 — a duplicate type label with no description, a used
# label that no declaration blessed, and open issues carrying two type labels
# or none. They are reproduced rather than described, because the taxonomy
# had already been repaired once by hand and the prose recording that repair
# did not prevent the second one.
#
# The case marked ORTHOGONALITY is the parser this gate replaced: it asserts
# the OLD regex over-matched the fixture BEFORE asserting the new reader gets
# it right, so the case proves the two differ rather than restating what the
# new one does.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-label-taxonomy.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

# run NAME WANT_EXIT WANT_GREP -- <argv...>   (env passed through the caller)
run() {
    local name="$1" want_exit="$2" want_grep="$3"; shift 3
    [ "${1:-}" = "--" ] && shift
    "$@" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ]; then
        command grep -q -- "$want_grep" "$TMP/out" || ok=0
    fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit / grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# ------------------------------------------------------------ fixtures
GOOD_LABELS="$TMP/labels.yml"
cat > "$GOOD_LABELS" <<'EOF'
# a comment, ignored
- name: bug
  role: type
  description: Something isn't working

- name: ci
  role: type
  description: CI, gates, runners, release plumbing

- name: security
  role: area
  description: 'Area: trust boundaries'

- name: critical
  role: severity
  description: 'Critical: data loss'

- name: code-review
  role: status
  description: Found during a code review pass

- name: backlog
  role: status
  description: Tracked, not scheduled for a release
EOF

GOOD_MAP="$TMP/map.yml"
cat > "$GOOD_MAP" <<'EOF'
bug:
  - '/^fix(\([^)]*\))?!?:/i'

ci:
  - '/^ci(\([^)]*\))?!?:/i'
EOF

# A workflow with MORE indented content after the block. This shape is the
# whole point: it is what the real workflow looks like, and it is what the
# old regex walked straight through.
GOOD_WF="$TMP/workflow.yml"
cat > "$GOOD_WF" <<'EOF'
env:
  ALLOWED_LABELS: |
    bug
    ci
    security

jobs:
  label:
    runs-on: ubuntu-latest
    steps:
      - name: Rule pass
        run: |
          set -euo pipefail
          echo bug
          echo not-a-label
EOF

# ------------------------------------------------------------ static: clean
run "well-formed declaration passes" 0 "Label taxonomy OK (static)" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$GOOD_MAP" "$GOOD_WF"

# ------------------------------------------------- static: usage and inputs
run "no mode is a usage error" 2 "usage:" -- \
    bash "$CHECK" --nonsense
run "missing declaration cannot check" 2 "missing declaration" -- \
    bash "$CHECK" --static "$TMP/absent.yml" "$GOOD_MAP" "$GOOD_WF"
run "missing map cannot check" 2 "missing:" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$TMP/absent.yml" "$GOOD_WF"

# ------------------------------------------------ static: declaration rules
mk() { cp "$GOOD_LABELS" "$1"; }

# REGRESSION: `security` shipped with no description and sat on 13 issues.
NO_DESC="$TMP/no-desc.yml"
cat > "$NO_DESC" <<'EOF'
- name: bug
  role: type
  description: Something isn't working

- name: security
  role: area
EOF
run "REGRESSION a label with no description is red" 1 "has no description" -- \
    bash "$CHECK" --static "$NO_DESC" "$GOOD_MAP" "$GOOD_WF"

NO_ROLE="$TMP/no-role.yml"
printf -- '- name: bug\n  description: x\n' > "$NO_ROLE"
run "a label with no role is red" 1 "has no role" -- \
    bash "$CHECK" --static "$NO_ROLE" "$GOOD_MAP" "$GOOD_WF"

BAD_ROLE="$TMP/bad-role.yml"
printf -- '- name: bug\n  role: kind-of\n  description: x\n' > "$BAD_ROLE"
run "an unknown role is red" 1 "not one of" -- \
    bash "$CHECK" --static "$BAD_ROLE" "$GOOD_MAP" "$GOOD_WF"

DUPE="$TMP/dupe.yml"
cat > "$DUPE" <<'EOF'
- name: bug
  role: type
  description: Something isn't working

- name: bug
  role: area
  description: A second entry for the same name
EOF
run "a name declared twice is red" 1 "declared twice" -- \
    bash "$CHECK" --static "$DUPE" "$GOOD_MAP" "$GOOD_WF"

# The empty-set guard. Without it, "exactly one type label" is satisfied
# vacuously by every issue on the tracker and the live half reports clean.
NO_TYPE="$TMP/no-type.yml"
printf -- '- name: security\n  role: area\n  description: x\n' > "$NO_TYPE"
run "a declaration with no type label is red" 1 "no label has role 'type'" -- \
    bash "$CHECK" --static "$NO_TYPE" "$GOOD_MAP" "$GOOD_WF"

EMPTY="$TMP/empty.yml"
printf '# nothing but a comment\n' > "$EMPTY"
run "an empty declaration is red, not clean" 1 "no labels declared" -- \
    bash "$CHECK" --static "$EMPTY" "$GOOD_MAP" "$GOOD_WF"

# ---------------------------------------------------- static: labeller ties
UNDECLARED_MAP="$TMP/undeclared-map.yml"
printf 'tests:\n  - %s\n' "'/^tests:/i'" > "$UNDECLARED_MAP"
run "a rule label that is not declared is red" 1 "is not declared" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$UNDECLARED_MAP" "$GOOD_WF"

# The labeller must never reach workflow state. `backlog` is a status label:
# an issue title cannot establish that something is descheduled.
STATUS_MAP="$TMP/status-map.yml"
printf 'backlog:\n  - %s\n' "'/^later:/i'" > "$STATUS_MAP"
run "the labeller may not apply a status label" 1 "may only apply" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$STATUS_MAP" "$GOOD_WF"

WF_UNDECLARED="$TMP/wf-undeclared.yml"
cat > "$WF_UNDECLARED" <<'EOF'
env:
  ALLOWED_LABELS: |
    bug
    tests

jobs:
  label:
    runs-on: ubuntu-latest
EOF
run "ALLOWED_LABELS naming an undeclared label is red" 1 "ALLOWED_LABELS names" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$GOOD_MAP" "$WF_UNDECLARED"

WF_NO_BLOCK="$TMP/wf-no-block.yml"
printf 'jobs:\n  label:\n    runs-on: ubuntu-latest\n' > "$WF_NO_BLOCK"
run "a missing ALLOWED_LABELS block is red" 1 "no ALLOWED_LABELS block" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$GOOD_MAP" "$WF_NO_BLOCK"

# ------------------------------------------------------- ORTHOGONALITY
# The gate this one replaced read the block with
#   ALLOWED_LABELS:\s*\|\s*\n((?:\s+\S.*\n)+)
# in which `\s` matches a newline, so `\s+` steps over the blank line that
# ends the block and consumes the rest of the file. Assert the old reader
# over-matches THIS fixture before trusting that the new one does not —
# otherwise the case only restates the new behaviour and would still pass
# against the parser it was written to replace.
old_count=$(python3 - "$GOOD_WF" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
m = re.search(r"^\s*ALLOWED_LABELS:\s*\|\s*\n((?:\s+\S.*\n)+)", text, re.MULTILINE)
print(len({ln.strip() for ln in m.group(1).splitlines() if ln.strip()}) if m else 0)
PY
)
if [ "$old_count" -gt 3 ]; then
    echo "PASS: ORTHOGONALITY the old regex over-matched ($old_count entries, 3 intended)"
else
    echo "FAIL: ORTHOGONALITY the old regex read $old_count entries — fixture no longer distinguishes the two parsers"
    failures=$((failures + 1))
fi
# `not-a-label` lives past the block's end. If the new reader swallowed it
# the way the old one did, it would be reported as undeclared.
run "ORTHOGONALITY the new reader stops at the block" 0 "Label taxonomy OK (static)" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$GOOD_MAP" "$GOOD_WF"

# --------------------------------------------------------------- live
LIVE_OK_LABELS="$TMP/live-labels.tsv"
printf 'bug\tSomething isn'"'"'t working\n'          > "$LIVE_OK_LABELS"
printf 'ci\tCI, gates, runners, release plumbing\n' >> "$LIVE_OK_LABELS"
printf 'security\tArea: trust boundaries\n'         >> "$LIVE_OK_LABELS"
printf 'critical\tCritical: data loss\n'            >> "$LIVE_OK_LABELS"
printf 'code-review\tFound during a code review pass\n' >> "$LIVE_OK_LABELS"
printf 'backlog\tTracked, not scheduled for a release\n' >> "$LIVE_OK_LABELS"

LIVE_OK_ISSUES="$TMP/live-issues.tsv"
printf '1\tbug,security\n'            > "$LIVE_OK_ISSUES"
printf '2\tci\n'                     >> "$LIVE_OK_ISSUES"
printf '3\tbug,code-review,critical\n' >> "$LIVE_OK_ISSUES"

live() {
    local name="$1" want_exit="$2" want_grep="$3" labels="$4" issues="$5"
    REPO=fixture/repo LABELS_TSV="$labels" ISSUES_TSV="$issues" \
        run "$name" "$want_exit" "$want_grep" -- \
        bash "$CHECK" --live "$GOOD_LABELS"
}

live "live: a conforming tracker passes" 0 "Label taxonomy OK (live)" \
    "$LIVE_OK_LABELS" "$LIVE_OK_ISSUES"

# REGRESSION: `tests` was created on the tracker, duplicating `testing`,
# and no file said it was not allowed to exist.
EXTRA="$TMP/live-extra.tsv"; cat "$LIVE_OK_LABELS" > "$EXTRA"; printf 'tests\t\n' >> "$EXTRA"
live "REGRESSION live: an undeclared tracker label is red" 1 "is not declared in" \
    "$EXTRA" "$LIVE_OK_ISSUES"

MISSING="$TMP/live-missing.tsv"; command grep -v '^ci\b' "$LIVE_OK_LABELS" > "$MISSING"
live "live: a declared label absent from the tracker is red" 1 "does not exist on" \
    "$MISSING" "$LIVE_OK_ISSUES"

DESC="$TMP/live-desc.tsv"; sed 's/^bug\t.*/bug\tsomething else entirely/' "$LIVE_OK_LABELS" > "$DESC"
live "live: a drifted description is red" 1 "description differs" \
    "$DESC" "$LIVE_OK_ISSUES"

# REGRESSION: ten open issues carried two type labels, seven carried none.
TWO="$TMP/live-two-types.tsv"; printf '9\tbug,ci\n' > "$TWO"
live "REGRESSION live: two type labels is red" 1 "carries 2 type labels" \
    "$LIVE_OK_LABELS" "$TWO"

NONE="$TMP/live-no-type.tsv"; printf '9\tsecurity\n' > "$NONE"
live "REGRESSION live: no type label is red" 1 "carries no type label" \
    "$LIVE_OK_LABELS" "$NONE"

SEV="$TMP/live-sev.tsv"; printf '9\tbug,critical\n' > "$SEV"
live "live: severity without code-review is red" 1 "without 'code-review'" \
    "$LIVE_OK_LABELS" "$SEV"

# `backlog` with a milestone belongs to check-milestone-scope.sh. This gate
# must stay silent on it, or one defect goes red in two places and the rule
# has two homes.
BACKLOG="$TMP/live-backlog.tsv"; printf '9\tbug,backlog\n' > "$BACKLOG"
live "live: backlog is left to check-milestone-scope.sh" 0 "Label taxonomy OK (live)" \
    "$LIVE_OK_LABELS" "$BACKLOG"

# Absent data is not a zero.
: > "$TMP/empty.tsv"
live "live: an empty label list cannot check" 2 "cannot check" \
    "$TMP/empty.tsv" "$LIVE_OK_ISSUES"
live "live: an empty issue list cannot check" 2 "cannot check" \
    "$LIVE_OK_LABELS" "$TMP/empty.tsv"

echo
if [ "$failures" -ne 0 ]; then
    echo "failed: $failures"
    exit 1
fi
echo "all check-label-taxonomy.sh tests passed"
