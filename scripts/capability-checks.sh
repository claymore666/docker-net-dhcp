#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The attach, renewal and restart checks of capability-cell.sh (#690),
# sourced so scripts/test-capability-matrix.sh can drive them over a
# scripted server log. The caller defines DLOG, LEASES and say.

# link_of <ctr> <prefix> — "<link> <address>" for the container's address
# under prefix, whatever the link is called: bridge mode names it after the
# bridge, not eth0 (network.go Join DstPrefix, #690 draft run 37294535723).
link_of() {
    [ -n "$2" ] || return 1
    docker exec -u 0 "$1" ip -4 -o addr show 2>/dev/null \
        | awk -v p="$2" 'index($4, p) == 1 {sub(/\/.*/, "", $4); print $2, $4; exit}'
}
# mac_of <ctr> <prefix> — the MAC of the link carrying that address.
mac_of() {
    local l
    l="$(link_of "$1" "$2")" || return 1
    docker exec -u 0 "$1" cat "/sys/class/net/${l%% *}/address" 2>/dev/null
}

# attached <ctr> <prefix> — running, an address under prefix on a link,
# and the server's lease file holding that link's MAC with that address
# (#690 D5, D6).
attached() {
    local ctr="$1" prefix="$2" l addr mac
    [ "$(docker inspect -f '{{.State.Running}}' "$ctr" 2>/dev/null)" = true ] \
        || { say "$ctr is not running"; return 1; }
    l="$(link_of "$ctr" "$prefix")"
    addr="${l#* }"
    [ -n "$l" ] || { say "$ctr: no $prefix address on any link"; return 1; }
    mac="$(docker exec -u 0 "$ctr" cat "/sys/class/net/${l%% *}/address" 2>/dev/null)"
    [ -n "$mac" ] || { say "$ctr: no MAC on ${l%% *}"; return 1; }
    awk -v m="$mac" -v a="$addr" '$2 == m && $3 == a {f=1} END {exit !f}' "$LEASES" \
        || { say "$ctr: lease file holds no $mac -> $addr"; return 1; }
    say "$ctr: ${l%% *} $mac -> $addr, leased"
}

# seen <after-line> <mac> <TYPE> — server log lines of that type for that
# MAC written after the given line (#690).
seen() { bash "$(dirname "${BASH_SOURCE[0]}")/capability-matrix.sh" --dhcp-count "$DLOG" "$1" "$2" "$3"; }
mark() { wc -l < "$DLOG"; }

# renewed <ctr> <mac> — an ACK for the MAC after the wait starts and no new
# DISCOVER from it, so a fresh lease does not pass as a renewal (#690 D7).
renewed() {
    local from
    from="$(mark)"
    for _ in $(seq 1 30); do
        if [ "$(seen "$from" "$2" DHCPACK)" -gt 0 ]; then
            [ "$(seen "$from" "$2" DHCPDISCOVER)" -eq 0 ] \
                || { say "$1: a new DHCPDISCOVER, not a renewal"; return 1; }
            say "$1: renewal ACK seen"; return 0
        fi
        sleep 1
    done
    say "$1: no renewal ACK within 30 s"; return 1
}

# restart as stop then start: the MAC is reused from the tombstone and
# renews every T1, so the stopped container must go one T1 plus slack with
# no ACK before an ACK after the start counts (#690 D8).
restarted() {
    local mac from
    mac="$(mac_of cm-c-mv 10.98.1.)"
    docker stop -t 2 cm-c-mv >/dev/null || { say "stop refused"; return 1; }
    from="$(mark)"
    sleep 12
    [ "$(seen "$from" "${mac:-none}" DHCPACK)" -eq 0 ] \
        || { say "cm-c-mv: $mac acknowledged while stopped"; return 1; }
    from="$(mark)"
    docker start cm-c-mv >/dev/null || { say "start refused"; return 1; }
    for _ in $(seq 1 20); do
        mac="$(mac_of cm-c-mv 10.98.1.)"
        if [ -n "$mac" ] && [ "$(seen "$from" "$mac" DHCPACK)" -gt 0 ]; then
            attached cm-c-mv 10.98.1.; return
        fi
        sleep 1
    done
    say "cm-c-mv: no ACK after the start"; return 1
}
