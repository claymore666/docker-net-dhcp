#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-issue-label-map.sh (#393). The gate's own
# failure modes are the point: a gate that cannot fail is not a gate, and
# this one guards a file (the rule map) whose breakage is otherwise
# invisible — a dead pattern just silently pushes issues onto the model
# pass. Each case feeds a synthetic map/workflow/fixture and asserts the
# verdict.
set -u

CHECK="$(dirname "$0")/check-issue-label-map.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A workflow stub carrying just the block the gate reads out of it.
WF="$TMP/workflow.yml"
cat > "$WF" <<'EOF'
env:
  ALLOWED_LABELS: |
    bug
    ci
    testing

jobs:
  label:
    runs-on: ubuntu-latest
EOF

# A workflow whose allowlist block is missing entirely.
WF_NO_LIST="$TMP/workflow-no-list.yml"
cat > "$WF_NO_LIST" <<'EOF'
jobs:
  label:
    runs-on: ubuntu-latest
EOF

GOOD_MAP="$TMP/good-map.yml"
cat > "$GOOD_MAP" <<'EOF'
# comment lines are ignored
bug:
  - '/^fix(\([^)]*\))?!?:/i'

ci:
  - '/^ci(\([^)]*\))?!?:/i'
EOF

GOOD_FIXTURE="$TMP/good-fixture.tsv"
printf 'fix(plugin): a real bug\tbug\n'          > "$GOOD_FIXTURE"
printf 'ci: a workflow change\tci\n'            >> "$GOOD_FIXTURE"
printf 'fixing things generally\t-\n'           >> "$GOOD_FIXTURE"

failures=0
# check NAME WANT_EXIT MAP WORKFLOW FIXTURE GREP_PATTERN
check() {
    local name="$1" want_exit="$2" map="$3" wf="$4" fixture="$5" want_grep="$6"
    bash "$CHECK" "$map" "$wf" "$fixture" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

check "well-formed map passes" 0 \
    "$GOOD_MAP" "$WF" "$GOOD_FIXTURE" "Issue label map OK"

# A pattern that does not compile. Unbalanced group.
BAD_REGEX="$TMP/bad-regex.yml"
cat > "$BAD_REGEX" <<'EOF'
bug:
  - '/^fix(\([^)]*\)?!?:/i'
EOF
check "uncompilable regex fails" 1 \
    "$BAD_REGEX" "$WF" "$GOOD_FIXTURE" "bad regex"

# A label the model pass has never heard of.
UNKNOWN_LABEL="$TMP/unknown-label.yml"
cat > "$UNKNOWN_LABEL" <<'EOF'
wontfix:
  - '/^fix:/i'
EOF
check "label outside ALLOWED_LABELS fails" 1 \
    "$UNKNOWN_LABEL" "$WF" "$GOOD_FIXTURE" "not in ALLOWED_LABELS"

# The regression this gate exists for: a pattern that stops matching what
# the fixture says it must.
DRIFTED="$TMP/drifted.yml"
cat > "$DRIFTED" <<'EOF'
bug:
  - '/^bugfix:/i'
ci:
  - '/^ci(\([^)]*\))?!?:/i'
EOF
check "pattern that no longer matches the fixture fails" 1 \
    "$DRIFTED" "$WF" "$GOOD_FIXTURE" "want \[bug\]"

# A label with no patterns under it applies to nothing — dead config.
EMPTY_LABEL="$TMP/empty-label.yml"
cat > "$EMPTY_LABEL" <<'EOF'
bug:
ci:
  - '/^ci:/i'
EOF
check "label with no patterns fails" 1 \
    "$EMPTY_LABEL" "$WF" "$GOOD_FIXTURE" "has no patterns"

# Flags that would change matching semantics are refused rather than
# silently approximated by the Python side of the gate.
BAD_FLAG="$TMP/bad-flag.yml"
cat > "$BAD_FLAG" <<'EOF'
bug:
  - '/^fix:/s'
EOF
check "unsupported regex flag fails" 1 \
    "$BAD_FLAG" "$WF" "$GOOD_FIXTURE" "not supported"

check "workflow without an allowlist fails" 1 \
    "$GOOD_MAP" "$WF_NO_LIST" "$GOOD_FIXTURE" "no ALLOWED_LABELS block"

# Malformed fixture row — two columns are required.
BAD_FIXTURE="$TMP/bad-fixture.tsv"
printf 'no tab on this row\n' > "$BAD_FIXTURE"
check "fixture row without a tab fails" 1 \
    "$GOOD_MAP" "$WF" "$BAD_FIXTURE" "want"

check "missing map is a usage error" 2 \
    "$TMP/nope.yml" "$WF" "$GOOD_FIXTURE" "missing"

# The allowlist block is read by INDENTATION, not by a regex (#715).
#
# The reader here used to be
#   ALLOWED_LABELS:\s*\|\s*\n((?:\s+\S.*\n)+)
# in which `\s` matches a newline, so `\s+` steps over the blank line that
# ends the block and consumes every indented line to the end of the file. On
# the real workflow that returned 108 entries where 8 were meant.
#
# It hid because this gate only ever asked "is every rule label IN the
# allowlist?" — a subset test, which a polluted superset can only make pass
# more easily. A guard fails in one direction, and nothing had ever asked
# this one the question that exposes it.
#
# The fixture is that exact shape: `testing` is NOT in the allowlist, but the
# word appears on its own line inside a later `run:` block. Under the old
# reader it was accepted as an allowlist entry and the case passed; the check
# below therefore goes red on the old parser and green on the new one.
WF_TRAP="$TMP/workflow-trap.yml"
cat > "$WF_TRAP" <<'EOF'
env:
  ALLOWED_LABELS: |
    bug
    ci

jobs:
  label:
    steps:
      - name: A step whose body mentions a label name
        run: |
          echo picking a lane
          testing
EOF
TRAP_MAP="$TMP/trap-map.yml"
cat > "$TRAP_MAP" <<'EOF'
testing:
  - '/^tests?(\([^)]*\))?!?:/i'
EOF
TRAP_FIXTURE="$TMP/trap-fixture.tsv"
printf 'test(harness): a fixture change\ttesting\n' > "$TRAP_FIXTURE"
check "a label named past the allowlist block is not in the allowlist" 1 \
    "$TRAP_MAP" "$WF_TRAP" "$TRAP_FIXTURE" "not in ALLOWED_LABELS"

if [ "$failures" -ne 0 ]; then
    echo "$failures test(s) failed" >&2
    exit 1
fi
echo "All check-issue-label-map.sh tests passed."
