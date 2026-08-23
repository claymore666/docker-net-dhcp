#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Read the per-package numbers out of a Coverage run's log and print them
# beside BOTH floor sets. Prints NUMBERS, not a verdict.
#
# Non-vacuity is part of the measurement: the expected package count is
# DERIVED from the baseline the run used, never typed.
#
# COVREAD_LOG=<file> makes the self-test drive THIS script instead of a
# copy of its pipeline.
set -uo pipefail
export LC_ALL=C            # awk's %f follows the locale; this host prints 0,5
REPO=claymore666/docker-net-dhcp
RUN="${1:-}"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

if [ -n "${COVREAD_LOG:-}" ]; then
    cp "$COVREAD_LOG" "$TMP/log"
else
    [ -n "$RUN" ] || { echo "usage: covread.sh <run-id>" >&2; exit 2; }
    gh run view "$RUN" --repo "$REPO" --log > "$TMP/log" 2>/dev/null || true
fi
if [ ! -s "$TMP/log" ]; then
    echo "*** NO LOG: empty output. A log that returns nothing with rc=0 is not a measurement." >&2
    exit 2
fi

# The ratchet's own verdict lines carry both got and the floor it used.
key() { awk -F/ '{ if (NF>=2) print $(NF-1)"/"$NF; else print $0 }'; }
grep -oE '(PASS|FAIL)  [^ ]+: [0-9.]+% ' "$TMP/log" \
  | sed -E 's/^(PASS|FAIL)  ([^ ]+): ([0-9.]+)% $/\1 \2 \3/' \
  | while read -r v pkg got; do echo "$v $(echo "$pkg" | key) $got"; done \
  | sort -u > "$TMP/read"

# Floors. dev = the baseline the dispatch used (the ref's working copy);
# main = what the release PR will ratchet against.
BASE_DEV="${COVREAD_BASE_DEV:-$TMP/dev.base}"
BASE_MAIN="${COVREAD_BASE_MAIN:-$TMP/main.base}"
if [ -z "${COVREAD_BASE_DEV:-}" ]; then
    git show "${COVREAD_DEV_REF:-origin/dev}:.github/coverage-baseline.txt" > "$BASE_DEV"
fi
if [ -z "${COVREAD_BASE_MAIN:-}" ]; then
    git show "${COVREAD_MAIN_REF:-origin/main}:.github/coverage-baseline.txt" > "$BASE_MAIN"
fi
datalines() { grep -vE '^[ \t]*(#|$)' "$1" | awk -F'[ \t]+' 'NF==2 { n=split($1,a,"/"); k = (n>=2) ? a[n-1]"/"a[n] : $1; print k, $2 }'; }
datalines "$BASE_DEV"  > "$TMP/dev.data"
datalines "$BASE_MAIN" > "$TMP/main.data"

WANT=$(wc -l < "$TMP/dev.data" | tr -d ' ')
GOTN=$(wc -l < "$TMP/read"     | tr -d ' ')

printf '%-24s %8s %8s %7s %8s %8s %7s %8s\n' package measured dev-floor d-delta "dev" main-floor m-delta "main"
while read -r verdict pkg got; do
    short=${pkg##*/}
    d=$(awk -v p="$pkg" '$1==p{print $2}' "$TMP/dev.data")
    m=$(awk -v p="$pkg" '$1==p{print $2}' "$TMP/main.data")
    jd=$(awk -v g="$got" -v w="${d:-}" -v e="${RATCHET_EPSILON:-0.5}" 'BEGIN{if(w==""){print "n/a"}else if(g+e<w){print "RED"}else if(g>w){print "over"}else{print "holds"}}')
    jm=$(awk -v g="$got" -v w="${m:-}" -v e="${RATCHET_EPSILON:-0.5}" 'BEGIN{if(w==""){print "n/a"}else if(g+e<w){print "RED"}else if(g>w){print "over"}else{print "holds"}}')
    dd=$(awk -v g="$got" -v w="${d:-}" 'BEGIN{if(w==""){print "n/a"}else{printf "%+.1f", g-w}}')
    md=$(awk -v g="$got" -v w="${m:-}" 'BEGIN{if(w==""){print "n/a"}else{printf "%+.1f", g-w}}')
    printf '%-24s %8s %8s %7s %8s %8s %7s %8s   [ratchet said %s]\n' "$short" "$got" "${d:-none}" "$dd" "$jd" "${m:-none}" "$md" "$jm" "$verdict"
done < "$TMP/read"

# CROSS-CHECK against the raw measurement, not the ratchet's account of it.
# The ratchet step does `cat /tmp/covdata-percent.txt` before running the
# ratchet, so the log carries BOTH: the raw `go tool covdata percent` lines
# and the ratchet's verdict lines. They are two objects. If they disagree,
# the ratchet judged something other than what covdata produced.
RAWSTEP="${COVREAD_RAWSTEP:-Coverage ratchet}"
grep -F "	$RAWSTEP	" "$TMP/log" \
  | grep -oE '[^[:space:]]+[[:blank:]]+coverage: [0-9.]+% of statements' \
  | sed -E 's/^([^[:space:]]+)[[:blank:]]+coverage: ([0-9.]+)% of statements$/\1 \2/' \
  | while read -r pkg got; do echo "$(echo "$pkg" | key) $got"; done \
  | sort -u > "$TMP/raw"
if [ ! -s "$TMP/raw" ]; then
    echo "*** NO RAW covdata LINES in the log -- the numbers below rest on the ratchet's own account only."
else
    while read -r verdict pkg got; do
        r=$(awk -v p="$pkg" '$1==p{print $2}' "$TMP/raw")
        if [ -z "$r" ]; then
            echo "*** $pkg: ratchet says ${got}%, raw covdata has NO line for it"
        elif [ "$r" != "$got" ]; then
            echo "*** $pkg: ratchet says ${got}%, raw covdata says ${r}% -- two measurements of different objects"
        fi
    done < "$TMP/read"
fi

# Duplicate readings: the same package reported twice with different numbers
DUP=$(awk '{print $2}' "$TMP/read" | sort | uniq -d)
[ -n "$DUP" ] && echo "*** DUPLICATE READINGS for: $DUP"

echo
echo "NON-VACUITY"
echo "  baseline data lines (derived from the tree the run used): $WANT"
echo "  packages the ratchet reported a verdict on:               $GOTN"
if [ "$GOTN" -eq 0 ]; then
    echo "  *** VACUOUS: the ratchet compared nothing. A ratchet that compares nothing reports a clean pass."
elif [ "$GOTN" -lt "$WANT" ]; then
    echo "  *** INCOMPLETE: $GOTN of $WANT. Missing:"
    grep -vE '^[ \t]*(#|$)' "$BASE_DEV" | awk -F'[ \t]+' 'NF==2 {print $1}' | while read -r full; do
        k=$(echo "$full" | key)
        grep -q " $k " "$TMP/read" || echo "      $full"
    done
else
    echo "  every baselined package got a verdict."
fi
