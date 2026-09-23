#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-comment-budget.sh (#1056), keyed on exit codes over
# fixtures committed in a throwaway repository, each rule driven both ways.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"

GATE="$HERE/check-comment-budget.sh"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

code() { local i; for i in $(seq 1 "$1"); do printf 'var v%s = %s\n' "$i" "$i"; done; }
cmt() { local i; for i in $(seq 1 "$1"); do printf '// line %s %s\n' "$i" "${2-}"; done; }
gofile() { printf 'package p\n\n%s\n' "$1"; }

# A base commit, then a head commit that writes FILE with HEAD content
# (an empty HEAD deletes it); the gate runs with ARGS inside the repo.
run_case() { # NAME FILE BASE HEAD WANT_RC WANT_GREP [ARGS...]
    local name="$1" file="$2" base="$3" head="$4" want="$5" want_grep="$6"
    shift 6
    local dir rc out
    guarded_tmpdir dir
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir -p "$(dirname "$file")" q
        gofile "$(code 5)" > q/anchor.go
        [ -n "$base" ] && printf '%s\n' "$base" > "$file"
        git add -A; git commit -qm base
        if [ -n "$head" ]; then printf '%s\n' "$head" > "$file"; else rm -f "$dir/$file"; fi
        git add -A; git commit -qm head
        if [ "$#" -eq 0 ]; then set -- HEAD~1..HEAD; fi
        bash "$GATE" "$@" > "$dir/out" 2>&1
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
    if [ -n "$want_grep" ] && ! printf '%s\n' "$out" | grep -E "$want_grep" >/dev/null; then
        no "$name (output lacks /$want_grep/)"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
        return
    fi
    ok "$name"
}

# Ten one-line comments over thirty lines of code, so an added block
# that brings enough code with it leaves the share where it was.
basebody() { local i; for i in $(seq 1 10); do printf '// base %s\n\nvar b%s = %s\n' "$i" "$i" "$i"; done; }
BASE="$(gofile "$(basebody)")"
grow() { gofile "$(basebody)
$1
$(code "${2:-40}" | sed 's/^var v/var w/')"; }

# Rule 1: block length.
run_case "an 11-line added block fails" p/a.go "$BASE" "$(grow "$(cmt 11 '#7')")" 1 'a.go:33: comment block of 11 lines'
run_case "a 10-line added block passes" p/a.go "$BASE" "$(grow "$(cmt 10 '#7')")" 0 ''
run_case "an 11-line /* */ block fails" p/a.go "$BASE" "$(grow "/* #7
$(cmt 9 | sed 's|^// ||')
end */")" 1 'comment block of 11 lines'
run_case "empty // lines inside a block are not counted" p/a.go "$BASE" "$(grow "$(cmt 5 '#7')
//
$(cmt 5)")" 0 ''
run_case "one added line in an old 11-line block judges the whole block" p/a.go \
    "$(gofile "$(code 20)
$(cmt 10 '#7')
var x = 1")" "$(gofile "$(code 20)
$(cmt 10 '#7')
// added
var x = 1
$(code 40 | sed 's/^var v/var w/')")" 1 'comment block of 11 lines'
run_case "an old 12-line block beside added code passes" p/a.go \
    "$(gofile "$(code 20)
$(cmt 12)
var x = 1")" "$(gofile "$(code 20)
$(cmt 12)
var x = 1
$(code 5 | sed 's/^var v/var w/')")" 0 ''

# Rule 2: reference.
run_case "a 2-line block without a reference fails" p/a.go "$BASE" "$(grow "$(cmt 2)")" 1 'carries no #N or RFC N reference'
run_case "a 2-line block with #N passes" p/a.go "$BASE" "$(grow "$(cmt 1)
// see #1056")" 0 ''
run_case "a 2-line block with RFC N passes" p/a.go "$BASE" "$(grow "$(cmt 1)
// RFC 2131 section 4.4.5")" 0 ''
run_case "a 1-line block without a reference passes" p/a.go "$BASE" "$(grow "$(cmt 1)")" 0 ''
run_case "a trailing comment on a code line is code" p/a.go "$BASE" "$(grow 'var t1 = 1 // one
var t2 = 2 // two')" 0 ''
run_case "a removed block passes" p/a.go "$(gofile "$(code 20)
$(cmt 12)
var x = 1")" "$(gofile "$(code 20)
var x = 1")" 0 ''

# Rule 3: per-package share.
run_case "a package whose share rises fails" p/a.go "$BASE" "$(grow "$(cmt 1)" 0)" 1 'FAIL rose'
run_case "a package whose share falls passes" p/a.go "$(gofile "$(code 20)
$(cmt 1)")" "$(gofile "$(code 30)
$(cmt 1)")" 0 ''
run_case "a deleted file passes" p/a.go "$(gofile "$(code 3)")" "" 0 'removed'

# Exemptions.
LIC='// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only'
run_case "the licence header is ignored" p/a.go "" "$LIC

$(gofile "$(code 5)")" 0 ''
run_case "the same header without SPDX is judged" p/a.go "" "// Copyright the docker-net-dhcp contributors.
// All rights reserved.

$(gofile "$(code 5)")" 1 'a.go:1: comment block of 2 lines'
run_case "//go: directives are ignored" p/a.go "$BASE" "$(grow '//go:generate true
//go:generate false
var d = 1')" 0 ''
run_case "//nolint directives are ignored" p/a.go "$BASE" "$(grow '//nolint:all
//nolint:errcheck
var d = 1')" 0 ''
run_case "the same lines as prose are judged" p/a.go "$BASE" "$(grow '// go generate true
// nolint all
var d = 1')" 1 'carries no #N or RFC N reference'
run_case "a doc.go package doc may exceed 10 lines" p/doc.go "" "$(cmt 14 '#9')
package p" 0 ''
run_case "a doc.go package doc still needs a reference" p/doc.go "" "$(cmt 3)
package p" 1 'doc.go:1: comment block of 3 lines carries no'
run_case "a long package doc outside doc.go is judged" p/a.go "" "$(cmt 11 '#9')
package p

$(code 40)" 1 'a.go:1: comment block of 11 lines'
run_case "a long comment elsewhere in doc.go is judged" p/doc.go "" "$(cmt 2 '#9')
package p

$(cmt 11 '#9')
var z = 1" 1 'doc.go:5: comment block of 11 lines'

# Multi-file fixtures (#1056): BEFORE writes the base commit's tree and
# AFTER changes it for the head commit; the gate judges HEAD~1..HEAD.
run_setup() { # NAME WANT_RC WANT_GREP BEFORE AFTER
    local name="$1" want="$2" want_grep="$3" before="$4" after="$5" dir rc
    guarded_tmpdir dir
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir p
        "$before"; git add -A; git commit -qm base
        "$after"; git add -A; git commit -qm head
        bash "$GATE" HEAD~1..HEAD > "$dir/out" 2>&1
        echo $? > "$dir/rc"
    ) >/dev/null 2>&1
    rc=$(cat "$dir/rc" 2>/dev/null)
    if [ "$rc" = "$want" ] && { [ -z "$want_grep" ] || grep -E "$want_grep" "$dir/out" >/dev/null; }; then
        ok "$name"
    else
        no "$name (exit $rc, want $want)"
        sed 's/^/      /' "$dir/out" >&2
    fi
    rm -rf "$dir"
}
# One comment over thirty lines of code: any comment line a new file
# brings in without code raises the share.
lean() { gofile "$(cmt 1)
$(code 30)" > p/a.go; }
base_old() { lean; gofile "$(cmt 12)
$(code 30 | sed 's/^var v/var o/')" > p/old.go; }
rename_edit() { git mv p/old.go p/new.go; sed -i 's/^var o30 = 30$/var o30 = 31/' p/new.go; }
run_setup "a renamed file with an old block and a one-line edit passes" 0 'gate passed' base_old rename_edit
header_file() { printf '%s\n\n%s\n' "$LIC" "$(gofile "$(code 3 | sed 's/^var v/var h/')")" > p/h.go; }
run_setup "a licence header in a new file stays out of the share" 0 'gate passed' lean header_file
doc_file() { printf '%s\npackage p\n' "$(cmt 12 '#9')" > p/doc.go; }
run_setup "a doc.go package doc stays out of the share" 0 'gate passed' lean doc_file
bad_file() { printf 'package p\n\nvar s = "open\n' > p/bad.go; }
run_setup "an added file that does not scan exits 2" 2 'bad.go does not scan' lean bad_file
base_tail() { gofile "$(basebody)
$(code 30)" > p/a.go; }
plus_line() {
    gofile "$(basebody)
var r = \`
++ changed
\`
$(code 30)
// one #7
$(code 20 | sed 's/^var v/var w/')" > p/a.go
}
run_setup "an added line starting ++ is not read as a file header" 0 'gate passed' base_tail plus_line
base_body() { printf '%s\n' "$BASE" > p/a.go; }
raw_string() {
    gofile "$(basebody)
// one #7
var r = \`
$(seq 1 12)
\`" > p/a.go
}
run_setup "every line of a multi-line raw string is code" 0 'gate passed' base_body raw_string
line_directive() {
    gofile "$(basebody)
//line gen.go:1
//line gen.go:90
$(code 10 | sed 's/^var v/var w/')
$(cmt 2)" > p/a.go
}
run_setup "//line directives do not move line numbers" 1 'a.go:45: .*carries no' base_body line_directive
line_only() {
    gofile "$(basebody)
//line gen.go:1
//line gen.go:90
$(code 10 | sed 's/^var v/var w/')" > p/a.go
}
run_setup "//line directives are ignored" 0 'gate passed' base_body line_only

# Ranges.
run_case "an unresolvable range exits 2" p/a.go "$BASE" "$(grow "")" 2 'cannot resolve' nosuch..HEAD
run_case "a bare revision exits 2" p/a.go "$BASE" "$(grow "")" 2 'not a <base>..<head> range' HEAD
run_case "no range skips with exit 0" p/a.go "$BASE" "$(grow "$(cmt 12)")" 0 'SKIP, no commit range' ""
run_case "three dots judge the same range" p/a.go "$BASE" "$(grow "$(cmt 2)")" 1 'carries no' HEAD~1...HEAD

# Proof mode.
PB="$(gofile "func f() {
	x := 1
	// a comment line
	y := 2 // trailing
	_ = x + y
}")"
run_case "proof passes on a comment-only change" p/a.go "$PB" "$(gofile "func f() {
	x := 1
	y := 2
	_ = x + y
}")" 0 'code tokens identical' --prove HEAD~1 HEAD
run_case "proof fails on a one-token change" p/a.go "$PB" "$(gofile "func f() {
	x := 1
	// a comment line
	y := 3 // trailing
	_ = x + y
}")" 1 'DIFFERS  p/a.go' --prove HEAD~1 HEAD
run_case "proof treats a directive as code" p/a.go "$PB" "//go:build linux

$PB" 1 'DIFFERS  p/a.go' --prove HEAD~1 HEAD
run_case "proof fails on a deleted file" p/a.go "$PB" "" 1 'present on one side only' --prove HEAD~1 HEAD
run_case "proof exits 2 on an unresolvable revision" p/a.go "$PB" "$PB
// x" 2 'cannot resolve' --prove nosuch HEAD
run_case "proof exits 2 on a file that does not parse" p/a.go "$PB" "$PB
func (" 2 'does not parse' --prove HEAD~1 HEAD

# The base branch moved on after the fork (#463): the share is judged
# against the merge base, not the base branch's newer tip.
dir=""
guarded_tmpdir dir
(
    cd "$dir" || exit 2
    git init -q .
    git config user.email t@t; git config user.name t
    git config commit.gpgsign false
    mkdir p; printf '%s\n' "$BASE" > p/a.go; git add -A; git commit -qm fork
    git checkout -q -b topic
    printf '%s\n' "$(grow "$(cmt 1)")" > p/a.go; git commit -qam topic
    git checkout -q -b moved HEAD~1
    gofile "$(code 100 | sed 's/^var v/var m/')" > p/b.go; git add -A; git commit -qm moved
    bash "$GATE" "moved..topic" > "$dir/out" 2>&1
    echo $? > "$dir/rc"
) >/dev/null 2>&1
if [ "$(cat "$dir/rc" 2>/dev/null)" = 0 ]; then
    ok "the share is judged against the merge base"
else
    no "the share is judged against the merge base"
    sed 's/^/      /' "$dir/out" >&2
fi
rm -rf "$dir"

# A range with no merge base: an orphan head skips, as for a push.
dir=""
guarded_tmpdir dir
(
    cd "$dir" || exit 2
    git init -q .
    git config user.email t@t; git config user.name t
    git config commit.gpgsign false
    gofile "$(code 3)" > a.go; git add -A; git commit -qm one
    first=$(git rev-parse HEAD)
    git checkout -q --orphan other; gofile "$(code 4)" > a.go; git add -A; git commit -qm two
    bash "$GATE" "$first..other" > "$dir/out" 2>&1
    echo $? > "$dir/rc"
) >/dev/null 2>&1
if [ "$(cat "$dir/rc" 2>/dev/null)" = 0 ] && grep -F 'has no merge base' "$dir/out" >/dev/null; then
    ok "a range with no merge base skips with exit 0"
else
    no "a range with no merge base skips with exit 0"
    sed 's/^/      /' "$dir/out" >&2
fi
rm -rf "$dir"

# The CI checkout is shallow: a merge base cut off by the shallow
# boundary is a refusal, never the skip above (#1056).
dir=""
guarded_tmpdir dir
(
    cd "$dir" || exit 2
    git init -q src; cd src || exit 2
    git config user.email t@t; git config user.name t
    git config commit.gpgsign false
    gofile "$(code 3)" > a.go; git add -A; git commit -qm one
    git branch side; git checkout -q -b trunk
    gofile "$(code 4)" > a.go; git commit -qam two
    git checkout -q side; gofile "$(code 5)" > b.go; git add -A; git commit -qm three
    cd "$dir" || exit 2
    git clone -q --depth 1 --no-single-branch "file://$dir/src" shallow; cd shallow || exit 2
    bash "$GATE" "origin/side..origin/trunk" > "$dir/out" 2>&1
    echo $? > "$dir/rc"
) >/dev/null 2>&1
if [ "$(cat "$dir/rc" 2>/dev/null)" = 2 ] && grep -F 'shallow clone' "$dir/out" >/dev/null; then
    ok "a merge base beyond a shallow boundary exits 2"
else
    no "a merge base beyond a shallow boundary exits 2"
    sed 's/^/      /' "$dir/out" >&2
fi
rm -rf "$dir"

echo
echo "test-check-comment-budget: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
