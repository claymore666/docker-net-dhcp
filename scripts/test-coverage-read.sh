#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
READER=$HERE/coverage-read.sh
D=$(mktemp -d); trap 'rm -rf "$D"' EXIT
P=github.com/devplayer0/docker-net-dhcp

cat > "$D/dev.base" <<B
# commentary that must not be counted
$P/pkg/util 95.0
$P/pkg/plugin 86.8
$P/pkg/dhcp 89.9
$P/cmd/net-dhcp 77.8
$P/cmd/dhcp-handler 74.0
B
cat > "$D/main.base" <<B
$P/pkg/util 95.0
$P/pkg/plugin 85.0
$P/pkg/dhcp 89.5
$P/cmd/net-dhcp 77.5
$P/cmd/dhcp-handler 74.0
B

# complete: 5 lines, with a gh-style job prefix, and one RED against dev
# that is fine against main (the 1.8-point gap master-release named)
cat > "$D/log.full" <<L
coverage	ratchet	2026-08-23T06:00:00Z PASS  $P/pkg/util: 95.7% holds baseline 95.0%
coverage	ratchet	2026-08-23T06:00:00Z FAIL  $P/pkg/plugin: 86.1% is below baseline 86.8% (epsilon 0.5)
coverage	ratchet	2026-08-23T06:00:00Z PASS  $P/pkg/dhcp: 90.4% beats baseline 89.9% — raise the floor
coverage	ratchet	2026-08-23T06:00:00Z PASS  $P/cmd/net-dhcp: 78.3% holds baseline 77.8%
coverage	ratchet	2026-08-23T06:00:00Z PASS  $P/cmd/dhcp-handler: 75.0% holds baseline 74.0%
L
head -2 "$D/log.full" > "$D/log.partial"
echo "some output with no verdict lines at all" > "$D/log.none"

run() { COVREAD_LOG="$1" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER"; }

pass=0; fail=0
chk() { if echo "$2" | grep -qF "$3"; then echo "ok   $1"; pass=$((pass+1)); else echo "FAIL $1 (wanted: $3)"; fail=$((fail+1)); fi; }

O=$(run "$D/log.full");    echo "--- full ---"; echo "$O"
chk "full: 5 of 5"            "$O" "every baselined package got a verdict."
chk "full: plugin RED on dev" "$O" "RED"
chk "full: plugin clears main" "$(echo "$O" | tr -s ' ')" "plugin 86.1 86.8 -0.7 RED 85.0 +1.1 over"
chk "full: no duplicate flag" "$(echo "$O" | grep -c DUPLICATE)" "0"

O=$(run "$D/log.partial"); echo "--- partial ---"; echo "$O"
chk "partial: incomplete 2/5" "$O" "*** INCOMPLETE: 2 of 5"
chk "partial: names dhcp"     "$O" "$P/pkg/dhcp"
chk "partial: names net-dhcp" "$O" "$P/cmd/net-dhcp"

O=$(run "$D/log.none");    echo "--- none ---"; echo "$O"
chk "none: vacuous"           "$O" "*** VACUOUS"


# --- cross-check fixtures: raw covdata vs the ratchet's account ---
# Real gh log shape: job<TAB>step<TAB>timestamp <content>, and covdata
# separates the package from "coverage:" with tabs. Written with printf
# so the tabs are real tabs, not the two characters backslash-t.
rawblock() { # $1 = plugin's raw number
  for spec in "pkg/util 95.7" "pkg/plugin $1" "pkg/dhcp 90.4" "cmd/net-dhcp 78.3" "cmd/dhcp-handler 75.0"; do
    set -- $spec
    printf 'coverage\tCoverage ratchet\t2026-08-23T06:00:00Z %s/%s\t\tcoverage: %s%% of statements\n' "$P" "$1" "$2"
  done
}
{ rawblock 86.1; cat "$D/log.full"; } > "$D/log.agree"
{ rawblock 91.2; cat "$D/log.full"; } > "$D/log.disagree"

O=$(run "$D/log.agree");    echo "--- agree ---"; echo "$O"
chk "agree: no NO RAW flag"  "$(echo "$O" | grep -c 'NO RAW')" "0"
chk "agree: no disagreement" "$(echo "$O" | grep -c 'different objects')" "0"

O=$(run "$D/log.disagree"); echo "--- disagree ---"; echo "$O"
chk "disagree: flagged"      "$O" "ratchet says 86.1%, raw covdata says 91.2% -- two measurements of different objects"

O=$(run "$D/log.full")
chk "no-raw: flagged"        "$O" "*** NO RAW covdata LINES"


# --- verdict-space fixture: every (dev,main) combination, on its own
# baseline pair so the realistic fixture's counts stay stable.
#
# The original five rows all held ONE side fixed: four were over/over and
# one was RED/over. A bug in whichever operand never moves cannot be caught,
# and three verdicts were never produced at all -- "holds", RED-on-main, and
# the epsilon boundary. RATCHET_EPSILON is 0.5 and the regressed test is
# got + eps < want, so got == want-0.5 must NOT be red.
cat > "$D/v.dev" <<B
$P/pkg/exact 90.0
$P/pkg/epsboundary 90.0
$P/pkg/bothred 90.0
$P/pkg/inverted 85.0
$P/pkg/invcontrol 85.0
B
cat > "$D/v.main" <<B
$P/pkg/exact 90.0
$P/pkg/epsboundary 90.0
$P/pkg/bothred 90.0
$P/pkg/inverted 88.0
$P/pkg/invcontrol 88.0
B
# main's floor ABOVE dev's for the last pair. Not the shape this repo has
# today -- which is exactly why it belongs in a fixture: the reader must
# judge each side from its own operand, not derive one from the other.
vrow() { # pkg got verdictword
  printf 'coverage\tCoverage ratchet\t2026-08-23T07:00:00Z %s/%s\t\tcoverage: %s%% of statements\n' "$P" "$1" "$2"
  printf 'coverage\tCoverage ratchet\t2026-08-23T07:00:00Z %s  %s/%s: %s%% of statements per baseline\n' "$3" "$P" "$1" "$2"
}
{
  vrow pkg/exact       90.0 PASS
  vrow pkg/epsboundary 89.5 PASS
  vrow pkg/bothred     89.4 FAIL
  vrow pkg/inverted    86.0 PASS
  vrow pkg/invcontrol  89.0 PASS
} > "$D/log.verdict"

O=$(COVREAD_LOG="$D/log.verdict" COVREAD_BASE_DEV="$D/v.dev" COVREAD_BASE_MAIN="$D/v.main" bash "$READER")
echo "--- verdict space ---"; echo "$O"
sq() { echo "$O" | tr -s ' '; }
chk "vs: exact -> holds/holds"        "$(sq)" "exact 90.0 90.0 +0.0 holds 90.0 +0.0 holds"
chk "vs: epsilon boundary is NOT red" "$(sq)" "epsboundary 89.5 90.0 -0.5 holds 90.0 -0.5 holds"
chk "vs: one tenth past eps IS red"   "$(sq)" "bothred 89.4 90.0 -0.6 RED 90.0 -0.6 RED"
chk "vs: over dev, RED on main"       "$(sq)" "inverted 86.0 85.0 +1.0 over 88.0 -2.0 RED"
chk "vs: inverted control preserved"  "$(sq)" "invcontrol 89.0 85.0 +4.0 over 88.0 +1.0 over"
chk "vs: 5 of 5 non-vacuous"          "$O"    "every baselined package got a verdict."
chk "vs: no raw/ratchet disagreement" "$(echo "$O" | grep -c 'different objects')" "0"


# --- regression for the two-raw-blocks defect, found on the REAL log ---
# The job log carries TWO blocks matching "coverage: N% of statements": the
# unit-test step's `go test` output (unit-only numbers) and the ratchet
# step's own `cat` of the merged file. Conflating them made the cross-check
# report a disagreement between two measurements of DIFFERENT OBJECTS --
# exactly the fault it exists to catch. Reproduced verbatim from run
# 32623575563: cmd/dhcp-handler reads 0.0% unit-only and 75.0% merged.
{
  printf 'coverage\tRun unit tests with coverage\t2026-08-23T07:11:56Z \t%s/cmd/dhcp-handler\t\tcoverage: 0.0%% of statements\n' "$P"
  printf 'coverage\tRun unit tests with coverage\t2026-08-23T07:11:56Z ok  \t%s/pkg/plugin\t9.2s\tcoverage: 56.0%% of statements in ./...\n' "$P"
  cat "$D/log.agree"
} > "$D/log.twoblocks"
O=$(COVREAD_LOG="$D/log.twoblocks" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER")
echo "--- two raw blocks ---"; echo "$O"
chk "2blk: unit-only block excluded" "$(echo "$O" | grep -c 'different objects')" "0"
chk "2blk: merged numbers still read" "$(echo "$O" | tr -s ' ')" "plugin 86.1 86.8 -0.7 RED 85.0 +1.1 over"
chk "2blk: no NO-RAW flag"            "$(echo "$O" | grep -c 'NO RAW')" "0"

# --- regression for the redaction defect ---
# GitHub masks a path segment as *** in job logs, so full-import-path
# equality against the baseline matches nothing and every floor reads
# "none" -- honest, but useless. Keying on the last two path segments
# survives a redaction anywhere in the prefix.
sed 's|github.com/devplayer0/|github.com/***/|g' "$D/log.agree" > "$D/log.redacted"
O=$(COVREAD_LOG="$D/log.redacted" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER")
echo "--- redacted org segment ---"; echo "$O"
chk "redact: floors still resolve"  "$(echo "$O" | tr -s ' ')" "plugin 86.1 86.8 -0.7 RED 85.0 +1.1 over"
chk "redact: no 'none' floors"      "$(echo "$O" | grep -c ' none ')" "0"
chk "redact: still 5 of 5"          "$O" "every baselined package got a verdict."

echo; echo "$pass passed, $fail failed"
