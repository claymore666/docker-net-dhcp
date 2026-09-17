#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-fuzz-budget.sh (#324). Feeds synthetic
# workflow files through the FUZZ_WORKFLOW seam and asserts the verdict.
#
# The cases that matter most are the two blindness guards: a fuzz step
# that has vanished must exit 2 (not 0), and a malformed budget must be
# judged rather than skipped. A gate that silently sees nothing is the
# failure mode this repo has shipped twice.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

CHECK="$(dirname "$0")/check-fuzz-budget.sh"
guarded_tmpdir TMP

# THE TRANSPORT IS STUBBED, NOT THE VERDICT. The gate now resolves every
# -fuzz name against its package with `go test -list`, which would tie
# these synthetic workflows to the real tree and make the table describe
# pkg/dhcp instead of the gate. So the two things that reach outside are
# seams: the lister, and the tree the gate enumerates targets from. Every
# judgement below is still the gate's own.
cat > "$TMP/list-stub" <<'STUB'
#!/usr/bin/env bash
# Args: <pattern> <pkg>. The package decides what exists, so a case can
# ask for a name that is not there.
case "${2:-}" in
    ./pkg/empty/) : ;;
    *) printf 'FuzzX
FuzzA
FuzzB
' ;;
esac
echo "ok   stub 0.001s"
STUB
chmod +x "$TMP/list-stub"
mkdir -p "$TMP/tree"
cat > "$TMP/tree/stub_test.go" <<'SEED'
package stub

func FuzzX(f *testing.F) {}
SEED
export FUZZ_LIST_CMD="$TMP/list-stub"
export FUZZ_TREE_ROOT="$TMP/tree"

# THE SECOND FILE THAT FUZZES. The gate reads the workflow and the local
# lane, because a name that resolves to nothing is the same defect in
# either, and only one of them was covered when this table was first
# written. Cases that say nothing about the lane get one that agrees
# with the default stub tree, so a case still measures the thing it
# names; the lane's own cases pass their own fifth argument.
LANE_DEFAULT='          go test ./pkg/dhcp/ -run "^$" -fuzz "^FuzzX$" -fuzztime 200000x -timeout 5m'

failures=0
# check NAME WANT_EXIT WORKFLOW_BODY GREP_PATTERN [LANE_BODY]
check() {
    local name="$1" want_exit="$2" body="$3" want_grep="$4" lane="${5:-$LANE_DEFAULT}"
    printf '%s\n' "$body" > "$TMP/wf.yaml"
    printf '%s\n' "$lane" > "$TMP/lane.sh"
    FUZZ_WORKFLOW="$TMP/wf.yaml" FUZZ_LANE="$TMP/lane.sh" bash "$CHECK" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

check "execution budget with a timeout passes" 0 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"bounded execution budget"

check "wall-clock budget is rejected" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 20s -timeout 5m" \
"wall-clock budget"

check "wall-clock in minutes is rejected too" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 2m -timeout 5m" \
"wall-clock budget"

check "execution budget without a -timeout is rejected" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x" \
"no -timeout"

check "a non-numeric count is judged, not waved through" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime abcx -timeout 5m" \
"not a valid execution count"

check "-fuzztime= spelling is understood" 0 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime=200000x -timeout=5m" \
"bounded execution budget"

# Blindness guard 1: the step is gone entirely.
check "no fuzz invocation at all exits 2, loudly" 2 \
"          go test ./... -race" \
"watching nothing"

# Blindness guard 2: prose must not satisfy or trip the gate.
check "a comment mentioning -fuzztime neither passes nor fails the gate" 2 \
"          # never use a wall-clock -fuzztime 20s here" \
"watching nothing"

check "a comment alongside a real invocation does not double-report" 0 \
"          # -fuzztime 20s used to be the shape here
          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"2 fuzz invocation(s) across 2 file(s)"

# Several invocations: every one is judged, not just the first.
check "a second, bad invocation is caught behind a good one" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzA$' -fuzztime 200000x -timeout 5m
          go test ./pkg/dhcp/ -fuzz '^FuzzB$' -fuzztime 30s -timeout 5m" \
"wall-clock budget"

# THE DEFECT THIS GATE MISSED FOR WEEKS (#1010), both directions.
#
# A name that resolves is the control: without it the case below proves
# only that the gate can say no, which is a verdict a broken gate also
# reaches.
check "a -fuzz name that resolves passes" 0 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"each smoked by every file"

check "a -fuzz name that resolves to nothing is refused" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzGone$' -fuzztime 200000x -timeout 5m
          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"has no such target"

check "a package with no targets at all is refused" 1 \
"          go test ./pkg/empty/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"has no such target"

# A name assembled at run time is the exact spelling that went unseen:
# the dead step fuzzed \"^\${target}\$\" from a shell loop.
check "a -fuzz target that is not a literal is refused" 1 \
"          go test ./pkg/dhcp/ -fuzz \"^\${target}\$\" -fuzztime 200000x -timeout 5m" \
"cannot read a package and a literal"

# The other direction: a target the workflow never runs. A second target
# in the tree, and a workflow that still smokes only the first.
mkdir -p "$TMP/two-tree"
printf 'package stub\n\nfunc FuzzX(f *testing.F) {}\nfunc FuzzA(f *testing.F) {}\n' > "$TMP/two-tree/two_test.go"
FUZZ_TREE_ROOT="$TMP/two-tree" check "a target in the tree that nothing fuzzes is refused" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"never fuzzes it"

# Vacuity: the gate must not pass a tree Scorecard reads as unfuzzed.
mkdir -p "$TMP/empty-tree"
FUZZ_TREE_ROOT="$TMP/empty-tree" check "a tree with no target Scorecard can see exits 2" 2 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"Scorecard can see"

# Scorecard matches per LINE, so a signature that wraps is invisible to
# it even though Go compiles it. The gate has to agree with Scorecard,
# not with the compiler.
mkdir -p "$TMP/wrapped-tree"
printf 'package stub\n\nfunc FuzzX(\n\tf *testing.F,\n) {}\n' > "$TMP/wrapped-tree/w_test.go"
FUZZ_TREE_ROOT="$TMP/wrapped-tree" check "a signature split across lines is invisible to Scorecard" 2 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"Scorecard can see"

# A target under testdata/ is skipped by Scorecard (listing.go:53-59).
mkdir -p "$TMP/testdata-tree/testdata"
printf 'package stub\n\nfunc FuzzX(f *testing.F) {}\n' > "$TMP/testdata-tree/testdata/t_test.go"
FUZZ_TREE_ROOT="$TMP/testdata-tree" check "a target under testdata/ does not count" 2 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"Scorecard can see"

# THE COPY THAT WAS UNCOVERED. Every judgement above is asked of the
# workflow; these four ask it of the lane, with the workflow held
# correct, because a gate that reads one of two files reproduces in the
# other exactly the silence it was built to end.
check "a lane -fuzz name that resolves passes" 0 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"across 2 file(s)" \
"          \"fuzz (short)|go|go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m\""

check "a lane -fuzz name that resolves to nothing is refused" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"lane.sh:1: -fuzz names FuzzGone" \
"          \"fuzz (short)|go|go test ./pkg/dhcp/ -fuzz '^FuzzGone$' -fuzztime 200000x -timeout 5m\""

check "a tree target the lane alone never fuzzes is refused" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"FuzzX exists in the tree and $TMP/lane.sh never fuzzes it" \
"          \"fuzz (short)|go|go test ./pkg/dhcp/ -fuzz '^FuzzA$' -fuzztime 200000x -timeout 5m\""

check "a lane with no fuzz invocation at all exits 2" 2 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"watching nothing" \
"          \"unit tests|go|go test ./...\""

# A wall-clock budget is the gate's original subject, asked of the lane.
check "a wall-clock budget in the lane is rejected" 1 \
"          go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 200000x -timeout 5m" \
"wall-clock budget" \
"          \"fuzz (short)|go|go test ./pkg/dhcp/ -fuzz '^FuzzX$' -fuzztime 20s -timeout 5m\""

FUZZ_WORKFLOW="$TMP/does-not-exist.yaml" FUZZ_LANE="$TMP/lane.sh" bash "$CHECK" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q "does not exist" "$TMP/out"; then
    echo "PASS: a missing workflow file exits 2"
else
    echo "FAIL: a missing workflow file exits 2"
    failures=$((failures + 1))
fi

FUZZ_WORKFLOW="$TMP/wf.yaml" FUZZ_LANE="$TMP/no-such-lane.sh" bash "$CHECK" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q "no-such-lane.sh does not exist" "$TMP/out"; then
    echo "PASS: a missing lane file exits 2"
else
    echo "FAIL: a missing lane file exits 2"
    sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi

# The real workflow AND the real lane must satisfy their own gate.
# The seams are dropped here on purpose: this case is the one that runs
# the gate against the real workflow and the real tree, which is what
# the whole table is a model of.
if (cd "$(dirname "$0")/.." && env -u FUZZ_LIST_CMD -u FUZZ_TREE_ROOT -u FUZZ_LANE bash scripts/check-fuzz-budget.sh > "$TMP/real" 2>&1); then
    echo "PASS: the committed workflow passes the gate"
else
    echo "FAIL: the committed workflow does not pass the gate"
    sed 's/^/    /' "$TMP/real"
    failures=$((failures + 1))
fi

total=$((failures == 0 ? 0 : failures))
if [ "$total" -eq 0 ]; then
    echo "all check-fuzz-budget tests passed"
    exit 0
fi
echo "$failures failed"
exit 1
