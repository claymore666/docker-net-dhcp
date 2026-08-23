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
run_case() {
    local name="$1" base_content="$2" new_content="$3" body="${4-}" want="$5"
    local dir
    dir=$(mktemp -d)
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir -p test/integration/harness
        printf '%s\n' "$base_content" > example_test.go
        git add -A; git commit -qm base
        printf '%s\n' "$new_content" > example_test.go
        git add -A; git commit -qm "change"
        local bodyfile=""
        if [ -n "$body" ]; then bodyfile="$dir/body.md"; printf '%s\n' "$body" > "$bodyfile"; fi
        bash "$GATE" HEAD~1..HEAD "$bodyfile" >/dev/null 2>&1
        echo $?
    ) > "$dir/rc" 2>/dev/null
    local rc
    rc=$(tail -1 "$dir/rc")
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then ok "$name"; else no "$name (exit $rc, want $want)"; fi
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
