#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Read a Docker Hub repository's descriptions back and compare them with
# what the release run pushed (#1324). Both sync steps answered 403 in
# every release from v1.7.0-rc2 to v2.6.0-rc1 while ending green.
#
# Expires-when: the description sync can fail its own step without
#   failing the release, so its outcome is visible on its own (#1324).
#
# Usage: check-hub-description.sh <namespace/repository> <readme> <short>
#        HUB_JSON=<file> reads that file instead of the Hub API.
# Exit:  0 both descriptions match byte for byte, 1 they differ,
#        2 the Hub answer or the README cannot be read.
set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2
# shellcheck source=scripts/tmpdir-guard.sh
. "$(dirname "${BASH_SOURCE[0]}")/tmpdir-guard.sh" || exit 2

[ "$#" -eq 3 ] || gate_refuse "usage: $0 <namespace/repository> <readme> <short-description>"
repo="$1" readme="$2" short="$3"
[[ "$repo" =~ ^[a-z0-9._-]+/[a-z0-9._-]+$ ]] || gate_refuse "'$repo' is not a namespace/repository name"
{ [ -f "$readme" ] && [ -r "$readme" ]; } || gate_refuse "$readme is not a readable file"
[ -s "$readme" ] || gate_refuse "$readme is empty; an empty Hub text would match it"
[ -n "$short" ] || gate_refuse "the short description is empty"

# peter-evans/dockerhub-description v5 truncates above 25000 bytes and
# only warns (src/readme-helper.ts MAX_BYTES, read 2026-10-11, #1324).
size=$(wc -c < "$readme")
if [ "$size" -gt 25000 ]; then
    echo "::error title=Hub description::$readme is $size bytes; the Hub takes 25000 and the sync truncates it." >&2
    exit 1
fi

work=""
guarded_tmpdir work

if [ -n "${HUB_JSON:-}" ]; then
    cp -- "$HUB_JSON" "$work/hub.json" 2>/dev/null || gate_refuse "HUB_JSON=$HUB_JSON is not readable"
else
    # no-cache and a fresh query, so no cached copy answers for the Hub (#1324).
    curl -fsS --retry 3 --max-time 30 -H 'Cache-Control: no-cache' \
        -o "$work/hub.json" \
        "https://hub.docker.com/v2/repositories/${repo}/?ts=$(date +%s)" \
        || gate_refuse "the Hub API did not answer for $repo"
fi

jq -e '(.full_description | type == "string") and (.description | type == "string")' \
    "$work/hub.json" >/dev/null 2>&1 \
    || gate_refuse "the Hub answer for $repo has no string full_description and description"
jq -j '.full_description' "$work/hub.json" > "$work/full" || gate_refuse "jq could not extract full_description"
jq -j '.description' "$work/hub.json" > "$work/short" || gate_refuse "jq could not extract description"
printf '%s' "$short" > "$work/short.want"

fail=0
if ! cmp -s -- "$readme" "$work/full"; then
    at="$(cmp -- "$readme" "$work/full" 2>&1 | head -n 1 | sed "s|$work/full|the Hub text|")"
    echo "::error title=Hub description::$repo full description differs from $readme ($size bytes here, $(wc -c < "$work/full") on the Hub): $at" >&2
    fail=1
fi
if ! cmp -s -- "$work/short.want" "$work/short"; then
    echo "::error title=Hub description::$repo short description is $(jq -c .description "$work/hub.json"), the workflow sets $(jq -n --arg s "$short" '$s')" >&2
    fail=1
fi
[ "$fail" -eq 0 ] || exit 1
echo "$repo: both descriptions match $readme and the workflow's short description."
