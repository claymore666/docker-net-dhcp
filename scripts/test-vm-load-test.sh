#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for vm-load-test.sh (#969): the parts its own --self-test cannot
# see. The self-test drives the arithmetic and the refusals from inside
# the script; this file runs the script from outside, the way CI and an
# operator do, and checks the seams between the host side and the rig it
# writes into the VM. No KVM, no network, no Docker.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
SUBJECT="$HERE/vm-load-test.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

guarded_tmpdir dir

# 1. The self-test is green when run as an operator runs it, and it ran
#    every case it is supposed to have: a case deleted from the self-test
#    leaves it green, so its population is pinned here, outside it. A
#    case added or removed changes this number in the same change.
#    A case the self-test skips for want of a tool is counted as skipped,
#    so passed plus skipped is the population on every kind of box.
SELF_TEST_CASES=67
out=$(bash "$SUBJECT" --self-test 2>&1); rc=$?
if [ "$rc" -eq 0 ] && grep -q '^passed [0-9]*, failed 0, skipped [0-9]*$' <<< "$out" \
    && ! grep -q '^  FAIL' <<< "$out"; then
    ok "--self-test exits 0 with no failing case"
else
    no "--self-test rc=$rc: $(tail -n 3 <<< "$out")"
fi
passed=$(sed -n 's/^passed \([0-9]*\), failed [0-9]*, skipped [0-9]*$/\1/p' <<< "$out")
skipped=$(sed -n 's/^passed [0-9]*, failed [0-9]*, skipped \([0-9]*\)$/\1/p' <<< "$out")
if [ -n "$passed" ] && [ -n "$skipped" ] && [ "$((passed + skipped))" -eq "$SELF_TEST_CASES" ]; then
    ok "--self-test accounted for its $SELF_TEST_CASES cases ($skipped skipped)"
else
    no "--self-test accounted for passed ${passed:-none} plus skipped ${skipped:-none}, this file expects $SELF_TEST_CASES; a case was added or removed without updating both"
fi

# 1b. The cidata cases need mtools, which the hosted CI image does not
#     carry: there they skip, and the pin above must hold with them
#     skipped. Measured here with mtools hidden from PATH, whatever this
#     box has installed.
# shellcheck disable=SC2154  # dir is assigned by guarded_tmpdir through a nameref
shim="$dir/no-mtools"; mkdir -p "$shim"
for d in ${PATH//:/ }; do
    [ -d "$d" ] && cp -sn "$d"/* "$shim"/ 2>/dev/null
done
rm -f "$shim/mformat" "$shim/mcopy" "$shim/mdir"
out2=$(PATH="$shim" bash "$SUBJECT" --self-test 2>&1); rc2=$?
passed2=$(sed -n 's/^passed \([0-9]*\), failed [0-9]*, skipped [0-9]*$/\1/p' <<< "$out2")
skipped2=$(sed -n 's/^passed [0-9]*, failed [0-9]*, skipped \([0-9]*\)$/\1/p' <<< "$out2")
if [ "$rc2" -eq 0 ] && [ "${skipped2:-0}" -eq 4 ] && [ "$((${passed2:-0} + ${skipped2:-0}))" -eq "$SELF_TEST_CASES" ]; then
    ok "without mtools the four cidata cases skip and are still accounted for"
else
    no "without mtools: rc=$rc2 passed=${passed2:-none} skipped=${skipped2:-none}; $(tail -n 2 <<< "$out2")"
fi
# On a box that has mtools both runs exist, and the skipped count must be
# the number of cases that ran only when the tool was there: a skip count
# standing beside the cases as a literal would pass the pin above while
# a guarded case is deleted. Where mtools is absent the two runs are the
# same run and this says so.
if [ "${skipped:-0}" -eq 0 ]; then
    if [ "$((${passed:-0} - ${passed2:-0}))" -eq "${skipped2:-0}" ]; then
        ok "the skipped count equals the cases that ran only with mtools present"
    else
        no "with mtools ${passed:-none} passed, without ${passed2:-none} passed and ${skipped2:-none} skipped; the skip count is not the guarded cases"
    fi
else
    ok "mtools is absent here, so the skip cross-check has one run to read and is not a verdict"
fi

# 2. An unknown flag is refused, not run: a typo must never build a VM.
bash "$SUBJECT" --sefl-test >/dev/null 2>&1; rc=$?
if [ "$rc" -eq 2 ]; then ok "an unknown flag exits 2"; else no "an unknown flag exited $rc"; fi

# 3. The rig the host writes into the VM parses, and every subcommand the
#    host side calls exists in the rig's dispatcher. The two halves live
#    in one file but run on different machines, and a host call to a
#    subcommand the rig does not carry would fail inside the VM as
#    "unknown subcommand" halfway through a matrix.
# shellcheck disable=SC2154  # dir is assigned by guarded_tmpdir through a nameref
rig="$dir/vmlt-rig.sh"
# shellcheck source=scripts/vm-load-test.sh
VMLT_LIB=1 . "$SUBJECT"
vmlt_rig_script > "$rig"
if bash -n "$rig" 2>/dev/null; then ok "the rig script parses"; else no "the rig script does not parse"; fi
if grep -q '^vmlt_lease_owner ()' "$rig"; then
    ok "the rig carries the lease-owner join the self-test drives, as one function text"
else
    no "the rig does not carry vmlt_lease_owner; the VM would join on its own copy"
fi
absent=""
for sub in $(grep -o 'vmlt-rig.sh [a-z]*' "$SUBJECT" | awk '{print $2}' | sort -u); do
    grep -q "^    $sub) " "$rig" || absent="$absent $sub"
done
if [ -z "$absent" ]; then
    ok "every subcommand the host calls is in the rig's dispatcher"
else
    no "the host calls rig subcommands the rig does not carry:$absent"
fi
# The other direction: a dispatcher entry no host line calls is code the
# matrix never runs, and a check that runs one way only lets it sit there.
uncalled=""
for sub in $(sed -n 's/^    \([a-z]*\)) *cmd_.*/\1/p' "$rig"); do
    grep -qE "vmlt-rig\.sh $sub([^a-z]|$)" "$SUBJECT" || uncalled="$uncalled $sub"
done
if [ -z "$uncalled" ]; then
    ok "every subcommand in the rig's dispatcher is called from the host side"
else
    no "the rig carries subcommands no host line calls:$uncalled"
fi

# 4. vm_stop signals only the process qemu recorded, and only when its
#    argv names this run's disk. A pid file outlives its process and the
#    number is reused, so a stale file must never kill whatever holds
#    the number now. The decoy is a foreign process that lives until
#    the case kills it.
export VMLT_WORK="$dir"
# shellcheck disable=SC2034  # read by vm_stop in the sourced script
VMLT_DISK="$dir/disk.qcow2"; VMLT_PIDFILE="$dir/qemu.pid"
tail -f /dev/null &
victim=$!
echo "$victim" > "$VMLT_PIDFILE"
vm_stop >/dev/null 2>&1; rc=$?
if [ "$rc" -ne 0 ] && kill -0 "$victim" 2>/dev/null && [ -f "$VMLT_PIDFILE" ]; then
    ok "vm_stop refuses a pid whose argv does not name this run's disk"
else
    no "vm_stop rc=$rc; the foreign process was signalled or the pidfile removed"
fi
kill "$victim" 2>/dev/null; wait "$victim" 2>/dev/null

# 5. A pid file of a process that is gone is cleaned up and is not an error.
echo "$victim" > "$VMLT_PIDFILE"
vm_stop >/dev/null 2>&1; rc=$?
if [ "$rc" -eq 0 ] && [ ! -f "$VMLT_PIDFILE" ]; then
    ok "vm_stop clears the pidfile of a process that is gone"
else
    no "vm_stop rc=$rc with a dead pid; pidfile present=$([ -f "$VMLT_PIDFILE" ] && echo yes || echo no)"
fi

# 6. Without a pidfile there is nothing to stop and that is not an error.
vm_stop >/dev/null 2>&1; rc=$?
if [ "$rc" -eq 0 ]; then ok "vm_stop with no pidfile is a no-op"; else no "vm_stop with no pidfile exited $rc"; fi

echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
