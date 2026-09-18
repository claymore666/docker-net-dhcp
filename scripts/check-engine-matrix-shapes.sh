#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every documented `docker network create` has a row in the engine
# matrix (#1013).
#
# WHAT WENT WRONG WITHOUT IT. docs/reference.md has promised
# `--ipam-driver <this plugin>` since v2.1.0, and every network the
# matrix created was created with `--ipam-driver null`, so that shape
# had never run on any engine in this project. It was broken on every
# engine below 28 the whole time (#1012), and nothing here could say so
# because nothing here drove it.
#
# WHAT THIS ASKS. scripts/engine-baseline.sh derives the networks it
# drives from the document, so the hole cannot come back as a list
# somebody forgot to extend. This gate is the half of that which runs on
# a pull request: the matrix runs on its own lane, and a document that
# gains a shape the cell cannot drive would otherwise sit green until
# that lane next ran.
#
# Three questions, and the last two are the ones that catch a silent
# drop:
#
#   1. the derivation SUCCEEDS and yields at least one shape. It refuses
#      a documented block it cannot turn into a network, by design, and
#      that refusal has to reach a check somebody reads.
#   2. every documented create is ACCOUNTED FOR. The population here is
#      lines, counted by this file with its own reading of the document;
#      the derivation's population is commands. A block the parser stops
#      seeing at all -- a fence it mis-tracks, a continuation it joins
#      wrongly -- changes the second population and not the first, so
#      the two are compared rather than one being trusted.
#   3. BOTH IPAM DRIVERS ARE STILL IN THE SET. Derivation makes the list
#      follow the document, which also means an edit to the document can
#      empty a whole half of it: reword the one `--ipam-driver
#      <this plugin>` example and every shape becomes a null-IPAM one,
#      the derivation still succeeds, the accounting still balances, and
#      no engine creates an IPAM-mode network again. That is the hole
#      this gate exists to close, so it is named here as a floor rather
#      than left to follow from the prose. The same floor holds the
#      other way: a document with no null-IPAM network would stop the
#      matrix from driving Docker's own IPAM beside the plugin's.
#
# Env (the seams scripts/test-engine-matrix-shapes.sh drives):
#   ENGINE_SHAPES_DOC   the document (default docs/reference.md)
#   ENGINE_SHAPES_CMD   the command that prints the derivation's sources
#                       (default the baseline cell's own)
#
# Exit: 0 every documented shape is driven, 1 one is not, 2 cannot check.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

DOC="${ENGINE_SHAPES_DOC:-docs/reference.md}"
CMD="${ENGINE_SHAPES_CMD:-bash scripts/engine-baseline.sh --print-shape-sources}"

[ -f "$DOC" ] || {
    echo "FAIL  $DOC does not exist; there is nothing to derive the matrix shapes from." >&2
    exit 2
}

# The derivation, with its refusals shown. A non-zero status is a
# documented block the cell cannot drive, and the reason is on stderr
# with the line it came from.
sources="$(ENGINE_SHAPES_DOC="$DOC" $CMD)"
rc=$?
if [ "$rc" -ne 0 ]; then
    echo "FAIL  the engine matrix cannot derive its networks from $DOC (status $rc)." >&2
    echo "  The message above names the block. Either the example is not a shape the" >&2
    echo "  plugin supports, or the cell needs to learn it; a documented network that" >&2
    echo "  nothing drives has no engine floor." >&2
    exit 1
fi

shapes="$(ENGINE_SHAPES_DOC="$DOC" bash scripts/engine-baseline.sh --print-shapes)" || {
    echo "FAIL  the shape list could not be printed from $DOC." >&2
    exit 1
}
if [ -z "$shapes" ]; then
    echo "FAIL  $DOC documents no network for this plugin." >&2
    echo "  A matrix with no shape to drive passes every row having measured nothing." >&2
    exit 1
fi

fail=0

# The floor (question 3). `<ipam>|<mode>|<interface>` is the derivation's
# shape spelling, and its first field is the IPAM driver the documented
# command named: `plugin` for this plugin, `null` for Docker's own. Both
# have to survive whatever the document says today.
for want in null plugin; do
    if ! grep -q "^$want|" <<< "$shapes"; then
        case "$want" in
        plugin) what="with \`--ipam-driver\` naming this plugin" ;;
        *) what="leaving IPAM to Docker" ;;
        esac
        echo "FAIL  $DOC documents no network $what." >&2
        echo "  The engine matrix drives what this document shows, so a shape that leaves" >&2
        echo "  the document leaves every engine row with it. Issue #1013 was exactly this:" >&2
        echo "  no engine below 29 had ever created a network with the plugin as its IPAM" >&2
        echo "  driver, and #1012 was broken on all of them and reported by a user. Keep an" >&2
        echo "  example of each in $DOC, or the matrix measures half of what ships." >&2
        fail=1
    fi
done

# The second population, read here and not taken from the derivation: a
# command line inside a fenced block. Indentation is allowed; prose that
# mentions the command mid-sentence is not a command line.
while IFS=: read -r lineno _; do
    [ -n "$lineno" ] || continue
    if ! grep -qE "^$lineno (in-scope|out-of-scope) " <<< "$sources"; then
        echo "FAIL  $DOC:$lineno documents a \`docker network create\` the matrix never saw." >&2
        echo "  The derivation in scripts/engine-baseline.sh did not report this line at" >&2
        echo "  all, so the shape it describes is driven on no engine." >&2
        fail=1
    fi
done < <(awk '
    /^[ \t]*```/ { inblk = 1 - inblk; next }
    inblk == 1 && /^[ \t]*docker network create/ { printf("%d:\n", NR) }
' "$DOC")

[ "$fail" -eq 0 ] || exit 1

echo "OK    $DOC documents $(wc -l <<< "$sources") network commands; the matrix drives $(wc -l <<< "$shapes") distinct shapes:"
awk '{ print "        " $0 }' <<< "$shapes"
exit 0
