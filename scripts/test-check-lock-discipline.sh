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

if [ "$fails" -ne 0 ]; then
    echo "lock discipline meta-test FAILED"
    exit 1
fi
echo "PASS  the lock-discipline gate catches both call shapes, the reach-back, and refuses to judge nothing"
