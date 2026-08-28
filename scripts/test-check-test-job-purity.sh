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
# GOOD_TEST carries EVERY member of the gate's ALLOWED set, and every
# fixture below is derived from it by exactly one change. That is not
# tidiness: arm B is two-directional, so a fixture that merely omits
# four allowed steps reports six findings and any case built on it would
# pass for a reason it was not testing. A control must move one variable.
#
# The last two blocks drive the ABSENCE: they delete what the gate
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

# A case that must NOT report a given finding. Used for the bounds this
# gate deliberately does not close: a bound nobody drives is a sentence
# in a comment, and this file exists because sentences decay.
#
# It takes the expected exit code as well as the absent string, because
# an absence alone has no direction: a refusal (exit 2) prints neither
# the finding nor anything else, and would score as a pass.
refutes() {
    local name="$1" want_exit="$2" wf="$3" absent="$4"
    bash "$GATE" "$wf" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && ! grep -qF -- "$absent" "$TMP/out"; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want_exit; or found $absent)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# Every fixture below is built with a bash pattern substitution, and a
# substitution whose pattern MISSES yields the original string in
# silence. That is how a case passes for a reason it was not testing --
# most dangerously the expect-PASS cases, which a base fixture satisfies
# trivially. So each derived fixture is asserted to differ from the base
# it was derived from, before it is used.
derived() {
    local name="$1" base="$2" variant="$3"
    if [ "$base" = "$variant" ]; then
        echo "FAIL: fixture $name did not apply -- its pattern matched nothing"
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

# Every member of ALLOWED, so that arm B's missing-direction stays quiet
# in every case that is not about arm B.
GOOD_TEST="$(cat <<'EOF'
      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1
      - uses: actions/setup-go@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb # v7.0.0
      - name: go.mod tidy check
        run: go mod tidy -diff
      - name: Build
        run: go build ./...
      - name: Vet
        run: go vet ./...
      - name: Format check
        run: gofmt -l .
      - name: Test (with race detector)
        run: go test -race -count=1 ./...
      - name: Fuzz (short)
        run: |
          for target in FuzzBuildEvent; do
            go test ./pkg/dhcp/ -fuzz "^${target}$" -fuzztime 200000x
          done
EOF
)"$'\n'

GOOD_GATES="$(cat <<'EOF'
      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1
      - name: Lock discipline
        run: bash scripts/check-lock-discipline.sh
EOF
)"$'\n'

# --- the clean case, and the pin bump that must not disturb it --------
check "a clean split passes" 0 "$(wf clean "$GOOD_TEST" "$GOOD_GATES")" "PASS"

BUMPED=${GOOD_TEST//aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/cccccccccccccccccccccccccccccccccccccccc}
derived BUMPED "$GOOD_TEST" "$BUMPED"
check "a version pin bump is not a finding" 0 "$(wf bumped "$BUMPED" "$GOOD_GATES")" "PASS"

# --- B: an unrecognised step in test ----------------------------------
UNKNOWN=$GOOD_TEST$'      - name: Pi watchdog wiring\n        run: true\n'
derived UNKNOWN "$GOOD_TEST" "$UNKNOWN"
check "an unrecognised step in test is a finding" 1 "$(wf unknown "$UNKNOWN" "$GOOD_GATES")" "unrecognised step"

UNNAMED=$GOOD_TEST$'      - run: true\n'
derived UNNAMED "$GOOD_TEST" "$UNNAMED"
check "an unnamed run step in test is a finding" 1 "$(wf unnamed "$UNNAMED" "$GOOD_GATES")" "<unnamed run step>"

# A step entry that is not a mapping at all -- a stray `-`. This used to
# crash the parser and reach the caller as a REFUSAL: fail-closed, so
# nothing got through, but the wrong verdict. It is a step in `test`,
# so it is a finding.
NONMAPPING=$GOOD_TEST$'      -\n'
derived NONMAPPING "$GOOD_TEST" "$NONMAPPING"
check "a non-mapping step in test is a finding, not a refusal" 1 "$(wf nonmapping "$NONMAPPING" "$GOOD_GATES")" "non-mapping step"

# B in the OTHER direction. Only checking for strangers lets `test` be
# emptied one deletion at a time, and every deletion leaves a required
# check whose NAME is unchanged. Measured before this arm existed: a
# `test` reduced to a checkout plus `go test -race ./...` passed.
SHRUNK=${GOOD_TEST/      - name: Vet$'\n'        run: go vet .\/...$'\n'/}
derived SHRUNK "$GOOD_TEST" "$SHRUNK"
check "a known step deleted from test is a finding" 1 "$(wf shrunk "$SHRUNK" "$GOOD_GATES")" "'Vet' is GONE"

# --- C: a gate script invoked from test -------------------------------
DRIFTED=${GOOD_TEST/go build .\/.../bash scripts/check-lock-discipline.sh}
derived DRIFTED "$GOOD_TEST" "$DRIFTED"
check "a gate script invoked from test is a finding" 1 "$(wf drifted "$DRIFTED" "$GOOD_GATES")" "invokes scripts/check-lock-discipline.sh"

# C is deliberately the BROAD predicate, unlike D and E: for C a match
# is a finding, so over-matching costs a false red, which is loud. A
# script merely NAMED in a live line of `test` is still a finding.
R_CMENTION='echo "scripts/check-lock-discipline.sh"'
CMENTION=${GOOD_TEST/go build .\/.../$R_CMENTION}
derived CMENTION "$GOOD_TEST" "$CMENTION"
check "a gate script merely named on a live line of test is a finding" 1 "$(wf cmention "$CMENTION" "$GOOD_GATES")" "invokes scripts/check-lock-discipline.sh"

# The neighbouring shape: the SAME script name, in a comment. This
# workflow's own prose names check-fuzz-budget.sh, so a gate that
# counted comments would report the file's explanation of itself.
COMMENTED=${GOOD_TEST/        run: go build .\/.../        run: |
          # see scripts/check-lock-discipline.sh for why
          go build ./...}
derived COMMENTED "$GOOD_TEST" "$COMMENTED"
check "a comment naming a gate script is not a finding" 0 "$(wf commented "$COMMENTED" "$GOOD_GATES")" "PASS"

# --- D: test emptied of its suite -------------------------------------
#
# The predicate is COMMAND POSITION, not the substring `go test`. The
# substring version was defeated by a one-line echo -- measured on the
# review of #834 -- and it certified a required check named `test`
# running no suite at all. Every case below keeps all eight ALLOWED
# steps, so only arm D moves.
R_NOSUITE='        run: echo "we no longer go test in CI"'
NOSUITE=${GOOD_TEST/        run: go test -race -count=1 .\/.../$R_NOSUITE}
R_NOFUZZ='            echo "go test used to run here"'
NOSUITE=${NOSUITE/            go test .\/pkg\/dhcp\/ -fuzz \"^\$\{target\}\$\" -fuzztime 200000x/$R_NOFUZZ}
derived NOSUITE "$GOOD_TEST" "$NOSUITE"
check "a test job that runs no suite is a finding" 1 "$(wf nosuite "$NOSUITE" "$GOOD_GATES")" "no step RUNS"

# The pinned decoy, stated as its own case because it is the shape that
# actually got past the first version: prose that MENTIONS go test.
refutes "the no-suite fixture really is prose, not a stray invocation" 1 \
    "$(wf nosuite "$NOSUITE" "$GOOD_GATES")" "unrecognised step"

# The other direction, and the reason the anchor allows leading
# whitespace: the only surviving invocation is INDENTED inside a loop,
# exactly as `Fuzz (short)` writes it in the real workflow. This must
# still pass, or the anchor has traded one false verdict for another.
R_LOOPONLY='        run: echo "the race step moved"'
LOOPONLY=${GOOD_TEST/        run: go test -race -count=1 .\/.../$R_LOOPONLY}
derived LOOPONLY "$GOOD_TEST" "$LOOPONLY"
check "an indented go test inside a loop still satisfies D" 0 "$(wf looponly "$LOOPONLY" "$GOOD_GATES")" "PASS"

# --- E: policy-gates emptied of its corpus -----------------------------------
NOCORPUS=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - name: Provide PyYAML\n        run: python3 -c "import yaml"\n'
check "a policy-gates job invoking no script is a finding" 1 "$(wf nocorpus "$GOOD_TEST" "$NOCORPUS")" "has been emptied"

# A policy-gates job whose only script is in a COMMENT is the same emptiness
# wearing prose, and must be caught by the same arm.
PROSECORPUS=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - name: Provide PyYAML\n        run: |\n          # scripts/check-lock-discipline.sh runs elsewhere\n          python3 -c "import yaml"\n'
check "a policy-gates job whose corpus is only named in prose is a finding" 1 "$(wf prosecorpus "$GOOD_TEST" "$PROSECORPUS")" "has been emptied"

# The decoy that DEFEATED the first version of arm E: the same sentence
# on a live line rather than in a comment. A comment is one spelling of
# prose; an echo is another, and enumerating one of two means the third
# is live. Measured passing before the command-position anchor landed.
ECHOCORPUS=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - name: Nothing\n        run: echo "scripts/check-lock-discipline.sh runs elsewhere"\n'
check "a policy-gates job whose corpus is only echoed is a finding" 1 "$(wf echocorpus "$GOOD_TEST" "$ECHOCORPUS")" "has been emptied"

# The preservation control for that anchor: the invocation forms this
# repository actually uses, and the two neighbouring spellings, must all
# still count. An anchor that rejected `./scripts/x.sh` would have made
# arm E fire on a job that is carrying its corpus.
FORMS=$'      - uses: actions/checkout@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # v7.0.1\n      - name: Forms\n        run: |\n          bash scripts/check-lock-discipline.sh\n          ./scripts/check-go-pins.sh\n          sh scripts/check-docs-drift.sh --static\n          scripts/check-option-docs.sh\n'
check "every invocation form this repo uses counts as wiring" 0 "$(wf forms "$GOOD_TEST" "$FORMS")" "4 distinct gate script(s)"

# --- F: the job may not swallow its own result ------------------------
#
# D and E ask whether the work is written down. F asks whether its
# failure can still turn the check red. All four shapes below were
# MEASURED passing before this arm existed.
JOBIF="$(printf 'name: Test\non:\n  pull_request:\njobs:\n  test:\n    if: false\n    runs-on: ubuntu-latest\n    steps:\n%s  policy-gates:\n    runs-on: ubuntu-latest\n    steps:\n%s' "$GOOD_TEST" "$GOOD_GATES" > "$TMP/jobif.yaml"; echo "$TMP/jobif.yaml")"
check "an if: on the test job is a finding" 1 "$JOBIF" "the job carries an \`if:\`"

STEPIF=${GOOD_TEST/      - name: Vet$'\n'/      - name: Vet
        if: false
}
derived STEPIF "$GOOD_TEST" "$STEPIF"
check "an if: on a step of test is a finding" 1 "$(wf stepif "$STEPIF" "$GOOD_GATES")" "carries an \`if:\`"

MATRIX="$(printf 'name: Test\non:\n  pull_request:\njobs:\n  test:\n    strategy:\n      matrix:\n        go: ["1.27.0"]\n    runs-on: ubuntu-latest\n    steps:\n%s  policy-gates:\n    runs-on: ubuntu-latest\n    steps:\n%s' "$GOOD_TEST" "$GOOD_GATES" > "$TMP/matrix.yaml"; echo "$TMP/matrix.yaml")"
check "a strategy matrix on the test job is a finding" 1 "$MATRIX" "has a \`strategy:\`"

JOBCOE="$(printf 'name: Test\non:\n  pull_request:\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n%s  policy-gates:\n    continue-on-error: true\n    runs-on: ubuntu-latest\n    steps:\n%s' "$GOOD_TEST" "$GOOD_GATES" > "$TMP/jobcoe.yaml"; echo "$TMP/jobcoe.yaml")"
check "continue-on-error on the policy-gates job is a finding" 1 "$JOBCOE" "the job sets \`continue-on-error\`"

STEPCOE=${GOOD_TEST/        run: go test -race -count=1 .\/.../        run: go test -race -count=1 ./...
        continue-on-error: true}
derived STEPCOE "$GOOD_TEST" "$STEPCOE"
check "continue-on-error on the suite step is a finding" 1 "$(wf stepcoe "$STEPCOE" "$GOOD_GATES")" "cannot turn the check red"

GATESCOE=${GOOD_GATES/        run: bash scripts\/check-lock-discipline.sh/        run: bash scripts/check-lock-discipline.sh
        continue-on-error: true}
derived GATESCOE "$GOOD_GATES" "$GATESCOE"
check "continue-on-error on a gate step is a finding" 1 "$(wf gatescoe "$GOOD_TEST" "$GATESCOE")" "cannot turn the check red"

ORTRUE=${GOOD_TEST/        run: go test -race -count=1 .\/.../        run: go test -race -count=1 ./... || true}
derived ORTRUE "$GOOD_TEST" "$ORTRUE"
check "|| true on the suite step is a finding" 1 "$(wf ortrue "$ORTRUE" "$GOOD_GATES")" "discards an exit status"

# A block scalar, because `run: x || :` is not valid YAML -- the
# trailing `: ` reads as a mapping. The gate REFUSED that fixture rather
# than passing it, which is the right direction, but a refusal is not
# the verdict this case is about.
R_ORCOLON='        run: |
          bash scripts/check-lock-discipline.sh || :'
ORCOLON=${GOOD_GATES/        run: bash scripts\/check-lock-discipline.sh/$R_ORCOLON}
derived ORCOLON "$GOOD_GATES" "$ORCOLON"
check "|| : in a gate step is a finding" 1 "$(wf orcolon "$GOOD_TEST" "$ORCOLON")" "\`|| :\`"

SETPLUSE=${GOOD_GATES/        run: bash scripts\/check-lock-discipline.sh/        run: |
          set +e
          bash scripts/check-lock-discipline.sh}
derived SETPLUSE "$GOOD_GATES" "$SETPLUSE"
check "set +e in a gate step is a finding" 1 "$(wf setpluse "$GOOD_TEST" "$SETPLUSE")" "discards an exit status"

# F's other direction. `continue-on-error: false` says the opposite of
# what the arm is looking for and must not fire, and an ordinary run
# line that merely contains the word `true` is not a swallow.
COEFALSE=${GOOD_TEST/        run: go test -race -count=1 .\/.../        run: go test -race -count=1 ./...
        continue-on-error: false}
derived COEFALSE "$GOOD_TEST" "$COEFALSE"
check "continue-on-error: false is not a finding" 0 "$(wf coefalse "$COEFALSE" "$GOOD_GATES")" "PASS"

R_WORDTRUE='        run: go vet ./... \&\& echo "true"'
WORDTRUE=${GOOD_TEST/        run: go vet .\/.../$R_WORDTRUE}
derived WORDTRUE "$GOOD_TEST" "$WORDTRUE"
check "the word true on a run line is not a swallow" 0 "$(wf wordtrue "$WORDTRUE" "$GOOD_GATES")" "PASS"

# A DECLARED BOUND, pinned as a case so it cannot decay into a comment
# nobody drives. `|| echo` discards an exit status exactly as `|| true`
# does, and F does not catch it: the three spellings F names are the
# ones with no legitimate use in these two jobs, and a wider net would
# red-flag ordinary shell in a future gate step. This case asserts
# today's KNOWN GAP, not that the gap is correct.
R_ORECHO='        run: bash scripts/check-lock-discipline.sh || echo "failed"'
ORECHO=${GOOD_GATES/        run: bash scripts\/check-lock-discipline.sh/$R_ORECHO}
derived ORECHO "$GOOD_GATES" "$ORECHO"
refutes "KNOWN GAP: || echo is not caught by F" 0 "$(wf orecho "$GOOD_TEST" "$ORECHO")" "discards an exit status"

# The same, for indirection: a `make` target inside an ALLOWED step runs
# whatever the Makefile says and no arm here can see it.
MAKEIND=${GOOD_TEST/        run: go build .\/.../        run: |
          go build ./...
          make check-all-policy-gates}
derived MAKEIND "$GOOD_TEST" "$MAKEIND"
refutes "KNOWN GAP: a make target inside an allowed step is invisible to C" 0 "$(wf makeind "$MAKEIND" "$GOOD_GATES")" "FINDING"

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
