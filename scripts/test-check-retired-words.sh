#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-retired-words.sh (external review row E-4; #911).
#
# It builds a throwaway git repository with the shape the gate expects,
# and drives its three outcomes:
#
#   0  a clean tree passes
#   1  a planted word is caught
#   2  the gate REFUSES rather than passing when it cannot judge
#
# Two of these are the ones a gate is usually written with. The third is
# the one that matters most and is usually missing: a universal is
# satisfied by emptying its domain, so the refusal cases are driven
# individually -- an expected document deleted, each half of a domain
# emptied on its own, the history boundary absent.
#
# BOTH WORDS ARE DRIVEN, AND SO IS EVERY WAY THEY DIFFER. A list-of-
# words gate fails in one particular way: the loop runs, the first word
# is judged properly, and the boundary and allowance logic is never
# reached for the second -- so the second word is enforced everywhere,
# including the history it is permitted in, or nowhere. So `beta` is
# driven above AND below the RELEASE_NOTES boundary in its own right,
# `dhcpcd`'s whole-file allowance is shown NOT to cover `beta`, and each
# word's domain is shown to be its own: `beta` is caught in a Go source
# and `dhcpcd`, whose domain is documents, is shown not to be.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-retired-words.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0

# --- fixture ----------------------------------------------------------
# A repository the gate recognises: every EXPECTED document present,
# plus one extra living document and one Go source so neither domain is
# exactly the expected list (a gate that only ever read its own expected
# list would pass every case below and be useless against a new page).
build_repo() {
    local d="$1"
    rm -rf "$d"
    mkdir -p "$d/docs" "$d/pkg/plugin"
    (
        cd "$d" || exit 1
        git init -q .
        git config user.email t@example.invalid
        git config user.name t
        git config commit.gpgsign false
        for f in README.md SECURITY.md docs/reference.md docs/internals.md docs/roadmap.md; do
            printf 'clean document, no retired word here.\n' > "$f"
        done
        printf 'a living document that is not on the expected list.\n' > docs/extra.md
        printf 'package plugin\n\n// A clean comment.\nfunc f() {}\n' > pkg/plugin/thing.go
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

plant() {  # plant <repo> <file> <line...>
    local d="$1" f="$2"; shift 2
    printf '%s\n' "$@" >> "$d/$f"
    ( cd "$d" && git add -A && git commit -qm plant )
}

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
expect_out "the dhcpcd run announces the count it inspected" "inspecting 8 document(s) for the word 'dhcpcd'"
expect_out "the beta run announces a LARGER domain, including the Go source" \
    "inspecting 9 document(s) and Go source(s) for the word 'beta'"

# --- outcome 0, the other half of the allowlist ------------------------
# The two allowlisted documents contain `dhcpcd` in the fixture above,
# and the clean run passed, so permission is proved by that run rather
# than asserted here. What is proved here is that permission is REPORTED
# with its reason and its WORD -- an allowance whose reasons are
# invisible grows without anyone reading them, and one whose word is
# invisible cannot be told apart from a blanket exemption for the path.
expect_out "the whole-file allowance names its word and its reason" \
    "ALLOWED docs/release-runbook.md (whole file, 'dhcpcd') — 2026-09-04:"
expect_out "the region allowance names its word and its boundary" \
    "ALLOWED RELEASE_NOTES.md (from line "
expect_out "the region allowance is announced for BETA too" \
    "ALLOWED RELEASE_NOTES.md (from line 7 down, 'beta')"

# ...and it is BETA'S allowance, not dhcpcd's reached through a shared
# path. The two region entries happen to name the same file and the same
# regex, so an index lookup that dropped the word from its key would
# behave identically -- except in the reason it prints, which is the only
# place the difference is visible. An allowance whose reason belongs to
# another word is how a second word inherits a decision nobody made for
# it, so the reason is asserted per word rather than for presence.
expect_out "the BETA region allowance carries BETA's dated reason" \
    "down, 'beta') — 2026-09-05:"
expect_out "the DHCPCD region allowance carries DHCPCD's dated reason" \
    "down, 'dhcpcd') — 2026-09-04:"

# --- outcome 1: a planted word in a living document is caught ---------
for target in README.md SECURITY.md docs/reference.md docs/extra.md; do
    build_repo "$R"
    plant "$R" "$target" 'The plugin drives dhcpcd over a FIFO.'
    expect "planted dhcpcd in $target is caught" 1 "$(run "$R")"
    expect_out "the dhcpcd failure names $target" "$target"

    build_repo "$R"
    plant "$R" "$target" 'This branch is the 2.0 beta.'
    expect "planted beta in $target is caught" 1 "$(run "$R")"
    expect_out "the beta failure names $target" "$target"
done

# The beta failure says what to write instead. A gate that only says
# "no" sends the next person to guess, and the guess was "2.x-beta".
build_repo "$R"
plant "$R" README.md 'This branch is the 2.0 beta.'
expect "a planted beta is red" 1 "$(run "$R")"
expect_out "the beta failure names the remedy, not only the word" \
    "the line is 2.0, its first pre-release is v2.0.0-rc1, and IPv6 parity is tracked in #911"

# --- outcome 1: THE DOMAINS ARE PER WORD, IN BOTH DIRECTIONS ----------
# beta reaches a Go source. This is the half the doc-only gate could not
# see, and it is where the word actually did the damage: an error string
# an operator reads.
build_repo "$R"
plant "$R" pkg/plugin/thing.go '// The beta refuses IPv6.'
expect "planted beta in a Go source is caught" 1 "$(run "$R")"
expect_out "the failure names the Go file" "pkg/plugin/thing.go"

# The CONTROL for the line above, and the reason the domains are a
# per-word property rather than one shared list: dhcpcd's domain is
# documents, so the same plant in the same Go file is NOT its business.
# Without this, a single shared domain would pass every case above.
build_repo "$R"
plant "$R" pkg/plugin/thing.go '// This once exec would have run dhcpcd.'
expect "planted dhcpcd in a Go source is NOT caught (control: domains are per word)" 0 "$(run "$R")"

# --- outcome 1: case and word boundaries ------------------------------
build_repo "$R"
plant "$R" README.md 'DHCPCD is shouted here.'
expect "an upper-case dhcpcd is caught" 1 "$(run "$R")"

build_repo "$R"
plant "$R" README.md 'The 2.0 BETA is shouted here.'
expect "an upper-case beta is caught" 1 "$(run "$R")"

build_repo "$R"
plant "$R" README.md 'Read dhcpcd.conf for the directives.'
expect "a word followed by a dot is caught" 1 "$(run "$R")"

build_repo "$R"
plant "$R" README.md 'The branch was called 2.x-beta.'
expect "a hyphenated beta is caught" 1 "$(run "$R")"

# The CONTROL for the four above. Without it a gate matching any
# substring at all would pass every case so far.
build_repo "$R"
plant "$R" README.md 'The dhcpcdfoo identifier is not the word.' 'Neither is Betamax.'
expect "a longer identifier is NOT caught (control)" 0 "$(run "$R")"

# --- outcome 1: the region allowlist is a region, not a file ----------
# The defeat this closes: a 2.0 release-notes section written above the
# history boundary, using the word, covered by a file-level allowance.
rewrite_v20_section() {
    python3 - "$1/RELEASE_NOTES.md" "$2" <<'PY'
import io,sys
p,repl=sys.argv[1],sys.argv[2]
s=io.open(p,encoding='utf-8').read()
s=s.replace("The plugin performs the exchange itself.",repl)
io.open(p,'w',encoding='utf-8').write(s)
PY
    ( cd "$1" && git add -A && git commit -qm plant )
}

build_repo "$R"
rewrite_v20_section "$R" "The plugin performs the exchange itself, replacing dhcpcd."
expect "dhcpcd ABOVE the history boundary is caught" 1 "$(run "$R")"
expect_out "the dhcpcd failure says which boundary" "ABOVE the history boundary"

build_repo "$R"
rewrite_v20_section "$R" "This is the 2.0 beta."
expect "beta ABOVE the history boundary is caught" 1 "$(run "$R")"
expect_out "the beta failure says which boundary" "ABOVE the history boundary"

# The other direction, driven per word and on its own so a pass cannot
# be read as the region check simply not running for that word: below
# the boundary, in the same file, each word is permitted.
build_repo "$R"
plant "$R" RELEASE_NOTES.md 'Another dhcpcd sentence, still inside the 1.x history.'
expect "a second dhcpcd BELOW the boundary is permitted" 0 "$(run "$R")"

build_repo "$R"
plant "$R" RELEASE_NOTES.md 'v1.5.0 shipped after a beta nobody remembers.'
expect "a beta BELOW the boundary is permitted (the second word reaches the boundary logic)" 0 "$(run "$R")"

# The whole-file allowance is keyed on (word, path) too, and this is the
# direction that proves it: docs/release-runbook.md is exempt for
# dhcpcd, and NOT for beta. A path-keyed allowance would pass here.
build_repo "$R"
plant "$R" docs/release-runbook.md 'Tag the 2.0 beta first.'
expect "beta in the dhcpcd-exempt file is still caught" 1 "$(run "$R")"
expect_out "the failure names the runbook" "docs/release-runbook.md"

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
expect_out "the refusal says nothing was inspected" "half of the domain for 'dhcpcd' is empty"

# HALF a domain empty. Every document is tracked; no Go file is. The
# union for `beta` is still eight documents, so a union-only emptiness
# check passes here having never opened a Go file -- which is the whole
# reason the Go half was added.
build_repo "$R"
( cd "$R" && git rm -q pkg/plugin/thing.go && git commit -qm 'drop the go tree' )
expect "the Go HALF of beta's domain being empty is a REFUSAL, not a pass" 2 "$(run "$R")"
expect_out "the refusal names the glob that matched nothing" "the '*.go' half of the domain for 'beta' is empty"

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
    echo "check-retired-words tests FAILED ($fails)"
    exit 1
fi
echo "all check-retired-words tests passed"
