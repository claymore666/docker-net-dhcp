#!/usr/bin/env bash
# Manual cleanup for integration-test orphans. Run as root after a
# test panics mid-setup. Safe to run repeatedly.
#
# Removes:
#   - dh-itest-* docker networks
#   - dh-itest-* docker containers
#   - dh-itest-* host network interfaces (veth pair, etc.)
#   - lingering dnsmasq processes started by the harness

set -u

if [[ $EUID -ne 0 ]]; then
    echo "must run as root" >&2
    exit 1
fi

echo "=== removing dh-itest-* containers ==="
ids=$(docker ps -a --filter 'name=dh-itest-' --format '{{.ID}}')
if [[ -n "$ids" ]]; then
    docker rm -f $ids 2>&1 | sed 's/^/  /'
fi

echo "=== removing dh-itest-* networks ==="
nets=$(docker network ls --filter 'name=dh-itest-' --format '{{.ID}}')
if [[ -n "$nets" ]]; then
    docker network rm $nets 2>&1 | sed 's/^/  /'
fi

echo "=== removing dh-itest-* host interfaces ==="
for if in $(ip -br link 2>/dev/null | awk '/^dh-itest-/{print $1}' | sed 's|@.*||'); do
    echo "  ip link del $if"
    ip link del "$if" 2>/dev/null || true
done

echo "=== ensuring plugin is enabled (recovery test may have left it disabled) ==="
# If the recovery test panicked between disable and enable, the plugin is
# stuck off and every subsequent run will fail at VerifyPluginEnabled.
# PluginEnable is idempotent — already-enabled returns an error we ignore.
plugin_ref="ghcr.io/claymore666/docker-net-dhcp:golang"
if docker plugin inspect "$plugin_ref" >/dev/null 2>&1; then
    docker plugin enable "$plugin_ref" 2>&1 | sed 's/^/  /' || true
fi

echo "=== killing lingering dnsmasq processes started by the harness ==="
# Both fixtures share the dh-itest-* prefix on their --interface= flag,
# so a single pgrep pattern catches both the macvlan-side dnsmasq
# (--interface=dh-itest-dhcp) and the bridge-side one
# (--interface=dh-itest-br2).
pids=$(pgrep -f -- '--interface=dh-itest-' || true)
if [[ -n "$pids" ]]; then
    kill -TERM $pids 2>/dev/null || true
    sleep 1
    kill -KILL $pids 2>/dev/null || true
    echo "  killed: $pids"
fi

echo "=== removing harness-installed iptables FORWARD rules ==="
# The bridge fixture inserts ACCEPT rules so docker's default-deny
# FORWARD policy doesn't drop bridged DHCP. -D is run in a loop because
# iptables only deletes one matching rule per call, and a panicked run
# can leave duplicates.
for direction in -i -o; do
    while iptables -C FORWARD "$direction" dh-itest-br2 -j ACCEPT 2>/dev/null; do
        iptables -D FORWARD "$direction" dh-itest-br2 -j ACCEPT 2>&1 | sed 's/^/  /'
    done
done

echo "=== done ==="
