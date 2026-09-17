#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-window-constants.sh (#984).
#
# The gate exists because the `release_lease=on_remove` window is three
# constants added up and the tree states the sum in prose. A self-test
# for it has one way to measure nothing: plants written by the person
# who wrote the matcher, in the spelling the matcher already handles. So
# the plants here are not invented. They are THE NINE SPELLINGS THIS
# TREE ACTUALLY USES, enumerated from the sites themselves --
#
#   65 to 80 seconds      band, prose          docs, release notes
#   65 to / 80 seconds    band, wrapped        integration test header
#   tombstone TTL, 60 s.  constant, digits     reference, integration
#   runs every 15 / secs  constant, WRAPPED    release notes
#   is 15 s               constant, bare `s`   deferred_release.go
#   (15s)                 constant, in parens  integration test
#   waits 5 seconds       constant, digits     reference
#   Five seconds is       constant, NUMBER-WORD deferred_release.go
#   5s past the deadline  constant, bare `s`   release notes
#
# -- each driven ONE AT A TIME, each asserted to produce a failure that
# NAMES ITS OWN FILE AND LINE. An exit code alone would pass for a plant
# the matcher never saw, as long as some other plant was still in the
# tree.
#
# Two spellings are wrapped across a line break on purpose: prose wraps,
# and a line-keyed scan reads "runs every 15" and "seconds" as different
# lines and sees no duration at all.
#
# Each plant is also checked to have CHANGED THE FILE. A sed that
# matches nothing leaves a tree that is simply clean, and a clean tree
# passing is not evidence about a matcher.
#
# The safe shapes are driven too, because a gate measured only against
# defects measures "hard to satisfy" and not "right": the fixture
# carries a 75-second test budget beside the sweep tick, a
# `window-exempt:` line, and a loose "about a minute", and the clean
# tree must stay green with all three in it.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"

CHECK="$HERE/check-window-constants.sh"
guarded_tmpdir TMP
OUT="$TMP/out"
failures=0

fixture() {
    rm -rf "$TMP/tree"
    mkdir -p "$TMP/tree/pkg/plugin" "$TMP/tree/docs" "$TMP/tree/test/integration"

    cat > "$TMP/tree/pkg/plugin/state.go" <<'EOF'
package plugin

import "time"

// tombstoneTTL is how long a stopped container keeps its MAC.
const tombstoneTTL = 60 * time.Second
EOF

    cat > "$TMP/tree/pkg/plugin/deferred_release.go" <<'EOF'
package plugin

import "time"

// Five seconds is three orders of magnitude more than the gap it
// covers, and it costs only lateness on an address that is going back
// anyway. The tick below is 15 s, so the wall clock from the stop to
// the datagram is 65 to 80 seconds.
const releaseSettle = 5 * time.Second
EOF

    cat > "$TMP/tree/pkg/plugin/ipam_reserve.go" <<'EOF'
package plugin

import "time"

// ipamSweepInterval is how often the reservation sweep runs.
const ipamSweepInterval = 15 * time.Second
EOF

    cat > "$TMP/tree/docs/reference.md" <<'EOF'
# Reference

| `release_lease` | all | `never` | The window is the tombstone TTL, 60 seconds, the same value that decides how long a stopped container keeps its MAC. The sweep runs every 15 seconds and waits 5 seconds past the deadline, so the wall clock from `docker stop` to the datagram is 65 to 80 seconds. |

The plugin keeps the DHCP identity for the tombstone TTL after a stop.
EOF

    cat > "$TMP/tree/docs/internals.md" <<'EOF'
# Internals

A sweeper ticks every 15 seconds on an `on_remove` network, and a
retained record whose deadline has passed by 5 seconds is handed back.
So the wall clock from `docker stop` to the datagram is 65 to 80
seconds.
EOF

    cat > "$TMP/tree/RELEASE_NOTES.md" <<'EOF'
## v2.2.0

- `release_lease=on_remove`, the third value of the per-network option
  (#984). The sweep that sends runs every 15
  seconds and waits 5s past the deadline, so the wall clock from
  `docker stop` to the datagram is 65 to 80 seconds. An address is free
  about a minute after the stop.

## v0.6.1

Tombstone TTL was 10s, shorter than a daemon restart. v0.6.1 bumps it
to 60s.
EOF

    cat > "$TMP/tree/test/integration/release_lease_on_remove_test.go" <<'EOF'
package integration

// THESE TESTS SPEND REAL TIME AND THAT IS THE POINT. The window is the
// tombstone TTL, 60 seconds, plus the settle and one sweep tick, so the
// wall clock from the stop to the datagram is 65 to
// 80 seconds.

// onRemoveVisibleBudget bounds the wait for the release to reach the
// server, from the stop.
//
// It is the window, plus the settle (5s), plus one sweep tick (15s),
// plus room for a slow runner. A POSITIVE event is being waited on.
const onRemoveVisibleBudget = onRemoveWindow + 75*time.Second
EOF
}

# run <expected-exit> <label>
run() {
    local want="$1" label="$2" got
    bash "$CHECK" --root "$TMP/tree" > "$OUT" 2>&1
    got=$?
    if [ "$got" -eq "$want" ]; then
        echo "PASS  $label (exit $got)"
        return 0
    fi
    echo "FAIL  $label: exit $got, want $want"
    sed 's/^/      /' "$OUT"
    failures=$((failures + 1))
    return 1
}

# plant <file> <sed-expression> <label> <expected-file:line-fragment>
#        [<text the failure must also carry>]
#
# Rebuilds the clean tree, applies ONE edit, refuses if the edit changed
# nothing, and requires the gate to fail NAMING THAT FILE. The fifth
# argument pins WHICH check reported it: a plant that deletes a
# statement rather than falsifying it can otherwise be carried by the
# missing-document check and read as a working matcher.
plant() {
    local file="$1" expr="$2" label="$3" named="$4" carries="${5:-}" before after
    fixture
    before=$(cksum < "$TMP/tree/$file")
    sed -i "$expr" "$TMP/tree/$file"
    after=$(cksum < "$TMP/tree/$file")
    if [ "$before" = "$after" ]; then
        echo "FAIL  $label: the plant matched nothing in $file, so this case drove"
        echo "      a clean tree and measured the gate not at all"
        failures=$((failures + 1))
        return
    fi
    if run 1 "$label"; then
        if ! grep -q "FAIL  $named" "$OUT"; then
            echo "FAIL  $label: the failure does not name $named"
            sed 's/^/      /' "$OUT"
            failures=$((failures + 1))
        elif [ -n "$carries" ] && ! grep -qF "$carries" "$OUT"; then
            echo "FAIL  $label: the failure does not say \"$carries\", so some other"
            echo "      check reported it and this case measures the wrong matcher"
            sed 's/^/      /' "$OUT"
            failures=$((failures + 1))
        fi
    fi
}

# --------------------------------------------------------------- green

fixture
run 0 "the clean tree is green"
if ! grep -q 'NOTE  RELEASE_NOTES.md' "$OUT"; then
    echo "FAIL  the loose \"about a minute\" statement is not reported as a NOTE"
    failures=$((failures + 1))
fi
if grep -q '75' "$OUT"; then
    echo "FAIL  the 75-second test budget beside the sweep tick was reported"
    failures=$((failures + 1))
fi

# --------------------------------------------------- a stale band, one
# spelling at a time

plant docs/reference.md 's/is 65 to 80 seconds/is 60 to 80 seconds/' \
    "a stale band in prose is red" "docs/reference.md:" \
    'states the window as "60 to 80 seconds"' 

plant test/integration/release_lease_on_remove_test.go \
    's|^// 80 seconds\.|// 95 seconds.|' \
    "a stale band WRAPPED across two lines is red" \
    "test/integration/release_lease_on_remove_test.go:" \
    'states the window as "65 to 95 seconds"' 

plant docs/reference.md 's/is 65 to 80 seconds/is 60-80 seconds/' \
    "a stale band written as a DASHED range is red" "docs/reference.md:" \
    'states the window as "60-80 seconds"' 

# ...and the same shape with the right numbers is not a finding: a gate
# measured only against defects measures "hard to satisfy", not "right".
fixture
sed -i 's/is 65 to 80 seconds/is 65-80 seconds/' "$TMP/tree/docs/reference.md"
run 0 "a correct band written as a dashed range is green"

# ------------------------------------------ a stale constant sentence,
# one spelling at a time

plant docs/reference.md 's/tombstone TTL, 60 seconds/tombstone TTL, 45 seconds/' \
    "a stale tombstone TTL in digits is red" "docs/reference.md:"

plant docs/reference.md 's/waits 5 seconds/waits 9 seconds/' \
    "a stale settle in digits is red" "docs/reference.md:"

plant docs/reference.md 's/runs every 15 seconds/runs every 30 seconds/' \
    "a stale tick in digits is red" "docs/reference.md:"

plant RELEASE_NOTES.md 's/runs every 15$/runs every 30/' \
    "a stale tick WRAPPED across two lines is red" "RELEASE_NOTES.md:"

plant RELEASE_NOTES.md 's/waits 5s past/waits 9s past/' \
    "a stale settle spelled \`5s\` is red" "RELEASE_NOTES.md:"

plant pkg/plugin/deferred_release.go 's/is 15 s,/is 30 s,/' \
    "a stale tick spelled \`15 s\` is red" "pkg/plugin/deferred_release.go:"

plant pkg/plugin/deferred_release.go 's/^\/\/ Five seconds is/\/\/ Nine seconds is/' \
    "a stale settle spelled as a NUMBER-WORD is red" "pkg/plugin/deferred_release.go:"

plant test/integration/release_lease_on_remove_test.go 's/(15s)/(30s)/' \
    "a stale tick spelled \`(15s)\` is red" \
    "test/integration/release_lease_on_remove_test.go:"

plant test/integration/release_lease_on_remove_test.go 's/(5s)/(9s)/' \
    "a stale settle spelled \`(5s)\` is red" \
    "test/integration/release_lease_on_remove_test.go:"

# ----------------------------------------------------- the right number
# attached to the wrong subject

# shellcheck disable=SC2016  # literal backticks in the fixture's markdown
plant docs/internals.md \
    's/A sweeper ticks every 15 seconds/An `on_remove` sweeper hands the address back 60 seconds after the stop, and ticks every 15 seconds/' \
    "one of this window's own numbers on the wrong subject is red" \
    "docs/internals.md:" 'states the sweep tick as 60s' 

plant docs/reference.md \
    's/runs every 15 seconds and waits 5 seconds/runs every 5 seconds and waits 15 seconds/' \
    "the two constants swapped is red, and that sentence is otherwise correct" \
    "docs/reference.md:"

fixture
sed -i 's/runs every 15 seconds and waits 5 seconds/runs every 5 seconds and waits 15 seconds/' \
    "$TMP/tree/docs/reference.md"
if run 1 "the swapped sentence is reported for BOTH numbers" > /dev/null; then
    if ! grep -q 'states the sweep tick as 5s' "$OUT" \
       || ! grep -q 'states the settle as 15s' "$OUT"; then
        echo "FAIL  the swap is reported as set membership and not as a pairing"
        sed 's/^/      /' "$OUT"; failures=$((failures + 1))
    fi
fi

# ...and the safe shape of the same sentence: each number is paired with
# the subject it is attached to and neither is a finding.
fixture
run 0 "the same sentence with the two numbers the right way round is green"

# ------------------------------------------ a range of seconds that is
# not this window, in a paragraph that is

fixture
sed -i 's|^A sweeper ticks|A slow runner may take 2 to 5 seconds to start the container. A sweeper ticks|' \
    "$TMP/tree/docs/internals.md"
run 0 "an unrelated range of seconds in a feature paragraph is not a finding"

fixture
# shellcheck disable=SC2016  # literal backticks in the fixture's markdown
sed -i 's|^A sweeper ticks|On an `on_remove` network a slow runner may take 2 to 5 seconds to start the container. A sweeper ticks|' \
    "$TMP/tree/docs/internals.md"
run 0 "...and naming the feature in that sentence does not make it the window"

# -------------------------------------- the population shrinking, not
# only reaching zero

fixture
sed -i 's/is 65 to 80 seconds/is prompt/' "$TMP/tree/docs/reference.md"
if run 1 "a document that loses its band is red while others still state one"; then
    if ! grep -q 'FAIL  docs/reference.md: states no band' "$OUT"; then
        echo "FAIL  the vanished band is not reported against its own document"
        sed 's/^/      /' "$OUT"; failures=$((failures + 1))
    fi
fi

fixture
rm -f "$TMP/tree/docs/internals.md"
run 2 "a document that must state the band and is not there refuses"

# ------------------------------------------- the real defect class: a
# constant moves and every sentence stays as it was

fixture
sed -i 's/const releaseSettle = 5 \* time.Second/const releaseSettle = 7 * time.Second/' \
    "$TMP/tree/pkg/plugin/deferred_release.go"
if run 1 "a constant that moves falsifies the prose that did not"; then
    grep -q '67 to 82 seconds' "$OUT" || {
        echo "FAIL  the derived band is not re-derived from the moved constant"
        sed 's/^/      /' "$OUT"; failures=$((failures + 1)); }
    # The band is judged against the DERIVATION and never against 65/80:
    # the sentence that was true a moment ago is the one that has to go
    # red, and a gate carrying the old numbers as literals reads it as
    # correct forever.
    grep -q 'states the window as "65 to 80 seconds"' "$OUT" || {
        echo "FAIL  \"65 to 80 seconds\" is not reported stale once the settle is 7s,"
        echo "      so the band is being compared to a literal and not to the constants"
        sed 's/^/      /' "$OUT"; failures=$((failures + 1)); }
    if ! grep -q 'RELEASE_NOTES.md' "$OUT" || ! grep -q 'docs/reference.md' "$OUT"; then
        echo "FAIL  the sweep stopped at the first file"
        failures=$((failures + 1))
    fi
fi

# ------------------------------------------------------- the quote and
# the derived value, because a hit nobody can act on is a hit nobody acts on

fixture
sed -i 's/is 65 to 80 seconds/is 60 to 80 seconds/' "$TMP/tree/docs/reference.md"
run 1 "a hit quotes the statement and the derivation"
grep -q '"60 to 80 seconds"' "$OUT" || {
    echo "FAIL  the hit does not quote the offending text"; failures=$((failures + 1)); }
grep -q 'derived band is 65 to 80 seconds' "$OUT" || {
    echo "FAIL  the hit does not state the derived value"; failures=$((failures + 1)); }
grep -q 'tombstoneTTL=60s' "$OUT" || {
    echo "FAIL  the hit does not state the constants it was derived from"
    failures=$((failures + 1)); }

# ------------------------------------------------------------ exempted

# A stale sentence in the domain is red without the marker (the plants
# above) and green with it.
fixture
sed -i 's|^A sweeper ticks every 15 seconds|The sweeper used to tick every 45 seconds. window-exempt: the v2.1 tick, quoted on purpose\nA sweeper ticks every 15 seconds|' \
    "$TMP/tree/docs/internals.md"
run 0 "an exempted line is not a failure"

# ...and the exemption cannot be used to empty the required set: a
# document whose only band is exempted states no band at all.
fixture
sed -i 's/is 65 to 80 seconds/is 65 to 80 seconds window-exempt: not a claim/' \
    "$TMP/tree/docs/reference.md"
if run 1 "exempting a document's only band is itself a failure"; then
    if ! grep -q 'FAIL  docs/reference.md: states no band' "$OUT"; then
        echo "FAIL  an exemption emptied the required set without saying so"
        sed 's/^/      /' "$OUT"; failures=$((failures + 1))
    fi
fi

# The marker reaches the statement it was written for, and not the next
# one. A fixed number of lines does two jobs at once: it reaches the
# second half of a wrapped statement, which is why it exists, and it
# reaches an unrelated neighbouring sentence, which is the gate emptied
# a marker at a time.
# shellcheck disable=SC2016  # literal backticks in the fixture's markdown
plant docs/internals.md \
    's|^A sweeper ticks|On `release_lease=on_remove` the sweep tick is 15 seconds. window-exempt: unrelated\nOn `release_lease=on_remove` the sweep runs every 30 seconds.\nA sweeper ticks|' \
    "a marker does not silence the statement on the line after it" \
    "docs/internals.md:" 'states the sweep tick as 30s'

# The other direction, because a guard fails in one direction and the
# opposite failure has to be named: a marker on the line AFTER the
# statement does not reach it either. The gate's report names the line
# to put it on, and a marker anywhere else leaves the statement red.
# shellcheck disable=SC2016  # literal backticks in the fixture's markdown
plant docs/internals.md \
    's|^A sweeper ticks|On `release_lease=on_remove` the sweep runs every 30 seconds.\nwindow-exempt: written on the wrong line\nA sweeper ticks|' \
    "a marker on the line after the statement does not reach it" \
    "docs/internals.md:" 'states the sweep tick as 30s'

# ...and it does still reach a statement that WRAPS out of its own line,
# which is the case the reach exists for.
fixture
# shellcheck disable=SC2016  # literal backticks in the fixture's markdown
sed -i 's|^A sweeper ticks|On `release_lease=on_remove` the old sweep ran every 45 window-exempt: the v2.1 tick\nseconds, which is not this window.\nA sweeper ticks|' \
    "$TMP/tree/docs/internals.md"
run 0 "a marker reaches the rest of the statement it starts"

# A range of seconds that does not say it is this window is not judged,
# and is printed so that the set left unjudged is in the output.
fixture
# shellcheck disable=SC2016  # literal backticks in the fixture's markdown
sed -i 's|^A sweeper ticks|On `release_lease=on_remove` the restart window is 60 to 90 seconds.\nA sweeper ticks|' \
    "$TMP/tree/docs/internals.md"
if run 0 "an unanchored range is not judged"; then
    if ! grep -q 'states "60 to 90 seconds", a range of seconds that does not say it is this window' "$OUT"; then
        echo "FAIL  the unjudged range is invisible, which is the blindness in the"
        echo "      header and nowhere in the output"
        sed 's/^/      /' "$OUT"; failures=$((failures + 1))
    fi
fi

# ------------------------------------------------------------ refusals

fixture
sed -i 's/const releaseSettle/const releaseSettleDuration/' \
    "$TMP/tree/pkg/plugin/deferred_release.go"
run 2 "a renamed constant refuses rather than passing"

fixture
sed -i 's/65 to 80 seconds/a while/; s/65 to/a while/' "$TMP/tree/docs/reference.md"
sed -i 's/65 to 80$/a while/; s/65 to 80 seconds/a while/' "$TMP/tree/docs/internals.md"
sed -i 's/65 to 80 seconds/a while/' "$TMP/tree/RELEASE_NOTES.md"
sed -i 's/is 65 to/is a while/; s/^\/\/ 80 seconds\./\/\/ or so./' \
    "$TMP/tree/test/integration/release_lease_on_remove_test.go"
sed -i 's/is 65 to 80 seconds/is a while/' "$TMP/tree/pkg/plugin/deferred_release.go"
run 2 "a tree that states no band at all refuses"

fixture
rm -f "$TMP/tree/docs/reference.md" "$TMP/tree/RELEASE_NOTES.md"
rm -f "$TMP/tree/test/integration/release_lease_on_remove_test.go"
rm -f "$TMP/tree/pkg/plugin/deferred_release.go"
cat > "$TMP/tree/pkg/plugin/deferred_release.go" <<'EOF'
package plugin

import "time"

const releaseSettle = 5 * time.Second
EOF
run 2 "an empty domain refuses rather than passing"

bash "$CHECK" --root "$TMP/nonexistent" > "$OUT" 2>&1
if [ $? -eq 2 ]; then
    echo "PASS  a root that is not a directory exits 2"
else
    echo "FAIL  a bad --root did not exit 2"; failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures test(s) failed"
    exit 1
fi
echo "all check-window-constants tests passed"
