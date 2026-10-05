#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The renewal and restart checks of capability-cell.sh (#690), sourced so
# scripts/test-capability-matrix.sh can drive them over a scripted server
# log. The caller defines DLOG, say, mac_of and attached.

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
    mac="$(mac_of cm-c-mv)"
    docker stop -t 2 cm-c-mv >/dev/null || { say "stop refused"; return 1; }
    from="$(mark)"
    sleep 12
    [ "$(seen "$from" "${mac:-none}" DHCPACK)" -eq 0 ] \
        || { say "cm-c-mv: $mac acknowledged while stopped"; return 1; }
    from="$(mark)"
    docker start cm-c-mv >/dev/null || { say "start refused"; return 1; }
    for _ in $(seq 1 20); do
        mac="$(mac_of cm-c-mv)"
        if [ -n "$mac" ] && [ "$(seen "$from" "$mac" DHCPACK)" -gt 0 ]; then
            attached cm-c-mv 10.98.1.; return
        fi
        sleep 1
    done
    say "cm-c-mv: no ACK after the start"; return 1
}
