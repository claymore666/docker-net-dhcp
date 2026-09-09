#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-netlink-dump-errors.sh (#802).
#
# The emptiable cases come first, because a universal rule is satisfied
# by emptying its domain: no work tree, no helper, no Go files and no
# dump call anywhere must each REFUSE (exit 2) rather than pass. A gate
# that reports success having examined nothing is the defect this file
# exists to make impossible, and it is the mutation control the issue
# asked for by name.
#
# The DRIVE-THE-ABSENCE case is the last one: it copies the SHIPPED
# tree, removes the wrap from one real call site, and requires the gate
# to name that file:line. A green self-test over synthetic trees alone
# would not say the gate works on the thing it guards.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-netlink-dump-errors.sh"
REPO="$(cd "$HERE/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
failures=0

# A synthetic tracked tree. The helper file is created because the gate
# refuses without it; its contents are never read.
tree() { # <name>
    local d="$TMP/$1"
    mkdir -p "$d/pkg/util" "$d/pkg/plugin"
    printf 'package util\n' > "$d/pkg/util/netlink.go"
    git -C "$d" init -q
    git -C "$d" config user.email t@example.invalid
    git -C "$d" config user.name t
    echo "$d"
}

track() { # <tree>
    git -C "$1" add -A >/dev/null 2>&1
}

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

check "a root that does not exist" 2 "$TMP/nowhere-at-all" "cannot cd to"

mkdir -p "$TMP/notgit/pkg/util"
printf 'package util\n' > "$TMP/notgit/pkg/util/netlink.go"
check "a directory that is not a work tree" 2 "$TMP/notgit" "not a git work tree"

d=$(tree nohelper); rm -f "$d/pkg/util/netlink.go"; printf 'package p\n' > "$d/pkg/plugin/a.go"; track "$d"
check "the helper is missing" 2 "$d" "is missing"

d=$(tree nogo); rm -f "$d/pkg/plugin"/*.go; track "$d"
check "no tracked Go file outside the helper" 2 "$d" "refusing to judge"

d=$(tree nodumps)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

func f() { _ = netlink.LinkByName("eth0") }
GO
track "$d"
check "no dump call anywhere refuses rather than passing" 2 "$d" "judging nothing"

# --- the rule ---------------------------------------------------------

d=$(tree wrapped)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

func f() {
	links, err := util.DumpResult(netlink.LinkList())
	addrs, err2 := util.DumpResult(nlAddrList(link, 2))
	_, _, _, _ = links, err, addrs, err2
}
GO
track "$d"
check "wrapped calls pass" 0 "$d" "2 netlink dump call"

d=$(tree bare)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

func f() {
	links, err := netlink.LinkList()
	_, _ = links, err
}
GO
track "$d"
check "a bare dotted call is refused" 1 "$d" "pkg/plugin/a.go:4"

d=$(tree bareseam)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

func f() {
	links, err := nlLinkList()
	_, _ = links, err
}
GO
track "$d"
check "a bare seam call is refused" 1 "$d" "pkg/plugin/a.go:4"

# The domain control. A method DECLARATION and an interface member are
# not calls: they carry no receiver dot and no seam prefix, so they must
# NOT be swept in -- and because the file then contains no call at all,
# the gate refuses rather than passing, which is the same guard as the
# empty-domain case reached from the other side.
d=$(tree decls)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

type linkLister interface {
	LinkList() ([]netlink.Link, error)
}

func (f fakeLinkLister) LinkList() ([]netlink.Link, error) { return nil, nil }
GO
track "$d"
check "declarations are not calls" 2 "$d" "judging nothing"

# The side rule: DumpResult must open to the LEFT of the call. A line
# that mentions it afterwards is not a wrap.
d=$(tree rightside)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

func f() {
	links, err := netlink.LinkList() // should use util.DumpResult(...)
	_, _ = links, err
}
GO
track "$d"
check "a mention to the right of the call is not a wrap" 1 "$d" "pkg/plugin/a.go:4"

# The bound this gate cannot close, driven rather than asserted in
# prose: a comment carrying DumpResult( BEFORE a bare call passes.
d=$(tree leftcomment)
cat > "$d/pkg/plugin/a.go" <<'GO'
package plugin

func f() {
	/* util.DumpResult( */ links, err := netlink.LinkList()
	_, _ = links, err
}
GO
track "$d"
check "KNOWN BOUND: a mention to the left admits a bare call" 0 "$d" "1 netlink dump call"

# --- drive the absence, on the shipped tree ---------------------------

real="$TMP/real"
mkdir -p "$real"
git -C "$REPO" ls-files -z '*.go' pkg/util/netlink.go | while IFS= read -r -d '' f; do
    mkdir -p "$real/$(dirname "$f")"
    cp "$REPO/$f" "$real/$f"
done
git -C "$real" init -q
git -C "$real" config user.email t@example.invalid
git -C "$real" config user.name t
track "$real"
check "the shipped tree is clean" 0 "$real" "all through util.DumpResult"

# The scan cannot fail into a pass. awk is stubbed to fail over the
# CLEAN shipped tree, where a gate that ignored the scan's status would
# print "all through util.DumpResult" and exit 0. This is not a
# hypothetical: the first version passed the expression with `awk -v`,
# whose escape processing left the regex unparseable, and gawk on the
# runner died on it while mawk on the author's box did not.
stub="$TMP/stub"
mkdir -p "$stub"
printf '#!/bin/sh\nexit 1\n' > "$stub/awk"
chmod +x "$stub/awk"
saved_path="$PATH"
PATH="$stub:$PATH"
check "a failing scan refuses rather than passing" 2 "$real" "the scan itself failed"
PATH="$saved_path"

# One real site loses its wrap. The line number is DERIVED from the
# shipped file rather than typed, so a rewrite of parent_attached.go
# moves the expectation with it instead of turning this case into a
# constant that passes for the wrong reason.
victim='pkg/plugin/parent_attached.go'
vline=$(grep -n 'util.DumpResult(nlLinkList())' "$real/$victim" | head -n 1 | cut -d: -f1)
if [ -z "$vline" ]; then
    echo "FAIL drive-the-absence: no wrapped nlLinkList() call found in $victim"
    failures=$((failures + 1))
else
    sed -i "${vline}s/util\.DumpResult(nlLinkList())/nlLinkList()/" "$real/$victim"
    track "$real"
    check "the shipped tree with one wrap removed is refused" 1 "$real" "$victim:$vline"
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures case(s) failed"
    exit 1
fi
echo "all cases passed"
