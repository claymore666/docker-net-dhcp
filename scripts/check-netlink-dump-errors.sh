#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every netlink dump-style call goes through util.DumpResult (#802).
#
# WHY THIS EXISTS
#
# vishvananda/netlink v1.3.1 returns ErrDumpInterrupted TOGETHER WITH A
# USABLE RESULT SET from every dump-style call. The sentinel means the
# kernel set NLM_F_DUMP_INTR because the table changed mid-dump, which
# a suite creating and tearing down macvlan children produces by design.
# Compared against nil it is a hard failure over results that were fine:
# it cost the arm64 lane a red on the v1.8.0-rc2 tag, and on the product
# side it made a mode-collision guard report a parent as free.
#
# The migration was a one-time act. This is the part that does not
# decay: the 26th call site goes red on the next push instead of the
# next time a link table changes under a dump.
#
# THE DOMAIN IS DERIVED, NOT LISTED
#
# Every tracked *.go file. A call is in the domain when it names a
# dump-style netlink function on a receiver (`x.LinkList(`) or through
# one of this repository's package-level seams (`nlAddrList(`). Method
# DECLARATIONS and interface members are not calls and carry no
# receiver dot, so `LinkList() ([]netlink.Link, error)` in an interface
# and `func (f fakeLinkLister) LinkList()` are outside it by
# construction rather than by an allowlist.
#
# THE RULE
#
# A call in the domain must appear on a line that also carries
# `DumpResult(` to its left. That is the shape every migrated site has:
#
#     links, err := util.DumpResult(netlink.LinkList())
#
# Anything else -- a bare call, a call whose results are assigned and
# checked against nil afterwards, a call split across lines -- is
# refused with its file:line. Keeping the wrapped call on one line is
# the cost, and it is the cost that makes the rule readable by grep
# rather than by a Go parser.
#
# WHAT IT CANNOT DO, said here rather than discovered later
#
#   * IT IS KEYED ON SPELLINGS -- the function names below and the
#     `nl` seam prefix. A netlink import renamed to something else, a
#     dump wrapped in a locally-named helper, or a v1.4 that adds a
#     dump call this list does not name all pass. The non-empty-domain
#     refusal below bounds that: it cannot silently become a gate over
#     nothing, but it does not make the list complete.
#   * IT DOES NOT READ THE HELPER. That util.DumpResult tolerates the
#     sentinel and refuses everything else is held by
#     pkg/util/netlink_dump_test.go, both directions, not by this.
#   * A LINE CARRYING THE WORD `DumpResult` IN A COMMENT BESIDE A BARE
#     CALL WOULD PASS. Driven in the self-test as the shape this rule
#     admits, so the bound is measured rather than assumed.
#
# Usage: check-netlink-dump-errors.sh [<repo root>]
# Exit:  0 clean, 1 a dump call does not go through the helper,
#        2 refuses to judge (no work tree, no Go files, an empty domain,
#          the helper missing).

set -uo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$ROOT" || { echo "check-netlink-dump-errors: cannot cd to $ROOT" >&2; exit 2; }

# The helper's own file. Excluded because it is where the sentinel is
# spelled out, and named because its absence means the migration this
# gate polices is not in the tree at all.
HELPER='pkg/util/netlink.go'

# The dump-style calls. Listed rather than pattern-matched on "List" so
# that a non-dump call ending in List cannot be swept in silently, and
# so a reader can check the list against the netlink package by eye.
DUMPS='LinkList|AddrList|RouteList|RouteListFiltered|NeighList|NeighProxyList|RuleList|ChainList|ClassList|FilterList|QdiscList|ConntrackTableList'

if ! git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "check-netlink-dump-errors: $ROOT is not a git work tree" >&2
    exit 2
fi

if [ ! -f "$HELPER" ]; then
    echo "check-netlink-dump-errors: $HELPER is missing, so the helper every call site is " \
         "required to use is not in this tree; refusing to judge" >&2
    exit 2
fi

mapfile -t GOFILES < <(git -C "$ROOT" ls-files '*.go' | grep -v -x "$HELPER")
if [ "${#GOFILES[@]}" -eq 0 ]; then
    echo "check-netlink-dump-errors: no tracked Go files outside the helper; refusing to judge" >&2
    exit 2
fi

# A call is `<something>.<Dump>(` or `nl<Dump>(`. The seam form is
# spelled without a dot on purpose: pkg/plugin/netlink_seam.go turns the
# package functions into package-level vars so unit tests can inject
# failures, and a rule that only saw the dotted form would let every
# seam call site out.
CALL_RE="(\.(${DUMPS})\(|\bnl(${DUMPS})\()"

# The same rule for awk, which is not grep. `\b` is a word boundary in
# GNU grep and is NOT one in every awk, and an `-v` assignment has its
# escape sequences processed BEFORE the regex is compiled, so `\(`
# reaches the matcher as a bare `(` and the expression stops parsing.
# gawk refuses it and mawk does not, which is a difference between a
# developer box and the runner. Written with bracket expressions so it
# carries no backslash at all, and passed through the environment,
# where nothing rewrites it.
AWK_RE="([.](${DUMPS})[(]|nl(${DUMPS})[(])"

ALL=$(grep -nEH "$CALL_RE" "${GOFILES[@]}" 2>/dev/null)
TOTAL=$(printf '%s' "$ALL" | grep -c . )

# A universal rule is satisfied by an empty domain. If nothing in this
# tree calls a netlink dump at all, this gate is judging nothing and
# says so rather than passing.
if [ "$TOTAL" -eq 0 ]; then
    echo "check-netlink-dump-errors: no netlink dump call found in any tracked Go file. " \
         "Either the spellings this gate knows are gone (an import rename, a new netlink " \
         "version) or the domain is empty; either way this gate is judging nothing." >&2
    exit 2
fi

# The rule. `DumpResult(` must open to the LEFT of the call, which is
# what wrapping means; a line carrying it to the right is not a wrap.
BAD=$(printf '%s\n' "$ALL" | AWK_RE="$AWK_RE" awk '
BEGIN { re = ENVIRON["AWK_RE"]; if (re == "") exit 3 }
{
    line = $0
    # Strip "file:lineno:" so a path containing DumpResult cannot pass a
    # call for us.
    i = index(line, ":")
    j = index(substr(line, i + 1), ":")
    head = substr(line, 1, i + j)
    code = substr(line, i + j + 1)
    w = index(code, "DumpResult(")
    if (w == 0) { print head code; next }
    if (match(code, re) == 0) next
    if (RSTART < w) print head code
}')
SCAN=$?

# The scan is the whole rule. If awk could not run it -- a regex this
# awk refuses, a missing interpreter -- the result is an empty BAD,
# which is indistinguishable from a clean tree. Read the status.
if [ "$SCAN" -ne 0 ]; then
    echo "check-netlink-dump-errors: the scan itself failed (awk exit $SCAN), so an empty " \
         "result means nothing; refusing to judge" >&2
    exit 2
fi

if [ -n "$BAD" ]; then
    echo "check-netlink-dump-errors: netlink dump call(s) not going through util.DumpResult (#802)." >&2
    echo "  netlink v1.3.1 returns ErrDumpInterrupted ALONGSIDE a usable result set; comparing" >&2
    echo "  the error against nil turns an ordinary mid-dump table change into a hard failure." >&2
    echo "  Write it as:  x, err := util.DumpResult(netlink.LinkList())" >&2
    printf '%s\n' "$BAD" | sed 's/^/    /' >&2
    echo "  $TOTAL dump call(s) examined." >&2
    exit 1
fi

echo "check-netlink-dump-errors: $TOTAL netlink dump call(s), all through util.DumpResult"
exit 0
