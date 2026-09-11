#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-manifest-delta-table.sh.
#
# BOTH DIRECTIONS, PER FIELD, AND THAT IS THE POINT. The defect this
# gate was written for was an UNDER-REPORT: the table listed four of six
# env names, so it claimed less had changed than had. A gate that only
# notices a token the manifest does not have would have passed it. So
# every field is driven twice — a token dropped from a cell and a token
# invented in one — on both the old and the new column.
#
# The refusal cases are driven separately, because a universal is
# satisfied by emptying its domain: no markers, no rows, no tag, no
# manifest at the tag. Each must exit 2 rather than 0, and 2 rather than
# 1: "I could not compare" is not "they disagree", and a reader fixing
# the second would edit the table.
#
# The PASS case is the control, and the (absent) cases are the other
# control: a field with no value in either manifest must agree, and must
# STOP agreeing the moment the manifest grows one.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-manifest-delta-table.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0

OLD_MANIFEST='{
    "network": { "type": "host" },
    "pidhost": true,
    "mounts": [
        { "source": "/var/run/docker.sock", "destination": "/run/docker.sock", "options": ["rbind", "ro"] },
        { "source": "/var/lib/net-dhcp", "destination": "/var/lib/net-dhcp" }
    ],
    "env": [
        { "name": "LOG_LEVEL", "value": "info" },
        { "name": "OUTAGE_TICK", "value": "30s" }
    ],
    "linux": { "capabilities": ["CAP_NET_ADMIN", "CAP_SYS_ADMIN"] }
}'

NEW_MANIFEST='{
    "network": { "type": "host" },
    "pidhost": true,
    "mounts": [
        { "source": "/var/run/docker.sock", "destination": "/run/docker.sock", "options": ["rbind", "ro"] },
        { "source": "/var/lib/net-dhcp", "destination": "/var/lib/net-dhcp" }
    ],
    "env": [
        { "name": "LOG_LEVEL", "value": "info" },
        { "name": "DOCKER_HOST", "value": "" }
    ],
    "linux": { "capabilities": ["CAP_NET_ADMIN", "CAP_NET_RAW", "CAP_SYS_ADMIN"] }
}'

# The correct table for the two manifests above. Every case below is
# this, with exactly one thing changed.
notes_with() { # <table body>
    cat <<MD
# Release notes

Prose above the block.

<!-- manifest-delta: begin baseline=v1.9.0 -->

| field | v1.9.0 | vNext | prompted |
| --- | --- | --- | --- |
$1
<!-- manifest-delta: end -->

Prose below the block.
MD
}

GOOD_ROWS='| `linux.capabilities` | `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` | `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_SYS_ADMIN` | **yes** |
| `network.type` | `host` | `host` | no change |
| `ipchost` | `false` | `false` | no change |
| `pidhost` | `true` | `true` | no change |
| `mounts` | `/var/run/docker.sock:rbind,ro`, `/var/lib/net-dhcp` | `/var/run/docker.sock:rbind,ro`, `/var/lib/net-dhcp` | no change |
| `propagatedmount` | `(absent)` | `(absent)` | no change |
| `linux.devices` | `(absent)` | `(absent)` | no change |
| `linux.allowalldevices` | `false` | `false` | no change |
| `env` | `LOG_LEVEL`, `OUTAGE_TICK` | `LOG_LEVEL`, `DOCKER_HOST` | no |'

# build <dir> — a repository whose v1.9.0 tag carries OLD_MANIFEST and
# whose working tree carries NEW_MANIFEST and a correct table.
build() {
    local d="$1"
    rm -rf "$d"
    mkdir -p "$d"
    git -C "$d" init -q
    git -C "$d" config user.email fixture@example.invalid
    git -C "$d" config user.name Fixture
    git -C "$d" config commit.gpgsign false
    # A developer's tag.gpgsign / tag.forceSignAnnotated turns the
    # lightweight tag below into an annotated one, which then demands a
    # message and aborts. MEASURED on this box: "fatal: no tag message?".
    git -C "$d" config tag.gpgsign false
    git -C "$d" config tag.forceSignAnnotated false
    printf '%s\n' "$OLD_MANIFEST" > "$d/config.json"
    git -C "$d" add config.json
    git -C "$d" commit -q -m 'v1.9.0 manifest'
    # Lightweight on purpose: an annotated tag would be signed under a
    # developer's tag.gpgsign and block on a hardware key.
    git -C "$d" tag v1.9.0
    printf '%s\n' "$NEW_MANIFEST" > "$d/config.json"
    notes_with "$GOOD_ROWS" > "$d/RELEASE_NOTES.md"
    git -C "$d" add -A
    git -C "$d" commit -q -m 'the alpha manifest'
}

run() { bash "$CHECK" "$1" >"$TMP/out" 2>&1; echo $?; }

expect() {
    local name="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "ok   $name (exit $got)"
    else
        echo "FAIL $name: exit $got, want $want"
        sed 's/^/     | /' "$TMP/out"
        fails=$((fails + 1))
    fi
}

D="$TMP/repo"

# --- the control ------------------------------------------------------
build "$D"
expect "the correct table passes" 0 "$(run "$D")"

# --- both directions, every field ------------------------------------
# Each case edits exactly one cell of the correct table. The `sed`
# targets the row by its field token, so a case cannot silently move to
# a different row when the table is reordered.
drop_from_cell() { # <field row match> <token>
    build "$D"
    python3 - "$D/RELEASE_NOTES.md" "$1" "$2" <<'PY'
import sys, re
path, field, token = sys.argv[1], sys.argv[2], sys.argv[3]
out = []
for line in open(path):
    if line.startswith('| `%s`' % field):
        # Drop the first occurrence of the backticked token.
        line = re.sub(r'`%s`(, )?' % re.escape(token), '', line, count=1)
    out.append(line)
open(path, 'w').write(''.join(out))
PY
}

add_to_cell() { # <field row match> <column 2 or 3> <token>
    build "$D"
    python3 - "$D/RELEASE_NOTES.md" "$1" "$2" "$3" <<'PY'
import sys
path, field, col, token = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
out = []
for line in open(path):
    if line.startswith('| `%s`' % field):
        cells = line.rstrip('\n').split('|')
        # split('|') on "| a | b | c | d |" yields ['', a, b, c, d, ''],
        # so column N is cells[N] with the leading empty at 0.
        cells[col] = cells[col].rstrip() + ', `%s` ' % token
        line = '|'.join(cells) + '\n'
    out.append(line)
open(path, 'w').write(''.join(out))
PY
}

# UNDER-REPORT — the shape that actually shipped.
for spec in \
    "linux.capabilities:CAP_SYS_ADMIN" \
    "mounts:/var/lib/net-dhcp" \
    "env:OUTAGE_TICK" ; do
    field="${spec%%:*}"; token="${spec#*:}"
    drop_from_cell "$field" "$token"
    expect "a token missing from the '$field' row (under-report)" 1 "$(run "$D")"
done

# OVER-REPORT — a privilege claimed that is not there. Driven on both
# columns: the old column is the one nothing in the tree could see.
add_to_cell "linux.capabilities" 2 "CAP_SYS_MODULE"
expect "an invented capability in the v1.9.0 column" 1 "$(run "$D")"
add_to_cell "linux.capabilities" 3 "CAP_SYS_MODULE"
expect "an invented capability in the current column" 1 "$(run "$D")"
add_to_cell "env" 2 "NEVER_EXISTED"
expect "an invented setting in the v1.9.0 column" 1 "$(run "$D")"
add_to_cell "mounts" 3 "/etc/shadow"
expect "an invented mount in the current column" 1 "$(run "$D")"

# The scalar fields, both directions.
build "$D"
sed -i 's/^| `pidhost` | `true` | `true` |/| `pidhost` | `false` | `true` |/' "$D/RELEASE_NOTES.md"
expect "a wrong pidhost value" 1 "$(run "$D")"
build "$D"
sed -i 's/^| `network.type` | `host` | `host` |/| `network.type` | `host` | `none` |/' "$D/RELEASE_NOTES.md"
expect "a wrong network.type" 1 "$(run "$D")"

# --- the (absent) control, and its other direction --------------------
build "$D"
expect "(absent) agrees when the manifest has no such field" 0 "$(run "$D")"

build "$D"
python3 - "$D/config.json" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
m['propagatedmount'] = '/var/lib/net-dhcp/propagated'
json.dump(m, open(sys.argv[1], 'w'), indent=4)
PY
expect "a manifest that GAINS propagatedmount no longer agrees with (absent)" 1 "$(run "$D")"

# --- the three the privilege review measured ---------------------------
# Each of these edits passed the WHOLE lane on 2026-09-05: the reviewer
# flipped both manifests read-write and nothing went red, because
# neither gate projected mount options, ipchost or allowAllDevices.
# They are driven here in the direction that used to pass, so a
# projection that narrows again fails this file rather than a release.

# 1. A mount flipped read-only -> read-write, table unchanged. Docker
#    does not re-prompt for it (computePrivileges takes the source path
#    alone), so this gate is the only thing that can notice.
build "$D"
python3 - "$D/config.json" <<'PY2'
import json, sys
p = sys.argv[1]
m = json.load(open(p))
m["mounts"][0]["options"] = ["rbind", "rw"]
json.dump(m, open(p, "w"), indent=4)
PY2
expect "a mount flipped read-only -> read-write" 1 "$(run "$D")"

# 2. ipchost: true, table unchanged.
build "$D"
python3 - "$D/config.json" <<'PY2'
import json, sys
p = sys.argv[1]
m = json.load(open(p))
m["ipchost"] = True
json.dump(m, open(p, "w"), indent=4)
PY2
expect "ipchost flipped true" 1 "$(run "$D")"

# 3. linux.allowAllDevices: true, table unchanged.
build "$D"
python3 - "$D/config.json" <<'PY2'
import json, sys
p = sys.argv[1]
m = json.load(open(p))
m["linux"]["allowAllDevices"] = True
json.dump(m, open(p, "w"), indent=4)
PY2
expect "linux.allowAllDevices flipped true" 1 "$(run "$D")"

# The preservation control: the same three edits, each with its row
# updated, agree. Without it the three cases above are satisfied by a
# gate that refuses every manifest carrying any of these keys.
build "$D"
python3 - "$D/config.json" <<'PY2'
import json, sys
p = sys.argv[1]
m = json.load(open(p))
m["mounts"][0]["options"] = ["rbind", "rw"]
m["ipchost"] = True
m["linux"]["allowAllDevices"] = True
json.dump(m, open(p, "w"), indent=4)
PY2
python3 - "$D/RELEASE_NOTES.md" <<'PY2'
import sys
p = sys.argv[1]
s = open(p).read()
# Only the CURRENT column moves: what the v1.9.0 tag carries is fixed.
s = s.replace("| `ipchost` | `false` | `false` | no change |",
              "| `ipchost` | `false` | `true` | **yes** |")
s = s.replace("| `linux.allowalldevices` | `false` | `false` | no change |",
              "| `linux.allowalldevices` | `false` | `true` | **yes** |")
s = s.replace("| `/var/run/docker.sock:rbind,ro`, `/var/lib/net-dhcp` | no change |",
              "| `/var/run/docker.sock:rbind,rw`, `/var/lib/net-dhcp` | no change |")
open(p, "w").write(s)
PY2
expect "the same three, each with its row, agree" 0 "$(run "$D")"

# --- structural cases -------------------------------------------------
build "$D"
sed -i '/^| `env` |/d' "$D/RELEASE_NOTES.md"
expect "a field with no row at all" 1 "$(run "$D")"

# The same, per field added by the privilege review. Deleting the ROW
# is what drives the FIELDS list itself: the per-row comparison above
# is row-driven, so a field dropped from FIELDS while its row survives
# is still compared, and only a missing row reaches the completeness
# loop. MEASURED 2026-09-05 as a surviving mutant: removing ipchost and
# linux.allowalldevices from FIELDS left every other case in this file
# green.
for f in ipchost linux.allowalldevices; do
    build "$D"
    sed -i "/^| \`$f\` |/d" "$D/RELEASE_NOTES.md"
    expect "no row for '$f'" 1 "$(run "$D")"
done

build "$D"
sed -i 's/^| `env` |/| `entrypoint` |/' "$D/RELEASE_NOTES.md"
expect "a row for a field this gate does not derive" 1 "$(run "$D")"

build "$D"
python3 - "$D/RELEASE_NOTES.md" <<'PY'
import sys
path = sys.argv[1]
out = []
for line in open(path):
    out.append(line)
    if line.startswith('| `pidhost`'):
        out.append(line)
open(path, 'w').write(''.join(out))
PY
expect "the same field twice" 1 "$(run "$D")"

build "$D"
sed -i 's/^| `network.type` | `host` | `host` |/| `network.type` | | `host` |/' "$D/RELEASE_NOTES.md"
expect "an empty cell rather than (absent)" 1 "$(run "$D")"

# --- refusals: 2, never 0 and never 1 ---------------------------------
build "$D"
sed -i '/manifest-delta: begin/d' "$D/RELEASE_NOTES.md"
expect "no begin marker" 2 "$(run "$D")"

build "$D"
sed -i '/manifest-delta: end/d' "$D/RELEASE_NOTES.md"
expect "no end marker" 2 "$(run "$D")"

build "$D"
sed -i 's/manifest-delta: begin baseline=v1.9.0/manifest-delta: begin/' "$D/RELEASE_NOTES.md"
expect "a marker naming no baseline tag" 2 "$(run "$D")"

build "$D"
sed -i 's/baseline=v1.9.0/baseline=v0.0.0-never-tagged/' "$D/RELEASE_NOTES.md"
expect "a baseline tag git cannot resolve (the shallow-clone shape)" 2 "$(run "$D")"

build "$D"
sed -i '/^| `/d' "$D/RELEASE_NOTES.md"
expect "a block with no rows" 2 "$(run "$D")"

build "$D"
rm -f "$D/RELEASE_NOTES.md"
expect "no release notes at all" 2 "$(run "$D")"

build "$D"
rm -f "$D/config.json"
expect "no current manifest" 2 "$(run "$D")"

# A repository that is not a git work tree cannot read the old manifest.
NOGIT="$TMP/nogit"
build "$D"
rm -rf "$NOGIT"; mkdir -p "$NOGIT"
cp "$D/RELEASE_NOTES.md" "$D/config.json" "$NOGIT/"
expect "not a git work tree" 2 "$(run "$NOGIT")"

# A tag that exists and carries no config.json — distinct from a tag
# that does not exist, and the message has to say which.
NOBLOB="$TMP/noblob"
rm -rf "$NOBLOB"; mkdir -p "$NOBLOB"
git -C "$NOBLOB" init -q
git -C "$NOBLOB" config user.email fixture@example.invalid
git -C "$NOBLOB" config user.name Fixture
git -C "$NOBLOB" config commit.gpgsign false
git -C "$NOBLOB" config tag.gpgsign false
git -C "$NOBLOB" config tag.forceSignAnnotated false
printf 'nothing here\n' > "$NOBLOB/README.md"
git -C "$NOBLOB" add README.md
git -C "$NOBLOB" commit -q -m 'no manifest'
git -C "$NOBLOB" tag v1.9.0
printf '%s\n' "$NEW_MANIFEST" > "$NOBLOB/config.json"
notes_with "$GOOD_ROWS" > "$NOBLOB/RELEASE_NOTES.md"
expect "a baseline tag with no config.json" 2 "$(run "$NOBLOB")"

# --- the repository itself --------------------------------------------
# The gate must pass on this tree. Without it every case above is
# satisfied by a gate that fails on everything.
REPO="$(cd "$(dirname "$0")/.." && pwd)"
expect "the repository's own table" 0 "$(run "$REPO")"

if [ "$fails" -ne 0 ]; then
    echo "$fails case(s) failed" >&2
    exit 1
fi
echo "all cases passed"
