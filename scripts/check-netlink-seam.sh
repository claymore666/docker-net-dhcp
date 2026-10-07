#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# pkg/plugin production code reaches the kernel through the netlink seam, never through the netlink package (#657).
#
# Expires-when: pkg/plugin stops calling the vishvananda/netlink package, or its unit tests stop
#   injecting netlink failures through package-level seam variables (#657, #220).
#
# WHY THIS EXISTS
#
# pkg/plugin/netlink_seam.go (v1.2.0, #220) turns the package-level netlink functions into variables so a unit
# test can inject a failure on a path that otherwise needs CAP_NET_ADMIN and a live namespace. Nothing made a
# call site use it: #657 measured 40 direct calls against 9 seam uses, and CreateEndpoint and
# createParentAttachedEndpoint, the two most complex functions in the package, were unreachable from a unit test
# for that reason alone. The routing was a one-time act; this is the part that does not decay.
#
# THE DOMAIN IS DERIVED, NOT LISTED
#
# Every tracked or untracked-not-ignored non-test .go file under pkg/plugin/, except netlink_seam.go (gatelib
# class go-src). A call is `netlink.<Name>(`, with whitespace or a newline allowed after the dot. Comments
# (`//` and `/* */`) and string, raw-string and rune literals are not code and are blanked before the match, so
# a `//` inside a string does not hide the call after it and a string naming a call is not a call.
#
# THE RULE
#
# A call whose <Name> is not in PURE below is refused with its file:line. PURE holds the netlink functions that
# build or parse a value and never touch the kernel, so there is nothing for a seam to inject.
#
# WHAT IT CANNOT DO, said here rather than discovered later
#
#   * IT IS KEYED ON THE SPELLING `netlink.<Name>(`. An import renamed to another identifier, and a function
#     value taken without a call (`f := netlink.LinkAdd`), pass.
#   * IT DOES NOT SEE METHODS on a *netlink.Handle. m.netHandle is a struct field and is the second seam: a
#     test builds the manager with its own handle. A handle method called on any other variable passes.
#   * IT DOES NOT SEE the raw request API in the nl package (nftForwardPolicy), which has its own seam.
#   * THE LEXER IS AWK, NOT THE GO PARSER. It knows `//`, `/* */`, "...", `...` and '.' and nothing else: a
#     build-tag-excluded file is still read, and a generated file is read like any other.
#
# Usage: check-netlink-seam.sh [<tree>]
# Exit:  0 clean, 1 a direct call, 2 refuses to judge (no work tree, no seam file, an empty domain).

set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

cd "$(dirname "$0")/.." || exit 2
cd "${1:-.}" || exit 2

SEAM='pkg/plugin/netlink_seam.go'
# netlink functions that construct or parse a value and never reach the kernel.
PURE='NewLinkAttrs|ParseAddr|ParseIPNet'

[ -f "$SEAM" ] || gate_refuse "$SEAM does not exist, so the seam this gate enforces is not in the tree"
grep -qE '^[[:space:]]*(var[[:space:]]+)?nl[A-Z][A-Za-z]*[[:space:]]*=[[:space:]]*netlink\.[A-Z]' "$SEAM" \
    || gate_refuse "$SEAM carries no 'nlName = netlink.Name' variable; there is no seam to route through"

gate_subjects gofiles go-src
mapfile -t gofiles < <(printf '%s\n' "${gofiles[@]}" | grep -E '^pkg/plugin/' | grep -v -x "$SEAM")
[ "${#gofiles[@]}" -gt 0 ] || gate_refuse "no production Go file under pkg/plugin/; a pass here would have read nothing"

# Blank comments and string, raw-string and rune literals (state carries across lines), then take every
# `netlink.<Name>(` in the whole file, so a call split after the dot is read too. awk, not grep -o, so the
# file:line survives.
hits=$(PURE_RE="^(${PURE})\$" awk '
    function nlcount(t) { return gsub(/\n/, "", t) }
    function flush(   rest, off, tok, idx, pos) {
        rest = buf; off = 0
        while (match(rest, /(^|[^A-Za-z0-9_.])netlink\.[ \t\n]*[A-Z][A-Za-z0-9]*[ \t\n]*\(/)) {
            tok = substr(rest, RSTART, RLENGTH)
            idx = index(tok, "netlink.")
            pos = off + RSTART - 1 + idx
            sub(/^.*netlink\.[ \t\n]*/, "", tok)
            sub(/[ \t\n]*\($/, "", tok)
            if (tok !~ pure) print fname ":" (1 + nlcount(substr(buf, 1, pos - 1))) ":" tok
            off += RSTART + RLENGTH - 1
            rest = substr(rest, RSTART + RLENGTH)
        }
        buf = ""
    }
    BEGIN { pure = ENVIRON["PURE_RE"]; if (pure == "") exit 3 }
    FNR == 1 { if (NR > 1) flush(); fname = FILENAME; in_block = 0; in_raw = 0 }
    {
        line = $0; n = length(line); out = ""; i = 1
        while (i <= n) {
            ch = substr(line, i, 1); two = substr(line, i, 2)
            if (in_block) {
                if (two == "*/") { in_block = 0; out = out "  "; i += 2 } else { out = out " "; i++ }
                continue
            }
            if (in_raw) { if (ch == "`") in_raw = 0; out = out " "; i++; continue }
            if (two == "//") break
            if (two == "/*") { in_block = 1; out = out "  "; i += 2; continue }
            if (ch == "`") { in_raw = 1; out = out " "; i++; continue }
            if (ch == "\"" || ch == "\047") {
                q = ch; out = out " "; i++
                while (i <= n) {
                    c = substr(line, i, 1)
                    if (c == "\\") { out = out "  "; i += 2; continue }
                    out = out " "; i++
                    if (c == q) break
                }
                continue
            }
            out = out ch; i++
        }
        buf = buf out "\n"
    }
    END { flush() }' "${gofiles[@]}") || gate_refuse "the scan itself failed, so an empty result would mean nothing"

if [ -n "$hits" ]; then
    echo "FAIL  pkg/plugin calls the netlink package directly instead of the seam in $SEAM (#657):" >&2
    printf '%s\n' "$hits" | sed 's/^/  /' >&2
    cat >&2 <<'MSG'

  Add `nl<Name> = netlink.<Name>` to netlink_seam.go (or use the variable that is already there) and call
  that, so a unit test can inject the failure without CAP_NET_ADMIN. A function that only builds or parses a
  value, and never reaches the kernel, goes into PURE at the top of this script.
MSG
    exit 1
fi

echo "PASS  no pkg/plugin production file calls the netlink package outside the seam (${#gofiles[@]} files read)"
