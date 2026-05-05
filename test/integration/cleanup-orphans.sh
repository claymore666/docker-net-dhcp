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

echo "=== killing lingering dnsmasq listening on dh-itest-dhcp ==="
# dnsmasq with --interface=dh-itest-dhcp argv signature
pids=$(pgrep -f -- '--interface=dh-itest-dhcp' || true)
if [[ -n "$pids" ]]; then
    kill -TERM $pids 2>/dev/null || true
    sleep 1
    kill -KILL $pids 2>/dev/null || true
    echo "  killed: $pids"
fi

echo "=== done ==="
