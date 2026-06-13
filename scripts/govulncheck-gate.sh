#!/usr/bin/env bash
# govulncheck gate (#127): fail on any vulnerability govulncheck reports
# as reachable from our code, unless its ID is in the reviewed
# allowlist. Warn (without failing) about allowlist entries that are no
# longer reported, so stale acceptances get cleaned up.
#
# Usage: govulncheck-gate.sh <govulncheck-text-output> <allowlist-file>
#   <govulncheck-text-output>: captured stdout of `govulncheck ./...`
#       (exit 3 from govulncheck means "findings exist" — capture the
#       output and let this gate decide).
#   <allowlist-file>: one GO-XXXX-XXXX ID per line, '#' comments ok.
set -u

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <govulncheck-text-output> <allowlist-file>" >&2
    exit 2
fi

REPORT="$1"
ALLOWLIST="$2"
fail=0

# IDs govulncheck reports as affecting our code (default text output
# lists only reachable findings as "Vulnerability #N: GO-...").
found=$(grep -E '^Vulnerability #' "$REPORT" | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u)
allowed=$(grep -vE '^[[:space:]]*(#|$)' "$ALLOWLIST" | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u)

for id in $found; do
    if printf '%s\n' "$allowed" | grep -qx "$id"; then
        echo "ALLOW $id: reported, accepted via $ALLOWLIST"
    else
        echo "FAIL  $id: reachable vulnerability not in $ALLOWLIST — fix the dependency or add a reviewed entry"
        fail=1
    fi
done

for id in $allowed; do
    if ! printf '%s\n' "$found" | grep -qx "$id"; then
        echo "WARN  $id: allowlisted but no longer reported — likely fixed; remove it from $ALLOWLIST"
    fi
done

if [ "$fail" -eq 0 ]; then
    echo "govulncheck gate passed"
fi
exit "$fail"
