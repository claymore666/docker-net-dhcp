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
#
# MUTATION PASS over arms F(`name:`) and G, through .claude/bin/mutate.sh,
# 12 mutants: 11 killed, 1 declared survivor. Recorded here because a
# survivor nobody wrote down is rediscovered at full price.
#
#   SURVIVOR: F's `name:` check deleted AND G's exclusion of the two
#   purity jobs dropped, in one change. It survives because it is
#   EQUIVALENT, not because a case is missing -- G's predicate is the
#   same `!= jname`, so the property simply moves arms. MEASURED on the
#   real workflow with both jobs' names swapped: the composed mutant
#   still reports both jobs; deleting F's check ALONE reports neither
#   and the suite kills it. Those two mutants differ by exactly one
#   line, which is what makes the survivor an adjudication rather than
#   a shrug.
#
#   It is deliberately NOT closed. Closing it means asserting WHICH arm
#   produced a finding, which couples these cases to the gate's shape
#   instead of to its property -- and the property is what a future
#   refactor must be allowed to move. The reason the exclusion exists
#   at all is pinned separately, by the JOB_IF buy-back case below.
set -u

GATE="$(dirname "$0")/check-test-job-purity.sh"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
failures=0
# Every verdict below increments this, and the run refuses at the end if
# it comes in under FLOOR. A suite that reports "all passed" having run
# nothing is the same absent-check-reads-as-green failure the gate it
# tests was written about: a universal is satisfied by emptying its
# domain, so the domain is measured rather than assumed.
cases=0
FLOOR=68

check() {
    local name="$1" want_exit="$2" wf="$3" want_grep="$4"
    cases=$((cases + 1))
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
    cases=$((cases + 1))
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
    cases=$((cases + 1))
    echo "PASS: an unreadable workflow refuses (declared undrivable as root)"
else
    check "an unreadable workflow refuses" 2 "$UNREADABLE" "not readable"
fi
chmod 644 "$UNREADABLE"

# --- F: the job `name:` key, and the SWAP -------------------------------
#
# `wf` hardcodes both job headers, so a job-level key cannot be
# expressed through it -- and a job-level key is the whole subject here.
# `jobkey` inserts one, and REFUSES unless its anchor matched exactly
# once, for the same reason `derived` exists: a substitution that misses
# yields the original in silence, and an expect-PASS case built on it
# would pass while asserting nothing.
jobkey() {   # jobkey OUTNAME SRC ANCHOR-LINE TEXT  -> writes $TMP/OUTNAME.yaml
    local out="$TMP/$1.yaml" src="$2" anchor="$3" text="$4"
    if ! python3 - "$src" "$out" "$anchor" "$text" <<'JOBKEY'
import sys
src, out, anchor, text = sys.argv[1:5]
lines = open(src).read().split("\n")
res, hit = [], 0
for ln in lines:
    res.append(ln)
    if ln == anchor:
        hit += 1
        res.extend(text.split("\n"))
if hit != 1:
    sys.stderr.write("anchor %r matched %d times\n" % (anchor, hit))
    sys.exit(1)
open(out, "w").write("\n".join(res))
JOBKEY
    then
        echo "FAIL: fixture $1 could not be built -- its anchor did not match exactly once"
        failures=$((failures + 1))
    fi
}

GOODWF="$(wf namebase "$GOOD_TEST" "$GOOD_GATES")"

# Direction 1: the one-line rename. The required context `test` stops
# existing; an ABSENT required check is quieter than a red one.
jobkey namerename "$GOODWF" '  test:' '    name: Unit tests'
check "a name: on the test job is a finding" 1 "$TMP/namerename.yaml" \
    "the job sets \`name:"

# Direction 2, and the one that matters: the SWAP. Both required
# contexts still exist and both are still green -- they have simply
# traded jobs, so `test` now reports on the gate corpus and the Go suite
# reports under a name nothing requires. MEASURED before this arm
# existed: the gate's output on this workflow was BYTE-IDENTICAL to the
# real one's, tally included. A case covering only direction 1 leaves
# the inversion open, which is why both are here.
jobkey nameswap0 "$GOODWF" '  test:' '    name: policy-gates'
jobkey nameswap "$TMP/nameswap0.yaml" '  policy-gates:' '    name: test'
check "the test/policy-gates name SWAP is a finding on BOTH jobs" 1 \
    "$TMP/nameswap.yaml" "policy-gates: the job sets \`name: 'test'\`"
check "the swap is also a finding on the test job" 1 \
    "$TMP/nameswap.yaml" "test: the job sets \`name: 'policy-gates'\`"

# Direction 3: an expression. A third spelling exists because two were
# enumerated. It is compared as the literal string it is written as --
# a finding, not an evaluation, which is the safe direction.
# shellcheck disable=SC2016  # the literal ${{ }} is the fixture's point
jobkey nameexpr "$GOODWF" '  test:' '    name: ${{ github.event_name }}'
check "a name: built from an expression is a finding" 1 \
    "$TMP/nameexpr.yaml" "the job sets \`name:"

# The preservation control. `name:` equal to the job key is a no-op and
# must NOT be a finding, or the arm is keyed on the key's presence
# rather than on the rename.
jobkey namesame "$GOODWF" '  test:' '    name: test'
refutes "a name: equal to the job key is not a finding" 0 \
    "$TMP/namesame.yaml" "the job sets \`name:"

# --- G: every OTHER job in this workflow --------------------------------
#
# Built from the REAL workflow, not a fixture: G is about the five jobs
# `wf` does not model, and `attribution` -- a required context carrying
# the very key F forbids -- is the reason this arm exists.
G_REAL="$REPO/.github/workflows/test.yaml"

jobkey gname "$G_REAL" '  staticcheck:' '    name: Static analysis'
check "a name: on a non-purity job is a finding" 1 "$TMP/gname.yaml" \
    "staticcheck: the job sets \`name:"

jobkey gmatrix "$G_REAL" '  govulncheck:' '    strategy:
      matrix:
        go: ["1.27.0"]'
check "a strategy: on a non-purity job is a finding" 1 "$TMP/gmatrix.yaml" \
    "govulncheck: the job has a \`strategy:\`"

jobkey gcoe "$G_REAL" '  package:' '    continue-on-error: true'
check "continue-on-error on a non-purity job is a finding" 1 "$TMP/gcoe.yaml" \
    "package: the job sets \`continue-on-error\`"

# The one STEP-level key G closes for the other jobs, driven on the job
# that made arm G necessary. `attribution` is a required context, and a
# `continue-on-error` on the step that does its work leaves the check
# green while the work failed -- the #829 failure at step scope.
python3 - "$G_REAL" "$TMP/gstepcoe.yaml" <<'GSTEPCOE'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
doc["jobs"]["attribution"]["steps"][-1]["continue-on-error"] = True
yaml.safe_dump(doc, open(sys.argv[2], "w"))
GSTEPCOE
check "step-level continue-on-error on a non-purity job is a finding" 1 \
    "$TMP/gstepcoe.yaml" "attribution: step"

# Its preservation control, and the bound beside it. A step-level `if:`
# is NOT closed for these jobs -- `if: failure()` diagnostics are
# ordinary -- and neither is a swallow on a run line: MEASURED at this
# head, the govulncheck job legitimately writes `... > vulns.txt || true`
# and adjudicates the status in a later step. An arm closing either would
# have fired on correct code the day it landed.
python3 - "$G_REAL" "$TMP/gstepbound.yaml" <<'GSTEPBOUND'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
doc["jobs"]["attribution"]["steps"][-1]["if"] = "failure()"
yaml.safe_dump(doc, open(sys.argv[2], "w"))
GSTEPBOUND
refutes "BOUND: a step-level if: on a non-purity job is not a finding" 0 \
    "$TMP/gstepbound.yaml" "FINDING"

jobkey gif "$G_REAL" '  staticcheck:' "    if: github.ref == 'refs/heads/never'"
check "an unrecorded job-level if: is a finding" 1 "$TMP/gif.yaml" \
    "an \`if:\` this gate does not record"

# The recorded entry, driven in both directions. A changed condition and
# a REMOVED condition are both findings: removal is the safe direction,
# and the friction is deliberate, because a JOB_IF entry that outlives
# its job is a standing permission nobody re-granted.
sed "s|    if: github.event_name == 'pull_request'|    if: false|" \
    "$G_REAL" > "$TMP/gifchanged.yaml"
if cmp -s "$G_REAL" "$TMP/gifchanged.yaml"; then
    echo "FAIL: fixture gifchanged did not apply -- its pattern matched nothing"
    failures=$((failures + 1))
fi
check "a CHANGED recorded if: is a finding" 1 "$TMP/gifchanged.yaml" \
    "and this gate records"

grep -v "^    if: github.event_name == 'pull_request'\$" "$G_REAL" \
    > "$TMP/gifgone.yaml"
if cmp -s "$G_REAL" "$TMP/gifgone.yaml"; then
    echo "FAIL: fixture gifgone did not apply -- its pattern matched nothing"
    failures=$((failures + 1))
fi
check "a recorded if: that is GONE is a finding" 1 "$TMP/gifgone.yaml" \
    "records an \`if:\` for a job that no longer has one"

# The stale-entry direction the GATE cannot carry: an entry whose JOB is
# gone. The gate is handed arbitrary workflows and every synthetic
# fixture here legitimately lacks `attribution`, so flagging an absent
# job there red-flagged the whole suite (measured this round). JOB_IF
# describes ONE file, so the assertion belongs against that file --
# here, reading the record out of the gate rather than restating it,
# because a second copy of an enumeration is how the two drift apart.
if python3 - "$REPO/scripts/check-test-job-purity.sh" "$G_REAL" <<'JOBIF'
import ast
import re
import sys
import yaml

src = open(sys.argv[1]).read()
m = re.search(r"^JOB_IF = (\{.*?^\})", src, re.S | re.M)
if not m:
    sys.stderr.write("JOB_IF is not where this assertion expects it\n")
    sys.exit(1)
record = ast.literal_eval(m.group(1))
if not record:
    sys.stderr.write("JOB_IF is empty -- this assertion would be vacuous\n")
    sys.exit(1)
jobs = yaml.safe_load(open(sys.argv[2]))["jobs"]
for jname in sorted(record):
    if jname not in jobs:
        sys.stderr.write(
            "JOB_IF records an `if:` for job %r, which is not in the real "
            "workflow. A permission that outlives its job is waved through "
            "if a job by that name comes back.\n" % jname
        )
        sys.exit(1)
JOBIF
then
    cases=$((cases + 1))
    echo "PASS: every JOB_IF entry names a job the real workflow still has"
else
    cases=$((cases + 1))
    echo "FAIL: a JOB_IF entry outlived its job"
    failures=$((failures + 1))
fi

# Every JOB_IF entry must be LOAD-BEARING, driven ONE AT A TIME. An
# allowlist nobody drives becomes a list of things nobody checks: if the
# real workflow still passes with an entry removed, that entry is buying
# nothing, and the permission it records was never the reason the check
# is green. Removing all of them at once would not show this -- one
# finding would cover every entry -- so each is lifted alone and the
# finding must NAME that job.
#
# This drives the GATE, not a fixture, so it needs its own copy: the
# gate `cd`s to its own parent, and the workflow is passed absolute.
mkdir -p "$TMP/lift/scripts"
LIFT_GATE="$TMP/lift/scripts/check-test-job-purity.sh"
JOBIF_KEYS="$(python3 - "$REPO/scripts/check-test-job-purity.sh" <<'KEYS'
import ast
import re
import sys

m = re.search(r"^JOB_IF = (\{.*?^\})", open(sys.argv[1]).read(), re.S | re.M)
if not m:
    sys.exit(1)
for k in sorted(ast.literal_eval(m.group(1))):
    print(k)
KEYS
)"
if [ -z "$JOBIF_KEYS" ]; then
    cases=$((cases + 1))
    echo "FAIL: JOB_IF is empty or unreadable, so the load-bearing drive is vacuous"
    failures=$((failures + 1))
fi
for k in $JOBIF_KEYS; do
    cases=$((cases + 1))
    if ! python3 - "$REPO/scripts/check-test-job-purity.sh" "$LIFT_GATE" "$k" <<'LIFT'
import ast
import re
import sys

src = open(sys.argv[1]).read()
m = re.search(r"^JOB_IF = (\{.*?^\})", src, re.S | re.M)
if not m:
    sys.stderr.write("JOB_IF is not where this drive expects it\n")
    sys.exit(1)
record = ast.literal_eval(m.group(1))
del record[sys.argv[3]]
body = "".join("    %r: %r,\n" % (a, b) for a, b in sorted(record.items()))
out = src[: m.start(1)] + "{\n" + body + "}" + src[m.end(1) :]
if out == src:
    sys.stderr.write("lifting %r changed nothing\n" % sys.argv[3])
    sys.exit(1)
open(sys.argv[2], "w").write(out)
LIFT
    then
        echo "FAIL: could not lift the JOB_IF entry for $k"
        failures=$((failures + 1))
        continue
    fi
    bash "$LIFT_GATE" "$G_REAL" > "$TMP/out" 2>&1
    got=$?
    if [ "$got" -eq 1 ] && grep -qF "$k: the job carries an \`if:\`" "$TMP/out"; then
        echo "PASS: the JOB_IF entry for $k is load-bearing"
    else
        echo "FAIL: lifting the JOB_IF entry for $k left the real workflow at exit $got"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
done

# JOB_IF must not be a way to BUY BACK an `if:` on a purity job. The
# header says so; nothing drove it until this case, and it was found by
# asking what a COMPOSED mutant could do: F's `if:` check and G's
# exclusion of the two purity jobs are separate sites, and if the
# exclusion were dropped, a JOB_IF entry naming `test` would make the
# very key F forbids into a recorded permission. F is unconditional
# today, so the entry buys nothing -- and that is the fact being pinned,
# because it is the reason the exclusion can be removed by accident
# without anything going red.
mkdir -p "$TMP/buyback/scripts"
BUYBACK_GATE="$TMP/buyback/scripts/check-test-job-purity.sh"
if ! python3 - "$REPO/scripts/check-test-job-purity.sh" "$BUYBACK_GATE" <<'BUYBACK'
import re
import sys

src = open(sys.argv[1]).read()
m = re.search(r"^JOB_IF = (\{.*?^\})", src, re.S | re.M)
if not m:
    sys.stderr.write("JOB_IF is not where this drive expects it\n")
    sys.exit(1)
entry = "{\n    'test': False,\n}"
out = src[: m.start(1)] + entry + src[m.end(1) :]
if out == src:
    sys.exit(1)
open(sys.argv[2], "w").write(out)
BUYBACK
then
    cases=$((cases + 1))
    echo "FAIL: could not build the JOB_IF buy-back gate"
    failures=$((failures + 1))
else
    cases=$((cases + 1))
    bash "$BUYBACK_GATE" "$JOBIF" > "$TMP/out" 2>&1
    got=$?
    # shellcheck disable=SC2016  # the backticks are the gate's own prose
    if [ "$got" -eq 1 ] && grep -qF 'test: the job carries an `if:`' "$TMP/out"; then
        echo "PASS: a JOB_IF entry cannot buy the test job an if:"
    else
        echo "FAIL: a JOB_IF entry bought the test job an if: (exit $got)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
fi

# Preservation control for G: a non-purity job that changes in ways G is
# not about must still pass. G is keyed on four job-level keys, not on
# "this job changed".
python3 - "$G_REAL" "$TMP/gbenign.yaml" <<'BENIGN'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
doc["jobs"]["package"]["timeout-minutes"] = 42
doc["jobs"]["staticcheck"]["env"] = {"CGO_ENABLED": "0"}
yaml.safe_dump(doc, open(sys.argv[2], "w"))
BENIGN
check "job-level keys G is not about still pass" 0 "$TMP/gbenign.yaml" "PASS"

# --- the real tree ----------------------------------------------------
check "the repository's own workflow passes" 0 "$REPO/.github/workflows/test.yaml" "PASS"

# The tally line carries every count this gate's header refuses to write
# down, so its FIELDS need an observer of their own: a field silently
# dropped would take the observer for a header sentence with it. Matched
# by NAME, not by value -- a hardcoded 4 or 7 here would be the stale
# number this whole file is about.
check "the tally names the no-invocation count" 0 \
    "$REPO/.github/workflows/test.yaml" "policy-gates step(s) invoking none"
check "the tally names the job count" 0 \
    "$REPO/.github/workflows/test.yaml" "job(s) in the workflow"

# The preservation control for the WIDENING. Arm G was added to this gate
# in #834 round 3 and holds every non-purity job to four job-level keys;
# a widening tested only by what it now CATCHES cannot say what it now
# BREAKS. Exit 0 alone is too weak to answer that -- the gate exits 0
# while printing findings only if there are none, but a future arm could
# print a FINDING line and still be judged by a case that only reads the
# exit code. So the legitimate file is required to yield no finding AT
# ALL, which is the standing form of the byte-identical comparison run
# by hand when G landed (G excised from the gate, same real workflow,
# same output).
refutes "the repository's own workflow yields no FINDING at all" 0 \
    "$REPO/.github/workflows/test.yaml" "FINDING"

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
    cases=$((cases + 1))
    python3 - "$wf" <<'WIRED'
import re
import sys
import yaml

# The invocation forms this repository actually uses for a gate, anchored
# at the start of the command. Anything else -- an echo argument, a here
# document, a quoted string -- names the script without running it.
#
# The trailing `\s*$` is the PURITY_WORKFLOW seam, closed here rather
# than in the gate because a gate cannot derive its own parameter from
# its own subject. `check-test-job-purity.sh some-decoy.yaml` is an
# invocation in command position and would have satisfied the first
# version of this regex, while judging a file nobody reads.
INVOKE = re.compile(
    r"^\s*(?:(?:bash|sh)\s+)?(?:\./)?scripts/check-test-job-purity\.sh\s*$"
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

# An ARGUMENT is the other half of the same seam, and it is invisible to
# the gate: `check-test-job-purity.sh docs/decoy.yaml` is a real
# invocation in command position that judges a file nobody requires.
python3 - "$REAL" "$TMP/decoy-arg.yaml" <<'ARGDECOY'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
for step in doc["jobs"]["policy-gates"]["steps"]:
    body = step.get("run")
    if isinstance(body, str) and "scripts/check-test-job-purity.sh" in body:
        step["run"] = "bash scripts/check-test-job-purity.sh docs/decoy.yaml"
        break
else:
    raise SystemExit("no invocation to redirect")
yaml.safe_dump(doc, open(sys.argv[2], "w"))
ARGDECOY
wired "an invocation carrying an argument does not count as wiring" 1 "$TMP/decoy-arg.yaml"

# The env half of the seam. PURITY_WORKFLOW exists so THIS suite can
# hand the gate a fixture; set from inside the workflow it would point
# the gate away from the file it is wired into, and every arm would pass
# over a workflow nobody judged.
unredirected() {
    local name="$1" want="$2" wf="$3"
    cases=$((cases + 1))
    python3 - "$wf" <<'REDIRECT'
import sys
import yaml

VAR = "PURITY_WORKFLOW"
doc = yaml.safe_load(open(sys.argv[1]))


def has(env):
    return isinstance(env, dict) and VAR in env


if has(doc.get("env")):
    sys.exit(1)
for job in (doc.get("jobs") or {}).values():
    if not isinstance(job, dict):
        continue
    if has(job.get("env")):
        sys.exit(1)
    for step in job.get("steps") or []:
        if isinstance(step, dict) and has(step.get("env")):
            sys.exit(1)
sys.exit(0)
REDIRECT
    local got=$?
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want)"
        failures=$((failures + 1))
    fi
}

unredirected "the real workflow sets PURITY_WORKFLOW nowhere" 0 "$REAL"

# Driven at all three levels the seam can be set from, so the assertion
# is not keyed on the one place someone happened to try first.
python3 - "$REAL" "$TMP" <<'REDIRECTDECOYS'
import sys
import yaml

real, tmp = sys.argv[1], sys.argv[2]

doc = yaml.safe_load(open(real))
doc["env"] = {"PURITY_WORKFLOW": "docs/decoy.yaml"}
yaml.safe_dump(doc, open(tmp + "/redirect-workflow.yaml", "w"))

doc = yaml.safe_load(open(real))
doc["jobs"]["policy-gates"]["env"] = {"PURITY_WORKFLOW": "docs/decoy.yaml"}
yaml.safe_dump(doc, open(tmp + "/redirect-job.yaml", "w"))

doc = yaml.safe_load(open(real))
for step in doc["jobs"]["policy-gates"]["steps"]:
    body = step.get("run")
    if isinstance(body, str) and "scripts/check-test-job-purity.sh" in body:
        step["env"] = {"PURITY_WORKFLOW": "docs/decoy.yaml"}
        break
else:
    raise SystemExit("no invocation to redirect")
yaml.safe_dump(doc, open(tmp + "/redirect-step.yaml", "w"))
REDIRECTDECOYS

unredirected "a workflow-level PURITY_WORKFLOW is caught" 1 "$TMP/redirect-workflow.yaml"
unredirected "a job-level PURITY_WORKFLOW is caught" 1 "$TMP/redirect-job.yaml"
unredirected "a step-level PURITY_WORKFLOW is caught" 1 "$TMP/redirect-step.yaml"

if [ "$failures" -ne 0 ]; then echo "$failures failure(s)"; exit 1; fi
if [ "$cases" -lt "$FLOOR" ]; then
    echo "REFUSE: $cases case(s) ran, floor is $FLOOR -- a suite that shrank"
    echo "        is indistinguishable from a suite that passed. Raise FLOOR"
    echo "        in the same commit that adds cases; never lower it to go green."
    exit 2
fi
echo "all $cases case(s) passed (floor $FLOOR)"
