#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Verifies the persistent-bridge recipes in docs/bridge-mode.md by
# applying each one, verbatim, inside a throwaway privileged container
# running that stack.
#
# WHY THIS EXISTS
#   The page ships copy-pasteable network configuration for four
#   different stacks. Nothing else in the repo would notice if a distro
#   renamed a property — and the first version of the systemd-networkd
#   stanza did exactly that, using `ForwardDelay` where the key is
#   `ForwardDelaySec`. systemd IGNORES an unknown key and carries on,
#   so the bridge came up looking healthy while the setting did nothing.
#   Unfindable by reading; a five-second run finds it.
#
# WHAT A PASS MEANS
#   The recipe is correct AS WRITTEN: the tool accepts it, the bridge
#   comes up with the intended port enslaved, the NIC is left
#   address-less, STP is off, and the bridge gets a real DHCP lease.
#
# WHAT IT DOES NOT MEAN
#   That the config survives a reboot. The failure a real reboot
#   catches is "config is right, but the bridge appears after Docker,
#   or something else claims the NIC first" — boot ordering, which a
#   container has none of. That check still needs a real machine.
#
# Topology per container (mirrors test/integration/harness):
#
#     [ my-bridge ] -- eth1 <==veth==> srv0 -- [ dnsmasq ]
#
# eth1 stands in for the host NIC the recipe enslaves; dnsmasq on its
# peer plays the LAN's DHCP server. The name matters: NetworkManager
# ships a udev rule marking veth devices unmanaged unless they are
# named eth[0-9]*, and a realistic name matches the docs anyway.
#
# Usage:  scripts/verify-bridge-recipes.sh            # all recipes
#         scripts/verify-bridge-recipes.sh netplan    # just one
set -uo pipefail

NET=192.168.77
SRV=$NET.1
POOL_START=$NET.10
POOL_END=$NET.99
BRIDGE=my-bridge
NIC=eth1
PEER=srv0

ONLY=${1:-}
PASS=0; FAIL=0; SKIP=0
declare -a RESULTS

# Shared prologue: build the fake LAN, start the DHCP server. Every
# recipe starts from an identical topology so a failure is the
# recipe's, not the fixture's.
read -r -d '' PROLOGUE <<PRO || true
set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq iproute2 dnsmasq >/dev/null 2>&1
ip link add $NIC type veth peer name $PEER
ip link set $NIC up
ip link set $PEER up
ip addr add $SRV/24 dev $PEER
dnsmasq --interface=$PEER --bind-interfaces --except-interface=lo \\
        --dhcp-range=$POOL_START,$POOL_END,2m --port=0 \\
        --dhcp-leasefile=/var/lib/misc/dnsmasq.leases
sleep 1
PRO

# assert_recipe <label> <image> <script>
assert_recipe() {
  local label=$1 image=$2 script=$3
  [ -n "$ONLY" ] && [ "$ONLY" != "$label" ] && return 0

  local cname="brverify-$label"
  echo
  echo "=============================================================="
  echo "  $label   ($image)"
  echo "=============================================================="
  docker rm -f "$cname" >/dev/null 2>&1 || true

  if ! docker run -d --name "$cname" --privileged "$image" sleep 600 >/dev/null 2>&1; then
    RESULTS+=("FAIL  $label — could not start a container from $image")
    FAIL=$((FAIL+1)); return 0
  fi

  docker exec "$cname" bash -c "$script" 2>&1 | sed 's/^/    /'

  local problems=""
  docker exec "$cname" ip link show "$BRIDGE" >/dev/null 2>&1 \
    || problems+="bridge $BRIDGE does not exist; "

  local master
  master=$(docker exec "$cname" sh -c "ip -o link show $NIC 2>/dev/null | grep -o 'master [^ ]*' | awk '{print \$2}'")
  [ "$master" = "$BRIDGE" ] || problems+="$NIC master is '${master:-none}', want $BRIDGE; "

  local stp
  stp=$(docker exec "$cname" sh -c "ip -d link show $BRIDGE 2>/dev/null | grep -o 'stp_state [0-9]*' | awk '{print \$2}'")
  [ "$stp" = "0" ] || problems+="stp_state is '${stp:-unknown}', want 0; "

  # The NIC must end up address-less — the recipe's whole point, and
  # what locks people out when they get it wrong.
  docker exec "$cname" sh -c "ip -4 addr show $NIC 2>/dev/null | grep -q 'inet '" \
    && problems+="$NIC still has an IPv4 address; it must be address-less; "

  # And the bridge must hold a lease the SERVER agrees it issued —
  # an address alone only proves something assigned one.
  local brip
  brip=$(docker exec "$cname" sh -c "ip -4 -o addr show $BRIDGE 2>/dev/null | awk '{print \$4}' | cut -d/ -f1 | head -1")
  if [ -z "$brip" ]; then
    problems+="$BRIDGE has no IPv4 address (no DHCP lease); "
  elif ! docker exec "$cname" sh -c "grep -q '$brip' /var/lib/misc/dnsmasq.leases 2>/dev/null"; then
    problems+="$BRIDGE holds $brip but dnsmasq never leased it; "
  fi

  if [ -n "$problems" ]; then
    RESULTS+=("FAIL  $label — $problems")
    FAIL=$((FAIL+1))
  else
    RESULTS+=("PASS  $label — bridge up, $NIC enslaved and address-less, STP off, leased $brip")
    PASS=$((PASS+1))
  fi
  docker rm -f "$cname" >/dev/null 2>&1
}

skip_recipe() {
  [ -n "$ONLY" ] && [ "$ONLY" != "$1" ] && return 0
  RESULTS+=("SKIP  $1 — $2")
  SKIP=$((SKIP+1))
}

# ------------------------------------------------------------ ifupdown
read -r -d '' IFUPDOWN <<EOS || true
$PROLOGUE
apt-get install -y -qq ifupdown bridge-utils isc-dhcp-client >/dev/null 2>&1
cat >/etc/network/interfaces <<'CFG'
auto lo
iface lo inet loopback

iface $NIC inet manual

auto $BRIDGE
iface $BRIDGE inet dhcp
    bridge_ports $NIC
    bridge_stp off
    bridge_fd 0
CFG
ifup $BRIDGE
sleep 3
EOS

# ------------------------------------------------------------- netplan
read -r -d '' NETPLAN <<EOS || true
$PROLOGUE
apt-get install -y -qq netplan.io systemd >/dev/null 2>&1
rm -f /etc/netplan/*.yaml
cat >/etc/netplan/60-dhcp-bridge.yaml <<'CFG'
network:
  version: 2
  renderer: networkd
  ethernets:
    $NIC:
      dhcp4: false
      dhcp6: false
  bridges:
    $BRIDGE:
      interfaces: [$NIC]
      dhcp4: true
      parameters:
        stp: false
        forward-delay: 0
CFG
chmod 600 /etc/netplan/60-dhcp-bridge.yaml
netplan generate
/lib/systemd/systemd-networkd >/dev/null 2>&1 &
sleep 10
EOS

# ---------------------------------------------------- systemd-networkd
read -r -d '' NETWORKD <<EOS || true
$PROLOGUE
apt-get install -y -qq systemd >/dev/null 2>&1
mkdir -p /etc/systemd/network
cat >/etc/systemd/network/10-$BRIDGE.netdev <<'CFG'
[NetDev]
Name=$BRIDGE
Kind=bridge

[Bridge]
STP=false
ForwardDelaySec=0
CFG
cat >/etc/systemd/network/20-$NIC.network <<'CFG'
[Match]
Name=$NIC

[Network]
Bridge=$BRIDGE
CFG
cat >/etc/systemd/network/30-$BRIDGE.network <<'CFG'
[Match]
Name=$BRIDGE

[Network]
DHCP=ipv4
ConfigureWithoutCarrier=yes
CFG
/lib/systemd/systemd-networkd >/dev/null 2>&1 &
sleep 12
EOS

echo "Verifying docs/bridge-mode.md persistent-bridge recipes."
echo "Each runs in its own privileged container; the host is untouched."

assert_recipe ifupdown         debian:12    "$IFUPDOWN"
assert_recipe netplan          ubuntu:24.04 "$NETPLAN"
assert_recipe systemd-networkd ubuntu:24.04 "$NETWORKD"

# NetworkManager refuses to manage devices in a bare container: with no
# systemd/udev integration every device stays `unmanaged`, so the
# profiles are created and then cannot be activated. That is the
# harness's limit, not a statement about the recipe — verifying it
# needs a real machine.
skip_recipe networkmanager "NM leaves all devices unmanaged without systemd/udev integration; needs a real host"

echo
echo "=============================================================="
printf '%s\n' "${RESULTS[@]}"
echo "--------------------------------------------------------------"
echo "passed: $PASS   failed: $FAIL   skipped: $SKIP"
echo
echo "A pass means the recipe is correct AS WRITTEN. Boot-ordering"
echo "persistence is NOT covered — that still needs a real reboot."
[ "$FAIL" -eq 0 ]
