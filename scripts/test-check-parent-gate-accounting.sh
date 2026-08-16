#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-parent-gate-accounting.sh (#571), driven
# through the --root and --manifest seams against synthetic trees.
#
# The cases that matter are the RED ones. A gate of this shape — grep
# the tree, compare against a list — passes trivially on a tree that
# happens to agree with its list, and there is no way to tell that
# apart from a gate whose pattern matches nothing at all. So every
# failure mode gets a case that must go red, and the green case is
# checked for the count it reports rather than only for exit 0.
#
# The last case is different in kind: it asserts the gate is WIRED into
# a workflow. #567's lesson was that a check can exist for a project's
# entire life with its input missing and nobody notices, because
# nothing fails. A gate that ships without its step is the same shape,
# and a note in a PR body is prose where an executable check belongs.
set -u

GATE="$(dirname "$0")/check-parent-gate-accounting.sh"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

# check NAME WANT_EXIT ROOT MANIFEST GREP
check() {
    local name="$1" want_exit="$2" root="$3" manifest="$4" want_grep="$5"
    bash "$GATE" --root "$root" --manifest "$manifest" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# mkroot DIR — a synthetic tree with one gated LinkAdd site.
mkroot() {
    mkdir -p "$1/pkg/plugin"
    cat > "$1/pkg/plugin/parent_attached.go" <<'EOF'
package plugin

func attach() error {
	defer p.lockParent(ctx, parent, "attach")()
	return netlink.LinkAdd(link)
}
EOF
}

# ---------------------------------------------------------------- green

mkroot "$TMP/ok"
cat > "$TMP/ok/manifest.txt" <<'EOF'
# A comment, and a blank line below it, are both ignored.

pkg/plugin/parent_attached.go 1
    Gated inline: lockParent is taken around the LinkAdd.
EOF
check "an accounted-for tree passes" 0 "$TMP/ok" "$TMP/ok/manifest.txt" \
    "1 netlink.LinkAdd site(s) across 1 file(s)"

# The passing output must keep saying what it does NOT prove. A gate
# that reports a bare "OK" invites the reader to believe the stronger
# claim, which here would be that the lock is held at every site.
check "and says what it does not prove" 0 "$TMP/ok" "$TMP/ok/manifest.txt" \
    "NOT that the gate is held"

# ------------------------------------------------------------------ red

# THE case: a new LinkAdd appears with nobody having thought about the
# gate. This is the failure that actually happens, and the only one the
# gate is claimed to catch.
cp -r "$TMP/ok" "$TMP/newsite"
cat > "$TMP/newsite/pkg/plugin/other.go" <<'EOF'
package plugin

func other() error { return netlink.LinkAdd(link) }
EOF
check "a new unaccounted-for LinkAdd fails" 1 "$TMP/newsite" "$TMP/newsite/manifest.txt" \
    "pkg/plugin/other.go"
check "and the message names lockParent as the thing to check" 1 "$TMP/newsite" \
    "$TMP/newsite/manifest.txt" "Plugin.lockParent"

# A second site in an already-listed file is the same hazard wearing the
# file name of something already reviewed, so the count is what catches it.
cp -r "$TMP/ok" "$TMP/drift"
cat >> "$TMP/drift/pkg/plugin/parent_attached.go" <<'EOF'

func second() error { return netlink.LinkAdd(other) }
EOF
check "a second site in a listed file fails on the count" 1 "$TMP/drift" \
    "$TMP/drift/manifest.txt" "accounted for 1"

# A path with no reason under it. Without this the file decays into a
# list of paths that somebody appends to without thinking, which is the
# state the gate exists to prevent.
cp -r "$TMP/ok" "$TMP/bare"
printf 'pkg/plugin/parent_attached.go 1\n' > "$TMP/bare/manifest.txt"
check "a bare entry with no justification fails" 1 "$TMP/bare" "$TMP/bare/manifest.txt" \
    "no justification"

cp -r "$TMP/ok" "$TMP/nocount"
printf 'pkg/plugin/parent_attached.go\n    Some reason.\n' > "$TMP/nocount/manifest.txt"
check "an entry with no site count fails" 1 "$TMP/nocount" "$TMP/nocount/manifest.txt" \
    "no site count"

# A justification for code that is gone reads as current. That is the
# prose-decays-silently failure applied to the manifest itself.
cp -r "$TMP/ok" "$TMP/stale"
cat >> "$TMP/stale/manifest.txt" <<'EOF'

pkg/plugin/removed.go 1
    Gated by the caller.
EOF
check "an entry for a file with no LinkAdd left fails" 1 "$TMP/stale" \
    "$TMP/stale/manifest.txt" "no netlink.LinkAdd left"

check "a missing manifest fails rather than passing vacuously" 1 "$TMP/ok" \
    "$TMP/ok/does-not-exist.txt" "accounting file missing"

# Usage errors must be distinguishable from findings: a gate invoked
# wrongly that exits 1 reads in a log exactly like a gate that found
# something, and the wrong one gets investigated.
bash "$GATE" --nonsense > "$TMP/out" 2>&1
if [ $? -eq 2 ]; then
    echo "PASS: an unknown flag exits 2, not 1"
else
    echo "FAIL: an unknown flag exits 2, not 1"
    failures=$((failures + 1))
fi

# --------------------------------------------------------- exclusions

# A LinkAdd in a test builds a fixture on a link the test owns; it is
# not the plugin contending for a shared parent. If this stops being
# excluded the manifest fills with entries that teach the reader
# nothing, and a real entry gets lost among them.
cp -r "$TMP/ok" "$TMP/testfile"
cat > "$TMP/testfile/pkg/plugin/thing_test.go" <<'EOF'
package plugin

func TestThing(t *testing.T) { _ = netlink.LinkAdd(fixture) }
EOF
check "a LinkAdd in a _test.go file needs no entry" 0 "$TMP/testfile" \
    "$TMP/testfile/manifest.txt" "1 netlink.LinkAdd site(s)"

# ------------------------------------------------- the committed tree

# Green against the real repo, with the real manifest. A gate that only
# ever runs against synthetic trees can be wired to a path that is empty
# in CI and nobody would know.
if bash "$GATE" > "$TMP/out" 2>&1; then
    echo "PASS: the committed tree is accounted for"
else
    echo "FAIL: the committed tree is not accounted for"
    sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi

real_sites=$(cd "$REPO" && grep -rn "netlink\.LinkAdd(" pkg/ --include='*.go' 2>/dev/null \
    | grep -vc '_test\.go:')
if [ "$real_sites" -ge 1 ]; then
    echo "PASS: the committed tree really has $real_sites LinkAdd site(s) to account for"
else
    echo "FAIL: no LinkAdd sites found in the committed pkg/ — the gate's pattern has stopped"
    echo "      matching, so every run above passed over nothing"
    failures=$((failures + 1))
fi

# ----------------------------------------------------- the gate is wired

# #567: a check whose input was missing sat green for the project's
# entire life. A gate with no step in any workflow is that same shape —
# it passes locally, it is referenced in a PR body, and it never runs.
if grep -rq -- "check-parent-gate-accounting.sh" "$REPO/.github/workflows" 2>/dev/null; then
    echo "PASS: a workflow runs the gate"
else
    echo "FAIL: no workflow under .github/workflows runs check-parent-gate-accounting.sh, so"
    echo "      the gate is committed but dead. Add a step; do not describe it in a PR body."
    failures=$((failures + 1))
fi

if [ "$failures" -eq 0 ]; then
    echo "all check-parent-gate-accounting tests passed"
    exit 0
fi
echo "$failures failed"
exit 1
