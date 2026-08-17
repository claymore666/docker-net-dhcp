#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

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
# It ALSO checks that every plugin image reference is one a reader can
# actually run: the published namespace, and a tag from a known set
# (#460). Matching only the well-formed reference is not enough — a gate
# that inspects `ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z` can by
# construction never see the reference that got the namespace wrong. The
# macvlan quick start shipped `ghcr.io/<your-namespace>/...:latest` for
# months precisely there: invisible to this gate because the namespace
# was wrong, and skipped by bump-version.sh because the tag was not a
# version.
#
# Usage: check-version-pins.sh [<file>...]
#   defaults: README.md docs/*.md (run from the repo root)
set -u

IMAGE="ghcr.io/claymore666/docker-net-dhcp"

# Any plugin image reference, at any namespace, with any tag — the
# superset this gate has to look at before it can judge a reference.
ANY_REF_RE='ghcr\.io/[^/[:space:]]+/docker-net-dhcp:[A-Za-z0-9._<>-]+'

# The tags a reference may legitimately carry. A concrete pin
# (vX.Y.Z with digits) or one of the placeholders the prose uses when
# the reader is meant to fill in a version. `latest` is deliberately
# absent: reference.md tells readers to pin, and a runnable snippet must
# not contradict it.
allowed_tag() {
    local t="$1"
    # Per-architecture tags (#507). Each architecture is published as its
    # own tag, because a plugin cannot be installed from a manifest list,
    # so the docs legitimately carry references like
    # `...:v1.7.0-linux-arm64-v8` and `...:VERSION-linux-arm64-v8`.
    #
    # A KNOWN suffix is stripped and the version underneath is judged by
    # the same rules as any other pin — rather than loosening those rules
    # — so `v1.7.0-linux-arm64-v8` is a pin at v1.7.0 while a suffix for
    # a platform this project does not publish is still rejected. Add a
    # platform here when one starts being published, which is the point:
    # the set of architectures a reader may be told to install is a
    # decision, not whatever happens to match.
    case "$t" in
        *-linux-amd64 | *-linux-arm64-v8) t="${t%-linux-*}" ;;
    esac

    # ANCHORED, and that is what makes the strip above an allowlist. The
    # old glob (`v[0-9]*.[0-9]*.[0-9]*`) let its trailing `*` swallow any
    # suffix, so `:v1.7.0-linux-riscv64` — or `:v1.7.0-rc1`, which no
    # reader should be pinned to — read as a valid pin. Every tag in the
    # tree was already of the strict form, so this rejects only what it
    # should have been rejecting.
    if [[ "$t" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        return 0                                     # a real pin
    fi
    case "$t" in
        vX.Y.Z|VERSION|vOLD|vNEW|vPREV) return 0 ;;  # documented placeholders
    esac
    return 1
}

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

# --- 1. Every image reference must be runnable ------------------------
# Checked before pin agreement, because a reference with the wrong
# namespace or a bogus tag is not a disagreeing pin — it is not a pin at
# all, and would otherwise pass unseen.
bad_refs=0
for f in "${files[@]}"; do
    while IFS= read -r ref; do
        [ -n "$ref" ] || continue
        ns="${ref#ghcr.io/}"; ns="${ns%%/*}"
        tag="${ref##*:}"
        # A reference at the end of a sentence swallows the full stop.
        # Docker tags may contain '.' but must not end with one, so
        # trimming trailing punctuation cannot hide a real tag.
        tag="${tag%%[.,;)]}"
        reason=""
        [ "$ns" = "claymore666" ] || reason="namespace '${ns}' is not the published one"
        if ! allowed_tag "$tag"; then
            [ -n "$reason" ] && reason="${reason}; "
            reason="${reason}tag '${tag}' is neither a vX.Y.Z pin nor a known placeholder"
        fi
        if [ -n "$reason" ]; then
            [ "$bad_refs" -eq 0 ] && echo "FAIL  unrunnable plugin image reference(s):" >&2
            echo "  ${f}: ${ref}" >&2
            echo "      ${reason}" >&2
            bad_refs=$((bad_refs + 1))
        fi
    done <<< "$(grep -hoE "$ANY_REF_RE" "$f" 2>/dev/null)"
done

if [ "$bad_refs" -ne 0 ]; then
    echo >&2
    echo "Snippets in README.md / docs/ are copy-pasted by readers. Every" >&2
    echo "reference must name ${IMAGE} and carry a pin (or one of the" >&2
    echo "placeholders vX.Y.Z / VERSION / vOLD / vNEW / vPREV). If a new" >&2
    echo "placeholder is genuinely needed, add it to allowed_tag() here so" >&2
    echo "the exemption is a decision rather than a gap." >&2
    exit 1
fi

# --- 2. Every concrete pin must agree on one version ------------------
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
