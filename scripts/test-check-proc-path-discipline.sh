#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-proc-path-discipline.sh.
#
# The case that matters is the first one: the gate must go red on the
# EXACT line that shipped. #688 fixed the resolv.conf path and the same
# hazard survived three files away in dhcp_manager.go, so a gate that
# only rejects hypothetical shapes would have been just as blind as the
# comment it replaces.
#
# The other direction matters as much. This gate greps, so it will meet
# /proc/self paths and test failure messages that mention a /proc path
# but open nothing. If it flagged those, the next person would learn to
# wave it through -- which is how a gate stops being one.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
CHECK="$HERE/check-proc-path-discipline.sh"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

# Fixtures are the real tree, copied, then broken in one specific way,
# so a fixture starts out passing for the same reasons the repo does and
# any failure is attributable to the single edit.
#
# Each mutator is checked to have actually CHANGED the tree. Every one of
# them is a sed anchored on a verbatim line of production source, so an
# unrelated edit to that line turns the mutation into a no-op silently --
# and a no-op fixture is a PRISTINE tree, which the gate passes. The
# result reads as "the gate stopped rejecting the shipped line", pointing
# the reader at the gate when the fixture is what broke. It has happened
# once: `m.hostname, _ = safeHostname(...)` became
# `m.hostname = safeHostname(...).name` and this file's first case went
# red with a diagnosis that named the wrong file.
fixture() {
    local name="$1"; shift
    local d="$TMP/$name"
    mkdir -p "$d"
    cp -r "$REPO/pkg" "$REPO/cmd" "$d/"
    if [ "$#" -gt 0 ]; then
        local before
        # sort, because `sed -i` rewrites a file even when its pattern
        # matched nothing: the rename gives a new inode, find's raw order
        # changes with it, and an unsorted checksum would differ for a
        # mutation that did nothing at all.
        before=$(find "$d" -name '*.go' -type f | sort | xargs cat | cksum)
        "$@" "$d"
        if [ "$(find "$d" -name '*.go' -type f | sort | xargs cat | cksum)" = "$before" ]; then
            echo "FAIL: fixture '$name' changed nothing — its sed anchor no longer matches the source." >&2
            echo "      Re-anchor the mutator; the gate itself is NOT what this case is reporting on." >&2
            # A marker file, not fails=1: fixture runs inside $( ), so a
            # variable set here dies with the subshell and the meta-test
            # would report the very "silent pass" this check exists to
            # stop.
            touch "$TMP/fixture-noop"
        fi
    fi
    bash "$CHECK" "$d" >"$TMP/$name.log" 2>&1
    echo $?
}

got=$(fixture pristine)
check "the real tree passes" 0 "$got"

# --- the regression this gate exists for -------------------------------
# Verbatim the line that shipped in dhcp_manager.go after #688.
reintroduce_688() {
    sed -i 's|^\tm.hostname = m.plugin.safeHostname(ctr.Config.Hostname).name$|\tm.nsPath = fmt.Sprintf("/proc/%v/ns/net", ctr.State.Pid)\n\tm.hostname = m.plugin.safeHostname(ctr.Config.Hostname).name|' \
        "$1/pkg/plugin/dhcp_manager.go"
}
got=$(fixture reintroduced reintroduce_688)
check "the exact line that shipped is rejected" 1 "$got"
grep -q 'dhcp_manager.go' "$TMP/reintroduced.log" \
    && echo "PASS: names the offending file and line" \
    || { echo "FAIL: rejected without naming where"; fails=1; }
grep -q 'openContainerProc' "$TMP/reintroduced.log" \
    && echo "PASS: names the function to use instead" \
    || { echo "FAIL: rejected without saying what to do"; fails=1; }

# --- and the netns path specifically, wherever it is written -----------
new_getfrompath() {
    sed -i 's|^func closeNsHandle|func evil(pid int) { _, _ = netns.GetFromPath(fmt.Sprintf("/proc/%d/ns/net", pid)) }\n\nfunc closeNsHandle|' \
        "$1/pkg/plugin/dhcp_manager.go"
}
got=$(fixture newsink new_getfrompath)
check "a fresh GetFromPath on a built path is rejected" 1 "$got"

# --- concatenation, not just Sprintf -----------------------------------
concat() {
    sed -i 's|^func closeNsHandle|func evil(pid string) string { return "/proc/" + pid + "/ns/net" }\n\nfunc closeNsHandle|' \
        "$1/pkg/plugin/dhcp_manager.go"
}
got=$(fixture concat concat)
check "string concatenation is rejected too" 1 "$got"

# --- the other direction: things that must NOT be flagged --------------
# /proc/self names the caller. There is no recycled-PID hazard in
# asking about yourself, and resolvconf.go legitimately does it.
selfpath() {
    sed -i 's|^func closeNsHandle|func fine() { _, _ = os.Open(fmt.Sprintf("/proc/self/task/%d/ns/mnt", 1)) }\n\nfunc closeNsHandle|' \
        "$1/pkg/plugin/dhcp_manager.go"
}
got=$(fixture selfpath selfpath)
check "a /proc/self path is not flagged" 0 "$got"

# A test's failure message mentions a path; it opens nothing.
message() {
    sed -i 's|^func closeNsHandle|func fine(t interface{ Fatalf(string, ...interface{}) }, pid int) { t.Fatalf("/proc/%d/cgroup is empty", pid) }\n\nfunc closeNsHandle|' \
        "$1/pkg/plugin/dhcp_manager.go"
}
got=$(fixture message message)
check "a test failure message is not flagged" 0 "$got"

# A deliberate, explained exemption is honoured -- and the marker may
# sit at the top of the paragraph that gives the reason.
allowed() {
    sed -i 's|^func closeNsHandle|// proc-path-discipline: allow -- reason goes here\n// and continues onto a second line.\nfunc fine(pid int) { _, _ = os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid)) }\n\nfunc closeNsHandle|' \
        "$1/pkg/plugin/dhcp_manager.go"
}
got=$(fixture allowed allowed)
check "an explained allow marker is honoured" 0 "$got"

# --- cannot check is not a pass ----------------------------------------
# If openContainerProc moves, the exemption no longer means what the
# script claims it means. Refusing to judge is the only honest answer.
moved() { sed -i 's|^func openContainerProc|func openContainerProcMoved|' "$1/pkg/plugin/resolvconf.go"; }
got=$(fixture moved moved)
check "the guard function going missing exits 2, not 0" 2 "$got"

got=$(bash "$CHECK" "$TMP/does-not-exist" >/dev/null 2>&1; echo $?)
check "a missing tree exits 2, not 0" 2 "$got"

if [ -e "$TMP/fixture-noop" ] || [ "$fails" -ne 0 ]; then
    echo "proc-path discipline meta-test FAILED"
    exit 1
fi
echo "PASS  the proc-path gate catches the shipped regression and leaves honest paths alone"
