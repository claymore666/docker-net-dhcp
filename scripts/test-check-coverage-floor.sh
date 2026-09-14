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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(cd "$(dirname "$0")" && pwd)/check-coverage-floor.sh"
GATE_BIN="$GATE"
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
    guarded_tmpdir dir
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
tmp=            # declared here so a reader (and shellcheck) sees the name
guarded_tmpdir tmp
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

# --- a floor removed WITH its package (2.0 M8b) ----------------------
#
# The gate treats a removed floor as a decrease, which is right while
# the package is still there. When the package itself is deleted the
# row cannot be honoured by anything, and keeping it would leave a
# floor nothing will ever test. All four outcomes are driven, because
# "it noticed the deletion" and "it stopped noticing anything" look the
# same from a single passing case.
#
# These cases need a real module: the gate asks the TREE whether any
# .go file remains under the package, so the fixture has to have one.
# run_case above deliberately does not, which is why the plain
# "dropping a package" case still trips the gate — it cannot resolve
# the module path and says so rather than assuming.
#
# run_pkg_case <name> <base-baseline> <head-baseline> <gomod> <base-pkgs> <head-pkgs> <want-exit> [want-grep] [head-gomod]
#
# [head-gomod] rewrites go.mod in the HEAD commit, which is what a
# major-version rename does and what the base-only $gomod cannot
# express. $GATE_BIN, not $GATE, so a case can drive the pre-fix script
# through the identical fixture.
run_pkg_case() {
    local name="$1" base="$2" head="$3" gomod="$4" basepkgs="$5" headpkgs="$6"
    local want="$7" want_grep="${8-}" headgomod="${9-}"
    local dir rc out
    guarded_tmpdir dir
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir -p .github
        printf '%s\n' "$base" > .github/coverage-baseline.txt
        [ -n "$gomod" ] && printf '%s\n' "$gomod" > go.mod
        for d in $basepkgs; do mkdir -p "$d"; printf 'package p\n' > "$d/p.go"; done
        printf 'base\n' > sentinel.txt
        git add -A; git commit -qm base
        printf '%s\n' "$head" > .github/coverage-baseline.txt
        [ -n "$headgomod" ] && printf '%s\n' "$headgomod" > go.mod
        for d in $basepkgs; do rm -rf "$d"; done
        for d in $headpkgs; do mkdir -p "$d"; printf 'package p\n' > "$d/p.go"; done
        printf 'head\n' > sentinel.txt
        git add -A; git commit -qm "drop a package"
        bash "$GATE_BIN" HEAD~1..HEAD > "$dir/out" 2>&1
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
    if [ -n "$want_grep" ] && ! printf '%s\n' "$out" | grep -- "$want_grep" >/dev/null; then
        no "$name (exit $rc as wanted, but the output never said '$want_grep')"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
        return
    fi
    ok "$name"
}

GOMOD='module example.com/mod

go 1.24'

TWO='example.com/mod/pkg/a 80.0
example.com/mod/pkg/b 50.0'
ONE='example.com/mod/pkg/a 80.0'

# 1. The case this is for: the row went because the package went.
run_pkg_case "a floor dropped with its deleted package is not a decrease" \
    "$TWO" "$ONE" "$GOMOD" "pkg/a pkg/b" "pkg/a" 0 "The package is gone"

# 2. The case it must not swallow. Same baseline edit, package still
#    there — this is the decrease the gate exists for, and if the two
#    were not driven together the exemption would look like a pass.
run_pkg_case "a floor dropped while its package remains is still a decrease" \
    "$TWO" "$ONE" "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 1 "the ratchet no longer judges"

# 3. Cannot tell is a FINDING, not a pass. A malformed or absent go.mod
#    must not switch the check off — wrong in the direction of
#    reporting, never of silence.
run_pkg_case "an unresolvable module path is reported, not assumed" \
    "$TWO" "$ONE" "" "pkg/a pkg/b" "pkg/a" 1 "cannot tell"

# 4. A floor on some other module's package is equally unresolvable
#    against this tree, and gets the same answer rather than the
#    convenient one.
run_pkg_case "a floor from another module is reported, not assumed gone" \
    'example.com/other/pkg/z 80.0
example.com/mod/pkg/a 80.0' 'example.com/mod/pkg/a 80.0' "$GOMOD" "pkg/a" "pkg/a" 1 "cannot tell"

# 5. The emptied domain. Every package gone means there is no ratchet
#    left at all, and "no floor lowered" would read as a healthy one.
run_pkg_case "dropping every floor with every package is refused, not passed" \
    "$TWO" '# nothing left' "$GOMOD" "pkg/a pkg/b" "" 2 "Nothing left to ratchet"

# 6. The other direction, so the note is not printed on every run.
run_pkg_case "an untouched baseline prints no dropped-package note" \
    "$TWO" "$TWO" "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 0 ""

# --- a major-version rename of the module (#979) ----------------------
#
# From v2 onward Go puts the major in the module path, so the bump
# moves every baseline row one segment at once. Before the retry the
# gate reported one finding per floor for a change in which no floor
# moved: measured on the real v2 rename, four findings, four floors,
# none lowered.
#
# The pair that matters is (1) and (2). A re-spelling that made the
# rename pass would also make a floor LOWERED under the new name pass,
# and one case alone cannot tell those apart.
GOMOD_V2='module example.com/mod/v2

go 1.24'

TWO_V2='example.com/mod/v2/pkg/a 80.0
example.com/mod/v2/pkg/b 50.0'

# 1. Every floor carried over to the new path. No floor moved, so the
#    gate must pass AND must say the rows were matched by rename --
#    otherwise a clean pass here is indistinguishable from a clean pass
#    over rows that never moved.
run_pkg_case "a major-version rename with every floor carried over passes" \
    "$TWO" "$TWO_V2" "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 0 \
    "compared under the module's renamed path" "$GOMOD_V2"

# 2. THE CASE THAT PROVES THE RETRY IS NOT A BYPASS. Same rename, one
#    floor lower under the new name. If this ever passes, the rename
#    has become a way to lower a floor without saying so.
run_pkg_case "a floor lowered under the renamed path still trips the gate" \
    "$TWO" 'example.com/mod/v2/pkg/a 71.0
example.com/mod/v2/pkg/b 50.0' "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 1 \
    "example.com/mod/v2/pkg/a (was example.com/mod/pkg/a" "$GOMOD_V2"

# 3. A row DROPPED during the rename is not re-spelled into existence.
#    pkg/b moves nowhere: the retry finds no head row for it, so it
#    falls back to its original name and the existing arm reports that
#    it cannot resolve it. A finding, never a silent pass.
run_pkg_case "a floor dropped during the rename is still reported" \
    "$TWO" 'example.com/mod/v2/pkg/a 80.0' "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 1 \
    "cannot tell" "$GOMOD_V2"

# 4. The retry is keyed on the two go.mod files naming the SAME module
#    at different majors. A head module that is a different module
#    entirely must not re-spell anything, or the gate would match rows
#    across unrelated trees.
run_pkg_case "a different module at a new major does not re-spell the rows" \
    "$TWO" 'example.com/other/v2/pkg/a 80.0
example.com/other/v2/pkg/b 50.0' "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 1 \
    "cannot tell" 'module example.com/other/v2

go 1.24'

# 5. `/vN` is digits after the v. A last segment like `v2beta` is an
#    ordinary directory, and stripping it would make two unrelated
#    modules compare equal.
run_pkg_case "a v2beta suffix is not a major-version suffix" \
    "$TWO" 'example.com/mod/v2beta/pkg/a 80.0
example.com/mod/v2beta/pkg/b 50.0' "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 1 \
    "cannot tell" 'module example.com/mod/v2beta

go 1.24'

# 6. The preservation control for the rows the retry must never touch:
#    no rename in the range at all, and the existing gone/present arms
#    answer exactly as before. Driven here as well as above so a change
#    to the retry cannot quietly move them.
run_pkg_case "with no rename, a deleted package's floor is still not a decrease" \
    "$TWO" "$ONE" "$GOMOD" "pkg/a pkg/b" "pkg/a" 0 "The package is gone"
run_pkg_case "with no rename, a dropped floor on a live package is still a decrease" \
    "$TWO" "$ONE" "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 1 "the ratchet no longer judges"

# --- THE PREVIOUS VERSION IS THE STRONGEST MUTANT ---------------------
#
# The pre-fix gate is built by cutting the retry out of the REAL
# script, not by keeping a copy of the block: a copy stops being the
# subject the moment the script moves on. The surgery asserts it found
# something, so a control that has quietly gone inert fails instead of
# passing.
PREFIX_DIR=
guarded_tmpdir PREFIX_DIR
PREFIX="$PREFIX_DIR/check-coverage-floor-prefix.sh"
if python3 - "$GATE" "$PREFIX" <<'SURGERY'
import sys
src = open(sys.argv[1]).read()
start = src.index('    renamed=""\n')
end = src.index('    if [ -z "$now" ]; then\n        case "$(package_state "$pkg")" in\n', start)
assert start < end, "the retry block is not where this surgery expects it"
cut = src[:start] + src[end:]
assert cut != src, "the surgery removed nothing: this control is inert"
assert 'renamed_path "$pkg"' not in cut, "the retry survived the cut"
open(sys.argv[2], "w").write(cut)
SURGERY
then
    GATE_BIN="$PREFIX"
    run_pkg_case "the pre-fix gate reports the rename as findings (control)" \
        "$TWO" "$TWO_V2" "$GOMOD" "pkg/a pkg/b" "pkg/a pkg/b" 1 \
        "cannot tell" "$GOMOD_V2"
    GATE_BIN="$GATE"
else
    no "the pre-fix control could not be built from the real script"
fi
rm -rf "$PREFIX_DIR"



printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
