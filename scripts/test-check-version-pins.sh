#!/usr/bin/env bash
# Table-driven tests for check-version-pins.sh and bump-version.sh
# (#251). Synthesizes README/docs fixtures with image-ref pins and bare
# prose markers, asserting: agreeing pins pass; a disagreeing pin fails
# and is named; no pins is an explicit failure; bare "vX.Y.Z" prose is
# never treated as a pin; and bump-version rewrites only the pins (not
# the prose) and leaves the tree internally consistent.
set -u

DIR="$(cd "$(dirname "$0")" && pwd)"
CHECK="$DIR/check-version-pins.sh"
BUMP="$DIR/bump-version.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

IMAGE="ghcr.io/claymore666/docker-net-dhcp"

# A fixture repo root with a README and a docs/ dir.
seed() {
    rm -rf "$TMP/repo"
    mkdir -p "$TMP/repo/docs"
    cat > "$TMP/repo/README.md" <<EOF
Install:

    docker plugin install ${IMAGE}:v1.1.1

Every release (v1.0.0 onward) is signed and attested.
EOF
    cat > "$TMP/repo/docs/reference.md" <<EOF
Create a network:

    docker network create -d ${IMAGE}:v1.1.1 mynet

The Docker-visible v6 address is renewed as of v1.2.0 (#152).

Compose:

    driver: ${IMAGE}:v1.1.1
EOF
}

failures=0
run_check() {
    local name="$1" want_exit="$2"
    ( cd "$TMP/repo" && bash "$CHECK" ) > "$TMP/out" 2>&1
    local got_exit=$?
    if [ "$got_exit" -eq "$want_exit" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit, got $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# 1. All pins agree -> green.
seed
run_check "agreeing pins pass" 0

# 2. One pin disagrees -> red, and the offending version is named.
seed
sed -i "s#${IMAGE}:v1.1.1#${IMAGE}:v1.0.0#" "$TMP/repo/docs/reference.md"  # first hit only
run_check "disagreeing pins fail" 1
grep -q 'v1.0.0' "$TMP/out" || { echo "FAIL: disagreement not named"; failures=$((failures + 1)); }
grep -q 'v1.1.1' "$TMP/out" || { echo "FAIL: other version not named"; failures=$((failures + 1)); }

# 3. No image pins at all -> explicit failure (not silent green).
seed
rm -f "$TMP/repo/README.md"
printf 'no pins here, just prose mentioning v1.2.0\n' > "$TMP/repo/docs/reference.md"
run_check "no pins fails" 1

# 4. Bare "vX.Y.Z" prose is not a pin: a doc with one real pin plus
#    several bare-version mentions (different versions) still passes.
seed
cat >> "$TMP/repo/docs/reference.md" <<'EOF'

Notes: works since v1.0.0; v6 renewal is v1.2.0+; signed since v1.1.0.
EOF
run_check "bare-version prose ignored" 0

# 5. bump-version rewrites only the pins and leaves the tree consistent.
seed
( cd "$TMP/repo" && bash "$BUMP" v1.2.0 ) > "$TMP/out" 2>&1 \
    || { echo "FAIL: bump-version exited nonzero"; sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); }
# All three pins now at v1.2.0.
if [ "$(grep -hoE "${IMAGE}:v[0-9.]+" "$TMP/repo/README.md" "$TMP/repo/docs/reference.md" | sort -u | paste -sd' ' -)" = "${IMAGE}:v1.2.0" ]; then
    echo "PASS: bump moved every pin to v1.2.0"
else
    echo "FAIL: bump left disagreeing pins"; failures=$((failures + 1))
fi
# The historical prose markers are untouched.
grep -q 'v1.0.0 onward' "$TMP/repo/README.md" || { echo "FAIL: bump rewrote historical prose"; failures=$((failures + 1)); }
grep -q 'renewed as of v1.2.0' "$TMP/repo/docs/reference.md" || { echo "FAIL: feature marker disturbed"; failures=$((failures + 1)); }
# And the consistency gate is green afterwards.
( cd "$TMP/repo" && bash "$CHECK" ) > /dev/null 2>&1 \
    && echo "PASS: gate green after bump" \
    || { echo "FAIL: gate red after bump"; failures=$((failures + 1)); }

# 6. bump-version rejects a malformed version argument.
seed
( cd "$TMP/repo" && bash "$BUMP" 1.2.0 ) > /dev/null 2>&1
if [ $? -eq 2 ]; then echo "PASS: bump rejects bad version"; else echo "FAIL: bump accepted bad version"; failures=$((failures + 1)); fi

# 7. check bad usage (no files anywhere) -> exit 2.
mkdir -p "$TMP/empty"
( cd "$TMP/empty" && bash "$CHECK" ) > /dev/null 2>&1
if [ $? -eq 2 ]; then echo "PASS: check bad usage exits 2"; else echo "FAIL: check bad usage"; failures=$((failures + 1)); fi

if [ "$failures" -ne 0 ]; then
    echo "$failures test(s) failed"
    exit 1
fi
echo "all check-version-pins tests passed"
