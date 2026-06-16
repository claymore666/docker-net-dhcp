#!/usr/bin/env bash
# Version-pin consistency gate (#251): every published-image pin in the
# docs must point at the SAME version. A pin is any occurrence of the
# plugin image reference
#   ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z
# (plugin install / network create / driver: / plugin inspect snippets).
# If two pins disagree, a release bump was applied to some snippets but
# not others — the failure mode scripts/bump-version.sh exists to avoid,
# and this gate catches it on any branch.
#
# It does NOT assert the pins equal the latest release tag: the release
# branch legitimately leads it (pins bumped to the version about to
# ship, before the tag exists). Internal agreement is the invariant that
# holds everywhere.
#
# Bare "vX.Y.Z" feature markers in prose carry no image ref and are
# correctly ignored.
#
# Usage: check-version-pins.sh [<file>...]
#   defaults: README.md docs/*.md (run from the repo root)
set -u

IMAGE="ghcr.io/claymore666/docker-net-dhcp"

files=("$@")
if [ "${#files[@]}" -eq 0 ]; then
    for f in README.md docs/*.md; do
        [ -f "$f" ] && files+=("$f")
    done
fi
if [ "${#files[@]}" -eq 0 ]; then
    echo "usage: $0 [<file>...]  (no README.md / docs/*.md found)" >&2
    exit 2
fi

# Collect "<version> <file>" for every pin, and the unique version set.
pins="$(grep -hoE "${IMAGE}:v[0-9]+\.[0-9]+\.[0-9]+" "${files[@]}" 2>/dev/null \
    | sed -E "s#.*:##" | sort)"
versions="$(printf '%s\n' "$pins" | grep -v '^$' | sort -u)"

count="$(printf '%s\n' "$versions" | grep -c . || true)"

if [ "$count" -eq 0 ]; then
    echo "FAIL  no image pins found in: ${files[*]}" >&2
    echo "      expected at least one ${IMAGE}:vX.Y.Z install snippet." >&2
    exit 1
fi

if [ "$count" -gt 1 ]; then
    # shellcheck disable=SC2086  # word-split the newline list into a space-joined line
    echo "FAIL  install pins disagree: $(printf '%s ' $versions)" >&2
    echo
    echo "Every ${IMAGE}:vX.Y.Z pin must point at the same version."
    echo "Run: scripts/bump-version.sh <vX.Y.Z>"
    echo
    echo "Per-file pin versions:"
    for f in "${files[@]}"; do
        fv="$(grep -hoE "${IMAGE}:v[0-9]+\.[0-9]+\.[0-9]+" "$f" 2>/dev/null \
            | sed -E "s#.*:##" | sort -u | paste -sd' ' -)"
        [ -n "$fv" ] && echo "  $f: $fv"
    done
    exit 1
fi

echo "PASS  all install pins at ${versions}"
exit 0
