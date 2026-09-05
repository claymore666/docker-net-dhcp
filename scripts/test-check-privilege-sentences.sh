#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-privilege-sentences.sh.
#
# The gate's whole value is that it is blind in NEITHER direction, so
# both directions are driven here against the same fixture, one variable
# at a time:
#
#   a grant in config.json with no row in SECURITY.md
#   a row in SECURITY.md for a grant config.json no longer asks for
#
# A gate written with only the first case passes a document that lists
# every privilege the plugin ever held, which is the failure that
# actually happens -- privileges are removed far more often than the
# prose describing them is.
#
# The refusal cases are driven separately, because a universal is
# satisfied by emptying its domain: no block markers, no rows, no
# manifest. Each must exit 2 rather than 0.
#
# The PASS case is the control. Without it every case below is satisfied
# by a gate that fails on everything.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-privilege-sentences.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0

# --- fixture ----------------------------------------------------------
# A miniature tree with two grants, two cited consumer files, and the
# markers. Two grants and not one: with a single row, "compared the sets"
# and "found the one row" are indistinguishable.
build() {
    local d="$1"
    rm -rf "$d"
    mkdir -p "$d/pkg/plugin"
    printf 'package plugin\n' > "$d/pkg/plugin/alpha.go"
    printf 'package plugin\n' > "$d/pkg/plugin/beta.go"
    cat > "$d/config.json" <<'JSON'
{
    "network": { "type": "host" },
    "pidhost": true,
    "mounts": [
        { "source": "/var/run/docker.sock", "destination": "/run/docker.sock", "type": "bind", "options": ["bind"] }
    ],
    "linux": { "capabilities": ["CAP_NET_ADMIN"] }
}
JSON
    cat > "$d/SECURITY.md" <<'MD'
# Security Policy

Prose above the block.

<!-- privilege-sentences: begin -->

| grant | what it is for | consumer |
|---|---|---|
| `network:host` | The plugin reads the host's own links to resolve a parent interface. | `pkg/plugin/alpha.go` |
| `pidhost` | The plugin enters a container's mount namespace through its PID to write resolv.conf. | `pkg/plugin/beta.go` |
| `mount:/var/run/docker.sock` | The Docker API, read-only, for network and container inspection. | `pkg/plugin/alpha.go` |
| `CAP_NET_ADMIN` | Addresses, routes and links inside the container's network namespace. | `pkg/plugin/beta.go` |

<!-- privilege-sentences: end -->

Prose below the block.
MD
}

run() { bash "$CHECK" "$1" >"$TMP/out" 2>&1; echo $?; }

expect() {
    local name="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "ok   $name (exit $got)"
    else
        echo "FAIL $name: exit $got, want $want"
        sed 's/^/       | /' "$TMP/out"
        fails=$((fails + 1))
    fi
}

expect_out() {
    local name="$1" want="$2"
    if grep -qF -- "$want" "$TMP/out"; then
        echo "ok   $name"
    else
        echo "FAIL $name: output does not mention $(printf '%q' "$want")"
        sed 's/^/       | /' "$TMP/out"
        fails=$((fails + 1))
    fi
}

R="$TMP/repo"

# --- the control ------------------------------------------------------
build "$R"
expect "an agreeing manifest and document pass" 0 "$(run "$R")"

# --- direction 1: a grant with no sentence ----------------------------
build "$R"
# CAP_SYS_ADMIN added to the manifest and to nothing else.
sed -i 's/\["CAP_NET_ADMIN"\]/["CAP_NET_ADMIN", "CAP_SYS_ADMIN"]/' "$R/config.json"
expect "a grant with no row fails" 1 "$(run "$R")"
expect_out "  and names the grant" "CAP_SYS_ADMIN"

# The same direction through a non-capability grant, because the grant
# set has four different shapes and a gate can easily read only one.
build "$R"
python3 - "$R/config.json" <<'PY'
import json, sys
p = sys.argv[1]
m = json.load(open(p))
m["mounts"].append({"source": "/var/lib/net-dhcp", "destination": "/var/lib/net-dhcp",
                    "type": "bind", "options": ["rbind", "rw"]})
json.dump(m, open(p, "w"), indent=4)
PY
expect "a MOUNT with no row fails" 1 "$(run "$R")"
expect_out "  and names the mount" "mount:/var/lib/net-dhcp"

# --- direction 2: a sentence with no grant ----------------------------
build "$R"
# The capability is dropped from the manifest; its row stays.
sed -i 's/"capabilities": \["CAP_NET_ADMIN"\]/"capabilities": []/' "$R/config.json"
expect "a row for a dropped grant fails" 1 "$(run "$R")"
expect_out "  and names the stale row" "no longer requests"

# pidhost turned off is the same direction with a different shape: the
# key is still present in the manifest and simply false.
build "$R"
sed -i 's/"pidhost": true/"pidhost": false/' "$R/config.json"
expect "a row for a disabled pidhost fails" 1 "$(run "$R")"
expect_out "  and names pidhost" "'pidhost'"

# --- substance --------------------------------------------------------
build "$R"
sed -i 's#| `pidhost` | The plugin enters a container.s mount namespace through its PID to write resolv.conf. |#| `pidhost` | yes. |#' "$R/SECURITY.md"
expect "a row with no real sentence fails" 1 "$(run "$R")"

build "$R"
sed -i 's#| `pidhost` \(.*\)| `pkg/plugin/beta.go` |#| `pidhost` \1| `pkg/plugin/nowhere.go` |#' "$R/SECURITY.md"
expect "a row citing a file that is not in the tree fails" 1 "$(run "$R")"
expect_out "  and names the missing file" "pkg/plugin/nowhere.go"

build "$R"
sed -i 's#| `pidhost` \(.*\)| `pkg/plugin/beta.go` |#| `pidhost` \1| none |#' "$R/SECURITY.md"
expect "a row citing no consumer at all fails" 1 "$(run "$R")"

# --- refusals ---------------------------------------------------------
build "$R"
sed -i '/privilege-sentences: begin/d' "$R/SECURITY.md"
expect "a document with no block REFUSES" 2 "$(run "$R")"

build "$R"
# Markers kept, every row removed: the emptied domain.
python3 - "$R/SECURITY.md" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
s = re.sub(r"(?s)(begin -->).*?(<!-- privilege-sentences: end)", r"\1\n\n\2", s)
open(p, "w").write(s)
PY
expect "a block with no rows REFUSES rather than agreeing with nothing" 2 "$(run "$R")"

build "$R"
rm "$R/config.json"
expect "a missing manifest REFUSES" 2 "$(run "$R")"

build "$R"
rm "$R/SECURITY.md"
expect "a missing document REFUSES" 2 "$(run "$R")"

# --- the real tree ----------------------------------------------------
# The gate must pass the repository it ships in. Without this the whole
# self-test could be green against a fixture the production tree does
# not resemble.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
expect "the repository itself passes" 0 "$(run "$REPO_ROOT")"

if [ "$fails" -ne 0 ]; then
    echo "FAILED: $fails case(s)"
    exit 1
fi
echo "PASS  check-privilege-sentences.sh self-test"
