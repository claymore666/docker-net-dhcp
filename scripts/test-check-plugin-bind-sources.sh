#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-plugin-bind-sources.sh.
#
# Runs the real shipped gate against mutated copies of the repo's own
# Makefile, manifests and workflows. The gate is copied, never reimplemented,
# so this cannot pass over a rewritten check.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
GATE=scripts/check-plugin-bind-sources.sh

pass=0; fail=0

# Build a throwaway repo root holding the gate plus the files it reads.
mkws() {
    local ws
    ws=$(mktemp -d)
    mkdir -p "$ws/scripts" "$ws/.github/workflows"
    cp "$REPO/$GATE" "$ws/scripts/"
    cp "$REPO/Makefile" "$REPO/config.json" "$REPO/config-cover.json" "$ws/"
    # BOTH extensions, or this suite reproduces the very narrowing it is
    # here to catch: a workspace built from `*.yml` alone cannot tell a
    # `*.yml`-only gate from a correct one.
    shopt -s nullglob
    local wfs=("$REPO"/.github/workflows/*.yml "$REPO"/.github/workflows/*.yaml)
    shopt -u nullglob
    cp "${wfs[@]}" "$ws/.github/workflows/"
    printf '%s' "$ws"
}

check() { # name expected_rc ws [expected_substring]
    local name=$1 want=$2 ws=$3 needle=${4:-} out got
    out=$(cd "$ws" && bash "$GATE" 2>&1) && got=0 || got=$?
    if [ "$got" -ne "$want" ]; then
        echo "FAIL: $name — expected exit $want, got $got"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    elif [ -n "$needle" ] && ! printf '%s\n' "$out" | grep -F "$needle" >/dev/null; then
        echo "FAIL: $name — exit $got as expected but output never mentions '$needle'"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    else
        echo "ok: $name"
        pass=$((pass + 1))
    fi
    rm -rf "$ws"
}

# 1. The repo as it stands is clean. If this fails, everything below is noise.
check "clean repo passes" 0 "$(mkws)" "bind source"

# 2. The exact defect that broke the coverage lane: the manifest-derived line
#    replaced by the hardcoded one it used to be.
ws=$(mkws)
python3 - "$ws" <<'PY'
import sys, re
p = sys.argv[1] + "/.github/workflows/coverage.yml"
s = open(p).read()
s = re.sub(r"          jq -r '\.mounts.*?xargs -r mkdir -p\n",
           "          mkdir -p /var/lib/net-dhcp\n", s, flags=re.S)
open(p, "w").write(s)
PY
check "hardcoded mkdir in coverage.yml is caught" 1 "$ws" "/var/lib/dh-capture"

# 3. The drift itself: a source added to a manifest that a workflow names its
#    sources by hand. This is the shape that shipped broken (#662 -> #666).
ws=$(mkws)
jq '.mounts += [{"name":"newstate","description":"x","destination":"/var/lib/newstate","source":"/var/lib/newstate","type":"bind","options":["rbind","rw"]}]' \
    "$ws/config.json" > "$ws/config.json.t" && mv "$ws/config.json.t" "$ws/config.json"
check "new bind source not created by a literal-mkdir workflow is caught" 1 "$ws" "/var/lib/newstate"

# 4. The counterpart, and the reason the fix is a jq line and not a longer
#    list: the derived form absorbs a new source with no workflow edit.
ws=$(mkws)
jq '.mounts += [{"name":"newstate","description":"x","destination":"/var/lib/newstate","source":"/var/lib/newstate","type":"bind","options":["rbind","rw"]}]' \
    "$ws/config-cover.json" > "$ws/config-cover.json.t" && mv "$ws/config-cover.json.t" "$ws/config-cover.json"
check "manifest-derived step absorbs a new source" 0 "$ws"

# 5. A socket source must never be mkdir'd — mkdir -p would replace it with a
#    directory and the plugin would lose the docker API.
ws=$(mkws)
jq '.mounts += [{"name":"sock2","description":"x","destination":"/var/run/other.sock","source":"/var/run/other.sock","type":"bind","options":["rbind","rw"]}]' \
    "$ws/config.json" > "$ws/config.json.t" && mv "$ws/config.json.t" "$ws/config.json"
check "a /var/run socket source is not demanded" 0 "$ws"

# 6. The mapping this gate stands on: if the Makefile stops populating the dir
#    a workflow installs, say so rather than checking nothing.
ws=$(mkws)
sed -i 's/^\tcp config-cover\.json \$@\/config\.json/\tcp cover-manifest.json $@\/config.json/' "$ws/Makefile"
check "a renamed manifest breaks the mapping loudly" 1 "$ws" "plugin-cover"

# 7. No silent clean over a repo with nothing to inspect.
ws=$(mkws)
rm -f "$ws"/.github/workflows/*.yml "$ws"/.github/workflows/*.yaml
: > "$ws/.github/workflows/empty.yml"
check "no plugin installs at all is a failure, not a pass" 1 "$ws" "never inspected"

# 8. Prose about the command is not the command (release.yml has both).
ws=$(mkws)
cat > "$ws/.github/workflows/prose.yml" <<'YML'
name: prose
on: workflow_dispatch
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      # This step re-ran `docker plugin create`, which re-tars the rootfs.
      - name: talk about it
        run: echo hi
YML
check "a comment mentioning the command is not an install" 0 "$ws"

# 9. The metacharacter false pass (#710). The bind path used to be
#    interpolated into an ERE, so '.' matched any character: a source of
#    /var/lib/net-dhcp.d was reported as created by a line that creates
#    /var/lib/net-dhcpXd, which is a different directory. The source is
#    genuinely absent here, and this fixture PASSED the old gate.
#
#    ORTHOGONALITY: run `git stash`-free — the previous version of this
#    check is reproduced inline below and asserted to accept the same
#    fixture, so this case proves the fix rather than restating it.
ws=$(mkws)
jq '.mounts += [{"name":"dotted","description":"x","destination":"/var/lib/net-dhcp.d","source":"/var/lib/net-dhcp.d","type":"bind","options":["rbind","rw"]}]' \
    "$ws/config.json" > "$ws/config.json.t" && mv "$ws/config.json.t" "$ws/config.json"
# The decoy: a literal mkdir of a DIFFERENT directory that the old regex
# matched because '.' is a metacharacter.
sed -i 's|^\( *\)mkdir -p /var/lib/net-dhcp$|\1mkdir -p /var/lib/net-dhcpXd /var/lib/net-dhcp|' \
    "$ws/.github/workflows/integration.yml"

# Prove the old check accepted it, so the case below is not a tautology.
if printf '%s\n' "          mkdir -p /var/lib/net-dhcpXd /var/lib/net-dhcp" \
    | grep -E "mkdir[[:space:]]+(-[a-z]+[[:space:]]+)*/var/lib/net-dhcp.d(\$|[[:space:]])" >/dev/null; then
    echo "ok: the old regex accepted the decoy (orthogonality confirmed)"
    pass=$((pass + 1))
else
    echo "FAIL: the old regex did NOT accept the decoy — this fixture does not"
    echo "      reproduce #710, so the case below proves nothing."
    fail=$((fail + 1))
fi

check "a dotted bind source is not matched by a lookalike mkdir" 1 "$ws" "/var/lib/net-dhcp.d"

# 10. THE DIRECTORY SCAN IS PART OF THE CHECK (#832). Every case above
#     plants its defect in a `.yml` file, so a gate that read only `.yml`
#     passed all of them — the domain could shrink to half the directory
#     and nothing here would go red. GitHub Actions honours both
#     extensions and `.github/workflows/` holds one `.yaml` today, so the
#     narrowing is not hypothetical.
#
#     ORTHOGONALITY, same as case 9: the narrowed gate is reproduced and
#     asserted to ACCEPT this fixture, so the case proves the widening
#     rather than restating it.
ws=$(mkws)
cat > "$ws/.github/workflows/planted.yaml" <<'YML'
name: planted
on: workflow_dispatch
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - name: install the plugin from a dir the Makefile never populates
        run: docker plugin create dnd:planted ./plugin-nowhere
YML

# The mutant: the scan narrowed back to a single extension.
narrowed="$ws/scripts/narrowed.sh"
sed -e 's|^WF_FILES=(.*)$|WF_FILES=("$WORKFLOW_DIR"/*.yml)|' \
    "$ws/$GATE" > "$narrowed"
if (cd "$ws" && bash scripts/narrowed.sh >/dev/null 2>&1); then
    echo "ok: a *.yml-only scan accepts the planted .yaml install (orthogonality confirmed)"
    pass=$((pass + 1))
else
    echo "FAIL: the *.yml-only scan did NOT accept the fixture — the case below"
    echo "      would pass for some other reason and proves nothing about the glob."
    fail=$((fail + 1))
fi
rm -f "$narrowed"

check "an install in a .yaml workflow is inspected too" 1 "$ws" "plugin-nowhere"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
