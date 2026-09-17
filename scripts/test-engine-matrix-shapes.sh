#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-engine-matrix-shapes.sh and for the derivation it
# reads (#1013).
#
# THE VERDICT THIS GATE MUST NEVER PRODUCE is a clean pass over a
# document whose shapes nothing drives, which is the state the project
# was in for two releases. So every red case here has a green twin that
# differs in one block, and the two halves of the gate are driven
# separately: a shape the derivation REFUSES, and a shape the derivation
# silently DROPS. They fail differently and only one of them shows up in
# the derivation's own exit status.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-engine-matrix-shapes.sh"
CELL="$HERE/engine-baseline.sh"
DOC="$HERE/../docs/reference.md"

tmp=""
pass=0
fail=0

# check <label> <want-exit> <want-substring> <output> <got-exit>
check() {
    local label="$1" want="$2" needle="$3" out="$4" got="$5"
    if [ "$got" -eq "$want" ] && printf '%s' "$out" | grep -F "$needle" >/dev/null; then
        echo "ok    $label"
        pass=$((pass + 1))
    else
        echo "FAIL  $label: exit $got (want $want), output '$out'"
        fail=$((fail + 1))
    fi
}

# doc_with <file> <extra-block> writes a copy of the shipped document
# with one more fenced block appended. A COPY OF THE REAL DOCUMENT and
# not a hand-written stub: a stub would drive the parser against prose
# nobody ships, and the case that matters is a new example landing in
# the file that is really read.
doc_with() {
    local dest="$1" extra="$2"
    cp "$DOC" "$dest"
    {
        printf '\n```bash\n'
        printf '%s\n' "$extra"
        printf '```\n'
    } >> "$dest"
}

# ---- the tree as shipped ---------------------------------------------
out="$(bash "$GATE" 2>&1)"; got=$?
check "the tree as shipped is green" 0 "the matrix drives" "$out" "$got"

# The shape this whole change is about has to be IN that answer. Without
# this line the gate passes on a derivation that lost the IPAM shape
# again, which is the defect it was written for.
out="$(bash "$CELL" --print-shapes 2>&1)"; got=$?
check "the documented IPAM-driver network is one of the derived shapes" 0 "plugin|macvlan|parent" "$out" "$got"

# ---- a documented shape dropped from the derivation -------------------
# INJECTED INTO THE REAL DERIVATION'S OUTPUT, not into a copy of it: the
# command runs the shipped cell and removes its last line, which is
# exactly what a parser that stopped seeing the final block would
# produce. The gate reads the document itself for its own population, so
# it has to notice.
guarded_tmpdir tmp
cat > "$tmp/dropper.sh" <<'DROP'
#!/usr/bin/env bash
out="$(bash "$1" --print-shape-sources)" || exit $?
printf '%s\n' "$out" | sed '$d'
DROP
out="$(ENGINE_SHAPES_CMD="bash $tmp/dropper.sh $CELL" bash "$GATE" 2>&1)"; got=$?
check "a documented shape missing from the derivation is red" 1 "the matrix never saw" "$out" "$got"
rm -rf "$tmp"

# ---- a documented shape the cell cannot drive -------------------------
guarded_tmpdir tmp
doc_with "$tmp/mode.md" 'docker network create -d ghcr.io/claymore666/docker-net-dhcp:v9.9.9 \
    --ipam-driver null \
    -o mode=teleport -o parent=eth0 \
    lan-dhcp'
out="$(ENGINE_SHAPES_DOC="$tmp/mode.md" bash "$GATE" 2>&1)"; got=$?
check "a documented network mode the plugin does not offer is red" 1 "mode=teleport" "$out" "$got"

# The twin: the same appended block, one field different, and the gate
# has to go green. Without it the case above is satisfied by a gate that
# refuses every document it did not ship with.
doc_with "$tmp/good.md" 'docker network create -d ghcr.io/claymore666/docker-net-dhcp:v9.9.9 \
    --ipam-driver null \
    -o mode=ipvlan -o parent=eth0 \
    lan-dhcp'
out="$(ENGINE_SHAPES_DOC="$tmp/good.md" bash "$GATE" 2>&1)"; got=$?
check "the same block naming a mode the plugin offers is green" 0 "the matrix drives" "$out" "$got"

# A documented network with no netdev to put it on. The cell has one
# bridge and two parents and nothing else, so an example that names
# neither cannot be driven and must not be skipped.
doc_with "$tmp/nodev.md" 'docker network create -d ghcr.io/claymore666/docker-net-dhcp:v9.9.9 \
    --ipam-driver null \
    -o mode=macvlan \
    lan-dhcp'
out="$(ENGINE_SHAPES_DOC="$tmp/nodev.md" bash "$GATE" 2>&1)"; got=$?
check "a documented network naming no interface is red" 1 "neither -o bridge= nor -o parent=" "$out" "$got"

# A documented network with no IPAM driver at all. It reaches the plugin
# with Docker built-in IPAM, which CreateNetwork refuses, so it is a
# documented shape that cannot work rather than one nobody drives.
doc_with "$tmp/noipam.md" 'docker network create -d ghcr.io/claymore666/docker-net-dhcp:v9.9.9 \
    -o mode=macvlan -o parent=eth0 \
    lan-dhcp'
out="$(ENGINE_SHAPES_DOC="$tmp/noipam.md" bash "$GATE" 2>&1)"; got=$?
check "a documented network naming no IPAM driver is red" 1 "names no --ipam-driver" "$out" "$got"
rm -rf "$tmp"

# ---- the IPAM-mode shape has a floor of its own -----------------------
# The defect #1013 records is not a shape that fails, it is a shape
# nothing drives. Derivation fixes the list somebody forgot to extend
# and opens a second way to lose it: edit the document. Here the one
# `--ipam-driver <plugin>` example is rewritten to name the null driver,
# which is a change a reader would call harmless. The derivation still
# succeeds, every documented line is still accounted for, and the only
# thing that changed is that no engine creates an IPAM-mode network
# again.
guarded_tmpdir tmp
sed 's#^    --ipam-driver ghcr.io/claymore666/docker-net-dhcp:.*#    --ipam-driver null \\#' \
    "$DOC" > "$tmp/no-ipam-shape.md"
if grep -q -- '--ipam-driver ghcr' "$tmp/no-ipam-shape.md"; then
    echo "FAIL  the fixture still documents an IPAM-driver network; the case below proves nothing"
    fail=$((fail + 1))
fi
out="$(ENGINE_SHAPES_DOC="$tmp/no-ipam-shape.md" bash "$GATE" 2>&1)"; got=$?
check "a document that stops showing the plugin as IPAM driver is red" 1 "documents no network with \`--ipam-driver\` naming this plugin" "$out" "$got"

# The twin, one edit away: the document as shipped still shows it, and
# the same gate is green. Without this line the case above is satisfied
# by a gate that reds every document.
out="$(ENGINE_SHAPES_DOC="$DOC" bash "$GATE" 2>&1)"; got=$?
check "the document as shipped still shows one, and is green" 0 "plugin|macvlan|parent" "$out" "$got"

# And the floor holds in the other direction: Docker's own IPAM beside
# the plugin's is half of what the matrix is for.
sed 's#^    --ipam-driver null \\#    --ipam-driver ghcr.io/claymore666/docker-net-dhcp:v2.2.0 \\#' \
    "$DOC" > "$tmp/no-null-shape.md"
out="$(ENGINE_SHAPES_DOC="$tmp/no-null-shape.md" bash "$GATE" 2>&1)"; got=$?
check "a document that stops showing a null-IPAM network is red" 1 "documents no network leaving IPAM to Docker" "$out" "$got"
rm -rf "$tmp"

# ---- the gate cannot pass on nothing ----------------------------------
guarded_tmpdir tmp
grep -v '^docker network create' "$DOC" > "$tmp/empty.md"
out="$(ENGINE_SHAPES_DOC="$tmp/empty.md" bash "$GATE" 2>&1)"; got=$?
check "a document promising no network is red, not vacuously green" 1 "documents no network" "$out" "$got"
rm -rf "$tmp"

out="$(ENGINE_SHAPES_DOC="/nonexistent/reference.md" bash "$GATE" 2>&1)"; got=$?
check "a document that does not exist is refused" 2 "does not exist" "$out" "$got"

echo
echo "engine-matrix-shapes self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
