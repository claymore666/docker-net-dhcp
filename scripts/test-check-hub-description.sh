#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-hub-description.sh (#1324), fed Hub API answers
# built in the live response's shape (read 2026-10-11).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-hub-description.sh"
WF="$HERE/../.github/workflows/release.yml"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"
dir=""
guarded_tmpdir dir

REPO=claymore666/net-dhcp
SHORT="DHCP-leased IPs for Docker containers (bridge/macvlan/ipvlan), as Docker network or IPAM driver."

failures=0
LAST_OUT=""
check() {
    local name="$1" want="$2" json="$3" readme="$4" short="$5" out got
    out="$(HUB_JSON="$json" bash "$GATE" "$REPO" "$readme" "$short" 2>&1)"; got=$?
    LAST_OUT="$out"
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name (exit $got)"
    else
        echo "FAIL: $name -- want $want, got $got"
        printf '%s\n' "$out" | sed 's/^/    /'
        failures=$((failures + 1))
    fi
}
want_in() {
    printf '%s' "$LAST_OUT" | grep -F -- "$1" >/dev/null && return 0
    echo "FAIL: the last case's output did not contain: $1"
    failures=$((failures + 1))
}
# hub <out> <full-description-file> <short>: a Hub answer in the live shape.
hub() {
    jq -n --rawfile full "$2" --arg short "$3" --arg repo "${REPO#*/}" \
        '{user: "claymore666", name: $repo, namespace: "claymore666",
          repository_type: "plugin", status: 1, is_private: false,
          description: $short, full_description: $full,
          last_updated: "2026-10-11T08:00:24.000000Z"}' > "$1"
}

printf '# net-dhcp\n\nA Docker network plugin, caf\303\251.\n' > "$dir/readme"
hub "$dir/same.json" "$dir/readme" "$SHORT"
check "an identical text and short description pass" 0 "$dir/same.json" "$dir/readme" "$SHORT"
want_in "both descriptions match"

# Hub's JSON escapes non-ASCII as \u; decoded it must still compare equal.
jq -c --ascii-output . "$dir/same.json" > "$dir/ascii.json"
grep -F 'u00e9' "$dir/ascii.json" >/dev/null || { echo "FAIL: the ascii fixture carries no escaped e-acute"; failures=$((failures + 1)); }
check "an escaped non-ASCII character compares equal" 0 "$dir/ascii.json" "$dir/readme" "$SHORT"

printf '# net-dhcp\n\nA Docker network plugin, cafe.\n' > "$dir/onebyte"
hub "$dir/onebyte.json" "$dir/onebyte" "$SHORT"
check "a text that differs in one character fails" 1 "$dir/onebyte.json" "$dir/readme" "$SHORT"
want_in "full description differs"
want_in "line 3"

{ cat "$dir/readme"; printf '\n'; } > "$dir/extra-nl"
hub "$dir/extra-nl.json" "$dir/extra-nl" "$SHORT"
check "a Hub text with one more trailing newline fails" 1 "$dir/extra-nl.json" "$dir/readme" "$SHORT"
head -c -1 "$dir/readme" > "$dir/no-nl"
hub "$dir/no-nl.json" "$dir/no-nl" "$SHORT"
check "a Hub text missing the trailing newline fails" 1 "$dir/no-nl.json" "$dir/readme" "$SHORT"

# The live primary repository's state on 2026-10-11: one trailing newline.
hub "$dir/short-nl.json" "$dir/readme" "$SHORT"$'\n'
check "a short description differing by a trailing newline fails" 1 "$dir/short-nl.json" "$dir/readme" "$SHORT"
want_in "short description is"
want_in '\n"'
hub "$dir/short-other.json" "$dir/readme" "Same image as claymore666/net-dhcp."
check "another short description fails" 1 "$dir/short-other.json" "$dir/readme" "$SHORT"

hub "$dir/both.json" "$dir/onebyte" "other"
check "both differing reports both" 1 "$dir/both.json" "$dir/readme" "$SHORT"
want_in "full description differs"
want_in "short description is"

head -c 25000 /dev/zero | tr '\0' 'a' > "$dir/at-limit"
hub "$dir/at-limit.json" "$dir/at-limit" "$SHORT"
check "a README of exactly 25000 bytes is compared" 0 "$dir/at-limit.json" "$dir/at-limit" "$SHORT"
{ cat "$dir/at-limit"; printf 'b'; } > "$dir/over-limit"
hub "$dir/over-limit.json" "$dir/over-limit" "$SHORT"
check "a README over 25000 bytes fails even when the Hub holds it whole" 1 "$dir/over-limit.json" "$dir/over-limit" "$SHORT"
want_in "is 25001 bytes; the Hub takes 25000"

echo '{"name": "net-dhcp", "description": "x"}' > "$dir/missing.json"
check "an answer without full_description refuses" 2 "$dir/missing.json" "$dir/readme" "$SHORT"
want_in "no string full_description"
echo '{"description": "x", "full_description": null}' > "$dir/null.json"
check "a null full_description refuses" 2 "$dir/null.json" "$dir/readme" "$SHORT"
echo '<html>rate limited</html>' > "$dir/html.json"
check "a non-JSON answer refuses" 2 "$dir/html.json" "$dir/readme" "$SHORT"
check "an unreadable answer refuses" 2 "$dir/absent.json" "$dir/readme" "$SHORT"
: > "$dir/empty"
hub "$dir/empty.json" "$dir/empty" "$SHORT"
check "an empty README refuses, even against an empty Hub text" 2 "$dir/empty.json" "$dir/empty" "$SHORT"
check "an absent README refuses" 2 "$dir/same.json" "$dir/nope" "$SHORT"
check "an empty short description refuses" 2 "$dir/same.json" "$dir/readme" ""
out="$(HUB_JSON="$dir/same.json" bash "$GATE" "$REPO" "$dir/readme" 2>&1)"; got=$?
if [ "$got" -eq 2 ]; then echo "PASS: two arguments refuse (exit 2)"; else echo "FAIL: two arguments -- want 2, got $got: $out"; failures=$((failures + 1)); fi
out="$(HUB_JSON="$dir/same.json" bash "$GATE" "net-dhcp" "$dir/readme" "$SHORT" 2>&1)"; got=$?
if [ "$got" -eq 2 ]; then echo "PASS: a name without a namespace refuses (exit 2)"; else echo "FAIL: bare name -- want 2, got $got: $out"; failures=$((failures + 1)); fi

# The release workflow reads BOTH Hub names back, on the sync steps' own
# condition, in a step that cannot be continue-on-error (#1324).
job="$(awk '/^  verify-hub-description:/{f=1; print; next} f && /^  [^ #]/{exit} f' "$WF")"
wiring() {
    local name="$1" pattern="$2"
    if printf '%s\n' "$job" | grep -E -- "$pattern" >/dev/null; then
        echo "PASS: $name"
    else
        echo "FAIL: $name"
        failures=$((failures + 1))
    fi
}
wiring "release.yml has a verify-hub-description job" '^  verify-hub-description:'
wiring "it loops over HUB_NAME and HUB_ALIAS" 'for repo in "\$HUB_NAME" "\$HUB_ALIAS"; do'
wiring "it runs this script on README.md" 'bash scripts/check-hub-description.sh "\$repo" README.md "\$HUB_SHORT_DESCRIPTION"'
wiring "it is gated on HAS_HUB_CREDS" "if: env.HAS_HUB_CREDS == 'true'"
if printf '%s\n' "$job" | grep -E 'continue-on-error|steps\.[a-z-]+\.(outcome|conclusion)' >/dev/null; then
    echo "FAIL: verify-hub-description carries continue-on-error or reads a step outcome"
    failures=$((failures + 1))
else
    echo "PASS: verify-hub-description neither continues on error nor reads a step outcome"
fi
n="$(grep -cF 'short-description: ${{ env.HUB_SHORT_DESCRIPTION }}' "$WF")"
if [ "$n" -eq 2 ]; then
    echo "PASS: both sync steps push HUB_SHORT_DESCRIPTION"
else
    echo "FAIL: $n sync step(s) push HUB_SHORT_DESCRIPTION, want 2"
    failures=$((failures + 1))
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures case(s) failed"
    exit 1
fi
echo "all cases passed"
