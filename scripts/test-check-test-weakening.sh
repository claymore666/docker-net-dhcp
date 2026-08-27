#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-test-weakening.sh (#413).
#
# A gate that has never been observed rejecting anything is not known to
# work. This repo has shipped a guard that passed with the call it was
# guarding deleted, caught only by a negative control — so every signal
# below is exercised in BOTH directions: a diff that must trip it, and a
# nearby diff that must not.
set -u

GATE="$(cd "$(dirname "$0")" && pwd)/check-test-weakening.sh"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# Each case builds a throwaway repo with a base commit and one change,
# then runs the gate over that range. Real git history, not a mocked
# diff — the gate reads `git diff`, so a fake would test the fake.
#
# WHICH FILE the change lands in is a parameter since #828, because the
# domain is no longer one language. A case that cannot choose its path
# cannot tell "this pattern admits the file" from "some pattern does".
run_file_case() { # NAME FILE BASE NEW BODY WANT_RC [WANT_GREP]
    local name="$1" file="$2" base_content="$3" new_content="$4"
    local body="${5-}" want="$6" want_grep="${7-}"
    local dir rc out
    dir=$(mktemp -d)
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir -p "$(dirname "$file")"
        printf '%s\n' "$base_content" > "$file"
        git add -A; git commit -qm base
        printf '%s\n' "$new_content" > "$file"
        git add -A; git commit -qm "change"
        bodyfile=""
        if [ -n "$body" ]; then bodyfile="$dir/body.md"; printf '%s\n' "$body" > "$bodyfile"; fi
        bash "$GATE" HEAD~1..HEAD "$bodyfile" > "$dir/out" 2>&1
        echo $? > "$dir/rc"
    ) >/dev/null 2>&1
    rc=$(cat "$dir/rc" 2>/dev/null)
    out=$(cat "$dir/out" 2>/dev/null)
    rm -rf "$dir"
    if [ "$rc" != "$want" ]; then
        no "$name (exit $rc, want $want)"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
        return
    fi
    if [ -n "$want_grep" ] && ! printf '%s\n' "$out" | grep -q -- "$want_grep"; then
        no "$name (exit $rc as wanted, but the output never said '$want_grep')"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
        return
    fi
    ok "$name"
}

run_case() { # NAME BASE NEW BODY WANT   — the Go case, unchanged
    run_file_case "$1" example_test.go "$2" "$3" "${4-}" "$5"
}

BASE='package x
import "testing"
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}'

# --- 1. t.Skip -------------------------------------------------------
run_case "a new t.Skip trips the gate" "$BASE" 'package x
import "testing"
func TestThing(t *testing.T) {
	t.Skip("flaky here")
}' "" 1

run_case "a new t.Skip is waived by the Test-weakening trailer" "$BASE" 'package x
import "testing"
func TestThing(t *testing.T) {
	t.Skip("needs engine 28")
}' "Skipped pending the engine requirement.

Test-weakening: #125" 0

# The waiver must be a trailer, at column 0. Unanchored, any commit or
# PR body that quotes the trailer while explaining it switches the gate
# off — which is how the sibling coverage-floor gate (#735) was caught
# waiving itself on the very commit that introduced it.
run_case "an indented mention of the trailer does not waive" "$BASE" 'package x
import "testing"
func TestThing(t *testing.T) {
	t.Skip("flaky here")
}' "The escape hatch is written:

    Test-weakening: #125

and this paragraph is only describing it." 1

run_case "a bare issue mention does NOT waive it" "$BASE" 'package x
import "testing"
func TestThing(t *testing.T) {
	t.Skip("flaky here")
}' "Implements #367, part of the init-PID-1 work." 1

# --- 2. time.Sleep ---------------------------------------------------
run_case "a bare time.Sleep trips the gate" "$BASE" 'package x
import ("testing"; "time")
func TestThing(t *testing.T) {
	time.Sleep(2 * time.Second)
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}' "" 1

# A sleep that is the interval of a deadline-bounded poll is the
# recommended fix, not the smell. Every time.Sleep flagged across 20
# commits of real history was this shape.
run_case "a sleep inside a bounded poll is clean" "$BASE" 'package x
import ("testing"; "time")
func TestThing(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}
func ready() bool { return true }' "" 0

# --- 3. deleted assertions -------------------------------------------
run_case "deleting an assertion trips the gate" "$BASE" 'package x
import "testing"
func TestThing(t *testing.T) {
	if 2 != 2 {
		t.Fatalf("bang")
	}
}' "" 1

# Consolidating error propagation into a helper is not a weakened test.
# Measured on 5b6f94c: 27 t.Fatalf error checks moved into the harness,
# zero t.Errorf assertions removed. An earlier version of this gate
# reported that as 15 deleted checks and would have been waived by
# reflex on every refactor.
run_case "centralising error checks is not a deleted assertion" 'package x
import "testing"
func TestThing(t *testing.T) {
	h, err := get()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if h != 1 {
		t.Errorf("boom")
	}
}
func get() (int, error) { return 1, nil }' 'package x
import "testing"
func TestThing(t *testing.T) {
	h := mustGet(t)
	if h != 1 {
		t.Errorf("boom")
	}
}
func mustGet(t *testing.T) int { return 1 }' "" 0

run_case "adding assertions does not trip the gate" "$BASE" 'package x
import "testing"
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
	if 3 != 3 {
		t.Errorf("more")
	}
}' "" 0

# --- 4. timing budgets -----------------------------------------------
run_case "raising a timing budget trips the gate" 'package x
import ("testing"; "time")
const waitBudget = 5 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package x
import ("testing"; "time")
const waitBudget = 90 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' "" 1

run_case "raising a budget expressed in minutes trips the gate" 'package x
import ("testing"; "time")
const waitBudget = 90 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package x
import ("testing"; "time")
const waitBudget = 4 * time.Minute
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' "" 1

# #449 moved every budget in failure_test.go down once #356 removed the
# dnsmasq lease floor that had forced them up, and the gate flagged it
# anyway — it could only see that a budget changed. A waiver earned by
# tightening the suite is where waiver-by-reflex starts (#450).
run_case "lowering a timing budget is clean" 'package x
import ("testing"; "time")
const outageRiseBudget = 240 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package x
import ("testing"; "time")
const outageRiseBudget = 120 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' "" 0

run_case "lowering a budget across units is clean" 'package x
import ("testing"; "time")
const waitDeadline = 4 * time.Minute
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package x
import ("testing"; "time")
const waitDeadline = 90 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' "" 0

# Bare integers — seconds handed to a fixture option — are comparable to
# each other, but never to a Go duration.
run_case "lowering a bare-integer budget is clean" 'package x
import "testing"
const leaseGrace = 120
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package x
import "testing"
const leaseGrace = 20
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' "" 0

# 20 is not less than 120000000000; it is a different kind of number.
# Comparing across that boundary is how "lowered" gets asserted about a
# value that grew, so the gate declines to and reports.
run_case "swapping a budget's units reports rather than comparing" 'package x
import "testing"
const leaseGrace = 120
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package x
import ("testing"; "time")
const leaseGrace = 20 * time.Minute
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' "" 1

run_case "raising a bare-integer budget trips the gate" 'package x
import "testing"
const leaseGrace = 20
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package x
import "testing"
const leaseGrace = 120
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' "" 1

# The gate must not guess at what it cannot read. Silently passing an
# expression it does not understand is the gate not being there.
run_case "an unparseable budget change still reports" 'package x
import ("testing"; "time")
const waitBudget = 2 * baseUnit
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package x
import ("testing"; "time")
const waitBudget = 9 * baseUnit
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' "" 1

run_case "a brand-new budget constant is clean" "$BASE" 'package x
import ("testing"; "time")
const waitBudget = 90 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}' "" 0

# --- 5. opt-out helpers ----------------------------------------------
run_case "an opt-out helper trips the gate" "$BASE" 'package x
import "testing"
func HostConfigNoInit() int { return 0 }
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}' "" 1

# --- the quiet cases: the gate must not cry wolf ----------------------
run_case "an ordinary test edit is clean" "$BASE" 'package x
import "testing"
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom, with a better message")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}' "" 0

run_case "a brand-new assertion-only test is clean" "$BASE" 'package x
import "testing"
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}
func TestOther(t *testing.T) {
	if 4 != 4 {
		t.Errorf("nope")
	}
}' "" 0

# --- 6. the CI's own tests are tests (#828) ---------------------------
#
# `scripts/test-*.sh` was outside TEST_PATHS until #828, so a gate
# self-test could be gutted and the gate said "no test files changed" —
# byte-identical to what it says for a change that touched no test at
# all. Nothing distinguished a weakening from an unrelated edit.
#
# THE WIDENING IS THE DANGEROUS HALF, not the signals. Widening alone
# turns that honest silence into "clean (1 test file(s) inspected)": a
# claim of inspection over a gutted file, which is the class this gate's
# own header is written about. So the cases below drive BOTH — the
# mutant must go red, and the domain must be provably inspected.

SH_BASE='#!/usr/bin/env bash
set -u
fail=0
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"'

# THE MUTANT, reproduced rather than described: an assertion deleted and
# the helper switched off with a comment saying so. Before #828 this was
# exit 0, "no test files changed".
run_file_case "REGRESSION a gutted gate self-test is red" scripts/test-thing.sh \
    "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
check() { # <got> <want> <what>
    return 0  # temporarily skipped
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
exit "$fail"' "" 1

# THE OTHER HALF OF THE SAME CASE. Without this the suite cannot tell a
# gate that inspects the file from a gate that still ignores it and
# happens to be red for another reason: both are exit 1. The grep is on
# the count, because "1 test file(s) inspected" and "no test files
# changed" are the two outputs #828 measured as indistinguishable.
run_file_case "an ordinary edit to a gate self-test is inspected, not ignored" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3 (got $1, want $2)"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 0 "1 test file(s) inspected"

# The escape hatch reaches the new domain too, or the rule has two
# different shapes depending on the language.
run_file_case "the trailer waives a shell finding" scripts/test-thing.sh \
    "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
check() { # <got> <want> <what>
    return 0  # temporarily skipped
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
exit "$fail"' "The stub cannot run in this container.

Test-weakening: #828" 0

# THE 4-IN-60 CLASS THAT MUST STAY SILENT. A bare early `exit 0` fires
# on four commits in the history of scripts/test-*.sh, every one of them
# inside a generated stub heredoc — a fixture, not a weakening. The
# comment requirement is what takes that to zero, so it is driven here:
# an added bare `exit 0` with nothing calling it temporary is clean.
run_file_case "a bare exit 0 inside a generated stub is not a weakening" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
cat > /tmp/stub-$$ <<EOF
#!/usr/bin/env bash
exit 0
EOF
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 0

# THE COMMENT REQUIREMENT NEEDS AN INPUT WHERE ONLY IT STANDS. Every one
# of the 4 historical bare-`exit 0` hits is inside a generated stub
# heredoc, and those are data now, so the code/data classifier alone
# would silence all four — the requirement would read as covered while
# nothing measured it. The input where it alone decides is a bare early
# return in the self-test's OWN executable code: ordinary control flow,
# no marker, and it must stay silent.
run_file_case "an ordinary early return in a helper is not a weakening" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
quiet=0
say() {
    if [ "$quiet" = 1 ]; then
        return
    fi
    echo "$1"
}
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        say "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 0

# The marker on the line ABOVE the early exit is the same move written
# differently, and a signal that only reads one line is evaded by a
# newline.
run_file_case "a skip comment on the line above still counts" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
check() { # <got> <want> <what>
    # disabled until the fixture is rebuilt
    return
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 1

# The sleep rule, both directions, exactly as the Go one is driven.
run_file_case "a bare sleep in a gate self-test is red" scripts/test-thing.sh \
    "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
sleep 5
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 1

run_file_case "a sleep inside a bounded poll is clean" scripts/test-thing.sh \
    "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
deadline=$((SECONDS + 30))
while [ "$SECONDS" -lt "$deadline" ]; do
    [ -e /tmp/ready-$$ ] && break
    sleep 1
done
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 0

# --- 6b. the widening must not leak the Go signals (#828) -------------
#
# THIS IS THE ONE THE HISTORY SWEEP FOUND. Every Go signal was written
# when `*.go` was the whole domain, so none of them checked the
# language. The shell self-tests are full of Go source — they build
# fixture files to hand to the gate — so `t.Skip(`, `func …OptOut(` and
# `t.Errorf(` all appear in them as DATA.
#
# Measured over the last 400 non-merge commits: an unguarded widening
# moves three verdicts from clean to FAILED — 57a2232, f9cbf7c and
# 4530045 — and every one of them is THIS FILE's own self-test being
# accused of adding a t.Skip it merely quotes. A gate that fires on the
# commit writing its own tests is a cry-wolf on the file most likely to
# be edited next.
run_file_case "Go signals do not fire on Go source quoted in a shell fixture" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
cat > /tmp/example_test.go-$$ <<EOF
package x
import "testing"
func TestThing(t *testing.T) {
	t.Skip("the fixture this gate is handed")
}
func harnessOptOut(t *testing.T) {}
EOF
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 0

# The assertion counter is diff-WIDE, so a shell file removing quoted
# `t.Errorf(` lines would net out against a Go file adding none. Same
# leak, different signal.
run_file_case "the assertion count ignores t.Errorf quoted in a shell fixture" \
    scripts/test-thing.sh '#!/usr/bin/env bash
set -u
cat > /tmp/example_test.go-$$ <<EOF
package x
func TestThing(t *testing.T) {
	t.Errorf("one")
	t.Errorf("two")
	t.Errorf("three")
}
EOF
echo done' '#!/usr/bin/env bash
set -u
cat > /tmp/example_test.go-$$ <<EOF
package x
func TestThing(t *testing.T) {
	t.Errorf("one")
}
EOF
echo done' "" 0

# --- 6d. code, not data (#828) ----------------------------------------
#
# The moment the domain widened, the gate went red on THIS FILE: the
# cases above quote `return 0  # temporarily skipped` eight times as
# fixture text. It was right about the text and wrong about the file.
#
# That is structural, not a quirk. A gate that matches on content will
# always find its own triggers quoted in its own self-test, so admitting
# scripts/test-*.sh to a content signal guarantees a false positive on
# the commit that writes the tests. The discriminator is not spelling —
# it is whether the shell would execute the line.

run_file_case "the trigger inside a here-document is data, not a weakening" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
cat > /tmp/gutted-$$ <<EOF
check() {
    return 0  # temporarily skipped
}
EOF
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 0

run_file_case "the trigger inside a quoted fixture string is data, not a weakening" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
MUTANT='"'"'check() {
    return 0  # temporarily skipped
}'"'"'
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 0

# A single quote opened inside "$( ... )" is a real construct — two gates
# in scripts/ are written that way — and a classifier that models double
# quotes as a flat toggle never sees it. It then keeps calling the fixture
# CODE, and reports the very line the fixture is quoting.
run_file_case "a quote opened inside a command substitution is tracked" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
WANT="$(printf '"'"'%s\n'"'"' '"'"'
check() {
    return 0  # temporarily skipped
}'"'"')"
check() { # <got> <want> <what>
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
check "$(printf b)" b "and does it twice"
exit "$fail"' "" 0

# THE GUARD HAS A DIRECTION, so drive the other one. A classifier that
# loses track must judge the file whole rather than call everything data
# — wrong toward reporting, never toward silence. Here an unterminated
# quote leaves it out of its depth and the weakening below it must still
# be found.
run_file_case "a classifier that loses track still judges the file" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
stray='"'"'this quote is never closed
check() { # <got> <want> <what>
    return 0  # temporarily skipped
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
exit "$fail"' "" 1

# --- 6c. each pattern needs an input where only it stands (#828) ------
#
# TEST_PATHS is three patterns now. A suite that only ever exercises one
# of them reads as coverage while two are unmeasured — mutate the set
# down to a single pattern and nothing moves. So: one file per pattern
# that no other pattern admits, and two files that none admits.
run_file_case "pattern *_test.go: a Go test anywhere in the tree is inspected" \
    pkg/plugin/thing_test.go 'package plugin
import "testing"
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package plugin
import "testing"
func TestThing(t *testing.T) {
	t.Skip("flaky here")
}' "" 1

run_file_case "pattern test/integration/harness/*.go: a harness file with no _test suffix is inspected" \
    test/integration/harness/fixture.go 'package harness
import "testing"
func Fixture(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
}' 'package harness
import "testing"
func Fixture(t *testing.T) {
	t.Skip("not here")
}' "" 1

run_file_case "pattern scripts/test-*.sh: a gate self-test is inspected" \
    scripts/test-thing.sh "$SH_BASE" '#!/usr/bin/env bash
set -u
fail=0
check() { # <got> <want> <what>
    return 0  # temporarily skipped
    if [ "$1" != "$2" ]; then
        echo "FAIL $3"
        fail=$((fail + 1))
    fi
}
check "$(printf a)" a "printf emits its argument"
exit "$fail"' "" 1

# The complement. Weakening production code is a different problem with
# different reviewers, and the gate must keep saying so rather than
# growing into a general linter — which is what a widening drifts into
# if nothing pins the edge.
run_file_case "a gate itself is not a test file" scripts/check-thing.sh \
    '#!/usr/bin/env bash
set -u
echo ok
exit 0' '#!/usr/bin/env bash
set -u
run_it() {
    return 0  # temporarily skipped
    echo ok
}
run_it' "" 0 "no test files changed"

run_file_case "production Go is not a test file" cmd/net-dhcp/main.go \
    'package main
import "testing"
func main() {
	println("hi")
}' 'package main
import "testing"
func main() {
	t.Skip("nope")
	println("hi")
}' "" 0 "no test files changed"

# --- divergent history (#463) ----------------------------------------
#
# run_case builds LINEAR history, where the merge base of HEAD~1..HEAD
# *is* HEAD~1 — so it cannot see this class at all, which is why the
# class shipped. CI passes the base branch's tip at event time, not the
# fork point, and `git diff A..B` is a tree comparison: the moment dev
# moves ahead, everything landed in the meantime reads as a revert on
# the branch. PR #461 touched no test file and was told it changed a
# timing budget in failure_test.go.
#
# So: fork at base, land something on the base branch, and judge the
# branch. Both directions — the base's changes must not be attributed to
# it, and its own weakening must still be caught.
run_diverged() {
    local name="$1" base_content="$2" dev_content="$3" branch_content="$4" want="$5"
    local dir
    dir=$(mktemp -d)
    (
        cd "$dir" || exit 2
        git init -q -b dev .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        printf '%s\n' "$base_content" > example_test.go
        git add -A; git commit -qm base
        git branch fork
        printf '%s\n' "$dev_content" > example_test.go
        git add -A; git commit -qm "landed on dev meanwhile"
        git checkout -q fork
        printf '%s\n' "$branch_content" > example_test.go
        git add -A; git commit -qm "the branch's own change"
        # Exactly what .github/workflows/test.yaml passes: the base
        # branch's CURRENT tip, not the fork point.
        bash "$GATE" "$(git rev-parse dev)..$(git rev-parse HEAD)" >/dev/null 2>&1
        echo $?
    ) > "$dir/rc" 2>/dev/null
    local rc
    rc=$(tail -1 "$dir/rc")
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then ok "$name"; else no "$name (exit $rc, want $want)"; fi
}

DEV_TIGHTENED='package x
import ("testing"; "time")
const waitBudget = 30 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}'

BASE_BUDGET='package x
import ("testing"; "time")
const waitBudget = 60 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}'

# The branch changed only a message. Against dev's tip it looks like it
# raised waitBudget from 30s back to 60s — it did no such thing.
run_diverged "a branch behind dev is not blamed for what landed meanwhile" \
    "$BASE_BUDGET" "$DEV_TIGHTENED" 'package x
import ("testing"; "time")
const waitBudget = 60 * time.Second
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("boom, with a better message")
	}
	if 2 != 2 {
		t.Fatalf("bang")
	}
}' 0

# Same divergence, but this branch really does weaken. Judging on the
# merge base must not become judging on nothing.
run_diverged "a real weakening on a stale branch is still caught" \
    "$BASE_BUDGET" "$DEV_TIGHTENED" 'package x
import ("testing"; "time")
const waitBudget = 60 * time.Second
func TestThing(t *testing.T) {
	t.Skip("flaky here")
}' 1

# The other direction, and the worse one: the gate failing OPEN.
#
# Assertion accounting is diff-wide. Diffed against dev's tip, a branch
# inherits the *inverse* of whatever dev did — so if dev consolidated
# assertions away, the branch appears to add them back, and its own
# deletion nets out to a gain. Three assertions gone on dev, one gone on
# the branch: 2 added, 0 removed, reported clean. The branch really did
# delete a check.
run_diverged "a deleted assertion is not netted out by consolidation on dev" \
    'package x
import "testing"
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("one")
	}
	if 2 != 2 {
		t.Errorf("two")
	}
	if 3 != 3 {
		t.Errorf("three")
	}
	if 4 != 4 {
		t.Errorf("four")
	}
}' 'package x
import "testing"
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("one")
	}
}' 'package x
import "testing"
func TestThing(t *testing.T) {
	if 1 != 1 {
		t.Errorf("one")
	}
	if 2 != 2 {
		t.Errorf("two")
	}
	if 3 != 3 {
		t.Errorf("three")
	}
}' 1

# --- 6. uncommitted work is outside the range (#569) ------------------
#
# The gate judges a commit range. Run by hand before committing it saw
# no test file in the range and said so with an exit 0 — a clean pass
# over work it never opened. These cases pin the OUTPUT rather than the
# exit code, because the exit code was never the defect: 0 is still the
# right answer. What was wrong was claiming a verdict.
#
# Both directions, as everywhere else here: dirty must refuse, and clean
# must still give the plain line. A one-directional check here would let
# the refusal fire always, which is the same uselessness wearing the
# opposite mask.
run_dirty() {
    local name="$1" setup="$2" want_present="$3" want_absent="$4" want_rc="$5"
    local dir out rc
    dir=$(mktemp -d)
    out=$(
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir -p test/integration/harness
        printf 'package x\n' > doc.go
        git add -A; git commit -qm base
        # The range is real and non-empty; it simply carries no test file.
        printf 'package x\n// touched\n' > doc.go
        git add -A; git commit -qm "a commit that touches no test file"
        eval "$setup"
        bash "$GATE" HEAD~1..HEAD 2>&1
    )
    rc=$?
    rm -rf "$dir"
    local bad=""
    [ "$rc" = "$want_rc" ] || bad="exit $rc, want $want_rc"
    if [ -n "$want_present" ] && ! printf '%s\n' "$out" | grep -F "$want_present" >/dev/null; then
        bad="${bad:+$bad; }missing '$want_present'"
    fi
    if [ -n "$want_absent" ] && printf '%s\n' "$out" | grep -F "$want_absent" >/dev/null; then
        bad="${bad:+$bad; }must not print '$want_absent'"
    fi
    if [ -z "$bad" ]; then ok "$name"; else no "$name ($bad)"; fi
}

CLEAN_LINE="test-weakening gate: no test files changed"

run_dirty "an unstaged test file is not reported as a clean pass" \
    "printf 'package x\n' > a_test.go" \
    "NOT INSPECTED" "$CLEAN_LINE" 0

run_dirty "a STAGED test file is not reported as a clean pass" \
    "printf 'package x\n' > a_test.go; git add a_test.go" \
    "NOT INSPECTED" "$CLEAN_LINE" 0

# A file being written for the first time is untracked, and untracked is
# exactly the blind spot that #564 fixed in a different gate. Tracked-only
# discovery would report clean here.
run_dirty "an UNTRACKED test file is not reported as a clean pass" \
    "printf 'package x\n' > brand_new_test.go" \
    "brand_new_test.go" "$CLEAN_LINE" 0

# The harness counts as test-bearing everywhere else in this gate; it
# has to count here too, or the refusal has a hole the verdict does not.
run_dirty "an uncommitted harness file counts as test work" \
    "printf 'package harness\n' > test/integration/harness/x.go" \
    "test/integration/harness/x.go" "$CLEAN_LINE" 0

# The other direction. Nothing uncommitted, so the plain line is right
# and the refusal must NOT fire.
run_dirty "a clean tree still gets the plain no-test-files line" \
    "true" \
    "$CLEAN_LINE" "NOT INSPECTED" 0

# And uncommitted work that is not test work is none of this gate's
# business. A refusal here would fire on every ordinary edit.
run_dirty "an uncommitted NON-test file does not trigger the refusal" \
    "printf 'package x\n// more\n' > doc.go" \
    "$CLEAN_LINE" "NOT INSPECTED" 0

# The empty range was the stark case, not the only one. "clean (1 test
# file(s) inspected)" while two more sit uncommitted makes the same
# claim, so the notice rides along with a real verdict too.
run_dirty "a real verdict still names the work it did not inspect" \
    "printf 'package x\nimport \"testing\"\nfunc TestA(t *testing.T){ if 1!=1 { t.Errorf(\"a\") } }\n' > a_test.go
     git add -A; git commit -qm 'add a test'
     printf 'package x\n' > later_test.go" \
    "NOT INSPECTED" "" 0

# --- usage -----------------------------------------------------------
if bash "$GATE" >/dev/null 2>&1; then
    no "no arguments should be a usage error"
else
    [ "$?" = 2 ] && ok "no arguments exits 2" || ok "no arguments is an error"
fi

if bash "$GATE" definitely-not-a-range >/dev/null 2>&1; then
    no "an unresolvable range should not report clean"
else
    ok "an unresolvable range is an error, not a pass"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
