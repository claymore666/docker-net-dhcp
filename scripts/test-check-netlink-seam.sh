#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-netlink-seam.sh (#657).
#
# Refusals come first: a universal rule is satisfied by an empty domain, so a missing seam file, a seam with
# no variable and no production file must each exit 2, never pass. Then the verdicts on synthetic trees in
# both directions, the bounds the header states (measured, so they are not assumed), and last the case that
# says the gate works on the thing it guards: the SHIPPED tree, one routed call turned back into a direct one.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
GATE="$HERE/check-netlink-seam.sh"
guarded_tmpdir TMP
failures=0

SEAM_GO=$'package plugin\n\nimport "github.com/vishvananda/netlink"\n\nvar nlLinkAdd = netlink.LinkAdd\n'

# A synthetic tree with a valid seam file; the caller adds files, then tracks them.
tree() { # <name>
    local d="$TMP/$1"
    mkdir -p "$d/pkg/plugin"
    printf '%s' "$SEAM_GO" > "$d/pkg/plugin/netlink_seam.go"
    git -C "$d" init -q
    echo "$d"
}

track() { git -C "$1" add -A >/dev/null 2>&1; }

check() { # <name> <want-exit> <root> <want-grep>
    local name="$1" want_exit="$2" root="$3" want_grep="$4"
    bash "$GATE" "$root" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && { [ -z "$want_grep" ] || grep -q -- "$want_grep" "$TMP/out"; }; then
        echo "ok   $name"
    else
        echo "FAIL $name: exit $got (want $want_exit), grep '$want_grep'"
        sed 's/^/       /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# --- refusals: the domain cannot be emptied into a pass ---------------

check "a root that does not exist" 2 "$TMP/nowhere-at-all" ""

mkdir -p "$TMP/notgit/pkg/plugin"
printf '%s' "$SEAM_GO" > "$TMP/notgit/pkg/plugin/netlink_seam.go"
check "a directory that is not a work tree" 2 "$TMP/notgit" "not in a git work tree"

d=$(tree noseam); rm -f "$d/pkg/plugin/netlink_seam.go"
printf 'package plugin\n' > "$d/pkg/plugin/a.go"; track "$d"
check "the seam file is missing" 2 "$d" "does not exist"

d=$(tree novar); printf 'package plugin\n' > "$d/pkg/plugin/netlink_seam.go"
printf 'package plugin\n' > "$d/pkg/plugin/a.go"; track "$d"
check "the seam file carries no variable" 2 "$d" "no seam to route through"

d=$(tree nofiles); track "$d"
check "no production file besides the seam" 2 "$d" "would have read nothing"

# --- verdicts ----------------------------------------------------------

d=$(tree clean)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

func f() error { return nlLinkAdd(nil) }
GO
track "$d"
check "a file routed through the seam passes" 0 "$d" "PASS"

d=$(tree direct)
printf 'package plugin\n\nfunc f() error {\n\treturn netlink.LinkAdd(nil)\n}\n' > "$d/pkg/plugin/a.go"; track "$d"
check "a direct kernel call is named by file:line and function" 1 "$d" "pkg/plugin/a.go:4:LinkAdd"

d=$(tree untracked)
printf 'package plugin\n\nfunc f() { _ = netlink.LinkByName("x") }\n' > "$d/pkg/plugin/a.go"
git -C "$d" add pkg/plugin/netlink_seam.go >/dev/null 2>&1
check "an untracked, not ignored file is in the domain" 1 "$d" "a.go:3:LinkByName"

d=$(tree unnamed)
printf 'package plugin\n\nfunc f() { _, _ = netlink.NeighList(0, 0) }\n' > "$d/pkg/plugin/a.go"; track "$d"
check "a netlink function nobody listed is refused" 1 "$d" "a.go:3:NeighList"

d=$(tree twoone)
printf 'package plugin\n\nfunc f() { _, _ = netlink.ParseAddr("x"); _ = netlink.LinkDel(nil) }\n' > "$d/pkg/plugin/a.go"; track "$d"
check "a kernel call after a pure call on one line" 1 "$d" "a.go:3:LinkDel"

d=$(tree twokernel)
printf 'package plugin\n\nfunc f() { _ = netlink.LinkAdd(nil); _ = netlink.LinkDel(nil) }\n' > "$d/pkg/plugin/a.go"; track "$d"
check "two kernel calls on one line are both named" 1 "$d" "a.go:3:LinkDel"

d=$(tree trailing)
printf 'package plugin\n\nfunc f() { _ = netlink.LinkDel(nil) } // routed elsewhere\n' > "$d/pkg/plugin/a.go"; track "$d"
check "a trailing comment does not hide the call before it" 1 "$d" "a.go:3:LinkDel"

# --- the other direction: what passed before still passes --------------

d=$(tree pure)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

func f() {
	la := netlink.NewLinkAttrs()
	_ = &netlink.Veth{LinkAttrs: la}
	_, _ = netlink.ParseAddr("192.0.2.1/24")
	_, _ = netlink.ParseIPNet("192.0.2.0/24")
	var l netlink.Link
	_ = l
}
GO
track "$d"
check "constructors, parsers and type names pass" 0 "$d" "PASS"

d=$(tree comment)
printf 'package plugin\n\n// the old code called netlink.LinkAdd(link) here\nfunc f() {}\n' > "$d/pkg/plugin/a.go"; track "$d"
check "a comment naming a call passes" 0 "$d" "PASS"

d=$(tree testfile)
printf 'package plugin\n\nfunc f() { _ = netlink.LinkAdd(nil) }\n' > "$d/pkg/plugin/a_test.go"
printf 'package plugin\n' > "$d/pkg/plugin/b.go"; track "$d"
check "a _test.go file is not production code" 0 "$d" "PASS"

d=$(tree outside)
mkdir -p "$d/pkg/util"
printf 'package util\n\nfunc f() { _ = netlink.LinkAdd(nil) }\n' > "$d/pkg/util/a.go"
printf 'package plugin\n' > "$d/pkg/plugin/b.go"; track "$d"
check "a file outside pkg/plugin is not in the domain" 0 "$d" "PASS"

d=$(tree seamcalls)
printf '%s\nfunc g() { _ = netlink.LinkAdd(nil) }\n' "$SEAM_GO" > "$d/pkg/plugin/netlink_seam.go"
printf 'package plugin\n' > "$d/pkg/plugin/a.go"; track "$d"
check "the seam file itself may call the package" 0 "$d" "PASS"

# --- the bounds the header states, measured ---------------------------

d=$(tree value)
printf 'package plugin\n\nvar add = netlink.LinkAdd\n' > "$d/pkg/plugin/a.go"; track "$d"
check "BOUND: a function value taken without a call passes" 0 "$d" "PASS"

d=$(tree handle)
printf 'package plugin\n\nfunc f(h *netlink.Handle) error { return h.LinkAdd(nil) }\n' > "$d/pkg/plugin/a.go"; track "$d"
check "BOUND: a method on a handle variable passes" 0 "$d" "PASS"

d=$(tree strlit)
printf 'package plugin\n\nvar s = "netlink.LinkAdd(x)"\n' > "$d/pkg/plugin/a.go"; track "$d"
check "BOUND: a string literal spelling a call is a loud false positive" 1 "$d" "a.go:3:LinkAdd"

# --- the shipped tree, and one routed call turned back ------------------

real="$TMP/real"
mkdir -p "$real"
git init -q "$real"
cp -r "$REPO/pkg" "$real/"
track "$real"
check "the shipped tree passes" 0 "$real" "PASS"

target=$(grep -n 'nlLinkAdd(' "$real/pkg/plugin/parent_gate.go" | head -1 | cut -d: -f1)
if [ -z "$target" ]; then
    echo "FAIL the shipped tree no longer routes LinkAdd in parent_gate.go; re-anchor this case"
    failures=$((failures + 1))
else
    sed -i "${target}s/nlLinkAdd(/netlink.LinkAdd(/" "$real/pkg/plugin/parent_gate.go"
    check "the shipped tree, one routed call turned back, is refused" 1 "$real" "parent_gate.go:${target}:LinkAdd"
fi

[ "$failures" -eq 0 ] || { echo "$failures case(s) failed"; exit 1; }
echo "all cases passed"
