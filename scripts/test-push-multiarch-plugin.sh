#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for push_multiarch_plugin.py's publishing shape (#507).
#
# THE INVARIANT. A Docker plugin cannot be installed from a manifest
# list, by any Docker version: Manager.Privileges() runs before the pull
# and matches single manifests only, so the walk stops at the index and
# install aborts with `did not find plugin config for specified
# reference` — on every architecture, including the one you are on.
#
# So the release must publish per-arch manifests and NO index at the
# version tag. `:vX.Y.Z` is the tag every existing amd64 user installs;
# an index landing there does not degrade the release, it breaks it for
# everybody. That is a one-line change away at all times — `make
# push-multiarch` still exists and still writes the index — so the shape
# is asserted here rather than remembered.
#
# The registry is stubbed (scripts/testdata/fake-registry), so this runs
# in the `test` job, which does not install scripts/requirements.txt. A
# self-test that needed the release job's dependencies would be a
# self-test that never ran.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/push_multiarch_plugin.py"
STUBS="$HERE/testdata/fake-registry"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

ok() {
    echo "  ok   — $1"
    pass=$((pass + 1))
}
no() {
    echo "  FAIL — $1"
    fail=$((fail + 1))
}

if [ ! -d "$STUBS" ]; then
    echo "::error title=Missing stubs::$STUBS is absent; this test would otherwise pass having exercised nothing." >&2
    exit 2
fi

# A buildx `-o type=local` tree: one subdirectory per platform, named
# with the platform's dirname (linux_amd64, linux_arm64).
ROOTFS="$TMP/multiarch"
for d in linux_amd64 linux_arm64; do
    mkdir -p "$ROOTFS/$d/usr/sbin"
    echo "$d binary" >"$ROOTFS/$d/usr/sbin/net-dhcp"
done
CONFIG="$TMP/config.json"
echo '{"description":"stub"}' >"$CONFIG"

LIST_MIME='application/vnd.docker.distribution.manifest.list.v2+json'
MF_MIME='application/vnd.docker.distribution.manifest.v2+json'

# push <log-file> [extra args...] -> prints exit code
push() {
    local log="$1"
    shift
    : >"$log"
    FAKE_REGISTRY_LOG="$log" \
        PYTHONPATH="$STUBS" \
        REGISTRY_USERNAME=u REGISTRY_PASSWORD=p \
        python3 "$SCRIPT" "$@" \
        "$CONFIG" "$ROOTFS" "ghcr.io/claymore666/docker-net-dhcp:v1.7.0" \
        >"$log.out" 2>&1
    echo $?
}

# manifest_refs <log> -> the refs that got a manifest PUT, sorted
manifest_refs() {
    awk -F'\t' '$1 == "manifest" { print $2 }' "$1" | sort
}

# mime_of <log> <ref> -> the Content-Type that ref was PUT with
mime_of() {
    awk -F'\t' -v r="$2" '$1 == "manifest" && $2 == r { print $3 }' "$1"
}

echo "push_multiarch_plugin.py self-test"

# ------------------------------------------------------ --no-index: red

log="$TMP/noindex.log"
rc=$(push "$log" --no-index -p linux/amd64,linux/arm64)

[ "$rc" = "0" ] && ok "--no-index exits 0" ||
    no "--no-index must succeed (exit was $rc); output: $(tail -3 "$log.out" | tr '\n' ' ')"

# THE case. The bare version tag must not be written at all — not as an
# index, and not as anything else.
if [ -n "$(mime_of "$log" v1.7.0)" ]; then
    no "--no-index must NOT write the bare tag v1.7.0 — that is the tag amd64 users install"
else
    ok "--no-index leaves the bare tag v1.7.0 untouched"
fi

if grep -q "$LIST_MIME" "$log"; then
    no "--no-index must push no manifest list at all"
else
    ok "--no-index pushes no manifest list"
fi

got="$(manifest_refs "$log" | paste -sd, -)"
want="v1.7.0-linux-amd64,v1.7.0-linux-arm64-v8"
[ "$got" = "$want" ] && ok "--no-index pushes exactly the per-arch tags ($want)" ||
    no "expected per-arch tags '$want', got '$got'"

[ "$(mime_of "$log" v1.7.0-linux-arm64-v8)" = "$MF_MIME" ] &&
    ok "the arm64 per-arch tag is a plain manifest, which is what installs" ||
    no "the arm64 per-arch tag must carry the single-manifest media type"

# Narrowing to one platform is how the release publishes arm64 only,
# leaving `:vX.Y.Z-linux-amd64` to be retagged off the already-signed
# `:vX.Y.Z`. The build still produced both subdirectories.
log="$TMP/arm-only.log"
rc=$(push "$log" --no-index -p linux/arm64)
got="$(manifest_refs "$log" | paste -sd, -)"
[ "$rc" = "0" ] && [ "$got" = "v1.7.0-linux-arm64-v8" ] &&
    ok "a single-platform push publishes only that platform's tag" ||
    no "expected only v1.7.0-linux-arm64-v8 (exit $rc), got '$got'"

# --------------------------------------------------- default: the index

# Without the flag the index is still written. push-multiarch is kept
# for inspection, and this pins that the flag is what changes the shape
# — not some unrelated edit.
log="$TMP/index.log"
rc=$(push "$log" -p linux/amd64,linux/arm64)
[ "$rc" = "0" ] && [ "$(mime_of "$log" v1.7.0)" = "$LIST_MIME" ] &&
    ok "without --no-index the bare tag still gets the manifest list" ||
    no "default behaviour must be unchanged (exit $rc, mime '$(mime_of "$log" v1.7.0)')"

# ------------------------------------------------- partial push is fatal

# Per-platform exceptions are caught and printed so one bad platform
# does not lose the others' work — right for a build, wrong for a
# release. Without this guard a release that published amd64 and lost
# arm64 would report success, and the missing architecture would surface
# as a user's 404 rather than as a red run.
log="$TMP/partial.log"
: >"$log"
FAKE_REGISTRY_LOG="$log" \
    PYTHONPATH="$STUBS" \
    FAKE_REGISTRY_FAIL_REF="manifests/v1.7.0-linux-arm64-v8" \
    REGISTRY_USERNAME=u REGISTRY_PASSWORD=p \
    python3 "$SCRIPT" --no-index -p linux/amd64,linux/arm64 \
    "$CONFIG" "$ROOTFS" "ghcr.io/claymore666/docker-net-dhcp:v1.7.0" \
    >"$log.out" 2>&1
rc=$?
[ "$rc" != "0" ] && ok "losing one platform's manifest is a non-zero exit, not a partial success" ||
    no "a partial push must fail the release; exit was $rc"

# --------------------------------------------------------------- verdict

echo
echo "push_multiarch_plugin.py: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
