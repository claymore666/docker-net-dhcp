#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Boot-persistence check for the recipes in docs/bridge-mode.md.
#
# verify-bridge-recipes.sh proves each recipe is correct AS WRITTEN by
# applying it by hand. This one proves the distro's init system applies
# it AT BOOT, which is the actual claim "persistent" makes and the one
# an operator finds out about the hard way.
#
# HOW IT IS A REAL BOOT
#   Each container runs systemd as PID 1, so `docker restart` kills PID
#   1 and systemd re-runs its entire unit sequence in dependency order
#   — networking.service / systemd-networkd / NetworkManager start the
#   way they would on a physical boot, from on-disk config, with
#   nothing applied by hand.
#
#   The NIC under test is Docker-provided (eth1 on a user-defined
#   network), so it disappears and reappears across the restart exactly
#   as a real NIC does across a reboot.
#
# WHAT IT STILL DOES NOT COVER
#   The host's own ordering between docker.service and its networking —
#   "bridge is configured correctly but dockerd started first". There is
#   no dockerd inside these containers, so that residual risk needs a
#   real machine. Everything upstream of it is covered here.
#
# Topology:
#
#   docker network dhcp-bootverify-lan (192.168.77.0/24)
#     ├── dnsmasq container  .2      serves .100-.150
#     └── test container     eth1    enslaved into my-bridge by the
#                                    recipe, bridge takes DHCP
#
# Docker's own IPAM is confined to .0/28 so it can never hand out an
# address from the DHCP range and make a failure look like a success.
#
# Usage:  scripts/verify-bridge-boot.sh            # every recipe
#         scripts/verify-bridge-boot.sh netplan    # just one
set -uo pipefail

LAN=dhcp-bootverify-lan
SUBNET=192.168.77.0/24
IPAM_RANGE=192.168.77.0/28       # Docker may only use .1-.15
DHCP_FROM=192.168.77.100
DHCP_TO=192.168.77.150
SRVIP=192.168.77.2
BRIDGE=my-bridge
NIC=eth1
DNSMASQ_C=dhcp-bootverify-dnsmasq

ONLY=${1:-}
PASS=0; FAIL=0
declare -a RESULTS
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

need_docker() {
  docker info >/dev/null 2>&1 || { echo "docker is not available"; exit 1; }
}

build_images() {
  # One image per stack: installing several network managers into the
  # same image makes them fight at boot, which would be a bug in the
  # harness rather than a finding about the recipe.
  cat >"$WORK/Dockerfile.ifupdown" <<'EOF'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive container=docker
RUN apt-get update -qq && apt-get install -y -qq \
        systemd systemd-sysv udev iproute2 ifupdown bridge-utils isc-dhcp-client \
    && rm -rf /var/lib/apt/lists/*
RUN systemctl mask systemd-resolved systemd-networkd-wait-online || true
RUN systemctl enable networking || true
CMD ["/lib/systemd/systemd"]
EOF

  cat >"$WORK/Dockerfile.networkd" <<'EOF'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive container=docker
RUN apt-get update -qq && apt-get install -y -qq \
        systemd systemd-sysv udev iproute2 netplan.io \
    && rm -rf /var/lib/apt/lists/*
RUN systemctl mask systemd-resolved systemd-networkd-wait-online || true
RUN systemctl enable systemd-networkd || true
CMD ["/lib/systemd/systemd"]
EOF

  cat >"$WORK/Dockerfile.nm" <<'EOF'
FROM fedora:41
ENV container=docker
RUN dnf install -y -q systemd NetworkManager iproute && dnf clean all
RUN systemctl mask systemd-resolved NetworkManager-wait-online || true
RUN systemctl enable NetworkManager || true
CMD ["/usr/lib/systemd/systemd"]
EOF

  cat >"$WORK/Dockerfile.dnsmasq" <<'EOF'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq && apt-get install -y -qq dnsmasq iproute2 \
    && rm -rf /var/lib/apt/lists/*
EOF

  echo "Building images (cached after the first run)..."
  for s in ifupdown networkd nm dnsmasq; do
    docker build -q -f "$WORK/Dockerfile.$s" -t "dhcp-bootverify-$s:local" "$WORK" >/dev/null \
      || { echo "failed to build dhcp-bootverify-$s"; exit 1; }
  done
}

start_lan() {
  docker rm -f "$DNSMASQ_C" >/dev/null 2>&1 || true
  docker network rm "$LAN" >/dev/null 2>&1 || true
  docker network create --subnet "$SUBNET" --ip-range "$IPAM_RANGE" "$LAN" >/dev/null \
    || { echo "could not create the $LAN network"; exit 1; }

  docker run -d --name "$DNSMASQ_C" --network "$LAN" --ip "$SRVIP" \
    --cap-add NET_ADMIN dhcp-bootverify-dnsmasq:local \
    dnsmasq --keep-in-foreground --interface=eth0 --bind-interfaces \
            --except-interface=lo --port=0 \
            --dhcp-range="$DHCP_FROM,$DHCP_TO,2m" \
            --dhcp-leasefile=/var/lib/misc/dnsmasq.leases \
            --log-facility=/var/log/dnsmasq.log --log-dhcp >/dev/null \
    || { echo "could not start the DHCP server"; exit 1; }
  sleep 2
}

stop_lan() {
  docker rm -f "$DNSMASQ_C" >/dev/null 2>&1 || true
  docker network rm "$LAN" >/dev/null 2>&1 || true
}

# wait_booted <container>  — systemd reports running or degraded (a
# masked/failed unit we do not care about should not stall the run).
#
# The budget is deliberately generous. ifupdown's networking.service
# runs `ifup -a` synchronously and blocks the whole boot on dhclient,
# whose default timeout is 60s — an earlier 40s budget here reported
# "never finished booting" for a container that was still legitimately
# working, i.e. manufactured a failure rather than found one.
wait_booted() {
  local c=$1 i
  for i in $(seq 1 180); do
    case "$(docker exec "$c" systemctl is-system-running 2>/dev/null)" in
      running|degraded) return 0 ;;
    esac
    sleep 1
  done
  return 1
}

# boot_check <label> <image> <config-script>
# The config script writes the recipe to disk. It must NOT bring the
# bridge up by hand — the whole point is that the restart does it.
boot_check() {
  local label=$1 image=$2 config=$3
  [ -n "$ONLY" ] && [ "$ONLY" != "$label" ] && return 0

  local c="dhcp-bootverify-$label"
  echo
  echo "=============================================================="
  echo "  $label   ($image)"
  echo "=============================================================="
  docker rm -f "$c" >/dev/null 2>&1 || true

  # eth0 on the default bridge stays as Docker's management link; eth1
  # on the LAN is what the recipe enslaves.
  docker run -d --name "$c" --privileged --cgroupns=host \
      --tmpfs /run --tmpfs /run/lock "$image" >/dev/null 2>&1 \
    || { RESULTS+=("FAIL  $label — container would not start"); FAIL=$((FAIL+1)); return 0; }
  docker network connect "$LAN" "$c" >/dev/null 2>&1 \
    || { RESULTS+=("FAIL  $label — could not attach to $LAN"); FAIL=$((FAIL+1)); docker rm -f "$c" >/dev/null 2>&1; return 0; }

  if ! wait_booted "$c"; then
    RESULTS+=("FAIL  $label — systemd never finished its first boot")
    FAIL=$((FAIL+1)); docker logs "$c" 2>&1 | tail -5 | sed 's/^/    /'
    docker rm -f "$c" >/dev/null 2>&1; return 0
  fi

  echo "    writing config, then rebooting (docker restart = systemd re-runs its units)"
  docker exec "$c" bash -c "$config" 2>&1 | sed 's/^/    /'

  docker restart -t 15 "$c" >/dev/null 2>&1
  if ! wait_booted "$c"; then
    RESULTS+=("FAIL  $label — systemd never finished the boot AFTER restart")
    FAIL=$((FAIL+1)); docker rm -f "$c" >/dev/null 2>&1; return 0
  fi
  # DHCP on a freshly-created bridge is not instant.
  sleep 20

  local problems=""
  docker exec "$c" ip link show "$BRIDGE" >/dev/null 2>&1 \
    || problems+="bridge $BRIDGE did not survive the reboot; "

  local master
  master=$(docker exec "$c" sh -c "ip -o link show $NIC 2>/dev/null | grep -o 'master [^ ]*' | awk '{print \$2}'")
  [ "$master" = "$BRIDGE" ] || problems+="$NIC master is '${master:-none}' after boot, want $BRIDGE; "

  local stp
  stp=$(docker exec "$c" sh -c "ip -d link show $BRIDGE 2>/dev/null | grep -o 'stp_state [0-9]*' | awk '{print \$2}'")
  [ "$stp" = "0" ] || problems+="stp_state is '${stp:-unknown}' after boot, want 0; "

  # The lease must come from OUR dnsmasq, not Docker's IPAM — hence the
  # deliberately disjoint ranges. Confirm from the server's own file.
  local brip
  brip=$(docker exec "$c" sh -c "ip -4 -o addr show $BRIDGE 2>/dev/null | awk '{print \$4}' | cut -d/ -f1 | head -1")
  if [ -z "$brip" ]; then
    problems+="$BRIDGE has no address after the reboot (DHCP did not run at boot); "
  elif ! docker exec "$DNSMASQ_C" grep -q "$brip" /var/lib/misc/dnsmasq.leases 2>/dev/null; then
    problems+="$BRIDGE holds $brip but the DHCP server never leased it; "
  fi

  if [ -n "$problems" ]; then
    RESULTS+=("FAIL  $label — $problems")
    FAIL=$((FAIL+1))
    docker exec "$c" sh -c "ip -o addr; ip -o link" 2>&1 | sed 's/^/    /' | head -12
  else
    RESULTS+=("PASS  $label — survived a reboot: $BRIDGE up with $NIC enslaved, STP off, leased $brip")
    PASS=$((PASS+1))
  fi
  docker rm -f "$c" >/dev/null 2>&1
}

read -r -d '' CFG_IFUPDOWN <<EOS || true
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
systemctl enable networking >/dev/null 2>&1 || true
EOS

read -r -d '' CFG_NETPLAN <<EOS || true
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
systemctl enable systemd-networkd >/dev/null 2>&1 || true
EOS

read -r -d '' CFG_NETWORKD <<EOS || true
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
rm -f /etc/netplan/*.yaml
systemctl enable systemd-networkd >/dev/null 2>&1 || true
EOS

# nmcli writes profiles to /etc/NetworkManager/system-connections, which
# is exactly the persistence being tested — nothing else to do.
read -r -d '' CFG_NM <<EOS || true
nmcli con add type bridge ifname $BRIDGE con-name $BRIDGE \\
    ipv4.method auto bridge.stp no bridge.forward-delay 0 >/dev/null
nmcli con add type ethernet ifname $NIC con-name $BRIDGE-port \\
    master $BRIDGE slave-type bridge >/dev/null
echo "profiles written to /etc/NetworkManager/system-connections"
EOS

need_docker
build_images
start_lan
trap 'stop_lan; rm -rf "$WORK"' EXIT

echo
echo "Boot-persistence check: config is written, then the container is"
echo "restarted so systemd re-applies it from disk on its own."

boot_check ifupdown         dhcp-bootverify-ifupdown:local "$CFG_IFUPDOWN"
boot_check netplan          dhcp-bootverify-networkd:local "$CFG_NETPLAN"
boot_check systemd-networkd dhcp-bootverify-networkd:local "$CFG_NETWORKD"

# NetworkManager's boot path cannot be tested this way, and the reason
# is specific rather than a shrug: Docker configures the container's
# NIC before PID 1 runs, so NM finds eth1 already addressed, reports it
# as `connected (externally)` and assumes the device instead of
# applying the bridge-port profile. The bridge profile does autoconnect
# — it just never gets a port. A real machine boots with the NIC
# unconfigured, which is the case NM handles correctly, and
# verify-bridge-recipes.sh already proves the profile itself works when
# activated. Nothing to fix; this axis needs real hardware.
if [ -z "$ONLY" ] || [ "$ONLY" = "networkmanager" ]; then
  RESULTS+=("SKIP  networkmanager — Docker pre-addresses the NIC, so NM assumes it as 'connected (externally)'; needs a real boot")
fi

echo
echo "=============================================================="
printf '%s\n' "${RESULTS[@]}"
echo "--------------------------------------------------------------"
echo "passed: $PASS   failed: $FAIL"
echo
echo "A pass means the init system applied the recipe at boot, from"
echo "disk, unaided. The host's own docker.service-vs-network ordering"
echo "is NOT covered and still needs a real machine."
[ "$FAIL" -eq 0 ]
