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
# claim, which here would be that the lock is held at every site — that
# is addChildLink's *parentGuard doing the work, not this.
check "and says what it does not prove" 0 "$TMP/ok" "$TMP/ok/manifest.txt" \
    "does NOT prove the gate is held"
check "and points at what does prove it" 0 "$TMP/ok" "$TMP/ok/manifest.txt" \
    "parentGuard"

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

# The gate turned on itself. Every check here is a grep over $ROOT/pkg,
# so a wrong root makes all of them match nothing — and a gate that
# reports success having scanned no code is the exact failure the whole
# file exists to prevent.
mkdir -p "$TMP/norootpkg"
cp "$TMP/ok/manifest.txt" "$TMP/norootpkg/manifest.txt"
check "a root with no pkg/ exits 2 rather than passing" 2 "$TMP/norootpkg" \
    "$TMP/norootpkg/manifest.txt" "would pass having scanned nothing"

# Same shape one level in: the tree is there, the pattern finds nothing.
mkdir -p "$TMP/nosites/pkg/plugin"
printf 'package plugin\n\nfunc nothing() {}\n' > "$TMP/nosites/pkg/plugin/quiet.go"
printf '# nothing declared\n' > "$TMP/nosites/manifest.txt"
check "zero LinkAdd sites is a broken pattern, not a clean tree" 1 "$TMP/nosites" \
    "$TMP/nosites/manifest.txt" "pattern has stopped matching"

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

# ------------------------------------------------- the forged guard

# The compile-time half of the parent gate rests on "only lockParent
# constructs a parentGuard", which Go cannot enforce: the zero value is
# valid, so `addChildLink(&parentGuard{}, link)` compiles and holds
# nothing. Verified by building exactly that against the real package
# before this check existed. These cases are what makes the claim true.

mkguard() {
    mkdir -p "$1/pkg/plugin"
    cat > "$1/pkg/plugin/parent_gate.go" <<'EOF'
package plugin

type parentGuard struct{ release func() }

func (p *Plugin) lockParent(parent string) *parentGuard {
	return &parentGuard{}
}

func addChildLink(_ *parentGuard, link netlink.Link) error {
	return netlink.LinkAdd(link)
}
EOF
    cat > "$1/manifest.txt" <<'EOF'
pkg/plugin/parent_gate.go 1
    The funnel; the guard is what makes the gate compile-checked.
EOF
}

mkguard "$TMP/guard"
check "the owning file may construct guards" 0 "$TMP/guard" "$TMP/guard/manifest.txt" \
    "1 netlink.LinkAdd site(s)"

# Taking one as a parameter is the whole point and must stay legal.
cp -r "$TMP/guard" "$TMP/guardparam"
cat > "$TMP/guardparam/pkg/plugin/user.go" <<'EOF'
package plugin

func attach(guard *parentGuard, link netlink.Link) error {
	return addChildLink(guard, link)
}
EOF
check "receiving a *parentGuard is legal" 0 "$TMP/guardparam" "$TMP/guardparam/manifest.txt" \
    "1 netlink.LinkAdd site(s)"

# THE case: a composite literal outside the owning file.
cp -r "$TMP/guard" "$TMP/forged"
cat > "$TMP/forged/pkg/plugin/user.go" <<'EOF'
package plugin

func attach(link netlink.Link) error {
	return addChildLink(&parentGuard{}, link)
}
EOF
check "a forged guard fails" 1 "$TMP/forged" "$TMP/forged/manifest.txt" \
    "built outside the file that owns it"
check "and the message says a forged guard is evidence of nothing" 1 "$TMP/forged" \
    "$TMP/forged/manifest.txt" "evidence of nothing"

# The same hole wearing a var declaration rather than a literal. Worth
# its own case: a check that only matched `parentGuard{` would pass this
# and the zero value is identical.
cp -r "$TMP/guard" "$TMP/forgedvar"
cat > "$TMP/forgedvar/pkg/plugin/user.go" <<'EOF'
package plugin

func attach(link netlink.Link) error {
	var g parentGuard
	return addChildLink(&g, link)
}
EOF
check "a zero-value guard declared with var also fails" 1 "$TMP/forgedvar" \
    "$TMP/forgedvar/manifest.txt" "built outside the file that owns it"

# A unit test may build one to exercise the type's contract — Unlock on
# a zero guard must not panic, and proving that needs a zero guard.
cp -r "$TMP/guard" "$TMP/guardtest"
cat > "$TMP/guardtest/pkg/plugin/gate_test.go" <<'EOF'
package plugin

func TestZeroGuardUnlockIsSafe(t *testing.T) {
	var zero parentGuard
	zero.Unlock()
}
EOF
check "a test may build one" 0 "$TMP/guardtest" "$TMP/guardtest/manifest.txt" \
    "1 netlink.LinkAdd site(s)"

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
