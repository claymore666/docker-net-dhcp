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
#   endpoint-release  the endpoint comes OFF the network and the network
#                     is removed, once per documented shape. A shape
#                     that leases correctly and cannot be dismantled is
#                     one a user meets the second time they run the
#                     documented commands, and until #1014 nothing here
#                     reached it: the only removals in this file were
#                     `docker rm -f` of the nested daemon's container.
#   option-<name>     each documented option, driven with an observer
#                     outside the plugin and a control (#1015); the list
#                     is derived like the shapes, `--print-option-steps`.
#   health-engine-version  Plugin.Health reports the nested daemon's
#                     version, carried in the verdict detail (#1015).
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
#   scripts/engine-baseline.sh --print-option-steps
#   scripts/engine-baseline.sh --print-documented-options
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
#   ENGINE_SHAPES_DOC the document the network shapes and the options
#                     are derived from (default docs/reference.md)
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


# ---- the options the documentation promises (#1015) ------------------
#
# One line per documented option: `<option>|<kind>|<observer>`. Kinds:
# shape (driven by the shape steps), step (driven below, observed outside
# the plugin), measure (recorded in the row's detail, not asserted) and
# not-driven (the observer needs a device the cell cannot have).
# scripts/check-engine-matrix-options.sh compares this catalogue with the
# reference on every pull request; the cell runs the documented set.
OPTION_CATALOGUE='mode|shape|the null-bridge, null-macvlan, null-ipvlan and plugin-macvlan shape steps
bridge|shape|the null-bridge shape step
parent|shape|the null-macvlan, null-ipvlan and plugin-macvlan shape steps
gateway|step|container default route via the named address; control without it: via the server router
ipv6|step|fresh DHCPv6 reply in the server log for the address the container holds; ipv6=true with ipv6_mode=off refused
ipv6_mode|step|dhcp: fresh DHCPv6 reply for the held address; slaac: fresh router advertisement, the container holds and Docker reports an address in the ra-only prefix; control off: no global address; bad value refused
ipv6_main_prefix|step|two advertised prefixes: Docker reports the address in the named prefix, both ways round; refused with ipv6_mode=dhcp
ipv6_auto_strict|step|managed-flag advertisement with a silent DHCPv6 server: true fails docker run, false holds an address in the autonomous prefix
lease_timeout|step|server-less bridge: docker run fails within the short timeout and not within the long one; a value under the probe window refused
conflict_check|step|server pins the MAC to an address a veth holds: wait logs the DHCPDECLINE before docker run returns, async runs on the address first, off sends no DHCPDECLINE
ignore_conflicts|step|a second network on the same bridge refused without it and accepted with it, its container ACKed
skip_routes|step|option 121 route in the container: present when false, absent when true
propagate_dns|step|option 6 server in /etc/resolv.conf when true, absent when false
propagate_mtu|step|option 26 sets the link MTU when true, not when false; an MTU under 576 not applied
client_id|step|client-id in the server lease file equals the option; control differs
vendor_class|step|fresh vendor class line in the server log names the option; control the default
validate_dhcp|step|server-less parent refused, served parent accepted, false on the server-less parent accepted, bridge mode refused
dhcp_servers|step|second server on the segment: the allowed server ACKs the address, both ways round
dhcp_deny_servers|step|second server on the segment: the other server ACKs the address, both ways round
register_dns|step|fresh option 81 in the server log when true, none when false
audit_log|step|leases.jsonl gains a bound line for the address when true, nothing when false
release_lease|step|fresh DHCPRELEASE for the address after docker stop with on_stop, none with never
host_ifname|step|ip link in the host netns shows the container name; control the generated name; macvlan refused naming the mode; bad value refused
ip|step|fresh ACK in the server log for the requested address, held by the container
com.docker.network.endpoint.ifname|measure|whether the container link carries the requested name is recorded; an invalid name is refused
--mac-address|step|fresh ACK in the server log carries the MAC
--hostname|step|fresh client-provided name in the server log equals the flag
--ip6|measure|whether the wire Solicit carries the requested address is recorded: with null IPAM Docker hands the plugin no IPv6 address at endpoint creation (#960)
--ip|step|IPAM-mode network with --subnet: fresh ACK in the server log for the requested address, held by the container; without --subnet the daemon answer is recorded as ip_without_subnet'

# derive_options prints `<table>|<option>` for the detailed network and
# per-endpoint tables (net, ep), the At a glance tables (gnet, gep) and
# each flag the container-level flags sentence names (flag) (#1015).
derive_options() {
    if [ ! -f "$SHAPES_DOC" ]; then
        echo "$SHAPES_DOC does not exist, so no option can be derived" >&2
        return 2
    fi
    awk '
    function row(kind,   f, name) {
        if ($0 !~ /^\| `/) return
        split($0, f, "|")
        name = f[2]
        gsub(/^[ \t]*`|`[ \t]*$/, "", name)
        print kind "|" name
    }
    /^## / { sec = $0; tbl = ""; inflags = 0; next }
    /^\|/ {
        if (tbl == "") {
            tbl = "other"
            if (sec == "## Driver options (network-level)" && !netdone && $0 ~ /^\| option \| modes \|/) tbl = "net"
            if (sec == "## Driver options (per-endpoint)" && !epdone && $0 ~ /^\| option \|/) tbl = "ep"
            if (sec == "## At a glance" && $0 ~ /^\| option \| modes \| default \|/) tbl = "gnet"
            if (sec == "## At a glance" && $0 ~ /^\| option \| default \|/) tbl = "gep"
            next
        }
        if (tbl != "other") row(tbl)
        next
    }
    { if (tbl == "net") netdone = 1; if (tbl == "ep") epdone = 1; tbl = "" }
    sec == "## At a glance" && /^\*\*\[Container-level flags\]/ { inflags = 1 }
    inflags && /^[ \t]*$/ { inflags = 0 }
    inflags {
        s = $0
        while (match(s, /`--[a-z0-9-]+`/)) {
            print "flag|" substr(s, RSTART + 1, RLENGTH - 2)
            s = substr(s, RSTART + RLENGTH)
        }
    }
    ' "$SHAPES_DOC"
}

# documented_options prints the options the cell must drive, and refuses
# a reference in which any of the three sources reads empty: a renamed
# heading must not turn into a row that drives nothing (#1015).
documented_options() {
    local raw kind
    raw="$(derive_options)" || return $?
    for kind in net ep flag; do
        printf '%s\n' "$raw" | grep "^$kind|" >/dev/null || {
            echo "$SHAPES_DOC: no $kind option could be read; the heading, table header or flags sentence moved" >&2
            return 1
        }
    done
    printf '%s\n' "$raw" | awk -F'|' '$1 == "net" || $1 == "ep" || $1 == "flag" { print $2 }'
}

# derive_option_steps prints the catalogue line of every documented
# option (`<option>||` when there is none), then every catalogue line no
# document row names (#1015).
derive_option_steps() {
    local documented
    documented="$(documented_options)" || return $?
    printf '%s\n' "$OPTION_CATALOGUE" | DOC="$documented" awk -F'|' '
    BEGIN { n = split(ENVIRON["DOC"], d, "\n"); for (i = 1; i <= n; i++) if (d[i] != "") isdoc[d[i]] = 1 }
    $1 != "" { cat[$1] = $0; order[++m] = $1 }
    END {
        for (i = 1; i <= n; i++) { if (d[i] == "") continue; if (d[i] in cat) print cat[d[i]]; else print d[i] "||" }
        for (j = 1; j <= m; j++) if (!(order[j] in isdoc)) print cat[order[j]]
    }'
}

case "${1:-}" in
    --print-shapes)              derive_shapes shapes;  exit $? ;;
    --print-shape-sources)       derive_shapes sources; exit $? ;;
    --print-option-steps)        derive_option_steps;   exit $? ;;
    --print-documented-options)  documented_options;    exit $? ;;
esac

ENGINE_TAG="${1:-}"
ROOTFS_DIR="${2:-}"
PLUGIN_REF="${PLUGIN_REF:-}"
TEST_IMAGE="${TEST_IMAGE:-alpine:3.20}"

if [ -z "$ENGINE_TAG" ]; then
    echo "usage: $0 <engine-tag> [plugin-rootfs-dir]" >&2
    exit 2
fi

# The plugin name inside the nested daemon when the cell installs a
# rootfs; the :engine-matrix tag says it is the tree's build, not a pull.
# The name must match the plugin's own driver pattern (pkg/plugin/plugin.go,
# #74): under any other name the bridge-in-use check cannot see this
# cell's networks and ignore_conflicts has nothing to override (#1015).
LOCAL_PLUGIN="claymore666/docker-net-dhcp:engine-matrix"

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
DNSMASQ2_LOG="$FIXTURE_DIR/dnsmasq2.log"
AUDIT_LOG="/var/lib/net-dhcp/leases.jsonl"
V6_BRIDGE="em-v6"
V6_POOL4="192.168.98.10,192.168.98.99"
V6_PREFIX_A="fd00:98::"
V6_PREFIX_B="fd00:97::"
V6_LOG="$FIXTURE_DIR/dnsmasq-v6.log"

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
d apk add --no-cache dnsmasq iproute2 curl tcpdump >/dev/null 2>&1 || fail "could not install dnsmasq, iproute2, curl and tcpdump"
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
ip link add em-sq type veth peer name em-sqp
ip link set em-sqp master $SEGMENT
ip link set em-sqp up
ip link set em-sq up
ip link add em-dead type veth peer name em-deadp
ip link set em-deadp up
ip link set em-dead up
ip link add em-quiet type bridge
ip link set em-quiet up
ip link add $V6_BRIDGE type bridge forward_delay 0
ip addr add 192.168.98.1/24 dev $V6_BRIDGE
ip addr add ${V6_PREFIX_A}1/64 dev $V6_BRIDGE nodad
ip addr add ${V6_PREFIX_B}1/64 dev $V6_BRIDGE nodad
ip link set $V6_BRIDGE up
ip netns add em-ns2
ip link add em-s2p type veth peer name em-s2
ip link set em-s2 netns em-ns2
ip link set em-s2p master $SEGMENT
ip link set em-s2p up
ip netns exec em-ns2 ip addr add 192.168.99.5/24 dev em-s2
ip netns exec em-ns2 ip link set em-s2 up
mkdir -p $FIXTURE_DIR /var/lib/net-dhcp
dnsmasq --interface=$SEGMENT --bind-interfaces --except-interface=lo \\
  --dhcp-range=$POOL_START,$POOL_END,$LEASE_TIME --log-dhcp \\
  --dhcp-host=02:00:00:00:e0:01,192.168.99.60 \\
  --dhcp-host=02:00:00:00:e0:02,192.168.99.61 \\
  --dhcp-host=02:00:00:00:e0:03,192.168.99.62 \\
  --dhcp-host=02:00:00:00:e1:01,set:emopt --dhcp-host=02:00:00:00:e1:02,set:emmtu \\
  --dhcp-option=tag:emopt,option:mtu,1400 \\
  --dhcp-option=tag:emopt,option:dns-server,192.168.99.53 \\
  --dhcp-option=tag:emopt,option:classless-static-route,10.77.0.0/16,192.168.99.1 \\
  --dhcp-option=tag:emmtu,option:mtu,400 \\
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
DRIVEN=""
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
    DRIVEN="$DRIVEN$net|$ctr|$shape_ipam-$shape_mode
"
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

# ---- step 7: the endpoint comes off, and the network goes away -------
# AFTER the restart, not inside lease_in_shape, because step 6 restarts
# one of these containers on one of these networks: dismantling a shape
# as soon as it leased would delete the subject the restart measures.
#
# The loop reads from a REDIRECT for the reason the drive loop gives:
# `fail` exits, and an exit inside a pipeline ends a subshell while the
# row walks on.
while IFS='|' read -r net ctr label; do
    [ -n "$net" ] || continue

    STEP="endpoint-release-$label"
    d docker network disconnect "$net" "$ctr" \
        || fail "docker network disconnect was refused on the $label network"

    # THE ENDPOINT IS GONE, not merely the command exited 0. A
    # disconnect that leaves the network on the container leaves a lease
    # nobody will renew and an address the DHCP server still believes
    # is taken. The network's own key is what is read, because an empty
    # address field is also what a template error prints.
    still="$(d docker inspect -f '{{json .NetworkSettings.Networks}}' "$ctr" 2>/dev/null | tr -d '\r')"
    case "$still" in
        *"\"$net\""*) fail "the $label endpoint is still attached to $net after disconnect" ;;
    esac

    d docker rm -f "$ctr" >/dev/null 2>&1 || true

    d docker network rm "$net" >/dev/null \
        || fail "docker network rm was refused on the $label network"

    say "== $label: disconnected and removed"
done <<EOF
$DRIVEN
EOF

# ---- step 8: every documented option (#1015) --------------------------
# Each observer is outside the plugin: the server's log or lease file, a
# netns, Docker's exit and message, or a file the feature writes. A log
# line counts only when it appeared after its step began (log_lines marks
# the start). Every step removes its networks, because a second network
# on em-seg is refused unless ignore_conflicts says otherwise.

log_lines() {
    local n
    n="$(d sh -c "wc -l < $1" 2>/dev/null | tr -dc '0-9')"
    printf '%s' "${n:-0}"
}

# fresh_has LOG FROM TEXT: TEXT is on a line after line FROM. The text
# travels as the inner shell's $0, so no quoting of it is needed (#1015).
fresh_has() {
    d sh -c "tail -n +$(( $2 + 1 )) $1 | grep -F -q -- \"\$0\"" "$3"
}

fresh_wait() {
    local _
    for _ in $(seq 1 "${4:-30}"); do
        fresh_has "$1" "$2" "$3" && return 0
        sleep 1
    done
    return 1
}

opt_net() {
    local net="$1"; shift
    d docker network create -d "$PLUGIN_NAME" --ipam-driver null "$@" "$net" >/dev/null \
        || fail "docker network create $* was refused"
}

# opt_refused WANT ARGS: the create fails and Docker relays a message
# containing WANT (#1015).
opt_refused() {
    local want="$1" out; shift
    if out="$(d docker network create -d "$PLUGIN_NAME" --ipam-driver null "$@" em-refused 2>&1)"; then
        d docker network rm em-refused >/dev/null 2>&1
        fail "docker network create $* was accepted; the reference says it is refused"
    fi
    case "$out" in
        *"$want"*) ;;
        *) fail "docker network create $* was refused without naming '$want': $out" ;;
    esac
}

opt_run() {
    local ctr="$1" net="$2"; shift 2
    d docker run -d --name "$ctr" --network "$net" "$@" "$TEST_IMAGE" sleep 600 >/dev/null \
        || fail "the container did not start on $net ($*)"
}

opt_down() {
    local net="$1" c; shift
    for c in "$@"; do d docker rm -f "$c" >/dev/null 2>&1; done
    d docker network rm "$net" >/dev/null || fail "docker network rm $net was refused"
}

v4_of() {
    d docker exec "$1" ip -4 addr 2>/dev/null \
        | awk '$1 == "inet" && $2 ~ /^192\.168\./ { sub(/\/.*/, "", $2); print $2; exit }'
}

link_mtu() {
    d docker exec "$1" ip -4 addr 2>/dev/null | awk '
        /^[0-9]+:/ { for (i = 1; i <= NF; i++) if ($i == "mtu") m = $(i + 1) }
        $1 == "inet" && $2 ~ /^192\.168\./ { print m; exit }'
}

wait_v4() {
    local _
    V4=""
    for _ in $(seq 1 30); do
        V4="$(v4_of "$1")"
        [ -n "$V4" ] && return 0
        sleep 1
    done
    fail "$1 never held an IPv4 address on the segment"
}

v6_in() {
    d docker exec "$1" ip -6 addr 2>/dev/null \
        | awk -v P="$2" '$1 == "inet6" && index($2, P) == 1 { sub(/\/.*/, "", $2); print $2; exit }'
}

wait_v6() {
    local _
    V6=""
    for _ in $(seq 1 30); do
        V6="$(v6_in "$1" "$2")"
        [ -n "$V6" ] && return 0
        sleep 1
    done
    fail "$1 never held an IPv6 address in $2"
}

inspect_v6() {
    d docker inspect -f "{{(index .NetworkSettings.Networks \"$2\").GlobalIPv6Address}}" "$1" 2>/dev/null | tr -d '\r'
}

# v6_server ARGS restarts the DHCPv6/RA server of the IPv6 segment with
# one mode's arguments; the modes are the integration harness's
# (test/integration/harness/v6modes.go, #1015).
v6_server() {
    d sh -c "if [ -f $FIXTURE_DIR/v6.pid ]; then kill \$(cat $FIXTURE_DIR/v6.pid); rm -f $FIXTURE_DIR/v6.pid; sleep 1; fi
        dnsmasq --interface=$V6_BRIDGE --bind-interfaces --except-interface=lo \
          --dhcp-range=$V6_POOL4,$LEASE_TIME $* --log-dhcp --log-facility=$V6_LOG --port=0 \
          --dhcp-leasefile=$FIXTURE_DIR/v6.leases --pid-file=$FIXTURE_DIR/v6.pid" \
        || fail "the IPv6 server did not start with $*"
}

second_server() {
    case "$1" in
        up) d ip netns exec em-ns2 dnsmasq --interface=em-s2 --bind-interfaces --except-interface=lo \
              --dhcp-range=192.168.99.100,192.168.99.120,$LEASE_TIME --log-dhcp \
              --log-facility=$DNSMASQ2_LOG --port=0 --dhcp-leasefile=$FIXTURE_DIR/leases2 \
              --pid-file=$FIXTURE_DIR/dnsmasq2.pid || fail "the second DHCP server did not start" ;;
        down) d sh -c "kill \$(cat $FIXTURE_DIR/dnsmasq2.pid); rm -f $FIXTURE_DIR/dnsmasq2.pid" ;;
    esac
}

opt_gateway() {
    opt_net em-o-gw -o bridge="$SEGMENT" -o gateway=192.168.99.254
    opt_run em-c-gw em-o-gw
    d docker exec em-c-gw ip route | grep '^default via 192.168.99.254 ' >/dev/null \
        || fail "gateway=192.168.99.254: the container's default route is not via it"
    opt_down em-o-gw em-c-gw
    opt_net em-o-gw -o bridge="$SEGMENT"
    opt_run em-c-gw em-o-gw
    d docker exec em-c-gw ip route | grep '^default via 192.168.99.1 ' >/dev/null \
        || fail "control without gateway: the default route is not via the server's router 192.168.99.1"
    opt_down em-o-gw em-c-gw
}

# v6_dhcp_lease NET-OPTS: a DHCPv6 address on the IPv6 segment whose reply
# the server logged after the step began (#1015).
v6_dhcp_lease() {
    local m
    m="$(log_lines "$V6_LOG")"
    opt_net em-o-v6 -o bridge="$V6_BRIDGE" "$@"
    opt_run em-c-v6 em-o-v6
    wait_v6 em-c-v6 "$V6_PREFIX_A"
    fresh_wait "$V6_LOG" "$m" "DHCPREPLY($V6_BRIDGE) $V6 " \
        || fail "$*: the container holds $V6 and the server logged no DHCPv6 reply for it"
    opt_down em-o-v6 em-c-v6
}

opt_ipv6() {
    v6_server "--dhcp-range=${V6_PREFIX_A}10,${V6_PREFIX_A}99,$LEASE_TIME --enable-ra"
    v6_dhcp_lease -o ipv6=true
    opt_refused "contradict" -o bridge="$V6_BRIDGE" -o ipv6=true -o ipv6_mode=off
}

opt_ipv6_mode() {
    local m addr
    v6_server "--dhcp-range=${V6_PREFIX_A}10,${V6_PREFIX_A}99,$LEASE_TIME --enable-ra"
    v6_dhcp_lease -o ipv6_mode=dhcp

    v6_server "--dhcp-range=$V6_PREFIX_A,ra-only,$LEASE_TIME --enable-ra"
    m="$(log_lines "$V6_LOG")"
    opt_net em-o-v6 -o bridge="$V6_BRIDGE" -o ipv6_mode=slaac
    opt_run em-c-v6 em-o-v6
    wait_v6 em-c-v6 "$V6_PREFIX_A"
    addr="$(inspect_v6 em-c-v6 em-o-v6)"
    [ "$addr" = "$V6" ] || fail "ipv6_mode=slaac: the container holds $V6 and Docker reports '$addr'"
    fresh_wait "$V6_LOG" "$m" "RTR-ADVERT($V6_BRIDGE)" \
        || fail "ipv6_mode=slaac: the server logged no router advertisement during the step"
    opt_down em-o-v6 em-c-v6

    # The control: on the same advertising segment, a network with no
    # IPv6 option gets no global address, so the kernel does not form the
    # slaac address above by itself (#1015).
    opt_net em-o-v6 -o bridge="$V6_BRIDGE"
    opt_run em-c-v6 em-o-v6
    sleep 5
    [ -z "$(v6_in em-c-v6 "$V6_PREFIX_A")" ] \
        || fail "control ipv6_mode=off: the container formed an address in $V6_PREFIX_A by itself"
    opt_down em-o-v6 em-c-v6
    opt_refused "is not one of" -o bridge="$V6_BRIDGE" -o ipv6_mode=bogus
}

opt_ipv6_main_prefix() {
    local pfx got
    v6_server "--dhcp-range=$V6_PREFIX_A,ra-only,$LEASE_TIME --dhcp-range=$V6_PREFIX_B,ra-only,$LEASE_TIME --enable-ra"
    for pfx in "$V6_PREFIX_A" "$V6_PREFIX_B"; do
        opt_net em-o-v6 -o bridge="$V6_BRIDGE" -o ipv6_mode=slaac -o ipv6_main_prefix="$pfx/64"
        opt_run em-c-v6 em-o-v6
        wait_v6 em-c-v6 "$pfx"
        got="$(inspect_v6 em-c-v6 em-o-v6)"
        case "$got" in
            "$pfx"*) ;;
            *) fail "ipv6_main_prefix=$pfx/64: Docker reports '$got', not the address in that prefix" ;;
        esac
        opt_down em-o-v6 em-c-v6
    done
    opt_refused "ipv6_main_prefix" -o bridge="$V6_BRIDGE" -o ipv6_mode=dhcp -o ipv6_main_prefix="$V6_PREFIX_B/64"
}

# The managed flag with a server that ignores every DHCPv6 message stands
# in for a blocked port 547: the advertisement says DHCPv6 and nothing
# answers (the harness's AutoFallback mode, #1015).
opt_ipv6_auto_strict() {
    v6_server "--dhcp-range=${V6_PREFIX_A}10,${V6_PREFIX_A}99,slaac,$LEASE_TIME --dhcp-ignore=tag:dhcpv6"
    opt_net em-o-v6 -o bridge="$V6_BRIDGE" -o ipv6_mode=auto -o ipv6_auto_strict=true
    if d docker run -d --name em-c-v6 --network em-o-v6 "$TEST_IMAGE" sleep 600 >/dev/null 2>&1; then
        fail "ipv6_auto_strict=true: docker run succeeded with no DHCPv6 server answering"
    fi
    opt_down em-o-v6 em-c-v6
    opt_net em-o-v6 -o bridge="$V6_BRIDGE" -o ipv6_mode=auto -o ipv6_auto_strict=false
    opt_run em-c-v6 em-o-v6
    wait_v6 em-c-v6 "$V6_PREFIX_A"
    opt_down em-o-v6 em-c-v6
}

# run_wall TIMEOUT: seconds docker run takes to fail on the server-less
# bridge (#1015).
run_wall() {
    local s
    opt_net em-o-lt -o bridge=em-quiet -o lease_timeout="$1"
    s="$(date +%s)"
    if d docker run -d --name em-c-lt --network em-o-lt "$TEST_IMAGE" sleep 600 >/dev/null 2>&1; then
        fail "lease_timeout=$1: docker run succeeded on a bridge with no DHCP server"
    fi
    WALL=$(( $(date +%s) - s ))
    opt_down em-o-lt em-c-lt
}

opt_lease_timeout() {
    local short long
    run_wall 8s; short="$WALL"
    run_wall 20s; long="$WALL"
    [ "$short" -lt 15 ] || fail "lease_timeout=8s: docker run took ${short}s to fail"
    [ "$long" -ge 18 ] || fail "lease_timeout=20s: docker run failed after ${long}s, before the timeout"
    opt_refused "probe window" -o bridge="$SEGMENT" -o lease_timeout=5s
}

opt_conflict_check() {
    local m addr ps _
    d sh -c "ip addr add 192.168.99.60/24 dev em-sq && ip addr add 192.168.99.61/24 dev em-sq && ip addr add 192.168.99.62/24 dev em-sq" \
        || fail "could not put the squatted addresses on em-sq"

    m="$(log_lines "$DNSMASQ_LOG")"
    opt_net em-o-cc -o bridge="$SEGMENT" -o conflict_check=wait
    opt_run em-c-cc em-o-cc --mac-address 02:00:00:00:e0:01
    fresh_has "$DNSMASQ_LOG" "$m" "DHCPDECLINE($SEGMENT) 192.168.99.60 " \
        || fail "conflict_check=wait: docker run returned before any DHCPDECLINE for the squatted 192.168.99.60"
    wait_v4 em-c-cc
    [ "$V4" != 192.168.99.60 ] || fail "conflict_check=wait: the container is on the squatted address"
    fresh_has "$DNSMASQ_LOG" "$m" "DHCPACK($SEGMENT) $V4 02:00:00:00:e0:01" \
        || fail "conflict_check=wait: no second ACK for $V4"
    opt_down em-o-cc em-c-cc

    m="$(log_lines "$DNSMASQ_LOG")"
    opt_net em-o-cc -o bridge="$SEGMENT" -o conflict_check=async
    opt_run em-c-cc em-o-cc --mac-address 02:00:00:00:e0:02
    addr="$(v4_of em-c-cc)"
    [ "$addr" = 192.168.99.61 ] \
        || fail "conflict_check=async: at docker run's return the container holds '$addr', not the leased 192.168.99.61"
    # Recorded, not asserted: with 0 the kernel drops a secondary with its
    # primary, which decides whether a same-subnet renumber keeps an address.
    ps="$(d docker exec em-c-cc ip -o -4 addr 2>/dev/null \
        | awk '$4 ~ /^192\.168\./ { sub(/@.*/, "", $2); print $2; exit }')"
    ps="$(d docker exec em-c-cc cat "/proc/sys/net/ipv4/conf/${ps:-none}/promote_secondaries" 2>/dev/null)"
    MEASURED="$MEASURED promote_secondaries=${ps:-unread}"
    fresh_wait "$DNSMASQ_LOG" "$m" "DHCPDECLINE($SEGMENT) 192.168.99.61 " \
        || fail "conflict_check=async: no DHCPDECLINE for the squatted 192.168.99.61"
    for _ in $(seq 1 30); do
        addr="$(v4_of em-c-cc)"
        [ -n "$addr" ] && [ "$addr" != 192.168.99.61 ] && break
        sleep 1
    done
    [ -n "$addr" ] && [ "$addr" != 192.168.99.61 ] \
        || fail "conflict_check=async: the container never left the declined 192.168.99.61; it holds '${addr:-no IPv4 address}' after 30 s, promote_secondaries=${ps:-unread}"
    fresh_has "$DNSMASQ_LOG" "$m" "DHCPACK($SEGMENT) $addr 02:00:00:00:e0:02" \
        || fail "conflict_check=async: no second ACK for $addr"
    opt_down em-o-cc em-c-cc

    m="$(log_lines "$DNSMASQ_LOG")"
    opt_net em-o-cc -o bridge="$SEGMENT" -o conflict_check=off
    opt_run em-c-cc em-o-cc --mac-address 02:00:00:00:e0:03
    wait_v4 em-c-cc
    # The documented probe runs 4.0 to 7.0 s after the lease (#1015), so a
    # plugin that probed anyway declines inside this wait.
    sleep 10
    V4="$(v4_of em-c-cc)"
    [ "$V4" = 192.168.99.62 ] || fail "conflict_check=off: the container holds $V4, not the pinned 192.168.99.62"
    ! fresh_has "$DNSMASQ_LOG" "$m" "DHCPDECLINE(" || fail "conflict_check=off: a DHCPDECLINE was sent"
    opt_down em-o-cc em-c-cc
    d sh -c "ip addr flush dev em-sq"
}

opt_ignore_conflicts() {
    local m
    opt_net em-o-ic -o bridge="$SEGMENT"
    opt_refused "bridge already in use" -o bridge="$SEGMENT"
    m="$(log_lines "$DNSMASQ_LOG")"
    opt_net em-o-ic2 -o bridge="$SEGMENT" -o ignore_conflicts=true
    opt_run em-c-ic em-o-ic2
    wait_v4 em-c-ic
    fresh_has "$DNSMASQ_LOG" "$m" "DHCPACK($SEGMENT) $V4 " \
        || fail "ignore_conflicts=true: no ACK for the second network's container ($V4)"
    opt_down em-o-ic2 em-c-ic
    opt_down em-o-ic
}

# One pair of runs judges the three options a server's DHCP options feed:
# the tagged MAC receives options 6, 26 and 121 (fixture step, #1015).
OPTS_DONE=""
dhcp_options_pair() {
    [ -n "$OPTS_DONE" ] && return 0
    opt_net em-o-op -o bridge="$SEGMENT" -o propagate_dns=true -o propagate_mtu=true -o skip_routes=false
    opt_run em-c-op em-o-op --mac-address 02:00:00:00:e1:01
    wait_v4 em-c-op
    OPT_ON_ROUTE="$(d docker exec em-c-op ip route | grep -c '^10\.77\.0\.0/16 ')"
    OPT_ON_DNS="$(d docker exec em-c-op cat /etc/resolv.conf | grep -c '^nameserver 192\.168\.99\.53')"
    OPT_ON_MTU="$(link_mtu em-c-op)"
    opt_down em-o-op em-c-op
    opt_net em-o-op -o bridge="$SEGMENT" -o propagate_dns=false -o propagate_mtu=false -o skip_routes=true
    opt_run em-c-op em-o-op --mac-address 02:00:00:00:e1:01
    wait_v4 em-c-op
    OPT_OFF_ROUTE="$(d docker exec em-c-op ip route | grep -c '^10\.77\.0\.0/16 ')"
    OPT_OFF_DNS="$(d docker exec em-c-op cat /etc/resolv.conf | grep -c '^nameserver 192\.168\.99\.53')"
    OPT_OFF_MTU="$(link_mtu em-c-op)"
    opt_down em-o-op em-c-op
    OPTS_DONE=1
}

opt_skip_routes() {
    dhcp_options_pair
    [ "$OPT_ON_ROUTE" = 1 ] || fail "skip_routes=false: the option 121 route 10.77.0.0/16 is not in the container"
    [ "$OPT_OFF_ROUTE" = 0 ] || fail "skip_routes=true: the option 121 route 10.77.0.0/16 is in the container"
}

opt_propagate_dns() {
    dhcp_options_pair
    [ "$OPT_ON_DNS" = 1 ] || fail "propagate_dns=true: option 6's 192.168.99.53 is not in /etc/resolv.conf"
    [ "$OPT_OFF_DNS" = 0 ] || fail "propagate_dns=false: option 6's 192.168.99.53 is in /etc/resolv.conf"
}

opt_propagate_mtu() {
    local mtu
    dhcp_options_pair
    [ "$OPT_ON_MTU" = 1400 ] || fail "propagate_mtu=true: the link MTU is '$OPT_ON_MTU', option 26 says 1400"
    [ "$OPT_OFF_MTU" = 1500 ] || fail "propagate_mtu=false: the link MTU is '$OPT_OFF_MTU', not the bridge's 1500"
    opt_net em-o-op -o bridge="$SEGMENT" -o propagate_mtu=true
    opt_run em-c-op em-o-op --mac-address 02:00:00:00:e1:02
    wait_v4 em-c-op
    mtu="$(link_mtu em-c-op)"
    [ "$mtu" = 1500 ] || fail "propagate_mtu=true with option 26 = 400: the link MTU is '$mtu', the reference refuses under 576"
    opt_down em-o-op em-c-op
}

# One pair of runs judges the options that change what the client sends;
# the second run is every one of them at its default (#1015).
ID_DONE=""
identity_pair() {
    local m a
    [ -n "$ID_DONE" ] && return 0
    a="$(log_lines "$AUDIT_LOG")"
    m="$(log_lines "$DNSMASQ_LOG")"
    opt_net em-o-id -o bridge="$SEGMENT" -o client_id=em-cid-1 -o vendor_class=em-vc-1 \
        -o register_dns=true -o audit_log=true
    opt_run em-c-id em-o-id --mac-address 02:00:00:00:e3:01 --hostname em-host-1
    wait_v4 em-c-id
    ID_ON_ADDR="$V4"
    fresh_wait "$DNSMASQ_LOG" "$m" "client provides name: em-host-1" 20 && ID_ON_NAME=1 || ID_ON_NAME=0
    fresh_has "$DNSMASQ_LOG" "$m" "DHCPACK($SEGMENT) $V4 02:00:00:00:e3:01" && ID_ON_MAC=1 || ID_ON_MAC=0
    fresh_has "$DNSMASQ_LOG" "$m" "vendor class: em-vc-1" && ID_ON_VC=1 || ID_ON_VC=0
    fresh_has "$DNSMASQ_LOG" "$m" "option: 81 " && ID_ON_FQDN=1 || ID_ON_FQDN=0
    ID_ON_CID="$(d awk '$2 == "02:00:00:00:e3:01" { print $5 }' "$FIXTURE_DIR/leases" | tr -d '\r')"
    fresh_has "$AUDIT_LOG" "$a" "\"ip\":\"$V4\"" && fresh_has "$AUDIT_LOG" "$a" '"kind":"bound"' \
        && ID_ON_AUDIT=1 || ID_ON_AUDIT=0
    opt_down em-o-id em-c-id

    a="$(log_lines "$AUDIT_LOG")"
    m="$(log_lines "$DNSMASQ_LOG")"
    opt_net em-o-id -o bridge="$SEGMENT"
    opt_run em-c-id em-o-id --mac-address 02:00:00:00:e3:02 --hostname em-host-2
    wait_v4 em-c-id
    fresh_wait "$DNSMASQ_LOG" "$m" "client provides name: em-host-2" 20 \
        || fail "control: the server never logged the name em-host-2, so the absence checks below would prove nothing"
    fresh_has "$DNSMASQ_LOG" "$m" "vendor class: docker-net-dhcp" && ID_OFF_VC=1 || ID_OFF_VC=0
    fresh_has "$DNSMASQ_LOG" "$m" "option: 81 " && ID_OFF_FQDN=1 || ID_OFF_FQDN=0
    ID_OFF_CID="$(d awk '$2 == "02:00:00:00:e3:02" { print $5 }' "$FIXTURE_DIR/leases" | tr -d '\r')"
    ID_OFF_AUDIT="$(( $(log_lines "$AUDIT_LOG") - a ))"
    opt_down em-o-id em-c-id
    ID_DONE=1
}

opt_client_id() {
    identity_pair
    # RFC 2132 opaque form: type 0x00, then the bytes of "em-cid-1".
    [ "$ID_ON_CID" = "00:65:6d:2d:63:69:64:2d:31" ] \
        || fail "client_id=em-cid-1: the server's lease file records client-id '$ID_ON_CID'"
    [ -n "$ID_OFF_CID" ] && [ "$ID_OFF_CID" != "$ID_ON_CID" ] \
        || fail "control: the default client-id '$ID_OFF_CID' is empty or equals the option's"
}

opt_vendor_class() {
    identity_pair
    [ "$ID_ON_VC" = 1 ] || fail "vendor_class=em-vc-1: no fresh 'vendor class: em-vc-1' in the server log"
    [ "$ID_OFF_VC" = 1 ] || fail "control: no fresh 'vendor class: docker-net-dhcp' in the server log"
}

opt_register_dns() {
    identity_pair
    [ "$ID_ON_FQDN" = 1 ] || fail "register_dns=true: the server logged no option 81 exchange"
    [ "$ID_OFF_FQDN" = 0 ] || fail "register_dns=false: the server logged an option 81 exchange"
}

opt_audit_log() {
    identity_pair
    [ "$ID_ON_AUDIT" = 1 ] || fail "audit_log=true: $AUDIT_LOG gained no bound line for $ID_ON_ADDR"
    [ "$ID_OFF_AUDIT" = 0 ] || fail "audit_log=false: $AUDIT_LOG gained $ID_OFF_AUDIT line(s)"
}

opt___mac_address() {
    identity_pair
    [ "$ID_ON_MAC" = 1 ] || fail "--mac-address 02:00:00:00:e3:01: no fresh ACK for $ID_ON_ADDR carries that MAC"
}

opt___hostname() {
    identity_pair
    [ "$ID_ON_NAME" = 1 ] || fail "--hostname em-host-1: the server logged no client-provided name em-host-1"
}

opt_validate_dhcp() {
    local m
    opt_refused "em-dead" -o mode=macvlan -o parent=em-dead -o validate_dhcp=true
    opt_net em-o-vd -o mode=macvlan -o parent=em-dead -o validate_dhcp=false
    opt_down em-o-vd
    m="$(log_lines "$DNSMASQ_LOG")"
    opt_net em-o-vd -o mode=macvlan -o parent="$PARENT" -o validate_dhcp=true
    fresh_has "$DNSMASQ_LOG" "$m" "DHCPOFFER($SEGMENT)" \
        || fail "validate_dhcp=true on $PARENT: accepted with no fresh OFFER in the server log"
    opt_down em-o-vd
    opt_refused "validate_dhcp" -o bridge="$SEGMENT" -o validate_dhcp=true
}

# server_pick OPTION VALUE WANT: with the second server up, the address
# comes from server WANT (1 the fixture's, 2 the second) and that
# server's log carries its fresh ACK (#1015).
server_pick() {
    local m1 m2 last
    m1="$(log_lines "$DNSMASQ_LOG")"
    m2="$(log_lines "$DNSMASQ2_LOG")"
    opt_net em-o-ds -o bridge="$SEGMENT" -o "$1=$2"
    opt_run em-c-ds em-o-ds
    wait_v4 em-c-ds
    last="${V4##*.}"
    if [ "$3" = 2 ]; then
        [ "$last" -ge 100 ] && [ "$last" -le 120 ] || fail "$1=$2: the container holds $V4, outside the second server's pool"
        fresh_has "$DNSMASQ2_LOG" "$m2" "DHCPACK(em-s2) $V4 " || fail "$1=$2: the second server logged no ACK for $V4"
    else
        [ "$last" -ge 10 ] && [ "$last" -le 99 ] || fail "$1=$2: the container holds $V4, outside the fixture server's pool"
        fresh_has "$DNSMASQ_LOG" "$m1" "DHCPACK($SEGMENT) $V4 " || fail "$1=$2: the fixture server logged no ACK for $V4"
    fi
    opt_down em-o-ds em-c-ds
}

opt_dhcp_servers() {
    second_server up
    server_pick dhcp_servers 192.168.99.5 2
    server_pick dhcp_servers 192.168.99.1 1
    second_server down
}

opt_dhcp_deny_servers() {
    second_server up
    server_pick dhcp_deny_servers 192.168.99.1 2
    server_pick dhcp_deny_servers 192.168.99.5 1
    second_server down
}

opt_release_lease() {
    local m
    opt_net em-o-rl -o bridge="$SEGMENT" -o release_lease=on_stop
    opt_run em-c-rl em-o-rl
    wait_v4 em-c-rl
    m="$(log_lines "$DNSMASQ_LOG")"
    d docker stop -t 1 em-c-rl >/dev/null || fail "docker stop failed"
    fresh_wait "$DNSMASQ_LOG" "$m" "DHCPRELEASE($SEGMENT) $V4 " 10 \
        || fail "release_lease=on_stop: no DHCPRELEASE for $V4 after docker stop"
    opt_down em-o-rl em-c-rl
    opt_net em-o-rl -o bridge="$SEGMENT" -o release_lease=never
    opt_run em-c-rl em-o-rl
    wait_v4 em-c-rl
    m="$(log_lines "$DNSMASQ_LOG")"
    d docker stop -t 1 em-c-rl >/dev/null || fail "docker stop failed"
    # The same 10 s the on_stop half allows its release to appear in (#1015).
    sleep 10
    ! fresh_has "$DNSMASQ_LOG" "$m" "DHCPRELEASE(" || fail "release_lease=never: a DHCPRELEASE followed docker stop"
    opt_down em-o-rl em-c-rl
}

opt_host_ifname() {
    local _ ok=""
    opt_net em-o-hi -o bridge="$SEGMENT" -o host_ifname=container_name
    opt_run em-c-hi em-o-hi
    for _ in $(seq 1 30); do
        d ip -o link show em-c-hi 2>/dev/null | grep '^[0-9]*: em-c-hi@' >/dev/null && { ok=1; break; }
        sleep 1
    done
    [ -n "$ok" ] || fail "host_ifname=container_name: no host link is named em-c-hi"
    opt_down em-o-hi em-c-hi
    opt_net em-o-hi -o bridge="$SEGMENT"
    opt_run em-c-hi2 em-o-hi
    wait_v4 em-c-hi2
    ! d ip link show em-c-hi2 >/dev/null 2>&1 || fail "control: a host link is named after the container without host_ifname"
    opt_down em-o-hi em-c-hi2
    opt_refused "mode=macvlan" -o mode=macvlan -o parent="$PARENT" -o host_ifname=container_name
    opt_refused "is not one of" -o bridge="$SEGMENT" -o host_ifname=bogus
}

# free_addr: an address in .70-.79 no lease in the server's file holds,
# so a request for it is decided by the request and not by a stale lease (#1015).
free_addr() {
    local i
    for i in 70 71 72 73 74 75 76 77 78 79; do
        d grep -q " 192.168.99.$i " "$FIXTURE_DIR/leases" || { printf '192.168.99.%s' "$i"; return 0; }
    done
    fail "every address in 192.168.99.70-79 is leased"
}

# attach_with OPT: a created container joins em-o-ep with one driver-opt
# and starts; Docker's relayed message is left in ATTACH_OUT (#1015).
attach_with() {
    d docker create --name em-c-ep --network none "$TEST_IMAGE" sleep 600 >/dev/null || fail "docker create failed"
    d docker network disconnect none em-c-ep >/dev/null || fail "could not take em-c-ep off the none network"
    ATTACH_OUT="$(d docker network connect --driver-opt "$1" em-o-ep em-c-ep 2>&1 && d docker start em-c-ep 2>&1)"
}

opt_ip() {
    local m want
    want="$(free_addr)"
    opt_net em-o-ep -o mode=macvlan -o parent="$PARENT"
    m="$(log_lines "$DNSMASQ_LOG")"
    attach_with "ip=$want" || fail "--driver-opt ip=$want: $ATTACH_OUT"
    wait_v4 em-c-ep
    [ "$V4" = "$want" ] || fail "--driver-opt ip=$want: the container holds $V4"
    fresh_has "$DNSMASQ_LOG" "$m" "DHCPACK($SEGMENT) $want " || fail "--driver-opt ip=$want: no fresh ACK for it"
    opt_down em-o-ep em-c-ep
}

opt_com_docker_network_endpoint_ifname() {
    opt_net em-o-ep -o mode=macvlan -o parent="$PARENT"
    attach_with com.docker.network.endpoint.ifname=emif0 || fail "endpoint ifname emif0: $ATTACH_OUT"
    if d docker exec em-c-ep ip link show emif0 >/dev/null 2>&1; then
        MEASURED="$MEASURED ifname=honoured"
    else
        MEASURED="$MEASURED ifname=ignored"
    fi
    d docker rm -f em-c-ep >/dev/null
    if attach_with com.docker.network.endpoint.ifname=em-name-too-long-x; then
        fail "endpoint ifname of 19 bytes: the container started"
    fi
    case "$ATTACH_OUT" in
        *IFNAMSIZ*) ;;
        *) fail "endpoint ifname of 19 bytes: refused without naming the limit: $ATTACH_OUT" ;;
    esac
    opt_down em-o-ep em-c-ep
}

# A measurement: the Solicit on the wire is read for the requested address.
# Docker hands the plugin no IPv6 address under null IPAM (#960), so the
# row records what this engine does instead of asserting it.
opt___ip6() {
    local want="${V6_PREFIX_A}56" hint="absent" out _
    v6_server "--dhcp-range=${V6_PREFIX_A}10,${V6_PREFIX_A}99,$LEASE_TIME --enable-ra"
    if ! out="$(d docker network create -d "$PLUGIN_NAME" --ipam-driver null --ipv6 -o bridge="$V6_BRIDGE" \
            -o ipv6_mode=dhcp em-o-i6 2>&1)"; then
        say "--ip6 network refused: $out"
        MEASURED="$MEASURED ip6=network-refused"
        return 0
    fi
    docker exec -d "$CONTAINER" sh -c "timeout 40 tcpdump -i $V6_BRIDGE -n -vv -l 'udp port 547' > $FIXTURE_DIR/ip6.txt 2>&1"
    sleep 2
    if ! d docker run -d --name em-c-i6 --network em-o-i6 --ip6 "$want" "$TEST_IMAGE" sleep 600 >/dev/null 2>&1; then
        MEASURED="$MEASURED ip6=run-refused"
    else
        for _ in $(seq 1 20); do
            d grep -q 'dhcp6 reply' "$FIXTURE_DIR/ip6.txt" && break
            sleep 1
        done
        d grep 'dhcp6 solicit' "$FIXTURE_DIR/ip6.txt" | grep -F "IA_ADDR $want " >/dev/null && hint="present"
        MEASURED="$MEASURED ip6_hint=$hint"
    fi
    d pkill tcpdump >/dev/null 2>&1
    opt_down em-o-i6 em-c-i6
}

# Without --subnet the daemon's answer differs by engine (26.1.4 refuses,
# 29.8.1 accepts), so that half is recorded; with --subnet the exact ACK
# is asserted on every engine (#1015).
opt___ip() {
    local m want out
    want="$(free_addr)"
    d docker network create -d "$PLUGIN_NAME" --ipam-driver "$PLUGIN_NAME" -o mode=macvlan -o parent="$PARENT" em-o-ipam >/dev/null \
        || fail "the IPAM-mode network for --ip without --subnet was refused"
    if out="$(d docker run -d --name em-c-ipam --network em-o-ipam --ip "$want" "$TEST_IMAGE" sleep 600 2>&1)"; then
        wait_v4 em-c-ipam
        if [ "$V4" = "$want" ]; then
            MEASURED="$MEASURED ip_without_subnet=accepted"
        else
            MEASURED="$MEASURED ip_without_subnet=accepted-holds-$V4"
        fi
    else
        say "--ip without --subnet: $out"
        MEASURED="$MEASURED ip_without_subnet=refused"
    fi
    opt_down em-o-ipam em-c-ipam
    want="$(free_addr)"
    d docker network create -d "$PLUGIN_NAME" --ipam-driver "$PLUGIN_NAME" --subnet 192.168.99.0/24 \
        -o mode=macvlan -o parent="$PARENT" em-o-ipam >/dev/null \
        || fail "the IPAM-mode network for --ip with --subnet 192.168.99.0/24 was refused"
    m="$(log_lines "$DNSMASQ_LOG")"
    opt_run em-c-ipam em-o-ipam --ip "$want"
    wait_v4 em-c-ipam
    [ "$V4" = "$want" ] || fail "--ip $want with --subnet: the container holds $V4"
    fresh_has "$DNSMASQ_LOG" "$m" "DHCPACK($SEGMENT) $want " || fail "--ip $want with --subnet: no fresh ACK for it"
    opt_down em-o-ipam em-c-ipam
}

STEP=option-steps
option_steps="$(derive_option_steps)" \
    || fail "the documented options could not be derived from $SHAPES_DOC; the messages above name the source"
MEASURED=""
while IFS='|' read -r opt kind obs; do
    [ -n "$opt" ] || continue
    STEP="option-$opt"
    fn="opt_$(printf '%s' "$opt" | tr -c 'a-zA-Z0-9' '_')"
    case "$kind" in
        shape)
            case "$opt" in
                mode)   want_shapes="bridge macvlan ipvlan" ;;
                bridge) want_shapes="bridge" ;;
                parent) want_shapes="macvlan ipvlan" ;;
                *) fail "the catalogue calls $opt a shape and no shape step drives it" ;;
            esac
            for s in $want_shapes; do
                case "$DRIVEN" in
                    *"-$s
"*) ;;
                    *) fail "$opt is covered by the shape steps and no $s shape was driven" ;;
                esac
            done ;;
        step|measure)
            declare -F "$fn" >/dev/null || fail "$opt is a documented $kind and this cell has no $fn"
            say "== option $opt ($kind)"
            "$fn" ;;
        not-driven) say "== option $opt not driven: $obs" ;;
        '') fail "$SHAPES_DOC documents $opt and the option catalogue has no line for it" ;;
        *) fail "the option catalogue gives $opt the unknown kind '$kind'" ;;
    esac
done <<EOF
$option_steps
EOF

# ---- step 9: the plugin reads the engine it runs on (#1015) -----------
STEP=health-engine-version
plugin_id="$(d docker plugin inspect -f '{{.Id}}' "$PLUGIN_NAME" | tr -d '\r')"
health_engine=""
for _ in $(seq 1 30); do
    health_engine="$(d curl -s --unix-socket "/run/docker/plugins/$plugin_id/net-dhcp.sock" http://localhost/Plugin.Health \
        | sed -n 's/.*"engine_version":"\([^"]*\)".*/\1/p')"
    [ "$health_engine" = "$ENGINE_VERSION" ] && break
    sleep 1
done
[ "$health_engine" = "$ENGINE_VERSION" ] \
    || fail "Plugin.Health reports engine_version '$health_engine' and the nested daemon answers $ENGINE_VERSION"

STEP=complete
verdict pass "macvlan=$macvlan_addr after_restart=$after engine_version=$health_engine options=$(printf '%s\n' "$option_steps" | grep -c .)$MEASURED"
exit 0
