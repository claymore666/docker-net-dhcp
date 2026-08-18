#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-reference-digests.sh (#502). Builds
# synthetic binaries and a synthetic doc through the DIGEST_DOCS seam,
# so no release, no network and no docker are involved.
#
# The cases that carry the most weight:
#   - a stale VERSION LABEL alone fails, even with correct digests. That
#     is the exact state the issue was filed about.
#   - a pre-release tag compares against its base version, so the rc
#     dry-run is a real rehearsal rather than a guaranteed red.
#   - a reshaped or absent digest section exits 2, not 0. A gate that
#     silently matches nothing is the failure this repo keeps shipping.
set -u

CHECK="$(dirname "$0")/check-reference-digests.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

printf 'main binary\n' > "$TMP/net-dhcp"
printf 'handler binary\n' > "$TMP/dhcp-handler"
D_MAIN=$(sha256sum "$TMP/net-dhcp" | cut -d' ' -f1)
D_HANDLER=$(sha256sum "$TMP/dhcp-handler" | cut -d' ' -f1)

# doc VERSION MAIN_DIGEST HANDLER_DIGEST -> writes $TMP/doc.md
doc() {
    cat > "$TMP/doc.md" <<EOF
## Rebuilding the binaries yourself

Some prose that mentions v9.9.9 in passing and must not be picked up.

The two pairs of digests must match. For **$1** they are:

\`\`\`
$2  net-dhcp
$3  dhcp-handler
\`\`\`
EOF
}

failures=0
# check NAME WANT_EXIT TAG GREP
check() {
    local name="$1" want_exit="$2" tag="$3" want_grep="$4"
    DIGEST_DOCS="$TMP/doc.md" bash "$CHECK" "$tag" \
        "$TMP/net-dhcp" "$TMP/dhcp-handler" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

doc "v1.6.0" "$D_MAIN" "$D_HANDLER"
check "matching digests and label pass" 0 "v1.6.0" "matches the published v1.6.0 amd64 binaries"

# THE case from the issue: digests right, heading a release behind.
doc "v1.5.0" "$D_MAIN" "$D_HANDLER"
check "a stale version label alone fails" 1 "v1.6.0" "documents reference digests for v1.5.0"

doc "v1.6.0" "0000000000000000000000000000000000000000000000000000000000000000" "$D_HANDLER"
check "a stale net-dhcp digest fails" 1 "v1.6.0" "net-dhcp digest in .* is stale"

doc "v1.6.0" "$D_MAIN" "0000000000000000000000000000000000000000000000000000000000000000"
check "a stale dhcp-handler digest fails" 1 "v1.6.0" "dhcp-handler digest in .* is stale"

# The rc rehearses the release: same source, so same digests, and the
# label must be compared against the base version.
doc "v1.6.0" "$D_MAIN" "$D_HANDLER"
check "a pre-release tag compares against its base version" 0 "v1.6.0-rc1" "matches the published v1.6.0"

doc "v1.5.0" "$D_MAIN" "$D_HANDLER"
check "a pre-release tag still catches a stale label" 1 "v1.6.0-rc1" "this release is v1.6.0"

# The failure message must be actionable — it prints the block to paste.
doc "v1.5.0" "$D_MAIN" "$D_HANDLER"
check "the failure prints the corrected block" 1 "v1.6.0" "$D_MAIN  net-dhcp"

# Blindness guards.
printf '## Rebuilding\n\nNo digest section here at all.\n' > "$TMP/doc.md"
check "a doc with no version label exits 2" 2 "v1.6.0" "watching nothing"

cat > "$TMP/doc.md" <<EOF
The two pairs of digests must match. For **v1.6.0** they are:

\`\`\`
$D_MAIN  net-dhcp
\`\`\`
EOF
check "a half-present digest block exits 2" 2 "v1.6.0" "could not read both reference digests"

# Per-arch blocks (#507). One doc, two labelled blocks with DIFFERENT
# digests; each arch must read only its own block. The arm64 "digests"
# are the amd64 pair reversed, so a scoping bug (whole-file grep takes
# the first block for both arches) fails loudly rather than passing by
# coincidence.
doc2() {
    cat > "$TMP/doc.md" <<EOF
The two pairs of digests must match. For **v1.6.0** (\`linux/amd64\`) they are:

\`\`\`
$D_MAIN  net-dhcp
$D_HANDLER  dhcp-handler
\`\`\`

The two pairs of digests must match. For **v1.6.0** (\`linux/arm64\`) they are:

\`\`\`
$D_HANDLER  net-dhcp
$D_MAIN  dhcp-handler
\`\`\`
EOF
}
# check_arch NAME WANT_EXIT TAG ARCH GREP
check_arch() {
    local name="$1" want_exit="$2" tag="$3" arch="$4" want_grep="$5"
    DIGEST_DOCS="$TMP/doc.md" bash "$CHECK" "$tag" \
        "$TMP/net-dhcp" "$TMP/dhcp-handler" "$arch" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

doc2
check_arch "labelled amd64 block passes for amd64" 0 "v1.6.0" amd64 "matches the published v1.6.0 amd64"
# The binaries carry D_MAIN/D_HANDLER; the arm64 block documents them
# reversed, so a correctly-scoped arm64 read MUST mismatch. If it
# passes, arm64 read the amd64 block — the scoping bug this guards.
check_arch "the arm64 read is scoped to the arm64 block" 1 "v1.6.0" arm64 "net-dhcp digest in .* is stale"
check_arch "an rc tag works against a labelled block" 0 "v1.6.0-rc1" amd64 "matches the published v1.6.0 amd64"

doc "v1.6.0" "$D_MAIN" "$D_HANDLER"
check_arch "a doc without an arm64 block exits 2 for arm64" 2 "v1.6.0" arm64 "no arm64 version label"
check_arch "the unlabelled legacy block still counts as amd64" 0 "v1.6.0" amd64 "matches the published v1.6.0 amd64"

# The corrected block a failure prints must carry the arch label, or
# pasting it produces a doc the arm64 gate cannot find.
doc2
DIGEST_DOCS="$TMP/doc.md" bash "$CHECK" v1.6.0 "$TMP/net-dhcp" "$TMP/dhcp-handler" arm64 > "$TMP/out" 2>&1
# The backticks are literal doc markup, not expansion.
# shellcheck disable=SC2016
if grep -q 'For \*\*v1.6.0\*\* (`linux/arm64`) they are' "$TMP/out"; then
    echo "PASS: the corrected block is arch-labelled"
else
    echo "FAIL: the corrected block is arch-labelled"
    sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi

# Usage / missing inputs.
DIGEST_DOCS="$TMP/doc.md" bash "$CHECK" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q "usage:" "$TMP/out"; then
    echo "PASS: no arguments exits 2 with usage"
else
    echo "FAIL: no arguments exits 2 with usage"
    failures=$((failures + 1))
fi

doc "v1.6.0" "$D_MAIN" "$D_HANDLER"
DIGEST_DOCS="$TMP/doc.md" bash "$CHECK" v1.6.0 "$TMP/absent" "$TMP/dhcp-handler" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q "nothing to compare against" "$TMP/out"; then
    echo "PASS: a missing binary exits 2 rather than passing"
else
    echo "FAIL: a missing binary exits 2 rather than passing"
    sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi

DIGEST_DOCS="$TMP/nope.md" bash "$CHECK" v1.6.0 "$TMP/net-dhcp" "$TMP/dhcp-handler" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q "does not exist" "$TMP/out"; then
    echo "PASS: a missing doc exits 2"
else
    echo "FAIL: a missing doc exits 2"
    failures=$((failures + 1))
fi

# The committed doc must still be parseable by this checker, even though
# its digests belong to whatever shipped last. Parse-only: a mismatch is
# expected here and is not what this case is about.
REAL_DOC="$(dirname "$0")/../docs/verifying-releases.md"
if [ -f "$REAL_DOC" ]; then
    v=$(sed -n 's/.*For \*\*\(v[0-9][0-9.]*\)\*\* they are.*/\1/p' "$REAL_DOC" | head -1)
    m=$(sed -n 's/^\([0-9a-f]\{64\}\)  *net-dhcp$/\1/p' "$REAL_DOC" | head -1)
    h=$(sed -n 's/^\([0-9a-f]\{64\}\)  *dhcp-handler$/\1/p' "$REAL_DOC" | head -1)
    if [ -n "$v" ] && [ -n "$m" ] && [ -n "$h" ]; then
        echo "PASS: the committed doc is parseable (label $v)"
    else
        echo "FAIL: the committed doc is not parseable by this checker"
        echo "    label='$v' net-dhcp='$m' dhcp-handler='$h'"
        failures=$((failures + 1))
    fi
fi

if [ "$failures" -eq 0 ]; then
    echo "all check-reference-digests tests passed"
    exit 0
fi
echo "$failures failed"
exit 1
