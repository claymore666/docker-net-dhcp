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
# A pin the index holds an OLDER version of (stale index or mirror lag) is
# reported as such and is not drift; "behind" means available is newer than
# pinned by apk's version ordering (#1398).
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
# up to the first shell operator or the end of its continued lines. The
# RUN that holds the list goes on with `&& for ...` and shell assignments
# of its own; reading to the end of the continuation took the trailing
# `&&` of the last pin and a `p="$(...)"` line as pins (#1398).
mapfile -t PINS < <(awk '
    /^[[:space:]]*#/ { next }
    {
        line = $0
        if (!in_apk) {
            if (!match(line, /apk add([[:space:]]|$)/)) next
            line = substr(line, RSTART + RLENGTH)
            in_apk = 1
        }
        n = split(line, tok, /[[:space:]]+/)
        for (k = 1; k <= n; k++) {
            t = tok[k]
            if (t == "" || t == "\\" || t ~ /^-/) continue
            if (t ~ /^[A-Za-z0-9][A-Za-z0-9._+-]*=[0-9][A-Za-z0-9._+~-]*$/) { print t; continue }
            if (t ~ /^[A-Za-z0-9][A-Za-z0-9._+-]*$/) continue
            in_apk = 0
            break
        }
        if (in_apk && line !~ /\\[[:space:]]*$/) in_apk = 0
    }
' "$DOCKERFILE")

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

# Run apk inside the alpine image once and ask for each pinned pkg.
# `apk policy` lists installed/available versions; we grep for the head
# of the indexed candidate. Cleaner than parsing the apk index by hand.
#
# Third column: `apk version -t PINNED AVAILABLE` (`<`, `=` or `>`), so the
# ordering is apk's own and not a second implementation here (#1398). A
# missing third column counts as "not older", the conservative reading.
#
# Test seam (#169): when APK_AVAIL_FILE is set, read pre-canned
# `name<TAB>version<TAB>cmp` lines from it instead of querying the registry,
# so the parse/compare logic is exercisable by scripts/test-check-apk-pins.sh
# without docker or network.
if [[ -n "${APK_AVAIL_FILE:-}" ]]; then
    LATEST=$(cat "$APK_AVAIL_FILE")
else
    LATEST=$(docker run --rm "$ALPINE_REF" sh -c '
        apk update -q >/dev/null
        for p in "$@"; do
            name="${p%%=*}"
            avail=$(apk policy "$name" 2>/dev/null | awk "/^[[:space:]]+[0-9]/ {print \$1; exit}")
            avail="${avail%:}"
            cmp=""
            [ -z "$avail" ] || cmp=$(apk version -t "${p#*=}" "$avail")
            printf "%s\t%s\t%s\n" "$name" "$avail" "$cmp"
        done
    ' -- "${PINS[@]}")
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
        printf "  %-20s pinned=%-15s available=%s  (index older than pin, not drift)\n" "$name" "$pinned" "$avail"
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
