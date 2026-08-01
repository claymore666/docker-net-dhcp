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

# The fixture carries STATE_DIR and its bind mount because the real
# manifests do, and because the check is deliberately strict about a
# manifest that declares neither: absence must not be read as "not
# applicable" and quietly skipped (#440).
base='{"interface":{"types":["docker.networkdriver/1.0"]},"network":{"type":"host"},"pidhost":true,"linux":{"capabilities":["CAP_NET_ADMIN","CAP_SYS_ADMIN","CAP_SYS_PTRACE"]},"env":[{"name":"STATE_DIR","value":"/var/lib/net-dhcp"}],"mounts":[{"source":"/var/lib/net-dhcp","destination":"/var/lib/net-dhcp","type":"bind","options":["rbind","rw"]}]}'

# identical privilege fields -> pass (cover-side extras are ignored)
echo "$base" > "$TMP/a.json"
echo "$base" | jq '.env += [{name:"GOCOVERDIR",value:"/x"}] | .mounts += [{source:"/var/lib/dh-cover",destination:"/coverage",type:"bind",options:["bind","rw"]}]' > "$TMP/b.json"
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

# The cover manifest losing the STATE_DIR mount -> fail. Without it a
# coverage run exercises a plugin whose state does NOT survive
# `plugin rm`, i.e. different persistence behaviour from what ships —
# the #317 shape applied to #440.
echo "$base" > "$TMP/a.json"
echo "$base" | jq '.mounts = []' > "$TMP/b.json"
got=$(bash "$CHECK" "$TMP/a.json" "$TMP/b.json" >/dev/null 2>&1 && echo pass || echo fail)
check "a manifest without the STATE_DIR mount fails" fail "$got"

# STATE_DIR repointed while the mount stays put -> fail. The mount then
# covers a path the plugin does not use: durability is gone, but the
# mount is still there to reassure whoever reads the manifest.
echo "$base" | jq '.env = [{name:"STATE_DIR",value:"/var/lib/elsewhere"}]' > "$TMP/a.json"
echo "$base" > "$TMP/b.json"
got=$(bash "$CHECK" "$TMP/a.json" "$TMP/b.json" >/dev/null 2>&1 && echo pass || echo fail)
check "STATE_DIR drifting away from its mount fails" fail "$got"

# No STATE_DIR at all -> fail, not skip. Absence of the setting must not
# read as "this manifest is exempt".
echo "$base" | jq 'del(.env)' > "$TMP/a.json"
echo "$base" > "$TMP/b.json"
got=$(bash "$CHECK" "$TMP/a.json" "$TMP/b.json" >/dev/null 2>&1 && echo pass || echo fail)
check "a manifest declaring no STATE_DIR fails rather than skipping" fail "$got"

if [ "$fails" -ne 0 ]; then
    echo "check-manifest-parity tests FAILED"
    exit 1
fi
echo "All check-manifest-parity tests passed"
