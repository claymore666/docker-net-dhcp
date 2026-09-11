#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# One cell of the engine matrix (#670): the plugin's baseline, driven
# against ONE chosen Docker Engine version.
#
# WHY A NESTED DAEMON AND NOT THE RUNNER'S OWN
#
# The question this answers is "which engines does this plugin work on",
# and the runner has exactly one. #670 assumed the integration image
# already pinned its engine through a build arg; it does not —
# ci/runner-image/Dockerfile installs whatever `docker-ce` the Debian
# repository currently offers, and that repository carries 28.1.0 and up
# for trixie only. So an engine floor cannot be measured from that image
# at all, on any argument.
#
# `docker:<version>-dind` is the mechanism that does carry old engines:
# 20.10 through the current release, one tag each, still downloadable.
# The cell starts one, builds the whole fixture INSIDE it, and drives the
# plugin there. Everything the cell touches is in that container's own
# network namespace, so the row cannot disturb the runner, and two rows
# cannot see each other's veths, bridges or leases.
#
# WHAT THE CELL ASSERTS, and why each of them
#
#   plugin-install    the documented install path resolves and enables.
#                     `docker plugin install` is a different code path
#                     from the API calls the plugin itself makes, so an
#                     engine can pass every later step and still fail
#                     here (#670's second open question).
#   network-create    the remote driver is reachable and CreateNetwork
#                     is answered.
#   container-lease   a container comes up with an address from the
#                     fixture's pool.
#   dnsmasq-ack       that address was ACKed by the DHCP server, read
#                     from the SERVER's log. The plugin's own report of
#                     an address is not evidence that a lease exists.
#   restart-endpoint  the endpoint survives `docker restart` and comes
#                     back with the same address.
#
# The verdict is the FIRST failing step, by name, because "27 fails" and
# "27 fails at plugin-install" are different facts and only the second
# one can be acted on.
#
# Usage:
#   scripts/engine-baseline.sh <engine-tag> [plugin-rootfs-dir]
#
# <engine-tag> is the docker library tag suffix, e.g. `29`, `26`,
# `20.10`; the cell runs `docker:<engine-tag>-dind`.
#
# With a rootfs directory the cell installs THAT build (the tree's own,
# `make plugin`), which is what a lane on a branch has to measure. With
# no directory it installs $PLUGIN_REF from the registry, which is what
# a measurement of a released build uses.
#
# Env:
#   PLUGIN_REF        registry reference when no rootfs directory is given
#   KEEP=1            leave the container running for inspection
#   TEST_IMAGE        image the test container runs (default alpine:3.20)
#
# Exit: 0 the whole baseline passed, 1 a step failed, 2 the cell could
# not be set up (the engine itself is not runnable here).

set -uo pipefail

ENGINE_TAG="${1:-}"
ROOTFS_DIR="${2:-}"
PLUGIN_REF="${PLUGIN_REF:-}"
TEST_IMAGE="${TEST_IMAGE:-alpine:3.20}"

if [ -z "$ENGINE_TAG" ]; then
    echo "usage: $0 <engine-tag> [plugin-rootfs-dir]" >&2
    exit 2
fi

# The plugin name inside the nested daemon. A local name, not a
# registry reference, when the cell installs a rootfs: `docker plugin
# create` takes any name, and using one that looks like a pull would
# invite a reader to think the row measured the registry path.
LOCAL_PLUGIN="net-dhcp-under-test:matrix"

CONTAINER="engine-matrix-${ENGINE_TAG//./-}-$$"
NETWORK="em-net"
TEST_CTR="em-ctr"

# The fixture's own names and addresses. Deliberately NOT the
# integration harness's (dh-itest-*, 192.168.99.0/24 there as well):
# this cell shares no state with that suite and a reader comparing a
# log from each should not have to ask which fixture produced it.
SEGMENT="em-seg"
PARENT="em-parent"
PARENT_PEER="em-parentp"
SERVER_ADDR="192.168.99.1/24"
PARENT_ADDR="192.168.99.2/24"
POOL_START="192.168.99.10"
POOL_END="192.168.99.99"
# dnsmasq rounds anything shorter up to two minutes, so a smaller value
# here would be a number the server does not honour.
LEASE_TIME="2m"
FIXTURE_DIR="/var/log/engine-matrix"
DNSMASQ_LOG="$FIXTURE_DIR/dnsmasq.log"

STEP=""
candidate=""
ENGINE_VERSION=""
ENGINE_API=""

say() { printf '%s\n' "$*"; }

# verdict prints the one line a matrix row is read from. It is printed
# on every exit path, including the setup failure, so a row that
# produced no verdict means the job itself died rather than the cell
# reaching a conclusion.
verdict() {
    local result="$1" detail="${2:-}"
    printf 'ENGINE_MATRIX_ROW tag=%s engine=%s api=%s result=%s step=%s %s\n' \
        "$ENGINE_TAG" "${ENGINE_VERSION:-unknown}" "${ENGINE_API:-unknown}" \
        "$result" "${STEP:-none}" "$detail"
}

d() { docker exec "$CONTAINER" "$@"; }

# di is d with stdin attached. `docker exec` without -i closes stdin, so a
# heredoc handed to d reaches the shell inside as end-of-file and the
# block runs as nothing at all -- silently, with exit 0.
di() { docker exec -i "$CONTAINER" "$@"; }

cleanup() {
    if [ "${KEEP:-}" = "1" ]; then
        say "KEEP=1: leaving $CONTAINER running"
        return
    fi
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
    local detail="$1"
    say "FAIL at step '$STEP': $detail"
    say "--- plugin log ---"
    plugin_log || true
    say "--- dnsmasq log (tail) ---"
    d sh -c "tail -40 $DNSMASQ_LOG" 2>/dev/null || true
    verdict fail "$detail"
    exit 1
}

# plugin_log prints the plugin's own log from inside the nested daemon.
# The path is the daemon's plugin root, which is where a managed
# plugin's rootfs lives; a refusal at startup is in here and nowhere
# else the cell can reach.
plugin_log() {
    d sh -c 'for f in /var/lib/docker/plugins/*/rootfs/var/log/net-dhcp.log; do
        [ -f "$f" ] || continue
        echo "== $f"
        tail -60 "$f"
    done' 2>/dev/null
}

# ---- step 1: the engine itself ---------------------------------------
STEP=engine-start
say "== starting docker:${ENGINE_TAG}-dind"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

run_args=(-d --privileged --name "$CONTAINER")
[ -n "$ROOTFS_DIR" ] && run_args+=(-v "$ROOTFS_DIR:/plugin-src:ro")
if ! docker run "${run_args[@]}" "docker:${ENGINE_TAG}-dind" >/dev/null 2>&1; then
    verdict unavailable "docker:${ENGINE_TAG}-dind did not start"
    exit 2
fi

# The version has to LOOK like one. `docker exec` against a container
# whose runtime cannot start a process writes its error to stdout here,
# and an unchecked capture put "OCI runtime exec failed: ..." into the
# row's engine field — a verdict line that reads as a measurement of an
# engine called OCI. A version that does not parse is no version.
for _ in $(seq 1 60); do
    candidate="$(d docker version --format '{{.Server.Version}}' 2>/dev/null | tr -d '\r')"
    case "$candidate" in
        [0-9]*.[0-9]*) ENGINE_VERSION="$candidate"; break ;;
    esac
    sleep 2
done
if [ -z "$ENGINE_VERSION" ]; then
    say "--- container log ---"
    docker logs --tail 40 "$CONTAINER" 2>&1 || true
    verdict unavailable "the nested daemon never answered"
    exit 2
fi
ENGINE_API="$(d docker version --format '{{.Server.APIVersion}}' 2>/dev/null | tr -d '\r')"
case "$ENGINE_API" in
    [0-9]*.[0-9]*) ;;
    *) ENGINE_API="unknown" ;;
esac
say "== engine $ENGINE_VERSION, API $ENGINE_API"

# ---- step 2: the DHCP fixture ----------------------------------------
STEP=fixture
say "== fixture"
d apk add --no-cache dnsmasq iproute2 >/dev/null 2>&1 || fail "could not install dnsmasq and iproute2"
di sh -s <<EOF || fail "could not build the veth segment or start dnsmasq"
set -e
ip link add $SEGMENT type bridge
ip addr add $SERVER_ADDR dev $SEGMENT
ip link set $SEGMENT up
ip link add $PARENT type veth peer name $PARENT_PEER
ip link set $PARENT_PEER master $SEGMENT
ip link set $PARENT_PEER up
ip link set $PARENT up
ip addr add $PARENT_ADDR dev $PARENT
mkdir -p $FIXTURE_DIR /var/lib/net-dhcp
dnsmasq --interface=$SEGMENT --bind-interfaces --except-interface=lo \\
  --dhcp-range=$POOL_START,$POOL_END,$LEASE_TIME --log-dhcp \\
  --log-facility=$DNSMASQ_LOG --port=0 \\
  --dhcp-leasefile=$FIXTURE_DIR/leases --pid-file=$FIXTURE_DIR/dnsmasq.pid
EOF

# The server must be listening before the plugin asks it for anything;
# without this the first DISCOVER can precede the bind and the row
# reports a lease timeout that says nothing about the engine.
for _ in $(seq 1 30); do
    d sh -c "[ -f $FIXTURE_DIR/dnsmasq.pid ]" 2>/dev/null && break
    sleep 1
done
d sh -c "[ -f $FIXTURE_DIR/dnsmasq.pid ]" || fail "dnsmasq did not start"

# ---- step 3: the plugin ----------------------------------------------
STEP=plugin-install
if [ -n "$ROOTFS_DIR" ]; then
    say "== plugin create from the tree's build"
    d docker plugin create "$LOCAL_PLUGIN" /plugin-src \
        || fail "docker plugin create was refused"
    PLUGIN_NAME="$LOCAL_PLUGIN"
    STEP=plugin-enable
    d docker plugin enable "$PLUGIN_NAME" \
        || fail "docker plugin enable was refused"
else
    [ -n "$PLUGIN_REF" ] || fail "no rootfs directory and no PLUGIN_REF"
    say "== plugin install $PLUGIN_REF"
    d docker plugin install --grant-all-permissions "$PLUGIN_REF" \
        || fail "docker plugin install was refused"
    PLUGIN_NAME="$PLUGIN_REF"
fi

STEP=plugin-enabled
d docker plugin inspect -f '{{.Enabled}}' "$PLUGIN_NAME" 2>/dev/null | grep -x true >/dev/null \
    || fail "the plugin is installed but not enabled"

# ---- step 4: the network ---------------------------------------------
STEP=network-create
say "== network create"
d docker network create -d "$PLUGIN_NAME" --ipam-driver null \
    -o mode=macvlan -o parent="$PARENT" "$NETWORK" >/dev/null \
    || fail "docker network create was refused"

# ---- step 5: a container with a lease ---------------------------------
STEP=container-lease
say "== container"
docker pull -q "$TEST_IMAGE" >/dev/null 2>&1 || true
docker save "$TEST_IMAGE" | docker exec -i "$CONTAINER" docker load >/dev/null 2>&1 \
    || d docker pull "$TEST_IMAGE" >/dev/null 2>&1 \
    || fail "could not get $TEST_IMAGE into the nested daemon"

d docker run -d --name "$TEST_CTR" --network "$NETWORK" "$TEST_IMAGE" sleep 600 >/dev/null \
    || fail "the container did not start on the plugin's network"

ipv4=""
for _ in $(seq 1 30); do
    ipv4="$(d docker inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" "$TEST_CTR" 2>/dev/null | tr -d '\r')"
    [ -n "$ipv4" ] && break
    sleep 1
done
[ -n "$ipv4" ] || fail "the container never reported an address"
say "== container address $ipv4"

mac="$(d docker inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").MacAddress}}" "$TEST_CTR" 2>/dev/null | tr -d '\r')"

# ---- step 6: the server's own record of that lease --------------------
STEP=dnsmasq-ack
d sh -c "grep -q 'DHCPACK($SEGMENT) $ipv4 ' $DNSMASQ_LOG" \
    || fail "the DHCP server logged no ACK for $ipv4"
say "== dnsmasq ACKed $ipv4"

# ---- step 7: the endpoint across a restart ----------------------------
STEP=restart-endpoint
d docker restart "$TEST_CTR" >/dev/null || fail "docker restart failed"
after=""
for _ in $(seq 1 30); do
    after="$(d docker inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" "$TEST_CTR" 2>/dev/null | tr -d '\r')"
    [ -n "$after" ] && break
    sleep 1
done
[ -n "$after" ] || fail "the endpoint reported no address after restart"
d sh -c "docker exec $TEST_CTR ip -4 addr" | grep "$after" >/dev/null \
    || fail "the container's own interface does not carry $after after restart"
say "== after restart: $after (was $ipv4, mac $mac)"

STEP=complete
verdict pass "address=$ipv4 after_restart=$after"
exit 0
