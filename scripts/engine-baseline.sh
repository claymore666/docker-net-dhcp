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
# THE CONTROL COMES FIRST, and it is the step that makes the rest of the
# row mean anything. An engine that cannot start ANY container in this
# rig — an old runtime on a cgroup v2 host is the case that actually
# occurs — would otherwise fail the first plugin step and be recorded as
# "the plugin fails on this engine", which is not what happened and not
# something this cell measured. So `engine-control` runs one ordinary
# container with no plugin, no driver and no network of ours, and a row
# that cannot get past it is `unavailable`: no evidence either way.
# It runs on EVERY row, not only the old ones, so a red control measures
# the engine rather than the difficulty of the rig.
#
# WHAT THE CELL ASSERTS, and why each of them
#
#   engine-control    the engine runs an ordinary container at all.
#   plugin-create     the tree's rootfs becomes a plugin and enables.
#                     NOT `docker plugin install`: with a rootfs there
#                     is nothing in a registry to install from, so this
#                     row does not answer #670's second open question
#                     (whether the registry install path imposes a
#                     higher floor). The install branch below is the one
#                     that does, and only a measurement of a published
#                     reference reaches it.
#   network-create    the remote driver is reachable and CreateNetwork
#                     is answered, once per documented network shape.
#   container-lease   a container comes up with an address from the
#                     fixture's pool, once per documented shape.
#   dnsmasq-ack       that address was ACKed by the DHCP server, read
#                     from the SERVER's log. The plugin's own report of
#                     an address is not evidence that a lease exists.
#   restart-endpoint  the endpoint survives `docker restart`, comes back
#                     with the SAME address, and the server logs a fresh
#                     ACK for it. An engine that re-leased the endpoint
#                     onto a different address would satisfy "it has an
#                     address" and break every container that was
#                     addressed by the old one.
#
# THE SHAPES ARE DERIVED FROM THE DOCUMENTATION, not listed here.
# bridge, macvlan and ipvlan take different paths through CreateNetwork
# and CreateEndpoint, and an engine that breaks one of them breaks the
# plugin for the users on that mode. They share one L2 segment and one
# DHCP server here, each on its own netdev (#556). The IPAM driver is
# the second axis and it was invisible: every network here was created
# with `--ipam-driver null`, so the documented `--ipam-driver <this
# plugin>` shape had never run on any engine in this matrix, on any row,
# since it was published (#1013).
#
# A list would have the same hole again at the next shape. So the cell
# reads every `docker network create` this project documents out of
# docs/reference.md and drives one network per distinct shape it finds,
# and a documented block it cannot turn into a network is a failure and
# never a silent skip. `--print-shapes` and `--print-shape-sources`
# print that derivation without starting anything, which is what
# scripts/check-engine-matrix-shapes.sh reads on every pull request.
#
# The verdict is the FIRST failing step, by name, because "27 fails" and
# "27 fails at plugin-create" are different facts and only the second
# one can be acted on.
#
# Usage:
#   scripts/engine-baseline.sh <engine-tag> [plugin-rootfs-dir]
#   scripts/engine-baseline.sh --print-shapes
#   scripts/engine-baseline.sh --print-shape-sources
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
#   ENGINE_SHAPES_DOC the document the network shapes are derived from
#                     (default docs/reference.md)
#
# Exit: 0 the whole baseline passed, 1 a step failed, 2 the cell could
# not be set up (the engine itself is not runnable here).

set -uo pipefail

# ---- the shapes the documentation promises ---------------------------
#
# The parser reads fenced blocks only, joins backslash continuations,
# and keeps the commands that name this plugin as network driver, as
# IPAM driver, or as both. Each one becomes `<ipam>|<mode>|<interface
# option>`: the fixture supplies the netdev, so two documented examples
# that differ only in the interface NAME are one shape.
#
# Every refusal below names the line it refused, and there is no branch
# that drops a documented command quietly: a block this cannot map is
# the case #1013 is about, and it has to be louder than a shape that is
# merely missing.
SHAPES_DOC="${ENGINE_SHAPES_DOC:-$(dirname "$0")/../docs/reference.md}"

# The marker that says a driver reference is this plugin. A reference
# carries a registry, a path and a tag, all of which move between
# releases; the project name does not.
SHAPES_PLUGIN_MARKER="docker-net-dhcp"

derive_shapes() {
    local mode="${1:-shapes}"
    if [ ! -f "$SHAPES_DOC" ]; then
        echo "$SHAPES_DOC does not exist, so no network shape can be derived" >&2
        return 2
    fi
    awk -v PLUGIN="$SHAPES_PLUGIN_MARKER" -v MODE="$mode" '
    function refuse(lineno, why) {
        printf("%s:%d: %s\n", FILENAME, lineno, why) > "/dev/stderr"
        bad = 1
    }
    function emit(   n, i, tok, kv, drv, ipam, mode, ifk, kind, key) {
        if (cmd == "") return
        if (index(cmd, "docker network create") == 0) { cmd = ""; return }
        n = split(cmd, tok, /[ \t]+/)
        drv = ""; ipam = ""; mode = ""; ifk = ""
        for (i = 1; i <= n; i++) {
            if (tok[i] == "-d" || tok[i] == "--driver") { i++; drv = tok[i]; continue }
            if (tok[i] == "--ipam-driver") { i++; ipam = tok[i]; continue }
            if (tok[i] == "-o" || tok[i] == "--opt") {
                i++
                split(tok[i], kv, "=")
                if (kv[1] == "mode") mode = kv[2]
                else if (kv[1] == "bridge" || kv[1] == "parent") ifk = kv[1]
                continue
            }
        }
        if (index(drv, PLUGIN) == 0 && index(ipam, PLUGIN) == 0) {
            if (MODE == "sources") printf("%d out-of-scope -\n", start)
            cmd = ""
            return
        }
        key = "-"
        if (index(drv, PLUGIN) == 0) {
            refuse(start, "this example asks this plugin for its addresses and creates the network with another driver (-d " drv "), which the plugin refuses; the cell has no network to drive for it")
        } else if (ipam == "") {
            refuse(start, "this example names no --ipam-driver, so the network would be given Docker built-in IPAM, which this plugin refuses at CreateNetwork")
        } else if (ipam != "null" && index(ipam, PLUGIN) == 0) {
            refuse(start, "this example names --ipam-driver " ipam ", which is neither null nor this plugin, and the cell cannot install it")
        } else {
            kind = (ipam == "null") ? "null" : "plugin"
            if (mode == "") mode = "bridge"
            if (mode != "bridge" && mode != "macvlan" && mode != "ipvlan") {
                refuse(start, "this example names -o mode=" mode ", which is not a mode this plugin offers")
            } else if (ifk == "") {
                refuse(start, "this example names neither -o bridge= nor -o parent=, so the cell cannot say which netdev of its fixture the network belongs on")
            } else {
                key = kind "|" mode "|" ifk
            }
        }
        if (MODE == "sources") {
            printf("%d in-scope %s\n", start, key)
        } else if (key != "-" && !(key in seen)) {
            seen[key] = 1
            print key
        }
        cmd = ""
    }
    /^[ \t]*```/ { emit(); inblk = 1 - inblk; cmd = ""; cont = 0; next }
    inblk == 0 { next }
    {
        line = $0
        sub(/^[ \t]*/, "", line)
        sub(/^\$[ \t]+/, "", line)
        cont_next = (line ~ /\\[ \t]*$/)
        sub(/\\[ \t]*$/, "", line)
        if (cont) {
            cmd = cmd " " line
        } else {
            emit()
            cmd = line
            start = NR
        }
        cont = cont_next
    }
    END { emit(); if (bad) exit 1 }
    ' "$SHAPES_DOC"
}

case "${1:-}" in
    --print-shapes)        derive_shapes shapes;  exit $? ;;
    --print-shape-sources) derive_shapes sources; exit $? ;;
esac

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

# The fixture's own names and addresses. Deliberately NOT the
# integration harness's (dh-itest-*, 192.168.99.0/24 there as well):
# this cell shares no state with that suite and a reader comparing a
# log from each should not have to ask which fixture produced it.
SEGMENT="em-seg"
PARENT="em-parent"
PARENT_PEER="em-parentp"
IPVLAN_PARENT="em-ipvl"
IPVLAN_PEER="em-ipvlp"
SERVER_ADDR="192.168.99.1/24"
PARENT_ADDR="192.168.99.2/24"
IPVLAN_ADDR="192.168.99.3/24"
POOL_START="192.168.99.10"
POOL_END="192.168.99.99"
# dnsmasq rounds anything shorter up to two minutes, so a smaller value
# here would be a number the server does not honour.
LEASE_TIME="2m"
FIXTURE_DIR="/var/log/engine-matrix"
DNSMASQ_LOG="$FIXTURE_DIR/dnsmasq.log"

# The macvlan cell is the one the restart is driven on, and its names
# are referenced after the mode loop.
MACVLAN_NET="em-net-macvlan"
MACVLAN_CTR="em-ctr-macvlan"

STEP=""
candidate=""
ENGINE_VERSION=""
ENGINE_API=""
MODE_ADDR=""

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

# unavailable ends the row with NO verdict about the plugin. It is for
# the cases where the rig never got far enough to ask the question: the
# nested daemon did not come up, or this engine cannot run a container
# here at all. scripts/engine-floor.sh treats such a row as unmeasured,
# which is a different thing from a failure and is reported differently.
unavailable() {
    local detail="$1"
    say "UNAVAILABLE at step '$STEP': $detail"
    say "--- container log ---"
    docker logs --tail 40 "$CONTAINER" 2>&1 || true
    verdict unavailable "$detail"
    exit 2
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

# acks counts the server's ACK lines for one address. A count and not a
# presence test, because the restart has to be shown to produce a NEW
# lease exchange rather than to have left the first one's line lying in
# the log.
acks() {
    local addr="$1"
    d sh -c "grep -c 'DHCPACK($SEGMENT) $addr ' $DNSMASQ_LOG 2>/dev/null" | tr -d '\r' | head -1
}

# lease_in_shape drives one documented network shape end to end and
# leaves the address in MODE_ADDR.
#
# A GLOBAL AND NOT AN ECHOED RETURN VALUE: `fail` exits, and an exit
# inside a command substitution ends the subshell, not the cell. A row
# would then continue past a failed step and report a later verdict.
#
# EVERY EXIT FROM HERE IS `fail`, never `unavailable`. A create this
# engine refuses is the measurement the row exists to take, and
# `unavailable` would record it as "the cell could not ask", which
# scripts/engine-floor.sh reads as no evidence either way.
lease_in_shape() {
    local ipam="$1" mode="$2" net="$3" ctr="$4"; shift 4
    local label="$ipam-$mode" ipam_arg=""
    MODE_ADDR=""

    case "$ipam" in
        null)   ipam_arg="null" ;;
        plugin) ipam_arg="$PLUGIN_NAME" ;;
        *)      STEP="network-create-$label"; fail "the derived shape names IPAM driver '$ipam', which this cell cannot install" ;;
    esac

    STEP="network-create-$label"
    d docker network create -d "$PLUGIN_NAME" --ipam-driver "$ipam_arg" -o mode="$mode" "$@" "$net" >/dev/null \
        || fail "docker network create --ipam-driver $ipam -o mode=$mode was refused"

    STEP="container-lease-$label"
    d docker run -d --name "$ctr" --network "$net" "$TEST_IMAGE" sleep 600 >/dev/null \
        || fail "the container did not start on the $label network"

    local addr=""
    for _ in $(seq 1 30); do
        addr="$(d docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$ctr" 2>/dev/null | tr -d '\r')"
        [ -n "$addr" ] && break
        sleep 1
    done
    [ -n "$addr" ] || fail "the $label container never reported an address"

    STEP="dnsmasq-ack-$label"
    d sh -c "grep 'DHCPACK($SEGMENT) $addr ' $DNSMASQ_LOG >/dev/null" \
        || fail "the DHCP server logged no ACK for $addr ($label)"

    say "== $label: $addr, ACKed by the DHCP server"
    MODE_ADDR="$addr"
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
    unavailable "the nested daemon never answered"
fi
ENGINE_API="$(d docker version --format '{{.Server.APIVersion}}' 2>/dev/null | tr -d '\r')"
case "$ENGINE_API" in
    [0-9]*.[0-9]*) ;;
    *) ENGINE_API="unknown" ;;
esac
say "== engine $ENGINE_VERSION, API $ENGINE_API"

# ---- step 2: the control ---------------------------------------------
# Nothing of ours runs in this step. It answers one question: can this
# engine, on this host, start a container and run a process in it. See
# the header — a row that fails here has measured the rig, not the
# plugin, and says so in its verdict.
STEP=engine-control
say "== control: one ordinary container, no plugin"
docker pull -q "$TEST_IMAGE" >/dev/null 2>&1 || true
docker save "$TEST_IMAGE" | docker exec -i "$CONTAINER" docker load >/dev/null 2>&1 \
    || d docker pull "$TEST_IMAGE" >/dev/null 2>&1 \
    || unavailable "could not get $TEST_IMAGE into the nested daemon"

if ! d docker run --rm "$TEST_IMAGE" true; then
    unavailable "this engine cannot start an ordinary container on this host, so nothing here measures the plugin"
fi
say "== control passed"

# ---- step 3: the DHCP fixture ----------------------------------------
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
ip link add $IPVLAN_PARENT type veth peer name $IPVLAN_PEER
ip link set $IPVLAN_PEER master $SEGMENT
ip link set $IPVLAN_PEER up
ip link set $IPVLAN_PARENT up
ip addr add $IPVLAN_ADDR dev $IPVLAN_PARENT
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

# ---- step 4: the plugin ----------------------------------------------
if [ -n "$ROOTFS_DIR" ]; then
    STEP=plugin-create
    say "== plugin create from the tree's build"
    d docker plugin create "$LOCAL_PLUGIN" /plugin-src \
        || fail "docker plugin create was refused"
    PLUGIN_NAME="$LOCAL_PLUGIN"
else
    STEP=plugin-install
    [ -n "$PLUGIN_REF" ] || fail "no rootfs directory and no PLUGIN_REF"
    say "== plugin install $PLUGIN_REF"
    d docker plugin install --grant-all-permissions "$PLUGIN_REF" \
        || fail "docker plugin install was refused"
    PLUGIN_NAME="$PLUGIN_REF"
fi

STEP=plugin-enable
d docker plugin enable "$PLUGIN_NAME" \
    || fail "docker plugin enable was refused"

STEP=plugin-enabled
d docker plugin inspect -f '{{.Enabled}}' "$PLUGIN_NAME" 2>/dev/null | grep -x true >/dev/null \
    || fail "the plugin is installed but not enabled"

# ---- step 5: a lease in every shape the documentation promises -------
STEP=shapes
shapes="$(derive_shapes shapes)" \
    || fail "the network shapes could not be derived from $SHAPES_DOC; the messages above name the block"
[ -n "$shapes" ] \
    || fail "$SHAPES_DOC documents no network for this plugin, so this row would measure nothing"

# The loop reads from a REDIRECT and not a pipe: `fail` exits, and an
# exit inside a pipeline ends a subshell while the row walks on.
macvlan_addr=""
while IFS='|' read -r shape_ipam shape_mode shape_ifk; do
    [ -n "$shape_ipam" ] || continue
    net="em-net-$shape_ipam-$shape_mode"
    ctr="em-ctr-$shape_ipam-$shape_mode"
    STEP="network-create-$shape_ipam-$shape_mode"
    case "$shape_mode:$shape_ifk" in
        bridge:bridge)  dev_opt=(-o bridge="$SEGMENT") ;;
        macvlan:parent) dev_opt=(-o parent="$PARENT") ;;
        ipvlan:parent)  dev_opt=(-o parent="$IPVLAN_PARENT") ;;
        *) fail "a documented shape asks for mode=$shape_mode on -o $shape_ifk=, and this fixture has no netdev for that pair" ;;
    esac
    lease_in_shape "$shape_ipam" "$shape_mode" "$net" "$ctr" "${dev_opt[@]}"
    if [ "$shape_ipam-$shape_mode" = "null-macvlan" ]; then
        MACVLAN_NET="$net"
        MACVLAN_CTR="$ctr"
        macvlan_addr="$MODE_ADDR"
    fi
done <<EOF
$shapes
EOF

# The restart below is driven on the documented macvlan network. It is
# derived like everything else, so a document that stops promising that
# shape takes the restart measurement with it and says so here instead
# of restarting a container that was never created.
STEP=restart-subject
[ -n "$macvlan_addr" ] \
    || fail "$SHAPES_DOC documents no macvlan network with --ipam-driver null, which is the shape the restart is measured on"

# ---- step 6: the endpoint across a restart ----------------------------
# Driven on the macvlan network. The restart path is CreateEndpoint and
# Join again with the stored lease, which is the same code for all three
# modes; what differs between them is the netdev, and that is what the
# three cells above measured.
STEP=restart-endpoint
acks_before="$(acks "$macvlan_addr")"
d docker restart "$MACVLAN_CTR" >/dev/null || fail "docker restart failed"
after=""
for _ in $(seq 1 30); do
    after="$(d docker inspect -f "{{(index .NetworkSettings.Networks \"$MACVLAN_NET\").IPAddress}}" "$MACVLAN_CTR" 2>/dev/null | tr -d '\r')"
    [ -n "$after" ] && break
    sleep 1
done
[ -n "$after" ] || fail "the endpoint reported no address after restart"

# THE ADDRESS, not merely an address. A restart that re-leases onto a
# different address keeps the container running and breaks everything
# that was addressed by the old one, and all of it passes a test that
# only asks whether the field is non-empty.
[ "$after" = "$macvlan_addr" ] \
    || fail "the endpoint came back on $after, not $macvlan_addr, after restart"

d sh -c "docker exec $MACVLAN_CTR ip -4 addr" | grep "$after" >/dev/null \
    || fail "the container's own interface does not carry $after after restart"

# The SERVER's record of the restart, for the reason the first ACK step
# gives: the engine's own report of an address is not evidence that a
# lease exists behind it. A fresh ACK line for the same address is.
acks_after="$acks_before"
for _ in $(seq 1 30); do
    acks_after="$(acks "$after")"
    case "$acks_after" in
        ''|*[!0-9]*) acks_after=0 ;;
    esac
    [ "$acks_after" -gt "$acks_before" ] && break
    sleep 1
done
[ "$acks_after" -gt "$acks_before" ] \
    || fail "the DHCP server logged no new ACK for $after after the restart (was $acks_before, still $acks_after)"
say "== after restart: $after, re-ACKed by the DHCP server"

STEP=complete
verdict pass "macvlan=$macvlan_addr after_restart=$after"
exit 0
