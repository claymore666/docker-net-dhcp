#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-manager-registration.sh (#480).
#
# The gate replaces a doc comment that asked callers to Stop the manager
# they displace. One caller ignored it for as long as that caller
# existed, so a gate that cannot catch the exact shape of that mistake
# would be strictly worse than the comment it replaces — it would also
# look like enforcement. Each direction is driven against a fixture
# here, including the two ways this gate could pass while seeing
# nothing.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
CHECK="$HERE/check-manager-registration.sh"

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

# Exit status is the verdict: 0 clean, 1 violation, 2 cannot check. The
# three are distinguished, because "cannot check" reported as "clean" is
# the failure this gate exists to avoid making.
run() {
    ( cd "$REPO" && bash "$CHECK" "$1" >"$TMP/log" 2>&1 )
    case $? in
        0) echo clean ;;
        1) echo violation ;;
        2) echo cannot-check ;;
        *) echo "unexpected" ;;
    esac
}

mk() { local d="$TMP/$1"; mkdir -p "$d"; echo "$d"; }

# --- the real tree must pass -------------------------------------------
got=$(run pkg/plugin)
check "the repository as it stands is clean" clean "$got"

# --- the bug that shipped ----------------------------------------------
# recoverOneEndpoint's exact former shape: register, ignore what came
# back, spawn the client anyway.
d=$(mk discard)
cat > "$d/plugin.go" <<'GO'
package plugin

func (p *Plugin) recoverOneEndpoint(endpointID string, m *dhcpManager) error {
	p.registerDHCPManager(endpointID, m)
	go m.Start()
	return nil
}
GO
check "a bare registration is a violation" violation "$(run "$d")"

# --- the louder version of the same mistake ----------------------------
d=$(mk blanked)
cat > "$d/plugin.go" <<'GO'
package plugin

func (p *Plugin) recoverOneEndpoint(endpointID string, m *dhcpManager) error {
	_ = p.registerDHCPManager(endpointID, m)
	return nil
}
GO
check "discarding through _ = is a violation" violation "$(run "$d")"

# --- the IfAbsent half -------------------------------------------------
# Ignoring "did I win?" is a different bug with the same root: the
# caller proceeds as though it owns an endpoint somebody else manages.
d=$(mk ifabsent-ignored)
cat > "$d/plugin.go" <<'GO'
package plugin

func (p *Plugin) recoverOneEndpoint(endpointID string, m *dhcpManager) error {
	p.registerDHCPManagerIfAbsent(endpointID, m)
	go m.Start()
	return nil
}
GO
check "ignoring the IfAbsent verdict is a violation" violation "$(run "$d")"

# --- both correct shapes -----------------------------------------------
d=$(mk bound)
cat > "$d/network.go" <<'GO'
package plugin

func (p *Plugin) join(endpointID string, m *dhcpManager) {
	if displaced := p.registerDHCPManager(endpointID, m); displaced != nil {
		go displaced.Stop()
	}
}

func (p *Plugin) recover(endpointID string, m *dhcpManager) bool {
	if !p.registerDHCPManagerIfAbsent(endpointID, m) {
		return false
	}
	return true
}
GO
check "stopping the displaced manager, and yielding, both pass" clean "$(run "$d")"

# --- the atomicity of the compare-and-set ------------------------------
# The shape no Go test could hold: a read, an unlock, and a later write.
# A concurrency test against exactly this passed 3/3 with 64 racers, so
# this is the only thing standing between the fix and its own regression.
d=$(mk two-step)
cat > "$d/plugin.go" <<'GO'
package plugin

func (p *Plugin) registerDHCPManagerIfAbsent(endpointID string, m *dhcpManager) bool {
	p.mu.Lock()
	_, exists := p.persistentDHCP[endpointID]
	p.mu.Unlock()
	if exists {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persistentDHCP[endpointID] = m
	return true
}

func (p *Plugin) join(endpointID string, m *dhcpManager) {
	if displaced := p.registerDHCPManager(endpointID, m); displaced != nil {
		go displaced.Stop()
	}
}
GO
check "a split check-then-write is a violation" violation "$(run "$d")"

d=$(mk one-section)
cat > "$d/plugin.go" <<'GO'
package plugin

func (p *Plugin) registerDHCPManagerIfAbsent(endpointID string, m *dhcpManager) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.persistentDHCP[endpointID]; exists {
		return false
	}
	p.persistentDHCP[endpointID] = m
	return true
}
GO
check "one critical section passes" clean "$(run "$d")"

# --- a gate whose engine died has not found nothing --------------------
# It has found out nothing. Without the exit-status check, a broken
# regex or a missing awk renders as an empty result and a PASS — the
# direction that does not fail loudly.
got=$( cd "$REPO" && AWK=/nonexistent-awk bash "$CHECK" pkg/plugin >"$TMP/log" 2>&1; \
       case $? in 0) echo clean ;; 1) echo violation ;; 2) echo cannot-check ;; *) echo unexpected ;; esac )
check "a dead scan engine reports cannot-check, not clean" cannot-check "$got"

# --- prose must not be judged ------------------------------------------
# This gate's own rule gets explained in comments beside the code it
# governs. A scan that read those would flag the explanation.
d=$(mk comment-only)
cat > "$d/doc.go" <<'GO'
package plugin

// Never write p.registerDHCPManager(endpointID, m) as a bare statement:
// the manager it displaces would leak.
func (p *Plugin) unrelated() {}
GO
check "a violation quoted in a comment is not a violation" cannot-check "$(run "$d")"

# --- the two ways to pass having seen nothing --------------------------
# Both must be "cannot check", never "clean". A gate that reports the
# repository clean after reading no file is the failure mode that let
# #644's engine die silently.
d=$(mk empty-dir)
check "a directory with no production Go cannot be judged" cannot-check "$(run "$d")"

d=$(mk renamed)
cat > "$d/plugin.go" <<'GO'
package plugin

// The helper was renamed and this gate was not.
func (p *Plugin) join(endpointID string, m *dhcpManager) {
	p.storeManager(endpointID, m)
}
GO
check "no call site at all cannot be judged" cannot-check "$(run "$d")"

d=$(mk tests-only)
cat > "$d/plugin_test.go" <<'GO'
package plugin

func TestSomething(t *testing.T) { p.registerDHCPManager("ep", m) }
GO
check "test files alone cannot be judged" cannot-check "$(run "$d")"

exit "$fails"
