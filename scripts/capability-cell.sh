#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# One cell of the capability matrix (#690): the installed plugin, with one
# capability removed or none, driven through eight scenarios on this host's
# own daemon. Every verdict is outside evidence: the address on the
# container's link, the server's lease file and log, the container's
# resolv.conf, the plugin process's CapEff. Prints one CAP_MATRIX_ROW line.
#
# Usage: sudo capability-cell.sh <none|CAP_...> <plugin-ref> <expected-caps-json>
set -uo pipefail

REMOVED="$1" REF="$2" WANT_CAPS="$3"
NS=cm-srv MV=cm-mv MVP=cm-mvp BR=cm-br0 BRV=cm-brv BRVP=cm-brvp
MV_NET=cm-macvlan BR_NET=cm-bridge
LOGDIR=/var/log/capability-matrix
LEASES="$LOGDIR/leases" DLOG="$LOGDIR/dnsmasq.log"
# Option 6 points at an address nothing serves: it only marks a
# resolv.conf the plugin wrote (#690).
DNS_MARK=10.98.1.53
IMAGE=alpine:3.22
declare -A R=()
mapfile -t COLUMNS < <(bash "$(dirname "$0")/capability-matrix.sh" --columns)

say() { printf '%s\n' "$*"; }
row() {
    local out="CAP_MATRIX_ROW removed=$REMOVED result=$1" k
    for k in "${COLUMNS[@]}"; do
        out+=" $k=${R[$k]:-fail}"
    done
    say "$out"
}
cleanup() {
    docker rm -f cm-c-bridge cm-c-mv cm-c-user >/dev/null 2>&1
    docker network rm "$MV_NET" "$BR_NET" >/dev/null 2>&1
    ip netns pids "$NS" 2>/dev/null | xargs -r kill
    ip netns del "$NS" 2>/dev/null
    ip link del "$MV" 2>/dev/null
    ip link del "$BRV" 2>/dev/null
    ip link del "$BR" 2>/dev/null
}
trap cleanup EXIT

# --- the cell is what it claims to be (#690 defeat list D2-D4) ---------
got="$(docker plugin inspect -f '{{json .Config.Linux.Capabilities}}' "$REF" 2>/dev/null | jq -cS . 2>/dev/null)"
if [ "$got" != "$(printf '%s' "$WANT_CAPS" | jq -cS .)" ]; then
    say "installed capabilities '$got' are not the cell's '$WANT_CAPS'"
    row misconfigured; exit 0
fi
R[mount]="$(findmnt -no PROPAGATION --target /run/docker/netns 2>/dev/null | head -n1 | tr -d ' ')"
[ -n "${R[mount]}" ] || R[mount]=unknown
if [ "$(docker plugin inspect -f '{{.Enabled}}' "$REF" 2>/dev/null)" != true ]; then
    R[enables]=no R[capeff]=n/a
    say "the plugin is not enabled; every scenario is recorded as fail"
    row ok; exit 0
fi
R[enables]=yes
mapfile -t pids < <(pgrep -x net-dhcp)
if [ "${#pids[@]}" -ne 1 ]; then
    say "want one net-dhcp process, found ${#pids[@]}"
    row misconfigured; exit 0
fi
capeff="$(awk '/^CapEff:/ {print $2}' "/proc/${pids[0]}/status")"
say "plugin pid ${pids[0]} CapEff $capeff"
if [ "$REMOVED" = none ]; then
    R[capeff]=n/a
else
    # Bit numbers from include/uapi/linux/capability.h (#690).
    case "$REMOVED" in
        CAP_NET_ADMIN) bit=12 ;; CAP_NET_RAW) bit=13 ;;
        CAP_SYS_PTRACE) bit=19 ;; CAP_SYS_ADMIN) bit=21 ;;
        *) say "no bit number for $REMOVED"; row misconfigured; exit 0 ;;
    esac
    if (( (16#$capeff >> bit) & 1 )); then R[capeff]=held; else R[capeff]=dropped; fi
fi

# --- fixture: one server namespace, a macvlan parent and a bridge -------
mkdir -p "$LOGDIR"
: > "$LEASES"; : > "$DLOG"
ip netns add "$NS" || { row no-fixture; exit 0; }
ip link add "$MV" type veth peer name "$MVP" netns "$NS"
ip link add "$BR" type bridge
ip link add "$BRV" type veth peer name "$BRVP" netns "$NS"
ip link set "$BRV" master "$BR"
for l in "$MV" "$BR" "$BRV"; do ip link set "$l" up; done
ip -n "$NS" link set lo up
ip -n "$NS" addr add 10.98.1.1/24 dev "$MVP"; ip -n "$NS" link set "$MVP" up
ip -n "$NS" addr add 10.98.2.1/24 dev "$BRVP"; ip -n "$NS" link set "$BRVP" up
# The fixture bridge is not the subject: a host FORWARD drop must not
# decide a scenario (#690).
iptables -I FORWARD -i "$BR" -o "$BR" -j ACCEPT 2>/dev/null
# T1 10 s, T2 20 s: a renewal lands inside the wait below; dnsmasq rounds
# the lease itself up to two minutes (#690).
ip netns exec "$NS" dnsmasq --port=0 --bind-interfaces --except-interface=lo \
    --interface="$MVP" --interface="$BRVP" \
    --dhcp-range=10.98.1.10,10.98.1.99,2m --dhcp-range=10.98.2.10,10.98.2.99,2m \
    --dhcp-option=58,10 --dhcp-option=59,20 --dhcp-option="option:dns-server,$DNS_MARK" \
    --dhcp-leasefile="$LEASES" --log-dhcp --log-facility="$DLOG" \
    --pid-file="$LOGDIR/dnsmasq.pid" || { row no-fixture; exit 0; }
docker pull -q "$IMAGE" >/dev/null || { say "cannot pull $IMAGE"; row no-fixture; exit 0; }

net() {
    docker network create -d "$REF" --ipam-driver null -o propagate_dns=true "$@" \
        || say "network create $* refused"
}
net -o mode=macvlan -o parent="$MV" "$MV_NET"
net -o mode=bridge -o bridge="$BR" "$BR_NET"

# shellcheck source=scripts/capability-checks.sh
. "$(dirname "$0")/capability-checks.sh"

resolv() {
    for _ in $(seq 1 15); do
        docker exec -u 0 "$1" grep -qx "nameserver $DNS_MARK" /etc/resolv.conf 2>/dev/null && return 0
        sleep 1
    done
    say "$1: resolv.conf never carried nameserver $DNS_MARK"; return 1
}
verdict() { if "${@:2}"; then R[$1]=pass; else R[$1]=fail; fi; }

docker run -d --name cm-c-bridge --network "$BR_NET" "$IMAGE" sleep 900 >/dev/null || say "bridge container refused"
docker run -d --name cm-c-mv --network "$MV_NET" "$IMAGE" sleep 900 >/dev/null || say "macvlan container refused"
docker run -d --name cm-c-user --user 1000:1000 --network "$MV_NET" "$IMAGE" sleep 900 >/dev/null || say "--user 1000 container refused"

verdict bridge attached cm-c-bridge 10.98.2.
verdict macvlan attached cm-c-mv 10.98.1.
verdict user attached cm-c-user 10.98.1.
if [ "${R[macvlan]}" = pass ]; then verdict dns resolv cm-c-mv; else R[dns]=fail; fi
if [ "${R[user]}" = pass ]; then verdict dns_user resolv cm-c-user; else R[dns_user]=fail; fi

if [ "${R[macvlan]}" = pass ]; then verdict renew renewed cm-c-mv "$(mac_of cm-c-mv 10.98.1.)"; else R[renew]=fail; fi
if [ "${R[user]}" = pass ]; then verdict renew_user renewed cm-c-user "$(mac_of cm-c-user 10.98.1.)"; else R[renew_user]=fail; fi

if [ "${R[macvlan]}" = pass ]; then verdict restart restarted; else R[restart]=fail; fi

for c in cm-c-bridge cm-c-mv cm-c-user; do docker logs "$c" 2>&1 | tail -n 5; done
say "--- dnsmasq log (tail) ---"; tail -n 40 "$DLOG"
say "--- plugin log (tail) ---"
tail -n 80 /var/lib/docker/plugins/*/rootfs/var/log/net-dhcp.log 2>/dev/null
row ok
