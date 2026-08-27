#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for coverage-read.sh (#794).
#
# THREE THINGS THIS SUITE EXISTS TO STOP.
#
# 1. A FIXTURE SHAPE THE SUBJECT DOES NOT HAVE. Every fixture here was
#    once written in the `job<TAB>step<TAB>line` shape `gh run view --log`
#    produces, and the reader scoped its raw block by that step column.
#    All 25 cases passed while the instrument could not read a single
#    real Coverage run older than about three days: GitHub drops the
#    per-step log files a few days after a run, `gh run view --log` then
#    prints NOTHING, and this is a release-runbook instrument whose
#    subject is routinely a run dispatched days before the rc. So the
#    load-bearing fixture is now scripts/testdata/coverage-run-*.log,
#    carved verbatim out of a real run's per-job log, in the shape that
#    survives retention -- and the old shape is kept beside it, because
#    a fresh run still produces it.
#
# 2. A UNIVERSAL OVER AN EMPTY DOMAIN. "every baselined package got a
#    verdict" is satisfied by a baseline with no packages in it. The
#    reader used to print exactly that line, and exit 0, when the dev
#    baseline's data lines were gone.
#
# 3. AN OPERAND THAT NEVER MOVES. The realistic rows all hold one floor
#    set fixed, so a bug in the frozen one cannot show. The verdict-space
#    fixture below varies both, including main's floor ABOVE dev's.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
READER=$HERE/coverage-read.sh
D=$(mktemp -d); trap 'rm -rf "$D"' EXIT
P=github.com/devplayer0/docker-net-dhcp

pass=0; fail=0
# `grep -F ... >/dev/null` and not `grep -qF`: -q exits at the first match
# and SIGPIPEs the echo feeding it, which under `set -o pipefail` makes a
# succeeding assertion report failure (scripts/check-pipefail-consumers.sh).
chk() { if echo "$2" | grep -F "$3" > /dev/null; then echo "ok   $1"; pass=$((pass+1)); else echo "FAIL $1 (wanted substring: $3)"; fail=$((fail+1)); fi; }
# Counts and exit codes are compared for EQUALITY, never as substrings:
# `echo 10 | grep -qF 0` succeeds, so a substring assertion for "0" is
# also satisfied by 10, 20 and 100.
eq()  { if [ "$2" = "$3" ]; then echo "ok   $1"; pass=$((pass+1)); else echo "FAIL $1 (want '$3', got '$2')"; fail=$((fail+1)); fi; }
no()  { if echo "$2" | grep -F "$3" > /dev/null; then echo "FAIL $1 (must NOT contain: $3)"; fail=$((fail+1)); else echo "ok   $1"; pass=$((pass+1)); fi; }

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

# A step region, in the shape `gh run view --log` produces for a run whose
# per-step files still exist: job<TAB>step<TAB>timestamp <content>, with
# the ##[group] markers gh passes through.
freshgroup() { # $1 = step label, rest = content lines on stdin
  printf 'coverage\t%s\t2026-08-23T06:00:00Z ##[group]Run %s\n' "$1" "$1"
  while IFS= read -r l; do printf 'coverage\t%s\t2026-08-23T06:00:00Z %s\n' "$1" "$l"; done
}
ratchetcmd='bash scripts/coverage-ratchet.sh /tmp/covdata-percent.txt "$baseline"'

verdicts() {
  cat <<V
PASS  $P/pkg/util: 95.7% holds baseline 95.0%
FAIL  $P/pkg/plugin: 86.1% is below baseline 86.8% (epsilon 0.5)
PASS  $P/pkg/dhcp: 90.4% beats baseline 89.9% — raise the floor
PASS  $P/cmd/net-dhcp: 78.3% holds baseline 77.8%
PASS  $P/cmd/dhcp-handler: 75.0% holds baseline 74.0%
V
}
rawblock() { # $1 = plugin's raw number
  for spec in "pkg/util 95.7" "pkg/plugin $1" "pkg/dhcp 90.4" "cmd/net-dhcp 78.3" "cmd/dhcp-handler 75.0"; do
    set -- $spec
    printf '%s/%s\t\tcoverage: %s%% of statements\n' "$P" "$1" "$2"
  done
}

{ echo "$ratchetcmd"; verdicts; } | freshgroup "Coverage ratchet" > "$D/log.full"
head -3 "$D/log.full" > "$D/log.partial"      # the group header, the command, one verdict
{ echo "$ratchetcmd"; echo "some output with no verdict lines at all"; } | freshgroup "Coverage ratchet" > "$D/log.none"

run() { COVREAD_LOG="$1" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER"; }
runx() { run "$1" > "$D/o" 2>&1; echo $?; }

O=$(run "$D/log.full");    X=$(runx "$D/log.full");    echo "--- full ---"; echo "$O"
chk "full: 5 of 5"             "$O" "every one of the 5 baselined package(s) got a verdict."
chk "full: plugin clears main" "$(echo "$O" | tr -s ' ')" "plugin 86.1 86.8 -0.7 RED 85.0 +1.1 over"
eq  "full: no duplicate flag"  "$(echo "$O" | grep -c DUPLICATE)" "0"
eq  "full: exit 0"             "$X" "0"

O=$(run "$D/log.partial"); X=$(runx "$D/log.partial"); echo "--- partial ---"; echo "$O"
chk "partial: incomplete 1/5" "$O" "*** INCOMPLETE: 1 of 5"
chk "partial: names dhcp"     "$O" "$P/pkg/dhcp"
chk "partial: names net-dhcp" "$O" "$P/cmd/net-dhcp"
eq  "partial: exit 2"         "$X" "2"

O=$(run "$D/log.none");    X=$(runx "$D/log.none");    echo "--- none ---"; echo "$O"
chk "none: vacuous"           "$O" "*** VACUOUS"
eq  "none: exit 2"            "$X" "2"


# --- cross-check fixtures: raw covdata vs the ratchet's account ---
{ { echo "$ratchetcmd"; rawblock 86.1; verdicts; } | freshgroup "Coverage ratchet"; } > "$D/log.agree"
{ { echo "$ratchetcmd"; rawblock 91.2; verdicts; } | freshgroup "Coverage ratchet"; } > "$D/log.disagree"

O=$(run "$D/log.agree");    X=$(runx "$D/log.agree"); echo "--- agree ---"; echo "$O"
eq  "agree: no NO RAW flag"  "$(echo "$O" | grep -c 'NO RAW')" "0"
eq  "agree: no disagreement" "$(echo "$O" | grep -c 'different objects')" "0"
eq  "agree: exit 0"          "$X" "0"

O=$(run "$D/log.disagree"); X=$(runx "$D/log.disagree"); echo "--- disagree ---"; echo "$O"
chk "disagree: flagged"      "$O" "ratchet says 86.1%, raw covdata says 91.2% -- two measurements of different objects"
eq  "disagree: exit 1"       "$X" "1"

O=$(run "$D/log.full")
chk "no-raw: flagged"        "$O" "*** NO RAW covdata LINES"


# --- THE RETENTION-TRIMMED SHAPE, from a real run --------------------
# scripts/testdata/coverage-run-32623575563.log is carved verbatim out of
# that run's per-job log: four ##[group] regions, no step-name column,
# real ANSI colour codes around the echoed commands, GitHub's masked org
# segment, and BOTH raw blocks -- the unit step's (cmd/dhcp-handler 0.0%)
# and the ratchet step's (75.0%). Its floors are pinned to the numbers
# the baseline carried at that commit so the case does not drift when a
# floor moves.
cat > "$D/r.dev" <<B
github.com/claymore666/docker-net-dhcp/pkg/util 95.0
github.com/claymore666/docker-net-dhcp/pkg/plugin 86.8
github.com/claymore666/docker-net-dhcp/pkg/dhcp 89.9
github.com/claymore666/docker-net-dhcp/cmd/net-dhcp 77.8
github.com/claymore666/docker-net-dhcp/cmd/dhcp-handler 74.0
B
cp "$D/r.dev" "$D/r.main"
REAL=$HERE/testdata/coverage-run-32623575563.log
rreal() { COVREAD_LOG="$REAL" COVREAD_BASE_DEV="$D/r.dev" COVREAD_BASE_MAIN="$D/r.main" bash "$READER"; }
O=$(rreal); X=$( rreal >/dev/null 2>&1; echo $? ); echo "--- real per-job log ---"; echo "$O"
chk "real: reads the merged numbers" "$(echo "$O" | tr -s ' ')" "plugin 88.3 86.8 +1.5 over"
chk "real: 5 of 5"                   "$O" "every one of the 5 baselined package(s) got a verdict."
eq  "real: unit-only block excluded" "$(echo "$O" | grep -c 'different objects')" "0"
eq  "real: raw block was found"      "$(echo "$O" | grep -c 'NO RAW')" "0"
eq  "real: exit 0"                   "$X" "0"

# ORTHOGONALITY CONTROL for the case above. A green case proves the
# mechanism only if the mechanism it replaced goes red on the same
# fixture. Reproduce the step-column scoping the reader used to do --
# `grep -F "<TAB>Coverage ratchet<TAB>"` -- and drive the same file
# through it. The real log has no step column, so it must find nothing.
python3 - "$READER" "$D/old-scoping.sh" <<'PY'
import re, sys
src = open(sys.argv[1]).read()
start = src.index("    awk '\n")
end = src.index("' \"$TMP/log\" > \"$TMP/step\"\n") + len("' \"$TMP/log\" > \"$TMP/step\"\n")
old = '    grep -F "\t""Coverage ratchet""\t" "$TMP/log" > "$TMP/step" || true\n'
assert src[start:end] != old
open(sys.argv[2], "w").write(src[:start] + old + src[end:])
PY
O=$(COVREAD_LOG="$REAL" COVREAD_BASE_DEV="$D/r.dev" COVREAD_BASE_MAIN="$D/r.main" bash "$D/old-scoping.sh" 2>&1)
chk "control: step-column scoping finds nothing in the real log" "$O" "*** NO RATCHET STEP"
# ...and it must still work on the fresh shape, or the control would be
# measuring a broken script rather than a shape the mechanism cannot read.
O=$(COVREAD_LOG="$D/log.agree" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$D/old-scoping.sh" 2>&1)
eq  "control: step-column scoping still reads the fresh shape" "$(echo "$O" | grep -c 'NO RATCHET STEP')" "0"


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
{
  echo "$ratchetcmd"
  while read -r pk got verd; do
    printf '%s/%s\t\tcoverage: %s%% of statements\n' "$P" "$pk" "$got"
    printf '%s  %s/%s: %s%% of statements per baseline\n' "$verd" "$P" "$pk" "$got"
  done <<R
pkg/exact 90.0 PASS
pkg/epsboundary 89.5 PASS
pkg/bothred 89.4 FAIL
pkg/inverted 86.0 PASS
pkg/invcontrol 89.0 PASS
R
} | freshgroup "Coverage ratchet" > "$D/log.verdict"

O=$(COVREAD_LOG="$D/log.verdict" COVREAD_BASE_DEV="$D/v.dev" COVREAD_BASE_MAIN="$D/v.main" bash "$READER")
echo "--- verdict space ---"; echo "$O"
sq() { echo "$O" | tr -s ' '; }
chk "vs: exact -> holds/holds"        "$(sq)" "exact 90.0 90.0 +0.0 holds 90.0 +0.0 holds"
chk "vs: epsilon boundary is NOT red" "$(sq)" "epsboundary 89.5 90.0 -0.5 holds 90.0 -0.5 holds"
chk "vs: one tenth past eps IS red"   "$(sq)" "bothred 89.4 90.0 -0.6 RED 90.0 -0.6 RED"
chk "vs: over dev, RED on main"       "$(sq)" "inverted 86.0 85.0 +1.0 over 88.0 -2.0 RED"
chk "vs: inverted control preserved"  "$(sq)" "invcontrol 89.0 85.0 +4.0 over 88.0 +1.0 over"
chk "vs: 5 of 5 non-vacuous"          "$O"    "every one of the 5 baselined package(s) got a verdict."
eq  "vs: no raw/ratchet disagreement" "$(echo "$O" | grep -c 'different objects')" "0"


# --- regression for the two-raw-blocks defect, in the fresh shape too ---
# The job log carries TWO blocks matching "coverage: N% of statements": the
# unit-test step's `go test` output (unit-only numbers) and the ratchet
# step's own `cat` of the merged file. Conflating them made the cross-check
# report a disagreement between two measurements of DIFFERENT OBJECTS --
# exactly the fault it exists to catch. Numbers taken from run 32623575563:
# cmd/dhcp-handler reads 0.0% unit-only and 75.0% merged.
{
  { printf '\t%s/cmd/dhcp-handler\t\tcoverage: 0.0%% of statements\n' "$P"
    printf 'ok  \t%s/pkg/plugin\t9.2s\tcoverage: 56.0%% of statements in ./...\n' "$P"
  } | freshgroup "Run unit tests with coverage"
  cat "$D/log.agree"
} > "$D/log.twoblocks"
O=$(COVREAD_LOG="$D/log.twoblocks" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER")
echo "--- two raw blocks ---"; echo "$O"
eq  "2blk: unit-only block excluded"  "$(echo "$O" | grep -c 'different objects')" "0"
chk "2blk: merged numbers still read" "$(echo "$O" | tr -s ' ')" "plugin 86.1 86.8 -0.7 RED 85.0 +1.1 over"
eq  "2blk: no NO-RAW flag"            "$(echo "$O" | grep -c 'NO RAW')" "0"

# --- regression for the redaction defect ---
# GitHub masks a path segment as *** in job logs, so full-import-path
# equality against the baseline matches nothing and every floor reads
# "none" -- honest, but useless. Keying on the last two path segments
# survives a redaction anywhere in the prefix.
sed 's|github.com/devplayer0/|github.com/***/|g' "$D/log.agree" > "$D/log.redacted"
O=$(COVREAD_LOG="$D/log.redacted" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER")
echo "--- redacted org segment ---"; echo "$O"
chk "redact: floors still resolve"  "$(echo "$O" | tr -s ' ')" "plugin 86.1 86.8 -0.7 RED 85.0 +1.1 over"
eq  "redact: no 'none' floors"      "$(echo "$O" | grep -c ' none ')" "0"
chk "redact: still 5 of 5"          "$O" "every one of the 5 baselined package(s) got a verdict."


# --- the refusals ------------------------------------------------------
# A baseline with no data lines empties the domain of "every baselined
# package got a verdict", and a universal over nothing is satisfied by
# nothing. The reader printed that reassuring line, and exited 0.
echo '# every data line lost to a rebase, only commentary left' > "$D/empty.base"
O=$(COVREAD_LOG="$D/log.full" COVREAD_BASE_DEV="$D/empty.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER" 2>&1); X=$?
echo "--- emptied dev baseline ---"; echo "$O"
chk "empty: refuses"                 "$O" "no <package> <percent> lines"
no  "empty: no reassuring universal" "$O" "got a verdict."
eq  "empty: exit 2"                  "$X" "2"

# A baseline ref that does not resolve used to leave an empty file, and
# every floor then read "none" with a clean exit.
O=$(COVREAD_LOG="$D/log.full" COVREAD_BASE_MAIN="$D/main.base" COVREAD_DEV_REF=refs/heads/no-such-branch bash "$READER" 2>&1); X=$?
chk "badref: refuses"  "$O" "the dev baseline will not resolve"
eq  "badref: exit 2"   "$X" "2"

# Two baselined packages sharing the last-two-segment key make the floor
# lookup print two numbers, and every comparison downstream nonsense.
{ cat "$D/dev.base"; echo "github.com/elsewhere/other/pkg/plugin 50.0"; } > "$D/dup.base"
O=$(COVREAD_LOG="$D/log.full" COVREAD_BASE_DEV="$D/dup.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER" 2>&1); X=$?
chk "keydup: refuses"      "$O" "sharing the last-two-segment key: pkg/plugin"
eq  "keydup: exit 2"       "$X" "2"
# ...and refuses BEFORE measuring anything. Without this the case passes
# for the wrong reason: the extra baseline line also makes the comparison
# incomplete, so exit 2 arrives from the non-vacuity check whether or not
# the collision was caught at all.
no  "keydup: no table printed" "$O" "dev-floor"

# A log with no step markers at all cannot have its raw block scoped, so
# the unit-only numbers cannot be told from the ratchet's. Saying so is
# the difference between this and reporting a cross-check that never ran.
{ echo "$ratchetcmd"; rawblock 86.1; verdicts; } > "$D/log.nogroups"
O=$(COVREAD_LOG="$D/log.nogroups" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER" 2>&1); X=$?
chk "nogroup: refuses to scope" "$O" "*** CANNOT SCOPE"
eq  "nogroup: exit 2"           "$X" "2"

# Steps exist, but none of them runs the ratchet: there is no raw block
# to cross-check against, and that is not the same as finding one empty.
{ rawblock 86.1; verdicts; } | freshgroup "Some other step" > "$D/log.noratchet"
O=$(COVREAD_LOG="$D/log.noratchet" COVREAD_BASE_DEV="$D/dev.base" COVREAD_BASE_MAIN="$D/main.base" bash "$READER" 2>&1); X=$?
chk "noratchet: names the step" "$O" "*** NO RATCHET STEP"
eq  "noratchet: exit 2"         "$X" "2"

echo; echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
