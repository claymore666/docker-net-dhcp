#!/usr/bin/env bash
# Meta-test for check-manifest-parity.sh: the failure mode it guards is
# a privilege field silently differing between the two plugin manifests.
set -euo pipefail

CHECK="$(dirname "$0")/check-manifest-parity.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

base='{"interface":{"types":["docker.networkdriver/1.0"]},"network":{"type":"host"},"pidhost":true,"linux":{"capabilities":["CAP_NET_ADMIN","CAP_SYS_ADMIN","CAP_SYS_PTRACE"]}}'

# identical privilege fields -> pass (cover-side extras are ignored)
echo "$base" > "$TMP/a.json"
echo "$base" | jq '. + {env:[{name:"GOCOVERDIR",value:"/x"}]}' > "$TMP/b.json"
got=$(bash "$CHECK" "$TMP/a.json" "$TMP/b.json" >/dev/null 2>&1 && echo pass || echo fail)
check "identical privileges (with cover-only extras) pass" pass "$got"

# missing capability -> fail (the #317 regression shape)
echo "$base" | jq '.linux.capabilities -= ["CAP_SYS_PTRACE"]' > "$TMP/b.json"
got=$(bash "$CHECK" "$TMP/a.json" "$TMP/b.json" >/dev/null 2>&1 && echo pass || echo fail)
check "capability drift fails" fail "$got"

# pidhost drift -> fail
echo "$base" | jq '.pidhost = false' > "$TMP/b.json"
got=$(bash "$CHECK" "$TMP/a.json" "$TMP/b.json" >/dev/null 2>&1 && echo pass || echo fail)
check "pidhost drift fails" fail "$got"

# the real repo manifests must currently agree
got=$(bash "$CHECK" >/dev/null 2>&1 && echo pass || echo fail)
check "repo config.json and config-cover.json agree" pass "$got"

if [ "$fails" -ne 0 ]; then
    echo "check-manifest-parity tests FAILED"
    exit 1
fi
echo "All check-manifest-parity tests passed"
