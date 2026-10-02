#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for drive_options in engine-baseline.sh (#1141), without Docker.
#
# THE VERDICT THIS MUST NEVER PRODUCE is a pass over a catalogue whose
# entries did not all run. A step that drains its stdin (`di` is
# `docker exec -i`) ended the loop after macvlan_mode while the row said
# options=37. The real function is cut out of the script and driven over
# stub steps; the old spelling is rebuilt from it by sed, so the red case
# fails if the descriptor change is reverted.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
CELL="$HERE/engine-baseline.sh"
pass=0
fail=0
tmp=""

# check <label> <want-exit> <want-substring> <output> <got-exit>
check() {
    local label="$1" want="$2" needle="$3" out="$4" got="$5"
    if [ "$got" -eq "$want" ] && printf '%s' "$out" | grep -F -- "$needle" >/dev/null; then
        echo "ok    $label"
        pass=$((pass + 1))
    else
        echo "FAIL  $label: exit $got (want $want), output '$out'"
        fail=$((fail + 1))
    fi
}

guarded_tmpdir tmp

fn="$(sed -n '/^drive_options() {/,/^}/p' "$CELL")"
if [ -z "$fn" ]; then
    echo "FAIL  $CELL has no drive_options function to drive"
    exit 1
fi
printf '%s\n' "$fn" > "$tmp/new.fn"
sed -e 's/read -r -u 3 opt/read -r opt/' -e 's/^    done 3<<EOF$/    done <<EOF/' "$tmp/new.fn" > "$tmp/old.fn"
if cmp -s "$tmp/new.fn" "$tmp/old.fn"; then
    echo "FAIL  the sed that rebuilds the old spelling changed nothing; the red case would prove nothing"
    exit 1
fi

# run <fn-file> <steps> <catalogue>: the function over stub steps; the
# steps are shell text, stdin is /dev/null as on a hosted runner.
run() {
    {
        printf 'say() { printf "%%s\\n" "$*"; }\n'
        printf 'fail() { say "FAIL at step $STEP: $1"; exit 1; }\n'
        printf 'STEP=""; DRIVEN=$'"'"'x-bridge\\n'"'"'\n'
        printf '%s\n' "$2"
        cat "$1"
        printf 'drive_options %q\n' "$3"
        printf 'echo "options=$OPTIONS_DRIVEN/$OPTIONS_LISTED"\n'
    } > "$tmp/run.sh"
    timeout 20 bash "$tmp/run.sh" </dev/null 2>&1
}

cat_three=$'a|step|x\nb|step|y\nc|measure|z'
drain='opt_a() { cat >/dev/null; }; opt_b() { :; }; opt_c() { :; }'
quiet='opt_a() { :; }; opt_b() { :; }; opt_c() { :; }'
closer='opt_a() { exec 3<&-; }; opt_b() { :; }; opt_c() { :; }'

out="$(run "$tmp/new.fn" "$drain" "$cat_three")"; got=$?
check "a step that drains stdin: every entry still runs" 0 "options=3/3" "$out" "$got"
out="$(run "$tmp/old.fn" "$drain" "$cat_three")"; got=$?
check "the old spelling with the same drain is red and names the first skipped entry" 1 "option b is in the catalogue and was never driven (1 of 3 driven)" "$out" "$got"
out="$(run "$tmp/old.fn" "$quiet" "$cat_three")"; got=$?
check "the old spelling without a drain was green (the drain is the cause)" 0 "options=3/3" "$out" "$got"
out="$(run "$tmp/new.fn" "$closer" "$cat_three")"; got=$?
check "a loop that ends early for another cause is red too" 1 "option b is in the catalogue and was never driven (1 of 3 driven)" "$out" "$got"
out="$(run "$tmp/new.fn" "opt_x() { :; }; DRIVEN=\$'a-bridge\\nb-macvlan\\nc-ipvlan\\n'" $'x|step|\nmode|shape|')"; got=$?
check "a shape entry counts once its shapes were driven" 0 "options=2/2" "$out" "$got"
out="$(run "$tmp/new.fn" "opt_x() { :; }" $'x|step|\nmode|shape|')"; got=$?
check "the shape entry fails when its shapes were not driven" 1 "no macvlan shape was driven" "$out" "$got"
out="$(run "$tmp/new.fn" "" $'x|step|\nmode|shape|')"; got=$?
check "a step the cell has no function for is refused" 1 "this cell has no opt_x" "$out" "$got"

out="$(run "$tmp/new.fn" "opt_x() { :; }" $'x|step|\ny|not-driven|needs a device the cell lacks')"; got=$?
check "a not-driven line is reported and is not counted as driven" 0 "options=1/2" "$out" "$got"

# The tested function is the one the cell calls, and the row prints its counters.
out="$(grep -cF 'drive_options "$option_steps"' "$CELL")"; got=$?
check "the cell calls the function it is tested through" 0 "1" "$out" "$got"
out="$(grep -cF 'options=$OPTIONS_DRIVEN/$OPTIONS_LISTED' "$CELL")"; got=$?
check "the verdict prints the driven count over the listed count" 0 "1" "$out" "$got"

rm -rf "$tmp"
echo
echo "engine-baseline option-loop self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
