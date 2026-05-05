#!/usr/bin/env bash
# Check the apk version pins in Dockerfile against the current Alpine
# package index. Prints a diff and exits non-zero when a pin is behind
# upstream — wire it into a weekly CI cron so an upgrade signal lands
# on the maintainer's desk without anyone having to remember to look.
#
# Pins live in the form `pkg=ver-rN` inside the `apk add --no-cache`
# block of Dockerfile. We extract the alpine base from the runtime
# stage's FROM line so the check tracks whatever the Dockerfile
# actually pins (no separate config to drift).
#
# Usage:
#   bash scripts/check-apk-pins.sh                # human-readable report
#   bash scripts/check-apk-pins.sh --strict       # exit 1 if any pin is behind
#
# Requires: docker, awk, sed.

set -euo pipefail

DOCKERFILE="${DOCKERFILE:-Dockerfile}"
STRICT=0
if [[ "${1:-}" == "--strict" ]]; then STRICT=1; fi

# Pull the alpine base off the runtime FROM (the second FROM, after the
# builder). Greedy match would also catch the golang stage; tail -1 keeps
# us pinned to the runtime image.
ALPINE_REF=$(awk '/^FROM alpine:/ {print $2}' "$DOCKERFILE" | tail -n1)
if [[ -z "$ALPINE_REF" ]]; then
    echo "could not find FROM alpine: line in $DOCKERFILE" >&2
    exit 2
fi

# Pinned packages: lines like `        pkg=ver-rN \` inside the apk block.
mapfile -t PINS < <(awk '
    /apk add/ { in_apk = 1; next }
    in_apk && /=/ {
        line = $0
        sub(/^[[:space:]]+/, "", line)
        sub(/[[:space:]]*\\$/, "", line)
        if (line ~ /=/) print line
    }
    in_apk && !/\\$/ { in_apk = 0 }
' "$DOCKERFILE")

if [[ "${#PINS[@]}" -eq 0 ]]; then
    echo "no apk pins found in $DOCKERFILE — nothing to check" >&2
    exit 0
fi

echo "Checking apk pins against $ALPINE_REF"
echo

# Run apk inside the alpine image once and ask for each pinned pkg.
# `apk policy` lists installed/available versions; we grep for the head
# of the indexed candidate. Cleaner than parsing the apk index by hand.
LATEST=$(docker run --rm "$ALPINE_REF" sh -c '
    apk update -q >/dev/null
    for p in "$@"; do
        name="${p%%=*}"
        printf "%s\t" "$name"
        apk policy "$name" 2>/dev/null | awk "/^[[:space:]]+[0-9]/ {print \$1; exit}"
    done
' -- "${PINS[@]}")

drift=0
while IFS=$'\t' read -r name avail; do
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
