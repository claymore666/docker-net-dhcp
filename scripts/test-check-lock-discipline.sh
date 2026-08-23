#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-lock-discipline.sh (#643).
#
# The gate replaces a comment that asked readers not to hold Plugin.mu
# and the tombstone lock together. A gate that cannot actually catch that
# would be strictly worse than the comment, because it also looks like
# enforcement — so each direction is driven against a fixture here, and
# the real tree is required to pass.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
CHECK="$HERE/check-lock-discipline.sh"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

run() { ( cd "$REPO" && bash "$CHECK" "$1" >"$TMP/log" 2>&1 ) && echo pass || echo fail; }

mk() {
    local d="$TMP/$1"; mkdir -p "$d"
    cat > "$d/tombstone_store.go" <<'GO'
package plugin

type tombstoneStore struct{ mu sync.Mutex }

func (s *tombstoneStore) add(a string) error { s.mu.Lock(); defer s.mu.Unlock(); return nil }
GO
    echo "$d"
}

# --- the real tree must pass -------------------------------------------
got=$(run pkg/plugin)
check "the repository as it stands passes" pass "$got"

# --- the violation the comment used to guard ---------------------------
d=$(mk violation)
cat > "$d/network.go" <<'GO'
package plugin

func (p *Plugin) somethingUnderLock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consumeTombstone("net", "host")
}
GO
got=$(run "$d")
check "holding Plugin.mu across a tombstone call fails" fail "$got"
grep -q "deadlock" "$TMP/log" \
    && echo "PASS: names the consequence" \
    || { echo "FAIL: rejected without saying why it matters"; fails=1; }

# The same violation through the store's method directly.
cat > "$d/network.go" <<'GO'
package plugin

func (p *Plugin) somethingUnderLock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tombstones.consume("net", "host")
}
GO
got=$(run "$d")
check "the same violation via the store method fails" fail "$got"

# --- the legitimate shape must pass ------------------------------------
# Reading under the lock and calling the store AFTER releasing it is the
# correct pattern, but `defer` makes that indistinguishable textually —
# so the gate is coarse on purpose and this asserts the shape it DOES
# accept: no p.mu at all.
d=$(mk ok)
cat > "$d/network.go" <<'GO'
package plugin

func (p *Plugin) noPluginLock() {
	p.consumeTombstone("net", "host")
}

func (p *Plugin) onlyPluginLock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.joinHints
}
GO
got=$(run "$d")
check "a tombstone call with no Plugin.mu passes" pass "$got"

# --- the store must not reach back into Plugin -------------------------
d=$(mk reachback)
cat >> "$d/tombstone_store.go" <<'GO'

func (s *tombstoneStore) bad(p *Plugin) { p.mu.Lock() }
GO
got=$(run "$d")
check "a store that references Plugin fails" fail "$got"

# ...but its own documentation may discuss p.mu without tripping it.
d=$(mk prose)
cat >> "$d/tombstone_store.go" <<'GO'

// This comment mentions p.mu and p.tombstoneMu and p.consumeTombstone
// on purpose: explaining the rule must not violate it.
GO
got=$(run "$d")
check "prose naming p.mu does not trip the reach-back check" pass "$got"

# --- inspecting nothing is not a pass ----------------------------------
mkdir -p "$TMP/empty"
got=$(cd "$REPO" && bash "$CHECK" "$TMP/empty" >/dev/null 2>&1; echo $?)
check "a directory with no Go files exits 2" 2 "$got"

got=$(cd "$REPO" && bash "$CHECK" "$TMP/does-not-exist" >/dev/null 2>&1; echo $?)
check "a missing directory exits 2" 2 "$got"

# --- the scan must not depend on how awk treats -v ---------------------
# POSIX leaves undefined escape sequences in a -v assignment undefined.
# The gate used to hand awk its pattern that way; the awk on the author's
# machine kept the backslashes and the one on CI did not, and a stripped
# `\(` left an unbalanced paren that would not compile. awk died, the
# violation list came back empty, and the gate said PASS on a planted
# violation. It failed in the silent direction, which is the only
# direction that matters for a gate.
#
# This drives the real gate through an awk that strips those escapes, so
# the property is proven rather than asserted about the source text.
STRIPPING_AWK="$TMP/stripping-awk"
cat > "$STRIPPING_AWK" <<'STUB'
#!/usr/bin/env bash
# Emulates an awk that strips undefined escape sequences from -v
# assignments. Anything not passed via -v is handed through untouched.
args=()
while [ $# -gt 0 ]; do
    if [ "$1" = "-v" ] && [ $# -ge 2 ]; then
        args+=(-v "${2//\\/}")
        shift 2
        continue
    fi
    args+=("$1")
    shift
done
exec awk "${args[@]}"
STUB
chmod +x "$STRIPPING_AWK"

d=$(mk stripped)
cat > "$d/network.go" <<'GO'
package plugin

func (p *Plugin) somethingUnderLock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consumeTombstone("net", "host")
}
GO
# The exit code is asserted exactly, not merely as non-zero: with the
# pattern handed through -v the scan dies and the engine check below
# exits 2, which a pass/fail assertion cannot tell apart from the 1 that
# means "violation found". 1 is the only answer that says the gate still
# did its job.
got=$(cd "$REPO" && AWK="$STRIPPING_AWK" bash "$CHECK" "$d" >/dev/null 2>&1; echo $?)
check "an awk that strips -v escapes still catches the violation" 1 "$got"

# --- a dead scan engine is not a clean tree ----------------------------
# Whatever kills awk — an uncompilable pattern, an unreadable file, no
# awk at all — must not render as "no violations found".
cat > "$TMP/broken-awk" <<'STUB'
#!/usr/bin/env bash
exit 3
STUB
chmod +x "$TMP/broken-awk"

d=$(mk deadengine)
cat > "$d/network.go" <<'GO'
package plugin

func (p *Plugin) somethingUnderLock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consumeTombstone("net", "host")
}
GO
got=$(cd "$REPO" && AWK="$TMP/broken-awk" bash "$CHECK" "$d" >"$TMP/log" 2>&1; echo $?)
check "a scan engine that dies exits 2, not 0" 2 "$got"
grep "inspected nothing" "$TMP/log" >/dev/null \
    && echo "PASS: says it judged nothing rather than reporting clean" \
    || { echo "FAIL: exited 2 without saying the scan never ran"; fails=1; }

if [ "$fails" -ne 0 ]; then
    echo "lock discipline meta-test FAILED"
    exit 1
fi
echo "PASS  the lock-discipline gate catches both call shapes, the reach-back, a stripped -v, a dead engine, and refuses to judge nothing"
