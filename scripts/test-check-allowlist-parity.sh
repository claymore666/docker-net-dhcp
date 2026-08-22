#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-allowlist-parity.sh (#741).
#
# The load-bearing case is "an id named only in a removal comment is not
# live". vuln-allowlist.txt documents every removal in a comment that
# NAMES the id it removed — that is the discipline working — so a gate
# that greps the raw file finds GO-2026-5746 whether it is accepted or
# explicitly un-accepted, and reports the drifted tree as clean. It is
# the one case where the obvious implementation passes its own author's
# reading and still answers the wrong question.
set -uo pipefail

GATE="$(cd "$(dirname "$0")" && pwd)/check-allowlist-parity.sh"
pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# write_case <dir> <config-body> <allowlist-body> <map-body>
write_case() {
    local d="$1"
    mkdir -p "$d"
    printf '%s\n' "$2" > "$d/config.yml"
    printf '%s\n' "$3" > "$d/allowlist.txt"
    printf '%s\n' "$4" > "$d/map.txt"
}

verdict() {
    bash "$GATE" "$1/config.yml" "$1/allowlist.txt" "$1/map.txt" >"$TMP/out" 2>&1
    echo $?
}

MAP='GHSA-x744-4wpc-v9h2 GO-2026-4887
GHSA-x86f-5xw2-fm2r GO-2026-5746'

# --- the shape that shipped --------------------------------------------
# Both GHSAs accepted by dependency-review; only one still live on the
# govulncheck side, the other named in the comment that removed it.
d="$TMP/drift"; write_case "$d" \
'fail-on-severity: high
allow-ghsas:
  - GHSA-x744-4wpc-v9h2
  - GHSA-x86f-5xw2-fm2r' \
'# accepted, no fixed release
GO-2026-4887
#
#   GO-2026-5746 was accepted here by the same review and removed on
#   2026-07-28: govulncheck stopped reporting it as reachable.' \
"$MAP"
[ "$(verdict "$d")" = 1 ] \
    && ok "a GHSA whose Go id was removed from the other list is reported" \
    || no "the drifted pair should exit 1 (got $(verdict "$d"))"

grep -F 'GO-2026-5746' "$TMP/out" >/dev/null \
    && ok "and the message names the advisory that drifted" \
    || no "the message does not name the drifted advisory"

# THE CASE THE OBVIOUS IMPLEMENTATION FAILS. The id above appears in the
# file — in the comment recording its removal. A gate that does not
# strip comments finds it and calls this clean. Asserted separately from
# the case above so a future rewrite that reintroduces the raw grep
# fails with a message saying exactly what it did.
raw_hit=$(grep -c 'GO-2026-5746' "$d/allowlist.txt")
[ "$raw_hit" -ge 1 ] \
    && ok "the fixture really does name the removed id in its prose (a raw grep would match)" \
    || no "fixture no longer exercises the comment-stripping property"

# --- the fix -----------------------------------------------------------
d="$TMP/clean"; write_case "$d" \
'fail-on-severity: high
allow-ghsas:
  - GHSA-x744-4wpc-v9h2' \
'GO-2026-4887
#   GO-2026-5746 removed on 2026-07-28.' \
"$MAP"
[ "$(verdict "$d")" = 0 ] \
    && ok "pruning the config to match reads as clean" \
    || no "the reconciled pair should be clean (got $(verdict "$d"))"

# --- an unmapped GHSA cannot be compared, so it is not passed ----------
d="$TMP/unmapped"; write_case "$d" \
'allow-ghsas:
  - GHSA-x744-4wpc-v9h2
  - GHSA-aaaa-bbbb-cccc' \
'GO-2026-4887' \
"$MAP"
[ "$(verdict "$d")" = 1 ] \
    && ok "a GHSA with no declared Go id is reported, not skipped" \
    || no "an unmapped GHSA should exit 1 (got $(verdict "$d"))"

# --- the direction this guard does NOT cover ---------------------------
# A Go id live on the govulncheck side with no GHSA in allow-ghsas is
# out of scope and must not fail: vuln-allowlist.txt records no
# severity, and this gate never acts below fail-on-severity. That
# direction fails safe on its own — dependency-review would reject a PR
# govulncheck accepts, which is a red check someone has to look at.
d="$TMP/extra"; write_case "$d" \
'allow-ghsas:
  - GHSA-x744-4wpc-v9h2' \
'GO-2026-4887
GO-2026-4883' \
"$MAP"
[ "$(verdict "$d")" = 0 ] \
    && ok "a live Go id with no GHSA counterpart is out of scope, not a failure" \
    || no "an extra live Go id should not fail this gate (got $(verdict "$d"))"

# --- inspecting nothing is not a pass ----------------------------------
d="$TMP/emptyallow"; write_case "$d" \
'allow-ghsas:
  - GHSA-x744-4wpc-v9h2' \
'# every entry was removed
#   GO-2026-4887 removed 2026-08-22.' \
"$MAP"
[ "$(verdict "$d")" = 2 ] \
    && ok "an allowlist that parses to zero live entries is rc2, not wholesale drift" \
    || no "a zero-entry allowlist should exit 2 (got $(verdict "$d"))"

d="$TMP/emptymap"; write_case "$d" \
'allow-ghsas:
  - GHSA-x744-4wpc-v9h2' \
'GO-2026-4887' \
'# nothing declared yet'
[ "$(verdict "$d")" = 2 ] \
    && ok "a map declaring no pairs is rc2, not a wall of unmapped findings" \
    || no "an empty map should exit 2 (got $(verdict "$d"))"

# A malformed map line is refused rather than skipped: a line the gate
# cannot parse is a mapping it silently does not have, which resurfaces
# as "no declared Go id" for an entry that was in fact declared.
d="$TMP/badmap"; write_case "$d" \
'allow-ghsas:
  - GHSA-x744-4wpc-v9h2' \
'GO-2026-4887' \
'GHSA-x744-4wpc-v9h2 GO-2026-4887 and some trailing prose'
[ "$(verdict "$d")" = 2 ] \
    && ok "a malformed map line is rc2, not a silently missing mapping" \
    || no "a malformed map line should exit 2 (got $(verdict "$d"))"

# An empty allow-ghsas is a legitimate answer — every acceptance
# withdrawn — but it is also what a broken parse of the config looks
# like, so it is stated rather than folded into a silent pass.
d="$TMP/noghsas"; write_case "$d" \
'fail-on-severity: high
allow-ghsas: []' \
'GO-2026-4887' \
"$MAP"
rc=$(verdict "$d")
if [ "$rc" = 0 ] && grep -F 'allow-ghsas is empty' "$TMP/out" >/dev/null; then
    ok "an empty allow-ghsas passes and says so, rather than passing silently"
else
    no "an empty allow-ghsas should pass with an explicit message (got $rc)"
fi

# --- unreadable inputs --------------------------------------------------
bash "$GATE" /nonexistent /nonexistent /nonexistent >/dev/null 2>&1
[ "$?" = 2 ] \
    && ok "unreadable inputs are rc2" \
    || no "unreadable inputs should exit 2"

# --- prose cannot answer for the config --------------------------------
# The config documents the two GHSAs it removed, by id, in the comment
# explaining why. If the gate read comments it would report them as
# still accepted and fail forever on a correct tree.
d="$TMP/configprose"; write_case "$d" \
'# GHSA-x86f-5xw2-fm2r was removed here in #741 because its Go id is no
# longer live in vuln-allowlist.txt.
allow-ghsas:
  - GHSA-x744-4wpc-v9h2' \
'GO-2026-4887' \
"$MAP"
[ "$(verdict "$d")" = 0 ] \
    && ok "a removed GHSA named in a config comment is not read as still accepted" \
    || no "a config comment was read as an acceptance (got $(verdict "$d"))"

# --- the real repository ------------------------------------------------
real=$( cd "$(dirname "$GATE")/.." && bash "$GATE" >/dev/null 2>&1; echo $? )
[ "$real" = 0 ] \
    && ok "this repository's two allowlists agree" \
    || no "this repository fails its own allowlist-parity gate (exit $real)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
