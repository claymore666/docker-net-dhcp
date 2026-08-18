#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-build-dir-refs.sh (#583), through the --root seam
# against synthetic trees. Red cases first: each way a test could name
# a build directory must be caught, and each way the gate could pass
# while seeing nothing (no test dir, accessor moved) must be exit 2, not
# 0. Then the real tree.
set -u

GATE="$(dirname "$0")/check-build-dir-refs.sh"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
failures=0

# tree NAME -> creates $TMP/NAME/test/integration/harness/build.go with the names
tree() {
    local d="$TMP/$1"
    mkdir -p "$d/test/integration/harness"
    printf 'package harness\nvar pluginBuildDirs = []string{"plugin", "plugin-cover"}\n' > "$d/test/integration/harness/build.go"
    echo "$d"
}

check() {
    local name="$1" want_exit="$2" root="$3" want_grep="$4"
    bash "$GATE" --root "$root" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && { [ -z "$want_grep" ] || grep -q -- "$want_grep" "$TMP/out"; }; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want_exit)"; sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

d=$(tree clean)
printf 'package integration\nfunc f() { _ = harness.BuiltPluginDir }\n' > "$d/test/integration/x_test.go"
check "clean tree passes" 0 "$d" "no test outside"

d=$(tree cover)
printf 'package integration\nvar dirs = []string{"plugin", "plugin-cover"}\n' > "$d/test/integration/x_test.go"
check "a test naming plugin-cover is red" 1 "$d" "x_test.go"

d=$(tree rootfs)
printf 'package integration\nconst p = "plugin/rootfs"\n' > "$d/test/integration/x_test.go"
check "a test naming plugin/rootfs is red" 1 "$d" "x_test.go"

d=$(tree join)
printf 'package integration\nvar p = filepath.Join(root, "plugin")\n' > "$d/test/integration/x_test.go"
check "a test assembling the plugin dir from parts is red" 1 "$d" "x_test.go"

d=$(tree comment)
printf 'package integration\n// copy from plugin-cover/rootfs\n' > "$d/test/integration/x_test.go"
check "a comment naming a build dir is red too" 1 "$d" "x_test.go"

d=$(tree helper)
printf 'package harness\nvar p = "plugin-cover/rootfs"\n' > "$d/test/integration/harness/other.go"
check "a harness file other than the accessor is red" 1 "$d" "other.go"

d=$(tree moved)
printf 'package harness\n' > "$d/test/integration/harness/build.go"
check "accessor without the names means the gate cannot see" 2 "$d" "watching the wrong file"

check "no test directory means the gate cannot see" 2 "$TMP/nowhere" "not a directory"

check "the real tree passes" 0 "$REPO" "no test outside"

if ! grep -q "check-build-dir-refs.sh" "$REPO/.github/workflows/test.yaml"; then
    echo "FAIL: gate is not wired into .github/workflows/test.yaml"; failures=$((failures + 1))
else
    echo "PASS: gate is wired into test.yaml"
fi

if [ "$failures" -ne 0 ]; then echo "$failures failure(s)"; exit 1; fi
echo "all passed"
