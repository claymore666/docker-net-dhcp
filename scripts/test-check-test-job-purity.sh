#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-test-job-purity.sh (#829), through the workflow
# argument against synthetic workflows.
#
# Every arm is driven in BOTH directions: the shape that must be caught,
# and beside it the neighbouring shape that must still pass. A gate with
# one possible verdict is not a gate, and the cheapest way to ship one
# here would be a refusal path that swallows every input -- so each
# refusal case is paired with the input that differs from it by exactly
# the thing being tested.
#
# The last two cases drive the ABSENCE: they delete what the gate
# guards and what invokes the gate, and check that something goes red.
set -u

GATE="$(dirname "$0")/check-test-job-purity.sh"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
failures=0

check() {
    local name="$1" want_exit="$2" wf="$3" want_grep="$4"
    bash "$GATE" "$wf" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && { [ -z "$want_grep" ] || grep -qF -- "$want_grep" "$TMP/out"; }; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want_exit)"; sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# wf NAME <test-steps-block> <policy-gates-steps-block> -> a workflow path
wf() {
    local f="$TMP/$1.yaml"
    {
        printf 'name: Test\non:\n  pull_request:\njobs:\n'
        printf '  test:\n    runs-on: ubuntu-latest\n    steps:\n'
        printf '%s' "$2"
        printf '  policy-gates:\n    runs-on: ubuntu-latest\n    steps:\n'
        printf '%s' "$3"
    } > "$f"
    echo "$f"
}

GOOD_TEST=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - uses: actions/setup-go@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb # v7.0.0\n      - name: Build\n        run: go build ./...\n      - name: Test (with race detector)\n        run: go test -race -count=1 ./...\n'
GOOD_GATES=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - name: Lock discipline\n        run: bash scripts/check-lock-discipline.sh\n'

# --- the clean case, and the pin bump that must not disturb it --------
check "a clean split passes" 0 "$(wf clean "$GOOD_TEST" "$GOOD_GATES")" "PASS"

BUMPED=${GOOD_TEST//aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/cccccccccccccccccccccccccccccccccccccccc}
check "a version pin bump is not a finding" 0 "$(wf bumped "$BUMPED" "$GOOD_GATES")" "PASS"

# --- B: an unrecognised step in test ----------------------------------
UNKNOWN=$GOOD_TEST$'      - name: Pi watchdog wiring\n        run: true\n'
check "an unrecognised step in test is a finding" 1 "$(wf unknown "$UNKNOWN" "$GOOD_GATES")" "unrecognised step"

UNNAMED=$GOOD_TEST$'      - run: true\n'
check "an unnamed run step in test is a finding" 1 "$(wf unnamed "$UNNAMED" "$GOOD_GATES")" "<unnamed run step>"

# --- C: a gate script invoked from test -------------------------------
DRIFTED=${GOOD_TEST/go build .\/.../bash scripts/check-lock-discipline.sh}
check "a gate script invoked from test is a finding" 1 "$(wf drifted "$DRIFTED" "$GOOD_GATES")" "invokes scripts/check-lock-discipline.sh"

# The neighbouring shape: the SAME script name, in a comment. This
# workflow's own prose names check-fuzz-budget.sh, so a gate that
# counted comments would report the file's explanation of itself.
COMMENTED=${GOOD_TEST/        run: go build .\/.../        run: |
          # see scripts/check-lock-discipline.sh for why
          go build ./...}
check "a comment naming a gate script is not a finding" 0 "$(wf commented "$COMMENTED" "$GOOD_GATES")" "PASS"

# --- D: test emptied of its suite -------------------------------------
NOSUITE=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - name: Build\n        run: go build ./...\n'
check "a test job that runs no suite is a finding" 1 "$(wf nosuite "$NOSUITE" "$GOOD_GATES")" "no step runs"

# --- E: policy-gates emptied of its corpus -----------------------------------
NOCORPUS=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - name: Provide PyYAML\n        run: python3 -c "import yaml"\n'
check "a policy-gates job invoking no script is a finding" 1 "$(wf nocorpus "$GOOD_TEST" "$NOCORPUS")" "has been emptied"

# A policy-gates job whose only script is in a COMMENT is the same emptiness
# wearing prose, and must be caught by the same arm.
PROSECORPUS=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - name: Provide PyYAML\n        run: |\n          # scripts/check-lock-discipline.sh runs elsewhere\n          python3 -c "import yaml"\n'
check "a policy-gates job whose corpus is only named in prose is a finding" 1 "$(wf prosecorpus "$GOOD_TEST" "$PROSECORPUS")" "has been emptied"

# --- refusals: every one must be 2, never 0 ---------------------------
check "a missing workflow refuses" 2 "$TMP/nowhere.yaml" "does not exist"

printf 'jobs:\n  test:\n   - [\n' > "$TMP/broken.yaml"
check "an unparsable workflow refuses" 2 "$TMP/broken.yaml" "could not be parsed"

printf -- '- a\n- b\n' > "$TMP/notmap.yaml"
check "a workflow that is not a mapping refuses" 2 "$TMP/notmap.yaml" "could not be parsed"

printf 'name: Test\non:\n  pull_request:\n' > "$TMP/nojobs.yaml"
check "a workflow with no jobs refuses" 2 "$TMP/nojobs.yaml" "could not be parsed"

{
    printf 'name: Test\non:\n  pull_request:\njobs:\n'
    printf '  policy-gates:\n    runs-on: ubuntu-latest\n    steps:\n%s' "$GOOD_GATES"
} > "$TMP/notest.yaml"
check "a workflow with no test job refuses" 2 "$TMP/notest.yaml" "could not be parsed"

{
    printf 'name: Test\non:\n  pull_request:\njobs:\n'
    printf '  test:\n    runs-on: ubuntu-latest\n    steps:\n%s' "$GOOD_TEST"
} > "$TMP/nogates.yaml"
check "a workflow with no policy-gates job refuses" 2 "$TMP/nogates.yaml" "could not be parsed"

{
    printf 'name: Test\non:\n  pull_request:\njobs:\n'
    printf '  test:\n    runs-on: ubuntu-latest\n    steps: []\n'
    printf '  policy-gates:\n    runs-on: ubuntu-latest\n    steps:\n%s' "$GOOD_GATES"
} > "$TMP/emptysteps.yaml"
check "a test job with no steps refuses rather than passes" 2 "$TMP/emptysteps.yaml" "could not be parsed"

# An unreadable file must refuse, not default to clean. Skipped under
# root, where chmod 000 does not deny -- a case that cannot be driven is
# declared, never quietly counted as a pass.
UNREADABLE="$TMP/unreadable.yaml"
cp "$(wf src "$GOOD_TEST" "$GOOD_GATES")" "$UNREADABLE"
chmod 000 "$UNREADABLE"
if [ "$(id -u)" -eq 0 ]; then
    echo "PASS: an unreadable workflow refuses (declared undrivable as root)"
else
    check "an unreadable workflow refuses" 2 "$UNREADABLE" "not readable"
fi
chmod 644 "$UNREADABLE"

# --- the real tree ----------------------------------------------------
check "the repository's own workflow passes" 0 "$REPO/.github/workflows/test.yaml" "PASS"

# --- drive the ABSENCE ------------------------------------------------
#
# 1. Delete the boundary the gate guards -- fold every gate step back
#    into `test`, which is exactly the state #829 was filed about -- and
#    confirm the gate goes red against the REAL workflow rather than a
#    fixture. If this passes, the gate is decorative.
python3 - "$REPO/.github/workflows/test.yaml" "$TMP/folded.yaml" <<'FOLD'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
doc["jobs"]["test"]["steps"] += doc["jobs"]["policy-gates"]["steps"]
yaml.safe_dump(doc, open(sys.argv[2], "w"))
FOLD
check "folding the corpus back into test goes red" 1 "$TMP/folded.yaml" "invokes scripts/"

# 2. Delete the INVOCATION. A gate nothing runs reports nothing, so the
#    wiring is asserted structurally: the script must appear in COMMAND
#    POSITION on a non-comment run line of the `policy-gates` job.
#
#    Command position is not pedantry. The first draft of this case
#    grepped the run lines for the script's name, and a mutant that
#    replaced the invocation with `echo "scripts/check-test-job-purity.sh
#    runs elsewhere"` SURVIVED it -- measured. That is #871's defect
#    reproduced inside the gate whose own header warns about it, and
#    scripts/workflow-shell-lines.sh names the same boundary in as many
#    words: it answers "is this token in something the workflow runs",
#    not "is this token the command being run".
#
#    So the assertion is driven against four workflows, not one: the
#    real tree, and three that NAME the script without running it.
wired() {
    local name="$1" want="$2" wf="$3"
    python3 - "$wf" <<'WIRED'
import re
import sys
import yaml

# The invocation forms this repository actually uses for a gate, anchored
# at the start of the command. Anything else -- an echo argument, a here
# document, a quoted string -- names the script without running it.
INVOKE = re.compile(
    r"^\s*(?:(?:bash|sh)\s+)?(?:\./)?scripts/check-test-job-purity\.sh(?:\s|$)"
)
doc = yaml.safe_load(open(sys.argv[1]))
for step in doc.get("jobs", {}).get("policy-gates", {}).get("steps", []) or []:
    body = step.get("run")
    if not isinstance(body, str):
        continue
    for ln in body.splitlines():
        if ln.strip().startswith("#"):
            continue
        if INVOKE.search(ln):
            sys.exit(0)
sys.exit(1)
WIRED
    local got=$?
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want)"
        failures=$((failures + 1))
    fi
}

REAL="$REPO/.github/workflows/test.yaml"
wired "gate is invoked from a run line of the policy-gates job" 0 "$REAL"

python3 - "$REAL" "$TMP" <<'DECOYS'
import sys
import yaml

real, tmp = sys.argv[1], sys.argv[2]
base = yaml.safe_load(open(real))


def rewrite(job_steps, replacement):
    for step in job_steps:
        body = step.get("run")
        if isinstance(body, str) and "scripts/check-test-job-purity.sh" in body:
            step["run"] = replacement
            return True
    return False


decoys = {
    # the mutant that survived the first draft
    "echo": 'echo "scripts/check-test-job-purity.sh runs elsewhere"',
    # the shape run-gate-selftests.sh was defeated by (#871)
    "comment": "# scripts/check-test-job-purity.sh\ntrue",
    # a name with no shell behind it at all
    "nameonly": "true",
}
for tag, repl in decoys.items():
    doc = yaml.safe_load(open(real))
    assert rewrite(doc["jobs"]["policy-gates"]["steps"], repl), tag
    yaml.safe_dump(doc, open("%s/decoy-%s.yaml" % (tmp, tag), "w"))
DECOYS

wired "an echo naming the gate does not count as wiring" 1 "$TMP/decoy-echo.yaml"
wired "a comment naming the gate does not count as wiring" 1 "$TMP/decoy-comment.yaml"
wired "a step that only names the gate does not count as wiring" 1 "$TMP/decoy-nameonly.yaml"

if [ "$failures" -ne 0 ]; then echo "$failures failure(s)"; exit 1; fi
echo "all passed"
