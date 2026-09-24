#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-engine-matrix-options.sh and the option derivation
# in engine-baseline.sh (#1015). Each red case changes one row, one
# heading or one catalogue line of the shipped reference or the shipped
# step list, and each has a green twin, so a gate that went red on
# everything would fail here too.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-engine-matrix-options.sh"
CELL="$HERE/engine-baseline.sh"
DOC="$HERE/../docs/reference.md"

tmp=""
pass=0
fail=0

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
STEPS="$tmp/steps"
bash "$CELL" --print-option-steps > "$STEPS" || { echo "FAIL  the shipped step list cannot be printed"; exit 1; }

# gate <doc> <steps-file>: the gate over a document and a fixed step list (#1015).
gate() {
    ENGINE_OPTIONS_DOC="$1" ENGINE_OPTIONS_STEPS_CMD="cat $2" bash "$GATE" 2>&1
}

# edit <dest> <sed-script>: a copy of the shipped reference with one edit (#1015).
edit() {
    sed "$2" "$DOC" > "$1"
    cmp -s "$DOC" "$1" && { echo "FAIL  the edit '$2' changed nothing; the case would prove nothing"; exit 1; }
}

# ---- the tree as shipped ---------------------------------------------
out="$(bash "$GATE" 2>&1)"; got=$?
check "the tree as shipped is green" 0 "every one has a matrix line" "$out" "$got"
for want in 'release_lease|step|' 'conflict_check|step|' '--ip|step|' '--ip6|measure|' 'mode|shape|'; do
    out="$(grep -F -- "$want" "$STEPS")"; got=$?
    check "the shipped step list has $want" 0 "$want" "$out" "$got"
done

# ---- a row documented in one place only -------------------------------
edit "$tmp/glance-only.md" '/^| `host_ifname` | bridge | \*(off)\* |$/a | `em_new` | all | `false` |'
out="$(gate "$tmp/glance-only.md" "$STEPS")"; got=$?
check "an option only in At a glance is red" 1 '`em_new` is in the At a glance tables and not in the detailed' "$out" "$got"

edit "$tmp/both.md" '/^| `host_ifname` | bridge | /a | `em_new` | all | `false` |'
out="$(gate "$tmp/both.md" "$STEPS")"; got=$?
check "an option documented in both tables with no matrix line is red" 1 'the option steps print no line for it' "$out" "$got"
out="$(ENGINE_SHAPES_DOC="$tmp/both.md" bash "$CELL" --print-option-steps | grep '^em_new|')"; got=$?
check "the derivation prints an empty line for it" 0 "em_new||" "$out" "$got"
{ cat "$STEPS"; echo 'em_new|step|a fresh line in the server log'; } > "$tmp/steps-new"
out="$(gate "$tmp/both.md" "$tmp/steps-new")"; got=$?
check "green twin: the same option with a step line" 0 "documents 30 options" "$out" "$got"

edit "$tmp/detailed-only.md" '/^| `host_ifname` | bridge | \*(off)\* | \*\*v2.2.0\*\* |/a | `em_new` | all | `false` | **v2.3.0** | A new option. |'
out="$(gate "$tmp/detailed-only.md" "$tmp/steps-new")"; got=$?
check "an option only in the detailed tables is red, even with a matrix line" 1 '`em_new` is in the detailed option tables and not in the At a glance' "$out" "$got"

# ---- a source the derivation stops seeing -----------------------------
edit "$tmp/renamed.md" 's/^## Driver options (per-endpoint)$/## Endpoint options/'
out="$(gate "$tmp/renamed.md" "$STEPS")"; got=$?
check "a renamed per-endpoint heading is red" 1 "cannot read the documented options" "$out" "$got"
edit "$tmp/noflags.md" 's/^\*\*\[Container-level flags\]/**[Container flags]/'
out="$(gate "$tmp/noflags.md" "$STEPS")"; got=$?
check "a reworded flags sentence is red" 1 "cannot read the documented options" "$out" "$got"
edit "$tmp/drop-flag.md" 's/`--hostname`, //'
out="$(gate "$tmp/drop-flag.md" "$STEPS")"; got=$?
check "a catalogue line for a flag the reference dropped is red" 1 'a line for `--hostname`, which' "$out" "$got"
out="$(ENGINE_OPTIONS_DOC="$tmp/drop-flag.md" bash "$GATE" 2>&1)"; got=$?
check "the derivation itself keeps the dropped flag's catalogue line" 1 'a line for `--hostname`, which' "$out" "$got"

# ---- bad catalogue lines ----------------------------------------------
sed 's/^release_lease|step|/release_lease|stepp|/' "$STEPS" > "$tmp/s1"
out="$(gate "$DOC" "$tmp/s1")"; got=$?
check "an unknown kind is red" 1 "unknown kind 'stepp'" "$out" "$got"
sed 's/^release_lease|step|.*/release_lease|step| /' "$STEPS" > "$tmp/s2"
out="$(gate "$DOC" "$tmp/s2")"; got=$?
check "an empty observer is red" 1 '`release_lease` (step) names no observer' "$out" "$got"
sed 's/^release_lease|step|/release_lease|shape|/' "$STEPS" > "$tmp/s3"
out="$(gate "$DOC" "$tmp/s3")"; got=$?
check "a shape kind on a non-shape option is red" 1 'the shape steps drive only' "$out" "$got"
sed 's/^release_lease|step|.*/release_lease|not-driven|too slow to run/' "$STEPS" > "$tmp/s4"
out="$(gate "$DOC" "$tmp/s4")"; got=$?
check "a not-driven reason naming no device is red" 1 "its reason names no missing device" "$out" "$got"
sed 's/^release_lease|step|.*/release_lease|not-driven|needs a DHCP relay device the cell cannot create/' "$STEPS" > "$tmp/s5"
out="$(gate "$DOC" "$tmp/s5")"; got=$?
check "green twin: a not-driven reason naming the device" 0 "(1 not driven)" "$out" "$got"
check "the not-driven line is annotated" 0 "::notice title=engine matrix: option not driven::release_lease" "$out" "$got"
grep -v '^release_lease|' "$STEPS" > "$tmp/s6"
out="$(gate "$DOC" "$tmp/s6")"; got=$?
check "a documented option with no line is red" 1 '`release_lease` and the option steps print no line' "$out" "$got"
sed 's/^release_lease|step|.*/release_lease||/' "$STEPS" > "$tmp/s7"
out="$(gate "$DOC" "$tmp/s7")"; got=$?
check "the derivation's empty line is red" 1 '`release_lease` and the engine matrix has no line' "$out" "$got"

echo
echo "engine-matrix-options self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
