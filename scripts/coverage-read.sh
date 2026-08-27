#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Read the per-package numbers out of a Coverage run's log and print them
# beside BOTH floor sets — dev's and main's. The pre-rc read (#794).
#
# Prints NUMBERS, not a release verdict: a package sitting below main's
# floor is information the release decision needs, not an error this
# script can settle. What it DOES refuse is a reading it cannot stand
# behind — see "exit" below.
#
# Non-vacuity is part of the measurement: the expected package count is
# DERIVED from the baseline the run used, never typed.
#
# WHICH LOG, AND WHY NOT `gh run view --log` (#794). That command builds
# its `job<TAB>step<TAB>line` output from the PER-STEP files inside the
# run's log archive, and GitHub drops those files a few days after the
# run while keeping the flat per-job log. Measured 2026-08-27:
#
#   Test run 33077398574 (same day)      75 per-step files, --log works
#   Test run 32672865227 (4 days old)     0 per-step files, --log EMPTY
#   Coverage 32623575563 (4 days old)     0 per-step files, --log EMPTY
#   Coverage 32650413324 (4 days old)     0 per-step files, --log EMPTY
#
# It is age, not the workflow: a Test run and a Coverage run of the same
# age behave the same. That matters here more than anywhere else, because
# this is a RELEASE-RUNBOOK instrument — the Coverage run it reads is
# routinely one that was dispatched days before the rc. So the fetch uses
# the per-job endpoint, which is present for the run's whole retention,
# and the step scoping below is derived from markers BOTH shapes carry.
#
# COVREAD_LOG=<file> makes the self-test drive THIS script instead of a
# copy of its pipeline.
#
# Exit: 0 a complete reading was printed
#       1 the ratchet's account and the raw measurement disagree, or a
#         package was read twice with two numbers
#       2 CANNOT JUDGE — no log, no floors, an empty baseline, an
#         incomplete comparison, or a raw block that cannot be scoped
set -uo pipefail
export LC_ALL=C            # awk's %f follows the locale; this host prints 0,5
REPO="${COVREAD_REPO:-claymore666/docker-net-dhcp}"
RUN="${1:-}"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
rc=0
refuse() { echo "*** CANNOT JUDGE: $*" >&2; exit 2; }

if [ -n "${COVREAD_LOG:-}" ]; then
    cp "$COVREAD_LOG" "$TMP/log"
else
    [ -n "$RUN" ] || { echo "usage: coverage-read.sh <run-id>" >&2; exit 2; }
    # Every job of the run, concatenated. Scoping below is by step region,
    # so a run that grows a second job does not need this line to change.
    jobs=$(gh run view "$RUN" --repo "$REPO" --json jobs --jq '.jobs[].databaseId' 2>/dev/null)
    [ -n "$jobs" ] || refuse "run $RUN reports no jobs. A run id that resolves to nothing is not a measurement."
    for j in $jobs; do
        gh api "repos/$REPO/actions/jobs/$j/logs" >> "$TMP/log" 2>/dev/null || true
    done
fi
[ -s "$TMP/log" ] || refuse "the log is empty. A log that returns nothing with rc=0 is not a measurement."

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
show_baseline() { # ref outfile label
    git show "$1:.github/coverage-baseline.txt" > "$2" 2>"$TMP/giterr" \
      || refuse "the $3 baseline will not resolve at '$1': $(tr '\n' ' ' < "$TMP/giterr" | cut -c1-160)"
}
[ -n "${COVREAD_BASE_DEV:-}"  ] || show_baseline "${COVREAD_DEV_REF:-origin/dev}"   "$BASE_DEV"  dev
[ -n "${COVREAD_BASE_MAIN:-}" ] || show_baseline "${COVREAD_MAIN_REF:-origin/main}" "$BASE_MAIN" main

datalines() { grep -vE '^[ \t]*(#|$)' "$1" | awk -F'[ \t]+' 'NF==2 { n=split($1,a,"/"); k = (n>=2) ? a[n-1]"/"a[n] : $1; print k, $2 }'; }
datalines "$BASE_DEV"  > "$TMP/dev.data"
datalines "$BASE_MAIN" > "$TMP/main.data"

# The floor lookup is keyed on the last two path segments, because the log
# arrives with its org segment masked (see the redaction case in the
# self-test). Two baselined packages sharing that key would make the
# lookup print two numbers and every comparison downstream nonsense, so
# the ambiguity is refused rather than resolved arbitrarily.
for side in dev main; do
    dup=$(awk '{print $1}' "$TMP/$side.data" | sort | uniq -d | tr '\n' ' ')
    [ -z "$dup" ] || refuse "the $side baseline has two packages sharing the last-two-segment key: $dup"
done

WANT=$(wc -l < "$TMP/dev.data" | tr -d ' ')
GOTN=$(wc -l < "$TMP/read"     | tr -d ' ')

# AN EMPTY DOMAIN IS NOT A CLEAN PASS. "every baselined package got a
# verdict" is a universal, and a universal over nothing is satisfied by
# nothing — a baseline whose data lines were lost to a rebase, a rename
# or a wrong path would have printed the reassuring line while the whole
# comparison rested on air. Refuse before any of it is printed.
[ "$WANT" -gt 0 ] || refuse "the dev baseline holds no <package> <percent> lines, so there is no package set to compare against."

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
#
# SCOPED TO THE RATCHET STEP, and scoped by what MAKES it the ratchet step.
# The log carries a SECOND block matching `coverage: N% of statements` —
# the unit-test step's `go test` output, which is unit-only. Conflating the
# two made this check report `cmd/dhcp-handler: ratchet says 75.0%, raw
# covdata says 0.0%`: a disagreement between two measurements of different
# objects, produced by the check that exists to catch exactly that.
#
# The region is delimited by the `##[group]` markers both log shapes carry,
# and is selected by containing the ratchet INVOCATION. Not by the step's
# name: a name is a label someone can change while the step keeps doing the
# same job, and the older, retention-trimmed log shape does not carry step
# names at all.
#
# NOT NAMED `GROUPS`: bash owns that name -- it is the supplementary-group
# array -- and silently DISCARDS an assignment to it, so `GROUPS=$(grep -c
# ...)` left the user's gid in place and the marker test compared 1000
# against 0. The refusal below never fired; the self-test's no-marker case
# is what found it.
STEP_MARKERS=$(grep -c '\[group\]' "$TMP/log")
if [ "$STEP_MARKERS" -eq 0 ]; then
    echo "*** CANNOT SCOPE the raw block: the log carries no ##[group] step markers, so the unit-only coverage block cannot be told from the ratchet step's own."
    rc=2
else
    awk '
        index($0, "[group]") { r++ }
        { line[NR] = $0; reg[NR] = r }
        index($0, "scripts/coverage-ratchet.sh") { mark[r] = 1 }
        END { for (i = 1; i <= NR; i++) if (mark[reg[i]]) print line[i] }
    ' "$TMP/log" > "$TMP/step"
    if [ ! -s "$TMP/step" ]; then
        echo "*** NO RATCHET STEP: no step region in this log runs scripts/coverage-ratchet.sh, so there is no raw block to cross-check against."
        rc=2
    fi
fi
grep -oE '[^[:space:]]+[[:blank:]]+coverage: [0-9.]+% of statements' "$TMP/step" 2>/dev/null \
  | sed -E 's/^([^[:space:]]+)[[:blank:]]+coverage: ([0-9.]+)% of statements$/\1 \2/' \
  | while read -r pkg got; do echo "$(echo "$pkg" | key) $got"; done \
  | sort -u > "$TMP/raw"
if [ ! -s "$TMP/raw" ]; then
    if [ "$rc" -eq 0 ]; then
        echo "*** NO RAW covdata LINES in the ratchet step -- the numbers above rest on the ratchet's own account only."
    fi
else
    while read -r verdict pkg got; do
        r=$(awk -v p="$pkg" '$1==p{print $2}' "$TMP/raw")
        if [ -z "$r" ]; then
            echo "*** $pkg: ratchet says ${got}%, raw covdata has NO line for it"
            rc=1
        elif [ "$r" != "$got" ]; then
            echo "*** $pkg: ratchet says ${got}%, raw covdata says ${r}% -- two measurements of different objects"
            rc=1
        fi
    done < "$TMP/read"
fi

# Duplicate readings: the same package reported twice with different numbers
DUP=$(awk '{print $2}' "$TMP/read" | sort | uniq -d)
if [ -n "$DUP" ]; then
    echo "*** DUPLICATE READINGS for: $DUP"
    rc=1
fi

echo
echo "NON-VACUITY"
echo "  baseline data lines (derived from the tree the run used): $WANT"
echo "  packages the ratchet reported a verdict on:               $GOTN"
# COMPARED BY NAME, NOT BY COUNT. "every baselined package got a verdict"
# is a claim about MEMBERSHIP, and the counts answer a different question.
# Two sets of equal size need not be the same set: one package dropping
# out while an unbaselined one gains a verdict leaves GOTN == WANT, and a
# cardinality test then prints "every one of them" over a set that is
# missing a member. Driven at 9f8d640 with a five-line baseline and five
# verdicts, one of them for an unbaselined `pkg/ghost`: this script said
# every one of the five baselined packages got a verdict and exited 0,
# while `cmd/dhcp-handler` had got none. In a release-runbook instrument
# whose entire job is refusing a reading it cannot stand behind.
#
# The sibling gets this right and says why -- coverage-ratchet.sh:154-160,
# "a substitution keeps the count and changes the verdict, which a count
# check reads as complete". The same paragraph, applied here.
#
# Deriving the expected count from the tree rather than typing it, which
# is what the header above is about, is necessary and not sufficient: a
# correctly derived count still cannot answer a membership question.
awk '{print $1}' "$TMP/dev.data" | sort -u > "$TMP/want.keys"
awk '{print $2}' "$TMP/read"     | sort -u > "$TMP/got.keys"
comm -23 "$TMP/want.keys" "$TMP/got.keys" > "$TMP/missing.keys"
comm -13 "$TMP/want.keys" "$TMP/got.keys" > "$TMP/extra.keys"
nmissing=$(wc -l < "$TMP/missing.keys" | tr -d ' ')
nextra=$(wc -l < "$TMP/extra.keys" | tr -d ' ')

if [ "$GOTN" -eq 0 ]; then
    echo "  *** VACUOUS: the ratchet compared nothing. A ratchet that compares nothing reports a clean pass."
    rc=2
else
    if [ "$nmissing" -gt 0 ]; then
        echo "  *** INCOMPLETE: $((WANT - nmissing)) of $WANT. Missing:"
        # Resolved back to the FULL baseline path, not the last-two-segment
        # key: the key is what the log lets us match on, but the reader has
        # to go find the package, and two repositories can share a key.
        grep -vE '^[ \t]*(#|$)' "$BASE_DEV" | awk -F'[ \t]+' 'NF==2 {print $1}' | while read -r full; do
            k=$(echo "$full" | key)
            grep -qxF "$k" "$TMP/missing.keys" && echo "      $full"
        done
        rc=2
    fi
    # The arm the count shape could not express at all. A verdict for a
    # package no baseline floors is not a harmless extra: it is what makes
    # the counts agree while the memberships differ, and until now it was
    # visible only as a "none n/a" cell in the table above.
    if [ "$nextra" -gt 0 ]; then
        echo "  *** UNBASELINED: $nextra package(s) got a verdict but are in no baseline:"
        sed 's/^/      /' "$TMP/extra.keys"
        echo "      A verdict here is compared against no floor, and it pads the count"
        echo "      that any cardinality check would read as completeness."
        rc=2
    fi
    if [ "$nmissing" -eq 0 ] && [ "$nextra" -gt 0 ]; then
        echo "  (every baselined package did get a verdict; the defect above is on the other side.)"
    fi
    if [ "$nmissing" -eq 0 ] && [ "$nextra" -eq 0 ]; then
        echo "  every one of the $WANT baselined package(s) got a verdict. Matched by name."
    fi
fi
exit "$rc"
