#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Manual cleanup for integration-test orphans. Run as root after a
# test panics mid-setup. Safe to run repeatedly.
#
# Removes:
#   - a previous job's suite processes (test binary, go test, make) and the
#     dnsmasq/kea fixtures they started
#   - dh-itest-* docker networks
#   - dh-itest-* docker containers
#   - dh-itest-* host network interfaces (veth pair, etc.)
#   - plugin state records (<network id>.json under NET_DHCP_STATE_DIR) of any
#     network the engine no longer has (#1174)

set -u

if [[ $EUID -ne 0 ]]; then
    echo "must run as root" >&2
    exit 1
fi

# Pids whose command line matches the ERE $1, never this script or an
# ancestor, never a zombie (it has no code left to run) (#1147).
suite_pids() {
    local pat=$1 p
    for p in $(pgrep -f -- "$pat" || true); do
        [[ " $protected " == *" $p "* ]] && continue
        [[ "$(ps -o stat= -p "$p" 2>/dev/null)" == Z* ]] && continue
        echo "$p"
    done
}

all_suite_pids() {
    local pat
    for pat in "${suite_patterns[@]}"; do suite_pids "$pat"; done | sort -un
}

# Full command lines (#1147): the test binary's comm is cut at 15 characters
# ("integration.tes") and it lives in a go-build temp dir, so `pgrep -x` never
# sees it. Both dnsmasq fixtures share the --interface=dh-itest- prefix; the
# ephemeral kea is told apart by its config dir, and outlives its deleted netns.
suite_patterns=(
    '(^|/)integration\.test( |$)'
    '(^|[ /])go test .*-tags integration'
    '(^|[ /])make .*integration-test'
    '--interface=dh-itest-'
    'kea-dhcp[46] .*dh-itest-ephemeral-'
    'kea-dhcp6 -c /etc/kea/dh-itest/'
)

# This script and every ancestor up to init are protected by pid.
protected=$$
for _ in $(seq 1 64); do
    ppid=$(ps -o ppid= -p "${protected##* }" 2>/dev/null | tr -d ' ')
    [[ -z "$ppid" || "$ppid" -le 1 ]] && break
    protected="$protected $ppid"
done

echo "=== killing the previous job's suite processes and fixtures ==="
# A cancelled job leaves its test binary running (#1147): it runs its own
# teardown minutes later and deletes the dh-itest-* links of the NEXT job.
# Parents, binary and fixtures go in one batch so a dying `make` cannot start
# the next package; the second collection catches anything spawned meanwhile.
pids=$(all_suite_pids)
if [[ -z "$pids" ]]; then
    echo "  none"
else
    for p in $pids; do
        echo "  found pid=$p age=$(ps -o etimes= -p "$p" 2>/dev/null | tr -d ' ')s: $(ps -o args= -p "$p" 2>/dev/null | cut -c1-120)"
    done
    # shellcheck disable=SC2086
    kill -TERM $pids 2>/dev/null || true
    for _ in $(seq 1 10); do
        [[ -z "$(all_suite_pids)" ]] && break
        sleep 0.5
    done
    pids=$(all_suite_pids)
    if [[ -n "$pids" ]]; then
        echo "  still running after TERM, KILL: ${pids//$'\n'/ }"
        # shellcheck disable=SC2086
        kill -KILL $pids 2>/dev/null || true
        for _ in $(seq 1 6); do
            [[ -z "$(all_suite_pids)" ]] && break
            sleep 0.5
        done
    fi
    left=$(all_suite_pids)
    if [[ -n "$left" ]]; then
        for p in $left; do
            echo "  SURVIVOR pid=$p state=$(ps -o stat= -p "$p" 2>/dev/null | tr -d ' '): $(ps -o args= -p "$p" 2>/dev/null | cut -c1-120)" >&2
        done
        echo "a suite process survived KILL; the links below would be deleted again by it, stopping" >&2
        exit 1
    fi
    echo "  all gone"
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

echo "=== removing plugin state records of networks the engine no longer has ==="
# The plugin keeps one <network id>.json per network and rebinds every
# persisted IPAM pool at start. Only DeleteNetwork removes the file, so a
# network removed above after its plugin was torn down leaves a record
# that refuses the next run's pool (#1174, #1165). The live set is read
# once; a failed read must not look like "every network is gone".
state_dir="${NET_DHCP_STATE_DIR:-/var/lib/net-dhcp}"
if [[ -d "$state_dir" ]]; then
    if live=$(docker network ls --no-trunc -q 2>/dev/null); then
        for f in "$state_dir"/*.json; do
            [[ -f "$f" ]] || continue
            id=$(basename "$f" .json)
            [[ "$id" =~ ^[0-9a-f]{64}$ ]] || continue
            if ! grep -qFx -- "$id" <<<"$live"; then
                rm -f -- "$f"
                echo "  removed $id.json (network gone)"
            fi
        done
    else
        echo "  engine unreachable; state records left alone"
    fi
fi

echo "=== removing dh-itest-* network namespaces ==="
# The ephemeral (failure-suite) fixture runs its kea in its own netns
# and moves the server end of the veth pair into it (#356). Delete the
# namespace FIRST: it takes that interface with it, so the interface
# sweep below then has nothing left to trip over.
for ns in $(ip netns list 2>/dev/null | awk '/^dh-itest-/{print $1}'); do
    echo "  ip netns del $ns"
    ip netns del "$ns" 2>/dev/null || true
done

echo "=== removing dh-itest-* host interfaces ==="
for if in $(ip -br link 2>/dev/null | awk '/^dh-itest-/{print $1}' | sed 's|@.*||'); do
    echo "  ip link del $if"
    ip link del "$if" 2>/dev/null || true
done

echo "=== ensuring plugin is enabled (recovery test may have left it disabled) ==="
# If the recovery test panicked between disable and enable, the plugin is
# stuck off and every subsequent run will fail at VerifyPluginEnabled.
# PluginEnable is idempotent — already-enabled returns an error we ignore.
#
# THIS ARM WAS A NO-OP ON EVERY CI LANE (#742). The tag was hardcoded to
# ":golang" while every lane installs and tests ":integration", so the
# one recovery this script performs was performed on a plugin the run
# was not using. It reported the same output either way — there is
# nothing to see when `plugin inspect` simply misses. The harness reads
# INTEGRATION_PLUGIN_REF with exactly this default
# (PluginRef in test/integration/harness/plugin.go); honour the same seam so
# the cleanup and the suite can never disagree about which plugin they
# mean.
plugin_ref="${INTEGRATION_PLUGIN_REF:-ghcr.io/claymore666/docker-net-dhcp:golang}"
if docker plugin inspect "$plugin_ref" >/dev/null 2>&1; then
    docker plugin enable "$plugin_ref" 2>&1 | sed 's/^/  /' || true
fi

echo "=== removing the Kea6 fixture's state files ==="
# The Kea6 fixture's server runs from /etc/kea/dh-itest/ and keeps files
# there, in /var/lib/kea and in /var/log/kea, the paths the packaged
# AppArmor profile allows (#214). Its process went with the batch above.
rm -rf /etc/kea/dh-itest
rm -f /var/lib/kea/kea-leases6.csv* /var/lib/kea/kea-dhcp6-serverid /var/log/kea/kea-dhcp6.log*

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
