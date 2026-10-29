#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Check the apk version pins in Dockerfile against the current Alpine
# package index. Prints a diff and, WITH --strict, exits non-zero when a
# pin is behind upstream.
#
# Expires-when: the Dockerfile stops pinning apk package versions, or a bot
#   that opens pin-bump PRs for Alpine packages replaces this cron (#742).
#
# --strict is not optional for the cron caller, and this header used to
# imply otherwise (#742). It said an upgrade signal "lands on the
# maintainer's desk without anyone having to remember to look", while
# apk-pin-check.yml invoked the script with no flags — so drift exited 0,
# GitHub sent nothing (it notifies on failed scheduled runs, not
# successful ones), and the report sat in a log. The plain form is for a
# human at a terminal, who is by definition already looking.
#
# Pins live in the form `pkg=ver-rN` inside the `apk add --no-cache`
# block of Dockerfile. We extract the alpine base from the runtime
# stage's FROM line so the check tracks whatever the Dockerfile
# actually pins (no separate config to drift).
#
# Usage:
#   bash scripts/check-apk-pins.sh                # human-readable report
#   bash scripts/check-apk-pins.sh --strict       # exit 1 if any pin is behind
#   bash scripts/check-apk-pins.sh --list-pins    # print the parsed pins only
#
# "Behind" means the newest version a repository offers is newer than the
# pin by apk's own ordering; a pin newer than anything offered is not in
# the index, so the image build could not install it, and fails too (#1398).
#
# Requires: docker, awk, sed.

set -euo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

DOCKERFILE="${DOCKERFILE:-Dockerfile}"
STRICT=0
LIST_PINS=0
case "${1:-}" in
    --strict) STRICT=1 ;;
    --list-pins) LIST_PINS=1 ;;
esac

# Pull the alpine base off the runtime FROM (the second FROM, after the
# builder). Greedy match would also catch the golang stage; tail -1 keeps
# us pinned to the runtime image.
ALPINE_REF=$(awk '/^FROM alpine:/ {print $2}' "$DOCKERFILE" | tail -n1)
if [[ -z "$ALPINE_REF" ]]; then
    echo "could not find FROM alpine: line in $DOCKERFILE" >&2
    exit 2
fi

# Pinned packages: the `name=version` tokens of each `apk add` command,
# up to the first shell operator or the end of its continued lines; a
# second `apk add` after an operator starts a new list. The RUN that holds
# the list goes on with `&& for ...` and shell assignments of its own;
# reading to the end of the continuation took the trailing `&&` of the
# last pin and a `p="$(...)"` line as pins (#1398). A `name=ver-rN` token
# after an operator inside a RUN that has an `apk add` is reported as
# SUSPECT instead of being dropped: it looks like a pin and is not checked.
mapfile -t PARSED < <(awk '
    /^[[:space:]]*#/ { next }
    {
        n = split($0, tok, /[[:space:]]+/)
        for (k = 1; k <= n; k++) {
            t = tok[k]
            if (t == "" || t == "\\") continue
            if (!in_apk) {
                if (t == "apk" && tok[k + 1] == "add") { in_apk = 1; run = 1; k++ }
                else if (run && t ~ /^[A-Za-z0-9][A-Za-z0-9._+-]*=[0-9][A-Za-z0-9._+~]*-r[0-9]+$/) print "SUSPECT\t" NR "\t" t
                continue
            }
            if (t ~ /^-/) continue
            if (t ~ /^[A-Za-z0-9][A-Za-z0-9._+-]*=[0-9][A-Za-z0-9._+~-]*$/) { print "PIN\t" t; continue }
            if (t ~ /^[A-Za-z0-9][A-Za-z0-9._+-]*$/) continue
            in_apk = 0
        }
        if ($0 !~ /\\[[:space:]]*$/) { in_apk = 0; run = 0 }
    }
' "$DOCKERFILE")

PINS=()
suspect=0
for row in "${PARSED[@]}"; do
    case "$row" in
        PIN$'\t'*) PINS+=("${row#PIN$'\t'}") ;;
        SUSPECT$'\t'*)
            IFS=$'\t' read -r _ lineno tokn <<< "$row"
            echo "$DOCKERFILE:$lineno: '$tokn' looks like an apk pin but stands after a shell operator, so it is not checked; put it in the apk add list" >&2
            suspect=1 ;;
    esac
done
if [[ $suspect -eq 1 ]]; then exit 2; fi

if [[ $LIST_PINS -eq 1 ]]; then
    printf '%s\n' "${PINS[@]}"
    exit 0
fi

if [[ "${#PINS[@]}" -eq 0 ]]; then
    echo "no apk pins found in $DOCKERFILE — nothing to check" >&2
    exit 0
fi

echo "Checking apk pins against $ALPINE_REF"
echo

# Run apk inside the alpine image once and ask each pinned pkg for the
# newest version a repository offers (scripts/apk-newest-available.sh).
#
# Test seam (#169): when APK_AVAIL_FILE is set, read pre-canned
# `name<TAB>version<TAB>cmp` lines from it instead of querying the registry,
# so the parse/compare logic is exercisable by scripts/test-check-apk-pins.sh
# without docker or network. The query script itself is tested apart, on
# real `apk policy` output, in the same self-test.
if [[ -n "${APK_AVAIL_FILE:-}" ]]; then
    LATEST=$(cat "$APK_AVAIL_FILE")
else
    LATEST=$(docker run --rm -i "$ALPINE_REF" sh -s -- "${PINS[@]}" \
        < "$(dirname "${BASH_SOURCE[0]}")/apk-newest-available.sh")
fi

drift=0
while IFS=$'\t' read -r name avail cmp; do
    [[ -n "$name" ]] || continue
    # `apk policy` prints the candidate version with a trailing colon
    # (e.g. `1.36.1-r31:`); strip it so a current pin compares equal
    # instead of being falsely flagged as drifted (#169).
    avail="${avail%:}"
    pinned=""
    for p in "${PINS[@]}"; do
        if [[ "${p%%=*}" == "$name" ]]; then pinned="${p#*=}"; fi
    done
    if [[ -z "$avail" ]]; then
        printf "  %-20s pinned=%-15s available=?  (not found in index — bad pin?)\n" "$name" "$pinned"
        drift=1
        continue
    fi
    if [[ "$pinned" == "$avail" ]]; then
        printf "  %-20s pinned=%-15s OK\n" "$name" "$pinned"
    elif [[ "$cmp" == ">" ]]; then
        printf "  %-20s pinned=%-15s newest in index=%s  ← pin not in the index (bad pin or index lag)\n" "$name" "$pinned" "$avail"
        drift=1
    else
        printf "  %-20s pinned=%-15s available=%s  ← upgrade candidate\n" "$name" "$pinned" "$avail"
        drift=1
    fi
done <<< "$LATEST"

echo
if [[ $drift -eq 0 ]]; then
    echo "All apk pins current."
    exit 0
fi
echo "At least one pin is behind. Bump the affected lines in $DOCKERFILE,"
echo "then verify the plugin still builds and integration tests pass on a"
echo "scratch host before promoting."
if [[ $STRICT -eq 1 ]]; then exit 1; fi
exit 0
