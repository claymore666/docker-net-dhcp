#!/usr/bin/env bash
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
}' "Skipped pending the engine requirement.\n\nTest-weakening: #125" 0

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
