#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-no-dhcpcd-word.sh (external review row E-4).
#
# It builds a throwaway git repository with the document shape the gate
# expects, and drives its three outcomes:
#
#   0  a clean tree passes
#   1  a planted word in a living document is caught
#   2  the gate REFUSES rather than passing when it cannot judge
#
# Two of these are the ones a gate is usually written with. The third is
# the one that matters most and is usually missing: a universal is
# satisfied by emptying its domain, so the refusal cases are driven
# individually -- an expected document deleted, the domain empty, the
# history boundary absent.
#
# The allowlist is driven in BOTH directions, which is the point of
# having a region rather than a file: the word is permitted below the
# history boundary and refused above it, in the same file, in one run.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-no-dhcpcd-word.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0

# --- fixture ----------------------------------------------------------
# A repository the gate recognises: every EXPECTED document present,
# plus one extra living document so the domain is not exactly the
# expected list (a gate that only ever read its own expected list would
# pass every case below and be useless against a new page).
build_repo() {
    local d="$1"
    rm -rf "$d"
    mkdir -p "$d/docs"
    (
        cd "$d" || exit 1
        git init -q .
        git config user.email t@example.invalid
        git config user.name t
        git config commit.gpgsign false
        for f in README.md SECURITY.md docs/reference.md docs/internals.md docs/roadmap.md; do
            printf 'clean document, no forbidden word here.\n' > "$f"
        done
        printf 'a living document that is not on the expected list.\n' > docs/extra.md
        cat > RELEASE_NOTES.md <<'RN'
# Release notes

## v2.0.0 (unreleased)

The plugin performs the exchange itself.

## v1.9.0

This release shipped dhcpcd, and saying so is the record.
RN
        cat > docs/release-runbook.md <<'RB'
# Release runbook

The 1.x runbook still names dhcpcd.
RB
        git add -A
        git commit -qm fixture
    )
}

run() { ( cd "$1" && bash "$CHECK" . ) >"$TMP/out" 2>&1; echo $?; }

expect() {
    local name="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "ok   $name (exit $got)"
    else
        echo "FAIL $name: exit $got, want $want"
        sed 's/^/       | /' "$TMP/out"
        fails=$((fails + 1))
    fi
}

expect_out() {
    local name="$1" needle="$2"
    if grep -qF -- "$needle" "$TMP/out"; then
        echo "ok   $name"
    else
        echo "FAIL $name: output does not contain $needle"
        sed 's/^/       | /' "$TMP/out"
        fails=$((fails + 1))
    fi
}

R="$TMP/repo"

# --- outcome 0: the clean tree passes ---------------------------------
build_repo "$R"
expect "clean tree passes" 0 "$(run "$R")"
expect_out "clean run announces the count it inspected" "inspecting 8 document(s)"

# --- outcome 0, the other half of the allowlist ------------------------
# The two allowlisted documents contain the word in the fixture above,
# and the clean run passed, so permission is proved by that run rather
# than asserted here. What is proved here is that permission is REPORTED
# with its reason -- an allowlist whose reasons are invisible grows
# without anyone reading them.
expect_out "the whole-file allowance names its reason" "ALLOWED docs/release-runbook.md (whole file) — 2026-09-04:"
expect_out "the region allowance names its boundary" "ALLOWED RELEASE_NOTES.md (from line "

# --- outcome 1: a planted word in a living document is caught ---------
for target in README.md SECURITY.md docs/reference.md docs/extra.md; do
    build_repo "$R"
    printf 'The plugin drives dhcpcd over a FIFO.\n' >> "$R/$target"
    ( cd "$R" && git add -A && git commit -qm plant )
    expect "planted word in $target is caught" 1 "$(run "$R")"
    expect_out "the failure names $target" "$target"
done

# --- outcome 1: case and word boundaries ------------------------------
build_repo "$R"
printf 'DHCPCD is shouted here.\n' >> "$R/README.md"
( cd "$R" && git add -A && git commit -qm plant )
expect "an upper-case spelling is caught" 1 "$(run "$R")"

build_repo "$R"
printf 'Read dhcpcd.conf for the directives.\n' >> "$R/README.md"
( cd "$R" && git add -A && git commit -qm plant )
expect "a word followed by a dot is caught" 1 "$(run "$R")"

# The CONTROL for the two above. Without it a gate matching any
# substring at all would pass every case so far.
build_repo "$R"
printf 'The dhcpcdfoo identifier is not the word.\n' >> "$R/README.md"
( cd "$R" && git add -A && git commit -qm plant )
expect "a longer identifier is NOT caught (control)" 0 "$(run "$R")"

# --- outcome 1: the region allowlist is a region, not a file ----------
# The defeat this closes: a 2.0 release-notes section written above the
# history boundary, using the word, covered by a file-level allowance.
build_repo "$R"
python3 - "$R/RELEASE_NOTES.md" <<'PY'
import io,sys
p=sys.argv[1]
s=io.open(p,encoding='utf-8').read()
s=s.replace("The plugin performs the exchange itself.",
            "The plugin performs the exchange itself, replacing dhcpcd.")
io.open(p,'w',encoding='utf-8').write(s)
PY
( cd "$R" && git add -A && git commit -qm plant )
expect "the word ABOVE the history boundary is caught" 1 "$(run "$R")"
expect_out "the failure says which boundary" "ABOVE the history boundary"

# The other direction, driven on its own so the pass above cannot be
# read as the region check simply not running: below the boundary, in
# the same file, a second occurrence is permitted.
build_repo "$R"
python3 - "$R/RELEASE_NOTES.md" <<'PY'
import io,sys
p=sys.argv[1]
s=io.open(p,encoding='utf-8').read()
s+="\nAnother dhcpcd sentence, still inside the 1.x history.\n"
io.open(p,'w',encoding='utf-8').write(s)
PY
( cd "$R" && git add -A && git commit -qm plant )
expect "a second word BELOW the boundary is permitted" 0 "$(run "$R")"

# --- outcome 2: the refusals ------------------------------------------
build_repo "$R"
( cd "$R" && git rm -q docs/internals.md && git commit -qm drop )
expect "an expected document missing is a REFUSAL, not a pass" 2 "$(run "$R")"
expect_out "the refusal names the missing document" "docs/internals.md is missing"

# An empty domain. Every expected document is still on disk (so the
# expected-document refusal is not what fires) but none is TRACKED, so
# git ls-files returns nothing. This is the shape that turns a universal
# into a no-op, and it must not report success.
build_repo "$R"
( cd "$R" && git rm -rq --cached . && git commit -qm untrack )
expect "an empty domain is a REFUSAL, not a pass" 2 "$(run "$R")"
expect_out "the refusal says nothing was inspected" "document set is empty"

# Not a work tree at all.
mkdir -p "$TMP/notrepo"
expect "outside a git work tree is a REFUSAL" 2 "$(run "$TMP/notrepo")"

# The history boundary cannot be found: RELEASE_NOTES.md exists but has
# no 1.x heading, so the gate does not know where history begins. It
# must not silently judge the whole file either way.
build_repo "$R"
python3 - "$R/RELEASE_NOTES.md" <<'PY'
import io,sys
p=sys.argv[1]
io.open(p,'w',encoding='utf-8').write("# Release notes\n\n## v2.0.0 (unreleased)\n\nNothing historical here.\n")
PY
( cd "$R" && git add -A && git commit -qm plant )
expect "an unfindable history boundary is a REFUSAL" 2 "$(run "$R")"
expect_out "the refusal names the boundary pattern" "boundary between the living part and the history part"

# --- verdict ----------------------------------------------------------
if [ "$fails" -ne 0 ]; then
    echo "check-no-dhcpcd-word tests FAILED ($fails)"
    exit 1
fi
echo "all check-no-dhcpcd-word tests passed"
