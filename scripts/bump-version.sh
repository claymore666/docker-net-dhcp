#!/usr/bin/env bash
# Release version-pin bump (#251): rewrite the published-image version
# pins in README.md and docs/ to a new release tag. Run on the release
# branch (runbook step 2) instead of hand-editing each snippet.
#
# Only occurrences of the plugin image reference
#   ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z
# are rewritten, so install/usage snippets (plugin install, network
# create, driver:, plugin inspect) all move to the new version while
# bare "vX.Y.Z" feature markers and historical prose ("as of v1.2.0",
# "v1.1.0 onward") are left untouched — the image ref is the thing that
# distinguishes a pin from prose.
#
# Idempotent and old-version-agnostic: it rewrites whatever version each
# pin currently carries, so a partially-bumped tree self-heals.
#
# Usage: scripts/bump-version.sh vX.Y.Z   (run from the repo root)
set -euo pipefail

VER="${1:-}"
case "$VER" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *)
        echo "usage: $0 vX.Y.Z (e.g. v1.2.0)" >&2
        exit 2
        ;;
esac

IMAGE="ghcr.io/claymore666/docker-net-dhcp"
# Escape dots for the regex (the image ref has none beyond the host, but
# keep it correct if the constant ever changes).
IMAGE_RE="${IMAGE//./\\.}"

files=()
for f in README.md docs/*.md; do
    [ -f "$f" ] && files+=("$f")
done
if [ "${#files[@]}" -eq 0 ]; then
    echo "FAIL  no README.md / docs/*.md found — run from the repo root" >&2
    exit 2
fi

changed=0
for f in "${files[@]}"; do
    before="$(cat "$f")"
    # Rewrite the version that immediately follows the image ref.
    sed -i -E "s#(${IMAGE_RE}:)v[0-9]+\.[0-9]+\.[0-9]+#\1${VER}#g" "$f"
    if [ "$before" != "$(cat "$f")" ]; then
        echo "bumped  $f"
        changed=$((changed + 1))
    fi
done

if [ "$changed" -eq 0 ]; then
    echo "no image pins changed (already at ${VER}?)"
else
    echo "bumped image pins to ${VER} in ${changed} file(s)"
fi
