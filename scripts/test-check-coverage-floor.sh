#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-coverage-floor.sh (#735).
#
# Every signal is exercised in BOTH directions — a change that must trip
# the gate and a neighbouring change that must not. A gate never observed
# rejecting anything is not known to work, and one that rejects ordinary
# edits gets waived by reflex, which is the same outcome by a slower road.
#
# Real git history in a throwaway repo, not a mocked diff: the gate reads
# two blobs through `git show`, so a fake would be testing the fake.
set -u

GATE="$(cd "$(dirname "$0")" && pwd)/check-coverage-floor.sh"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

BASE_BASELINE='# Per-package coverage floors.
#
# Commentary, of which the real file has 253 lines.

example.com/mod/pkg/a 80.0
example.com/mod/pkg/b 50.0'

# run_case <name> <base-baseline> <head-baseline> <commit-msg> <body> <want-exit>
run_case() {
    local name="$1" base="$2" head="$3" msg="$4" body="$5" want="$6"
    local dir rc
    dir=$(mktemp -d)
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir -p .github
        printf '%s\n' "$base" > .github/coverage-baseline.txt
        printf 'base\n' > sentinel.txt
        git add -A; git commit -qm base
        if [ "$head" = "DELETE" ]; then
            rm -f .github/coverage-baseline.txt
        else
            printf '%s\n' "$head" > .github/coverage-baseline.txt
        fi
        # A second file always changes, so the head commit exists even
        # when the baseline is byte-identical — otherwise `git commit`
        # finds nothing to do, HEAD~1 does not resolve, and the case
        # measures the fixture instead of the gate.
        printf 'head\n' > sentinel.txt
        git add -A; git commit -qm "$msg"
        local bodyfile=""
        if [ -n "$body" ]; then bodyfile="$dir/body.md"; printf '%s\n' "$body" > "$bodyfile"; fi
        bash "$GATE" HEAD~1..HEAD "$bodyfile" >/dev/null 2>&1
        echo $?
    ) > "$dir/rc" 2>/dev/null
    rc=$(tail -1 "$dir/rc")
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then ok "$name"; else no "$name (exit $rc, want $want)"; fi
}

# --- 1. the decrease this gate exists for ----------------------------
run_case "a lowered floor trips the gate" "$BASE_BASELINE" \
'# Per-package coverage floors.

example.com/mod/pkg/a 78.0
example.com/mod/pkg/b 50.0' "chore: adjust" "" 1

# The negative control: raising a floor is the outcome the ratchet asks
# for, and a gate that fired on it would be waived on every good PR.
run_case "a raised floor passes" "$BASE_BASELINE" \
'# Per-package coverage floors.

example.com/mod/pkg/a 84.2
example.com/mod/pkg/b 50.0' "test: cover the error paths" "" 0

run_case "an untouched baseline passes" "$BASE_BASELINE" "$BASE_BASELINE" \
    "fix: something unrelated" "" 0

# Editing only the commentary must not read as a floor change — 253 of
# the real file's 258 lines are exactly that.
run_case "a comment-only edit passes" "$BASE_BASELINE" \
'# Per-package coverage floors.
#
# Rewritten commentary, same numbers.

example.com/mod/pkg/a 80.0
example.com/mod/pkg/b 50.0' "docs: explain the floors" "" 0

# --- 2. deletion, which is a lowering to nothing ----------------------
run_case "dropping a package from the baseline trips the gate" "$BASE_BASELINE" \
'# Per-package coverage floors.

example.com/mod/pkg/a 80.0' "refactor: tidy the baseline" "" 1

# Adding a package is how a new floor arrives; it must not trip.
run_case "adding a package passes" "$BASE_BASELINE" \
'# Per-package coverage floors.

example.com/mod/pkg/a 80.0
example.com/mod/pkg/b 50.0
example.com/mod/pkg/c 61.0' "test: baseline the new package" "" 0

# --- 3. the waiver ----------------------------------------------------
run_case "a lowered floor is waived by the PR body trailer" "$BASE_BASELINE" \
'example.com/mod/pkg/a 78.0
example.com/mod/pkg/b 50.0' "chore: adjust" \
"The parent NIC path cannot run here yet.

Coverage-floor: #155" 0

run_case "a lowered floor is waived by a commit message trailer" "$BASE_BASELINE" \
'example.com/mod/pkg/a 78.0
example.com/mod/pkg/b 50.0' \
"chore: adjust the floor

Coverage-floor: #155" "" 0

# The whole point of the trailer's shape: almost every commit here cites
# an issue, so a bare reference must NOT waive. If this case ever passes
# as 0 the gate has become decorative.
run_case "a bare issue reference does not waive" "$BASE_BASELINE" \
'example.com/mod/pkg/a 78.0
example.com/mod/pkg/b 50.0' \
"fix(plugin): something real (#155)

Closes #155" "" 1

# The trailer must be a trailer. Found live: this gate's own commit body
# quoted the waiver as an indented example, and the gate read that as a
# waiver and passed a really-lowered floor. A gate that any text
# describing it can switch off is not a gate.
run_case "an indented mention of the trailer does not waive" "$BASE_BASELINE" \
'example.com/mod/pkg/a 78.0
example.com/mod/pkg/b 50.0' "chore: adjust" \
"The waiver for this gate is written:

    Coverage-floor: #123

and that is what this paragraph is explaining." 1

run_case "a quoted trailer in a commit message does not waive" "$BASE_BASELINE" \
'example.com/mod/pkg/a 78.0
example.com/mod/pkg/b 50.0' \
"docs: explain the waiver

The line is > Coverage-floor: #123 in the PR body." "" 1

# --- 4. refusing a verdict rather than rendering an empty one ---------
run_case "a base baseline with no data lines refuses a verdict" \
'# every floor was commentary
#
# and nothing else' \
'example.com/mod/pkg/a 80.0' "chore: repopulate" "" 2

run_case "deleting the baseline outright trips the gate" "$BASE_BASELINE" "DELETE" \
    "chore: remove the baseline" "" 1

# A range whose base does not resolve must refuse, not pass.
tmp=$(mktemp -d)
(
    cd "$tmp" || exit 2
    git init -q .
    git config user.email t@t; git config user.name t
    git config commit.gpgsign false
    mkdir -p .github
    printf 'example.com/mod/pkg/a 80.0\n' > .github/coverage-baseline.txt
    git add -A; git commit -qm base
    bash "$GATE" no-such-ref..HEAD >/dev/null 2>&1
    echo $?
) > "$tmp/rc" 2>/dev/null
rc=$(tail -1 "$tmp/rc")
rm -rf "$tmp"
if [ "$rc" = "2" ]; then ok "an unresolvable base ref refuses a verdict"; else no "an unresolvable base ref refuses a verdict (exit $rc, want 2)"; fi

if bash "$GATE" >/dev/null 2>&1; [ $? -eq 2 ]; then
    ok "usage error exits 2"
else
    no "usage error should exit 2"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
