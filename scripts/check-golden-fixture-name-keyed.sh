#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The metrics golden must move ONLY for the series that actually
# changed (#651).
#
# fixtureSnapshot used to number HealthResponse by field INDEX --
# (n-i)*10 -- which couples every field's value to every other field's
# position. Adding one field shifted n and reindexed everything below
# it, so a one-series change presented as a wall of renumbered values.
# Measured: inserting a single field in the middle of HealthResponse
# that renders NO series at all moved 74 lines of the golden.
#
# That is a check whose output is unreadable in exactly the situation it
# exists for. A diff that large reads as "regenerate it", which is the
# discharge that ships a defect -- and it is not hypothetical, the
# golden was regenerated twice under that scheme by someone reading a
# large diff and judging it fine.
#
# This gate is the observer for the fix. It inserts a field into the
# MIDDLE of HealthResponse, regenerates the golden, and requires the diff
# to be EMPTY. Any line that moves is a value that depended on a field's
# position rather than on its own name.
#
# It inserts TWO probes, one at a time, because a struct has two field
# orders and a fixture can be coupled to either:
#
#   json:"-"          moves only the UNRENDERED index
#   json:"zz_..."     moves only the RENDERED index
#
# The first probe alone is not enough, and this is measured rather than
# argued: a fixture keyed on a field's rank among the json-tagged fields,
# with untagged fields in a disjoint value band, passes the json:"-"
# probe cleanly -- no guard removed, package green -- and still moves 54
# golden lines when one real metric is inserted mid-struct. That is the
# #651 symptom exactly, sailing past the gate written to catch it.
# Neither probe subsumes the other, so both run.
#
# A tagged field renders no series on its own: the exposition is built
# from metricDefs(), not from reflection over the struct. So the rendered
# probe's expected diff is EMPTY too, for the same reason as the other.
#
# It cannot be a Go test: reflection cannot add a field to a struct type
# at runtime, so the in-package version degrades into asserting that a
# pure function is pure -- green, and proving nothing.
#
# Exit 0 pass, 1 the fixture is index-coupled, 2 cannot judge.

set -uo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
SRC="$ROOT/pkg/plugin/endpoints.go"
GOLDEN="$ROOT/pkg/plugin/testdata/metrics_exposition.golden"
# Each probe is a (field name, json tag) pair. The tag is a VARIABLE, used
# by both the awk that inserts it and the refusal text that names it: the
# previous version hardcoded `tagged json:"-"` in the message, so changing
# the tag would have left the red naming a property the gate no longer
# tested.
PROBES=(
    "ZZGoldenFixtureProbe|-|renders nothing, so it moves only the UNRENDERED field index"
    "ZZGoldenFixtureRenderedProbe|zz_golden_fixture_rendered_probe|carries a real json name, so it moves the RENDERED field index"
)

refuse() { echo "CANNOT JUDGE: $*" >&2; exit 2; }

[ -f "$SRC" ] || refuse "no $SRC"
[ -f "$GOLDEN" ] || refuse "no $GOLDEN"
command -v go >/dev/null 2>&1 || refuse "go is not on PATH"

# The insertion point is DERIVED, not transcribed: find HealthResponse,
# count its tagged fields, and insert after the middle one. Naming a
# field here would be one more hand-written copy of the struct, and a
# rename would silently move the probe to the top of it -- where an
# index-coupled fixture happens to be least disturbed.
mid_line="$(awk '
    /^type HealthResponse struct \{/ { in_struct = 1; next }
    in_struct && /^\}/ { exit }
    in_struct && /`json:"/ { n++; line[n] = NR }
    END { if (n > 2) print line[int(n / 2)] }
' "$SRC")"
[ -n "$mid_line" ] || refuse "could not find the middle of HealthResponse in $SRC"

SRC_BAK="$(mktemp)"; GOLDEN_BAK="$(mktemp)"; PRE="$(mktemp)"
cp "$SRC" "$SRC_BAK"; cp "$GOLDEN" "$GOLDEN_BAK"; cp "$GOLDEN" "$PRE"
restore() { cp "$SRC_BAK" "$SRC"; cp "$GOLDEN_BAK" "$GOLDEN"; rm -f "$SRC_BAK" "$GOLDEN_BAK" "$PRE"; }
trap restore EXIT

regen() {
    ( cd "$ROOT" && UPDATE_GOLDEN=1 go test ./pkg/plugin/ -run TestMetrics_GoldenExposition -count=1 ) >/dev/null 2>&1
}

# POSITIVE CONTROL, before the measurement that matters.
#
# If the regeneration silently does not run -- a build failure, a
# renamed test, a typo in the -run pattern -- the golden never changes,
# the diff below is empty, and this gate reports PASS. A measurement
# that never ran looks exactly like a result. So first prove the
# regeneration is live: damage the golden, regenerate, and require the
# damage to be undone.
printf '\nnet_dhcp_zz_control_line 1\n' >> "$GOLDEN"
if ! regen; then
    refuse "the golden regeneration does not run on this tree (go test failed)"
fi
if ! cmp -s "$PRE" "$GOLDEN"; then
    refuse "the golden regeneration did not restore a damaged golden; it is not writing the file"
fi

# THE MEASUREMENT. One field in the middle, once per probe.
fail=0
for spec in "${PROBES[@]}"; do
    probe="${spec%%|*}"; rest="${spec#*|}"
    tag="${rest%%|*}"; why="${rest#*|}"

    cp "$SRC_BAK" "$SRC"
    awk -v ln="$mid_line" -v probe="$probe" -v tag="$tag" '
        { print }
        NR == ln { printf "\t%s int32 `json:\"%s\"`\n", probe, tag }
    ' "$SRC_BAK" > "$SRC"

    if [ "$(grep -c "$probe" "$SRC")" -ne 1 ]; then
        refuse "the probe field $probe was not inserted; the gate would have measured an unmutated tree"
    fi
    if ! grep -q "$probe int32 \`json:\"$tag\"\`" "$SRC"; then
        refuse "the probe field $probe was inserted without its json:\"$tag\" tag; the gate would have measured the wrong field order"
    fi

    if ! regen; then
        refuse "the golden could not be regenerated with $probe (json:\"$tag\") present"
    fi

    changed="$(diff "$PRE" "$GOLDEN" | grep -cE '^[<>]')"
    if [ "$changed" -ne 0 ]; then
        cat >&2 <<EOF
FAIL: the metrics golden is coupled to HealthResponse's field ORDER.

  Inserting one field ($probe, tagged json:"$tag", after line $mid_line)
  changed $changed lines of the golden. That field renders NO series at
  all -- the exposition comes from metricDefs(), not from reflection over
  the struct -- so every one of those lines is a value that moved because
  a field's POSITION changed, not because anything it measures did.

  This probe $why.

  fixtureSnapshot must derive each field's value from the field's NAME.
  Numbering by position -- (n-i)*10, or a rank among the tagged fields --
  makes a one-series change present as a wall of renumbered values, and a
  diff that large reads as "regenerate it", which is the discharge that
  ships a defect.

  See fixtureValue in pkg/plugin/metrics_test.go.
EOF
        fail=1
    fi
done

[ "$fail" -eq 0 ] || exit 1

echo "PASS: the metrics golden moves only for the series that change (${#PROBES[@]} probes: unrendered and rendered field order)"
exit 0
