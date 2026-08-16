#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every netlink.LinkAdd in pkg/ must be accounted for in
# .github/linkadd-accounting.txt (#571).
#
# WHY THIS EXISTS
#
# A parent NIC registers one rx_handler, so it is a macvlan port or an
# ipvlan port and never both. Every child link attached to a parent has
# to be serialised against the other operations on that parent — that is
# Plugin.lockParent — or it can be refused with "device or resource
# busy", which reaches the user as a failed `docker run` caused by an
# unrelated container.
#
# WHAT IT CHECKS, AND WHAT THE COMPILER CHECKS INSTEAD
#
# Whether a guard is PRESENT at each LinkAdd is not this script's job
# and could not be done by a text search anyway — two of the sites take
# the lock several frames above the LinkAdd, and proving reachability
# from a caller needs a call graph. The type system does that half:
# addChildLink demands a *parentGuard, so a child link created through
# the helper has one as a matter of compilation.
#
# This script covers the two ways round that, and the second is the one
# that is easy to miss:
#
#  1. Call netlink.LinkAdd directly and skip the helper. Matched as the
#     SUPERSET — every netlink.LinkAdd in pkg/ however it is gated —
#     with each file required to carry a count and a justification.
#     Bridge networks legitimately need the direct call, so the bypass
#     cannot simply be banned; what stops is taking it without anyone
#     deciding to.
#
#  2. Forge a guard. parentGuard's zero value is valid Go, so
#     `addChildLink(&parentGuard{}, link)` compiles and holds nothing.
#     "Only lockParent constructs one" is not expressible in the type
#     system, so it is enforced below instead of asserted in a comment.
#
# Matching the superset rather than the interesting subset is the
# lesson from check-version-pins, which matched only well-formed pins
# and so could not see a broken one for months.
#
# Usage: check-parent-gate-accounting.sh [--root <dir>] [--manifest <file>]
# Exit: 0 accounted for, 1 drift, 2 bad usage.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
MANIFEST=""

while [ $# -gt 0 ]; do
    case "$1" in
        --root) ROOT="${2:-}"; shift 2 || exit 2 ;;
        --manifest) MANIFEST="${2:-}"; shift 2 || exit 2 ;;
        *) echo "usage: $0 [--root <dir>] [--manifest <file>]" >&2; exit 2 ;;
    esac
done
[ -n "$MANIFEST" ] || MANIFEST="$ROOT/.github/linkadd-accounting.txt"

if [ ! -f "$MANIFEST" ]; then
    echo "FAIL  accounting file missing: $MANIFEST" >&2
    exit 1
fi

# Validate the root before scanning it. Every check below reduces to a
# grep over $ROOT/pkg, so a wrong or missing root makes all of them
# match nothing and the gate reports success having examined no code —
# the failure this whole file exists to prevent, turned on itself.
if [ ! -d "$ROOT/pkg" ]; then
    echo "FAIL  no pkg/ directory under $ROOT" >&2
    echo "      Every check here is a grep over that tree, so without it this gate" >&2
    echo "      would pass having scanned nothing at all." >&2
    exit 2
fi

fail=0

# Actual sites, grouped by file. Test files are excluded on purpose: a
# LinkAdd in a test builds a fixture on a link the test owns, and is not
# the plugin contending for a shared parent.
actual="$(mktemp)"
trap 'rm -f "$actual" "$declared"' EXIT
declared="$(mktemp)"

( cd "$ROOT" && grep -rn "netlink\.LinkAdd(" pkg/ --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' \
    | cut -d: -f1 | sort | uniq -c \
    | awk '{print $2, $1}' ) > "$actual"

# Declared entries: "<path> <count>" at column 0, justification indented.
# An entry whose justification is empty is a bare path and fails — a
# list of paths with no reasons becomes a list somebody appends to.
awk '
    /^[[:space:]]*#/ { next }
    /^[^[:space:]]/ {
        if (path != "" && just == 0) { print "BARE " path > "/dev/stderr"; bad = 1 }
        path = $1; count = $2; just = 0
        if (count == "" || count !~ /^[0-9]+$/) { print "NOCOUNT " path > "/dev/stderr"; bad = 1 }
        print path, count
        next
    }
    /^[[:space:]]+[^[:space:]]/ { if (path != "") just = 1 }
    END {
        if (path != "" && just == 0) { print "BARE " path > "/dev/stderr"; bad = 1 }
        exit bad ? 3 : 0
    }
' "$MANIFEST" > "$declared" 2>"$declared.err"
awk_rc=$?

if [ "$awk_rc" -eq 3 ]; then
    while read -r kind path; do
        case "$kind" in
            BARE) echo "FAIL  $path is listed with no justification — say which gate covers it and why" ;;
            NOCOUNT) echo "FAIL  $path has no site count" ;;
        esac
        fail=1
    done < "$declared.err"
fi
rm -f "$declared.err"

# Every actual site accounted for, with the right count.
while read -r path count; do
    [ -n "$path" ] || continue
    want="$(awk -v p="$path" '$1 == p { print $2 }' "$declared")"
    if [ -z "$want" ]; then
        echo "FAIL  $path has $count netlink.LinkAdd site(s) and is not in $(basename "$MANIFEST")"
        echo "      A child link on a parent NIC must be serialised by Plugin.lockParent, or it"
        echo "      can be refused with EBUSY on an operation the user did not cause. Add an entry"
        echo "      naming the gate that covers it — and if nothing does, that is the bug, not this gate."
        fail=1
    elif [ "$want" != "$count" ]; then
        echo "FAIL  $path has $count netlink.LinkAdd site(s), accounted for $want"
        echo "      A site was added or removed. Re-read which gate covers each one before"
        echo "      updating the count; the count is the prompt, the justification is the point."
        fail=1
    fi
done < "$actual"

# Entries for files that no longer have any site: stale, and a stale
# justification is worse than none — it describes code that is gone.
while read -r path count; do
    [ -n "$path" ] || continue
    if ! awk -v p="$path" '$1 == p { found = 1 } END { exit found ? 0 : 1 }' "$actual"; then
        echo "FAIL  $path is accounted for ($count site(s)) but has no netlink.LinkAdd left"
        echo "      Remove the entry: a justification for code that no longer exists reads as"
        echo "      current and is the kind of prose that decays silently."
        fail=1
    fi
done < "$declared"

# A guard must come from lockParent — and THAT half is not a compiler
# guarantee, which is the part most likely to be believed anyway.
#
# parentGuard's zero value is valid Go, so
#
#     addChildLink(&parentGuard{}, link)
#
# compiles and takes no lock at all. lockParent returns exactly that
# literal for the no-parent case, so the shape is already in the file as
# a legitimate-looking pattern to copy. The realistic route to it is not
# malice: someone adds a parent-attached call site, the compiler asks
# for a guard, they do not know where one comes from, and the zero value
# is right there and builds.
#
# Checked here because Go cannot say "only this function may construct
# this type". Test files are excluded for the same reason as above — a
# unit test building one is exercising the type's contract, not
# attaching a child link to a contended parent.
forged="$(grep -rn "parentGuard" "$ROOT/pkg" --include='*.go' 2>/dev/null \
    | grep -v '_test\.go:' \
    | grep -v 'parent_gate\.go:' \
    | grep -v '\*parentGuard' \
    | sed "s|^$ROOT/||" || true)"
if [ -n "$forged" ]; then
    echo "FAIL  a parentGuard is built outside the file that owns it:"
    printf '%s\n' "$forged" | sed 's/^/      /'
    echo "      A guard is supposed to be EVIDENCE that lockParent was called. One built"
    echo "      any other way is evidence of nothing, and addChildLink cannot tell the two"
    echo "      apart. Take the gate instead: g := p.lockParent(ctx, parent, \"<op>\")."
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    exit 1
fi

total="$(awk '{ s += $2 } END { print s + 0 }' "$actual")"
files="$(wc -l < "$actual" | tr -d ' ')"

# Zero sites is not a clean tree, it is a broken pattern. The plugin
# attaches links; if this ever finds none, the grep has stopped
# matching and every check above passed over an empty set.
if [ "$total" -eq 0 ]; then
    echo "FAIL  no netlink.LinkAdd found anywhere in $ROOT/pkg" >&2
    echo "      The plugin creates links, so this means the pattern has stopped matching" >&2
    echo "      and every check above ran against nothing. Fix the gate, not the tree." >&2
    exit 1
fi
echo "parent-gate accounting: OK — $total netlink.LinkAdd site(s) across $files file(s), all accounted for."
echo "NOTE: this does NOT prove the gate is held — addChildLink taking a *parentGuard is what"
echo "      proves that. This covers the two ways round it: a direct netlink.LinkAdd, and a"
echo "      parentGuard forged outside parent_gate.go, which the compiler cannot refuse (#571)."
