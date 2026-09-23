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
run_case "two trailing comments are two one-line blocks" p/a.go "$BASE" "$(grow 'var t1 = 1 // one
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
trail() { local i; for i in $(seq 1 "$1"); do printf 'var t%s = %s // %s\n' "$i" "$i" "${2:-note}"; done; }
run_case "five trailing comments raise the share" p/a.go "$BASE" "$(gofile "$(basebody)
$(trail 5)")" 1 'p .*FAIL rose'
run_case "five trailing directives stay code" p/a.go "$BASE" "$(gofile "$(basebody)
$(trail 5 'nolint:all' | sed 's|// nolint|//nolint|')")" 0 'p .* ok$'
run_case "a trailing comment does not join the blocks around it" p/a.go "$BASE" "$(grow "// one
var t = 1 // two
// three")" 0 ''

# Exemptions.
LIC='// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only'
run_case "the licence header is ignored" p/a.go "" "$LIC

$(gofile "$(code 5)")" 0 ''
run_case "the same header without SPDX is judged" p/a.go "" "// Copyright the docker-net-dhcp contributors.
// All rights reserved.

$(gofile "$(code 5)")" 1 'a.go:1: comment block of 2 lines'
run_case "a block after a commented package clause is not a licence header" p/a.go "" "package p // the package

// SPDX-License-Identifier is named here
// and on a second line
var x = 1" 1 'a.go:3: comment block of 2 lines carries no'
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
run_setup() { # NAME WANT_RC WANT_GREP BEFORE AFTER [ARGS...]
    local name="$1" want="$2" want_grep="$3" before="$4" after="$5" dir rc
    shift 5
    if [ "$#" -eq 0 ]; then set -- HEAD~1..HEAD; fi
    guarded_tmpdir dir
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir p
        "$before"; git add -A; git commit -qm base
        "$after"; git add -A; git commit -qm head
        bash "$GATE" "$@" > "$dir/out" 2>&1
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
# Each adds one comment line, so the share rule binds the package (#1057).
header_file() { printf '%s\n\n%s\n' "$LIC" "$(gofile "// one
$(code 40 | sed 's/^var v/var h/')")" > p/h.go; }
run_setup "a licence header in a new file stays out of the share" 0 'gate passed' lean header_file
doc_file() { printf '%s\npackage p\n\n// one\n%s\n' "$(cmt 12 '#9')" "$(code 40 | sed 's/^var v/var d/')" > p/doc.go; }
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

# The share rule binds only a package that gains a comment line (#1057).
drop_code() { gofile "$(basebody)" > p/a.go; }
base_more() { gofile "$(basebody)
$(code 10)" > p/a.go; }
run_setup "a share rise from deleting code alone passes" 0 'rose, no comment added' base_more drop_code
two_pkgs() { base_more; mkdir q; gofile "$(code 10)" > q/b.go; }
cmt_elsewhere() { drop_code; gofile "// one
$(code 10)" > q/b.go; }
run_setup "a comment added in another package does not bind this one" 1 'p .*rose, no comment added' two_pkgs cmt_elsewhere
one_cmt_drop() { gofile "$(basebody)
// one" > p/a.go; }
run_setup "a rise with one added comment line fails" 1 'p .*FAIL rose' base_more one_cmt_drop
swap_code() { gofile "$(basebody)
$(code 3 | sed 's/^var v/var n/')" > p/a.go; }
run_setup "a rise with added code and no comment line passes" 0 'rose, no comment added' base_more swap_code
doc_drop() { drop_code; printf '%s\npackage p\n' "$(cmt 12 '#9')" > p/doc.go; }
run_setup "an added package doc does not bind the share" 0 'rose, no comment added' base_more doc_drop
base_body() { printf '%s\n' "$BASE" > p/a.go; }
even() { gofile "$(basebody)
$(cmt 10 '#7')
$(code 11 | sed 's/^var v/var e/')" > p/a.go; }
run_setup "an equal share with an added comment line passes" 0 'p .* ok$' base_body even
line_doc() {
    printf '//line gen.go:90\n\n%s\npackage p\n' "$(cmt 12 '#9')" > p/doc.go
}
run_setup "a doc.go package doc below a //line directive is still exempt" 0 'gate passed' lean line_doc

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

# Whole mode (#1056): every line of the named files, added or not.
OLD11="$(gofile "$(code 20)
$(cmt 11 '#7')
var x = 1")"
run_case "whole mode fails an untouched 11-line block" p/a.go "$OLD11" "$OLD11
var y = 2" 1 'p/a.go:23: comment block of 11 lines' --whole HEAD
run_case "whole mode fails an untouched unreferenced block" p/a.go "$(grow "$(cmt 2)")" "$(grow "$(cmt 2)")
var y = 2" 1 'p/a.go:.*carries no' --whole HEAD~1 p
run_case "whole mode passes a clean file" p/a.go "$BASE" "$BASE
var y = 2" 0 'comment share per package' --whole HEAD
run_case "whole mode judges only the named paths" p/a.go "$OLD11" "$OLD11
var y = 2" 0 'q .*' --whole HEAD q/anchor.go
run_case "whole mode judges a named directory" p/a.go "$OLD11" "$OLD11
var y = 2" 1 'p/a.go:23' --whole HEAD p
run_case "whole mode judges a file named twice once" p/a.go "$OLD11" "$OLD11
var y = 2" 1 'whole: 1 file\(s\), 1 failure' --whole HEAD p p/a.go
run_case "whole mode exits 2 on a path with no Go file" p/a.go "$BASE" "$BASE
var y = 2" 2 'no Go file' --whole HEAD nosuch
run_case "whole mode exits 2 on an unresolvable revision" p/a.go "$BASE" "$BASE
var y = 2" 2 'cannot resolve' --whole nosuch

dir=""
guarded_tmpdir dir
(
    cd "$dir" || exit 2
    git init -q .
    git config user.email t@t; git config user.name t
    git config commit.gpgsign false
    mkdir -p p q; printf '%s\n' "$OLD11" > p/a.go; gofile "$(code 3)" > q/b.go; git add -A; git commit -qm one
    cd q || exit 2
    bash "$GATE" --whole HEAD p > "$dir/out" 2>&1
    echo $? > "$dir/rc"
) >/dev/null 2>&1
if [ "$(cat "$dir/rc" 2>/dev/null)" = 1 ] && grep -F 'p/a.go:23: comment block of 11 lines' "$dir/out" >/dev/null; then
    ok "whole mode from a subdirectory judges repository paths"
else
    no "whole mode from a subdirectory judges repository paths"
    sed 's/^/      /' "$dir/out" >&2
fi
rm -rf "$dir"

# Marked proof (#1056): the pull-request body reaches the gate as PR_BODY.
one_token() { lean; sed -i 's/^var v30 = 30$/var v30 = 31/' p/a.go; }
cmt_only() { lean; sed -i '/^\/\/ line 1 $/d' p/a.go; }
two_files() { lean; gofile "$(code 5 | sed 's/^var v/var b/')" > p/b.go; }
both_token() { two_files; sed -i 's/^var v30 = 30$/var v30 = 31/' p/a.go; sed -i 's/^var b5 = 5$/var b5 = 6/' p/b.go; }
MARK='Summary line.

Comments-only: yes'
PROVE=(--prove-marked HEAD~1 HEAD)
PR_BODY="$MARK" run_setup "a marked body with one changed token fails" 1 'DIFFERS  p/a.go' lean one_token "${PROVE[@]}"
PR_BODY="$MARK" run_setup "a marked body with a comment-only change passes" 0 'code tokens identical in 1 file' lean cmt_only "${PROVE[@]}"
PR_BODY="$MARK
Comments-only-except: p/a.go" run_setup "an excepted file is not proved" 0 'exempt +p/a.go' lean one_token "${PROVE[@]}"
PR_BODY="$MARK
Comments-only-except: p/a.go" run_setup "an except line exempts only the named file" 1 'DIFFERS  p/b.go' two_files both_token "${PROVE[@]}"
PR_BODY="$MARK
Comments-only-except: p/a" run_setup "an except path is not a prefix" 1 'DIFFERS  p/a.go' lean one_token "${PROVE[@]}"
PR_BODY="Summary line." run_setup "an unmarked body skips with its reason" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
run_setup "an unset body skips with its reason" 0 'SKIP, PR_BODY is not set' lean one_token "${PROVE[@]}"
PR_BODY="$MARK" run_setup "no revisions skip as not a pull request" 0 'SKIP, no pull request' lean one_token --prove-marked
PR_BODY="$MARK" run_setup "a marked proof with one revision exits 2" 2 'usage' lean one_token --prove-marked HEAD
PR_BODY='Comments-only: no' run_setup "a marker saying no skips" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
PR_BODY=$'Summary.\r\n\r\nComments-only: yes\r\n' run_setup "a CRLF body is read" 1 'DIFFERS  p/a.go' lean one_token "${PROVE[@]}"
PR_BODY='```
Comments-only: yes
```' run_setup "a marker in a backtick fence is ignored" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
PR_BODY='~~~
Comments-only: yes
~~~' run_setup "a marker in a tilde fence is ignored" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
PR_BODY='````
```
Comments-only: yes
````' run_setup "a shorter fence does not close a fence" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
PR_BODY='```
```go
Comments-only: yes
```' run_setup "a fence line with an info string does not close a fence" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
PR_BODY='> Comments-only: yes' run_setup "a quoted marker is ignored" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
PR_BODY='    Comments-only: yes' run_setup "an indented marker is ignored" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
PR_BODY='<!--
Comments-only: yes
-->' run_setup "a marker in an HTML comment is ignored" 0 'SKIP, the pull-request body has no' lean one_token "${PROVE[@]}"
PR_BODY='```
x
```
<!-- note -->
Comments-only: yes' run_setup "a marker after a closed fence and comment counts" 1 'DIFFERS  p/a.go' lean one_token "${PROVE[@]}"
PR_BODY='<!--
template note
-->
Comments-only: yes' run_setup "a marker after a closed HTML comment counts" 1 'DIFFERS  p/a.go' lean one_token "${PROVE[@]}"
PR_BODY="$MARK
\`\`\`
Comments-only-except: p/a.go
\`\`\`" run_setup "an except line in a fence is ignored" 1 'DIFFERS  p/a.go' lean one_token "${PROVE[@]}"

# A marked proof judges the pull request's own change: the base branch
# moved on after the fork with a code change of its own (#1056).
dir=""
guarded_tmpdir dir
(
    cd "$dir" || exit 2
    git init -q .
    git config user.email t@t; git config user.name t
    git config commit.gpgsign false
    mkdir p; lean; gofile "$(code 3 | sed 's/^var v/var b/')" > p/b.go; git add -A; git commit -qm fork
    git checkout -q -b topic
    cmt_only; git commit -qam topic
    git checkout -q -b moved HEAD~1
    sed -i 's/^var b3 = 3$/var b3 = 4/' p/b.go; git commit -qam moved
    PR_BODY="$MARK" bash "$GATE" --prove-marked moved topic > "$dir/out" 2>&1
    echo $? > "$dir/rc"
) >/dev/null 2>&1
if [ "$(cat "$dir/rc" 2>/dev/null)" = 0 ] && grep -F 'code tokens identical in 1 file' "$dir/out" >/dev/null; then
    ok "a marked proof judges from the merge base"
else
    no "a marked proof judges from the merge base"
    sed 's/^/      /' "$dir/out" >&2
fi
rm -rf "$dir"

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
