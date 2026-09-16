#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Does the attach budget hold on a small, loaded host? (#969, under #403)
#
# THE QUESTION
#
# `AWAIT_TIMEOUT` caps the whole of the plugin's Join. When it runs out
# and the container is still running, the plugin counts
# `join_start_failures` and the container keeps an address that nothing
# renews: it works until the lease expires, and then the address is
# reassignable while the container still believes it holds it. CI has
# reached that state (#401). Whether a small production-shaped host can
# is unmeasured, and that is what this script measures.
#
# WHAT IT BUILDS
#
# A throwaway VM on the developer's own box: 2 vCPU, 2 GB, one disk,
# plain `qemu-system-x86_64 -enable-kvm` as an ordinary user, user-mode
# networking with ssh forwarded to a loopback port. No tap, no host
# bridge, nothing on the LAN, no libvirt and no root on the host. The VM
# is seeded with cloud-init through a FAT volume labelled `cidata`,
# built with mtools because this class of box has no genisoimage.
#
# Inside it: the Docker engine, this branch's plugin created from a
# rootfs built on the HOST (a 2 GB VM is no place for a Go build), a
# Linux bridge with dnsmasq bound to it on a fixture subnet (the shape
# the integration fixtures use; a bridge Docker owns is refused by the
# plugin), and bursts of containers started at once on a bridge-mode
# DHCP network while stress-ng holds the box down.
#
# Every container start also pays the plugin's RFC 5227 conflict check,
# 4 to 7 s inside CreateEndpoint with the default `conflict_check=wait`.
# That is a different budget from the Join window the counters measure,
# and it is why a burst's wall time is printed beside the counters rather
# than read as attach time.
#
# WHY THE READINGS ARE SHAPED THE WAY THEY ARE
#
# Deltas, per burst, never cumulative: the counters are plugin-wide
# totals since plugin start, so a row that printed them raw would show
# every earlier burst again and each level would look worse than the
# last for arithmetic reasons. `join_attach_ms_max` is the exception and
# is not a counter at all: it is a running maximum, so the column prints
# it only when it ROSE during that burst and `<= N` when it did not.
#
# A burst that attached nothing is refused, not reported as a row of
# zeroes, because a rig that quietly stopped exercising the plugin
# produces exactly the table an operator wants to see. The same applies
# to the load itself: the VM's load average is read at the end of every
# burst and a loaded level whose average never left the floor is a
# broken rig, not a quiet one.
#
# The counters say what the plugin believed. The dnsmasq lease file says
# what the DHCP server did, and that is the evidence the question turns
# on: a container the counters say has no client should, after the lease
# time, still hold its address with no lease on the server. That step
# runs only when a burst reported a start failure, and says it has no
# subject otherwise; it never prints a pass.
#
# THE VM IS STOPPED BY THE PID QEMU WROTE, never by name, and only after
# this run's disk path is confirmed in that process's argv.
#
# Usage:
#   scripts/vm-load-test.sh --self-test    logic only: no KVM, no network
#   scripts/vm-load-test.sh                build the VM and run the matrix
#   scripts/vm-load-test.sh --stop         stop the VM this work dir owns
#
# Env:
#   VMLT_WORK       work directory (default $TMPDIR/vm-load-test). The
#                   disk, the seed, the console log, the ssh key and the
#                   pidfile all live here and nowhere else.
#   VMLT_LEVELS     load levels to run (default "idle light medium heavy")
#   VMLT_BURSTS     burst sizes (default "10 20 50")
#   VMLT_REPEATS    repeats per burst size (default 3)
#   VMLT_SSH_PORT   loopback port forwarded to the VM's sshd (default 2229)
#   VMLT_IMAGE_URL / VMLT_IMAGE_SHA512   the cloud image and its checksum
#   VMLT_KEEP=1     leave the VM running at the end
#   VMLT_LIB=1      source this file for its functions and run nothing
#
# Exit: 0 the matrix ran, 1 a refusal or a failed step, 2 cannot run.

set -uo pipefail

VMLT_WORK="${VMLT_WORK:-${TMPDIR:-/tmp}/vm-load-test}"
VMLT_LEVELS="${VMLT_LEVELS:-idle light medium heavy}"
VMLT_BURSTS="${VMLT_BURSTS:-10 20 50}"
VMLT_REPEATS="${VMLT_REPEATS:-3}"
VMLT_SSH_PORT="${VMLT_SSH_PORT:-2229}"
VMLT_KEEP="${VMLT_KEEP:-0}"
VMLT_IMAGE_URL="${VMLT_IMAGE_URL:-https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2}"
# Refreshed from SHA512SUMS in the same directory as the image. `latest`
# moves, so a mismatch is expected the day Debian publishes a new build:
# the refusal names this variable and the check is never skipped.
VMLT_IMAGE_SHA512="${VMLT_IMAGE_SHA512:-95e110dfcdbd0ed8a82a75ed9579802f9950cabf51a810dcc6388e81bc778188713878b9f28d583a0ea602fbf48b35996ae9ad37f584166d8fbd6489df248f53}"

VMLT_PLUGIN_TAG="${VMLT_PLUGIN_TAG:-net-dhcp-load:local}"
VMLT_BRIDGE="${VMLT_BRIDGE:-dhload0}"
VMLT_SUBNET="${VMLT_SUBNET:-192.168.101.0/24}"
VMLT_GATEWAY="${VMLT_GATEWAY:-192.168.101.1}"
VMLT_POOL_START="${VMLT_POOL_START:-192.168.101.20}"
VMLT_POOL_END="${VMLT_POOL_END:-192.168.101.250}"
VMLT_LEASE_TIME="${VMLT_LEASE_TIME:-2m}"
VMLT_LEASE_SECONDS="${VMLT_LEASE_SECONDS:-120}"
# The plugin's attach window is AWAIT_TIMEOUT (10s default) plus a 60s
# daemon-busy grace; a burst is polled up to this long for its attaches
# to land in a counter after the last start returned.
VMLT_SETTLE_SECONDS="${VMLT_SETTLE_SECONDS:-80}"

# The seven readings #969 asks for, in the order the table prints them.
VMLT_FIELDS="join_attach_under_1s join_attach_1s_to_budget join_attach_slow join_attach_completed join_attach_ms_max join_start_failures join_aborted_container_gone"

# level|cpu workers|hdd workers|extra stress-ng arguments
VMLT_LEVEL_SPEC="\
idle|0|0|
light|1|1|--cpu-load 40 --hdd-bytes 64M
medium|2|1|--cpu-load 80 --hdd-bytes 128M
heavy|4|2|--cpu-load 100 --hdd-bytes 128M --vm 1 --vm-bytes 128M"

say()  { printf '%s\n' "$*"; }
note() { printf '\033[33mNOTE\033[0m  %s\n' "$*" >&2; }
die()  { printf '\033[31mREFUSED\033[0m  %s\n' "$*" >&2; exit 1; }
cannot() { printf '\033[31mCANNOT RUN\033[0m  %s\n' "$*" >&2; exit 2; }

# --- logic the self-test drives ----------------------------------------

# vmlt_lease_owner <lease file text> <ip> <container id>
# The dnsmasq lease file is "expiry mac ip hostname clientid" per line.
# The entry carrying an address is the container's own only when its
# hostname is the container id the persistent client sent at Join. The
# same address can sit in the file under a removed container's entry
# that has not expired (tombstone inheritance hands the next container
# the same address), or under the one-shot client's name when the
# persistent client never started; an address-only join counts both as
# this container's lease. Prints own, other:<hostname>, or none. Defined
# here, driven by the self-test, and carried into the rig verbatim.
vmlt_lease_owner() {
    printf '%s\n' "$1" | awk -v ip="$2" -v id="$3" '
        $3 == ip { if ($4 == id) { print "own"; found = 1; exit }; owner = $4; found = 2 }
        END { if (found == 2) print "other:" owner; else if (!found) print "none" }'
}

# vmlt_count_held <evidence rows "id ip lease=own|other|none entry=...">
# Running containers that hold an address the server does not lease to
# them: an address with no entry, or with an entry under another name.
# One count for both readings (right after the burst, and after the
# lease was waited out), so neither can drift back to "no entry" alone.
vmlt_count_held() {
    printf '%s\n' "$1" | awk '$2 != "none" && $2 != "" && $3 != "lease=own"' | wc -l
}

# vmlt_counters <json file>
# Prints "<field> <value>" for every field in VMLT_FIELDS. A field that
# is absent is a refusal and never a zero: a health document that lost a
# counter, or a read that returned an error page, would otherwise be
# reported as a burst in which nothing happened.
vmlt_counters() {
    local f=$1
    [ -r "$f" ] || { echo "vmlt_counters: cannot read $f" >&2; return 1; }
    VMLT_FIELDS="$VMLT_FIELDS" python3 - "$f" <<'PY'
import json, os, sys
try:
    with open(sys.argv[1]) as fh:
        doc = json.load(fh)
except Exception as exc:
    print("vmlt_counters: %s is not JSON: %s" % (sys.argv[1], exc), file=sys.stderr)
    sys.exit(1)
if not isinstance(doc, dict):
    print("vmlt_counters: %s is not a JSON object" % sys.argv[1], file=sys.stderr)
    sys.exit(1)
bad = 0
for name in os.environ["VMLT_FIELDS"].split():
    if name not in doc:
        print("vmlt_counters: %s carries no %s" % (sys.argv[1], name), file=sys.stderr)
        bad = 1
        continue
    v = doc[name]
    if isinstance(v, bool) or not isinstance(v, int):
        print("vmlt_counters: %s is %r, not an integer" % (name, v), file=sys.stderr)
        bad = 1
        continue
    print("%s %d" % (name, v))
iid = doc.get("instance_id")
if not isinstance(iid, str) or not iid:
    print("vmlt_counters: %s carries no instance_id; two reads are only "
          "comparable as a delta when they are the same plugin process"
          % sys.argv[1], file=sys.stderr)
    bad = 1
else:
    print("instance_id %s" % iid)
sys.exit(bad)
PY
}

# vmlt_delta <before json> <after json>
# Prints the burst's own numbers, space separated:
#   under1s budget slow completed maxms maxrose failures aborted
# maxrose is yes when the running maximum rose during the burst, which
# is the only case in which that maximum belongs to this burst.
# A counter that went BACKWARDS means the plugin restarted mid-burst;
# the deltas then describe two different processes, so it is refused.
vmlt_delta() {
    local before=$1 after=$2 b a
    b=$(vmlt_counters "$before") || return 1
    a=$(vmlt_counters "$after") || return 1
    B="$b" A="$a" VMLT_FIELDS="$VMLT_FIELDS" python3 - <<'PY'
import os, sys
def parse(text):
    out = {}
    for line in text.splitlines():
        if line.strip():
            k, v = line.split()
            out[k] = v if k == "instance_id" else int(v)
    return out
b = parse(os.environ["B"])
a = parse(os.environ["A"])
if b["instance_id"] != a["instance_id"]:
    print("vmlt_delta: the plugin process changed inside the burst "
          "(%s -> %s); the counters restarted at zero and no delta over "
          "them is a measurement" % (b["instance_id"], a["instance_id"]),
          file=sys.stderr)
    sys.exit(1)
order = os.environ["VMLT_FIELDS"].split()
maxname = "join_attach_ms_max"
counters = [n for n in order if n != maxname]
for n in counters:
    if a[n] < b[n]:
        print("vmlt_delta: %s fell from %d to %d; the plugin restarted "
              "inside the burst and the two snapshots are not one process"
              % (n, b[n], a[n]), file=sys.stderr)
        sys.exit(1)
if a[maxname] < b[maxname]:
    print("vmlt_delta: %s fell from %d to %d; the plugin restarted inside "
          "the burst" % (maxname, b[maxname], a[maxname]), file=sys.stderr)
    sys.exit(1)
d = {n: a[n] - b[n] for n in counters}
rose = "yes" if a[maxname] > b[maxname] else "no"
print("%d %d %d %d %d %s %d %d" % (
    d["join_attach_under_1s"], d["join_attach_1s_to_budget"],
    d["join_attach_slow"], d["join_attach_completed"],
    a[maxname], rose,
    d["join_start_failures"], d["join_aborted_container_gone"]))
PY
}

# vmlt_check_attached <containers running> <completed delta> <failures delta>
# A burst whose attaches are unaccounted for did not exercise the plugin,
# and a table of zeroes from such a burst reads as a clean result.
vmlt_check_attached() {
    local running=$1 completed=$2 failures=$3
    if [ "$running" -gt 0 ] && [ $((completed + failures)) -eq 0 ]; then
        echo "vmlt_check_attached: $running containers reached running and the" \
             "plugin counted no attach at all; this burst measured nothing" >&2
        return 1
    fi
    return 0
}

# vmlt_check_lease_key <completed delta> <leases matched> <endpoints not bound>
# The lease file is joined to the burst's containers by the hostname the
# persistent client sends at Join, the short container id. A key that
# matches nothing prints "0 leased" for every row, which reads as the
# server having dropped every lease. The key is proven the first time a
# lease carries one of the ids (0). It is blind when attaches completed,
# every endpoint the plugin lists is bound, and no lease carries an id:
# a bound client sent that REQUEST and the server wrote the name (1). A
# burst with clients still acquiring may match fewer than it completed,
# and a burst that completed nothing has nothing to say; neither proves
# nor refutes the key (2).
vmlt_check_lease_key() {
    local completed=$1 matched=$2 unbound=$3
    [ "$matched" -gt 0 ] && return 0
    if [ "$completed" -gt 0 ] && [ "$unbound" -eq 0 ]; then
        echo "vmlt_check_lease_key: $completed attaches completed, every endpoint is bound," \
             "and no lease carries a container id of this burst; the lease join key does not" \
             "match what the client sends, and the outside evidence would be blind" >&2
        return 1
    fi
    return 2
}

# vmlt_settled <before json> <after json> <starts>
# The attach runs in a goroutine the plugin spawns at Join, and Join
# returns before it finishes: `docker start` coming back says nothing
# about whether the attach has landed in a counter yet. A burst is
# settled when every start that succeeded has reached one of the five
# outcomes, and only then is the after snapshot a reading of the burst.
vmlt_settled() {
    local before=$1 after=$2 starts=$3
    STARTS="$starts" python3 - "$before" "$after" <<'PY'
import json, os, sys
outcomes = ["join_attach_completed", "join_start_failures",
            "join_aborted_container_gone", "join_aborted_no_container",
            "join_aborted_endpoint_left"]
docs = []
for path in sys.argv[1:]:
    try:
        with open(path) as fh:
            doc = json.load(fh)
    except Exception as exc:
        print("vmlt_settled: %s is not JSON: %s" % (path, exc), file=sys.stderr)
        sys.exit(2)
    for k in outcomes:
        if not isinstance(doc.get(k), int) or isinstance(doc.get(k), bool):
            print("vmlt_settled: %s carries no integer %s" % (path, k), file=sys.stderr)
            sys.exit(2)
    docs.append(doc)
b, a = docs
landed = sum(a[k] - b[k] for k in outcomes)
sys.exit(0 if landed >= int(os.environ["STARTS"]) else 1)
PY
}

# vmlt_stress_args <level>
# The stress-ng command line for a level: its worker counts and its
# extra arguments. The worker counts live in their own columns of the
# spec so the load floor can be derived from them, and this is the one
# place they turn back into arguments; a level whose workers were never
# passed to stress-ng runs no load at all and only the floor would say so.
# vmlt_cpu_share <cpustat before> <cpustat after>
# The VM's steal and iowait over the burst as a share of its own wall
# time, from the counters the rig read on either side. Steal is what the
# host's other tenants took; it says whether a row measured the level's
# stress or the host. A window in which the clock did not move, a counter
# that went backwards or a missing one is refused, not read as 0 %.
vmlt_cpu_share() {
    local b=$1 a=$2 k v v0 v1
    local -A d
    for k in steal iowait total; do
        v0=$(vmlt_field "$k" "$b"); v1=$(vmlt_field "$k" "$a")
        for v in "$v0" "$v1"; do
            case "$v" in ""|*[!0-9]*) echo "vmlt_cpu_share: no $k counter on both sides" >&2; return 2 ;; esac
        done
        [ "$v1" -ge "$v0" ] || { echo "vmlt_cpu_share: $k went backwards ($v0 -> $v1)" >&2; return 1; }
        d[$k]=$((v1 - v0))
    done
    [ "${d[total]}" -gt 0 ] || { echo "vmlt_cpu_share: the clock did not move" >&2; return 1; }
    LC_ALL=C awk -v s="${d[steal]}" -v w="${d[iowait]}" -v t="${d[total]}" \
        'BEGIN { printf("steal=%.1f iowait=%.1f\n", 100 * s / t, 100 * w / t) }'
}

# vmlt_unbound <health json>
# How many endpoints the plugin lists in a lease_state other than bound
# after the burst settled: a client still acquiring, or one that gave up.
# The health view cannot say whether the container's link still carries
# an address; burst() reads that from inside the container when this is
# not zero. A view without an endpoints list is refused.
vmlt_unbound() {
    python3 - "$1" <<'PY'
import json, sys
try:
    with open(sys.argv[1]) as fh:
        doc = json.load(fh)
except Exception as exc:
    print("vmlt_unbound: %s is not JSON: %s" % (sys.argv[1], exc), file=sys.stderr)
    sys.exit(2)
eps = doc.get("endpoints")
if not isinstance(eps, list):
    print("vmlt_unbound: %s carries no endpoints list" % sys.argv[1], file=sys.stderr)
    sys.exit(2)
print(sum(1 for e in eps if e.get("lease_state") != "bound"))
PY
}

vmlt_stress_args() {
    local level=$1 cpu hdd extra out=""
    cpu=$(vmlt_level_field "$level" 1) || return 1
    hdd=$(vmlt_level_field "$level" 2) || return 1
    extra=$(vmlt_level_field "$level" 3) || return 1
    [ "$cpu" -gt 0 ] && out="--cpu $cpu"
    [ "$hdd" -gt 0 ] && out="$out --hdd $hdd"
    out="$out $extra"
    out=${out## }; out=${out%% }
    printf '%s\n' "$out"
}

# vmlt_level_field <level> <1=cpu workers|2=hdd workers|3=extra args>
vmlt_level_field() {
    local level=$1 idx=$2 line
    line=$(printf '%s\n' "$VMLT_LEVEL_SPEC" | awk -F'|' -v l="$level" '$1==l {print; exit}')
    [ -n "$line" ] || { echo "vmlt_level_field: no such level: $level" >&2; return 1; }
    printf '%s\n' "$line" | cut -d'|' -f$((idx + 1))
}

# vmlt_load_floor <level>
# In hundredths, so the comparison is integer arithmetic. Derived from
# the level's own worker count, not from a number that happened to match
# one run: half a runnable process per stress-ng worker is the
# weakest claim that still separates "the load is on" from "it is not".
vmlt_load_floor() {
    local level=$1 cpu hdd
    cpu=$(vmlt_level_field "$level" 1) || return 1
    hdd=$(vmlt_level_field "$level" 2) || return 1
    echo $(((cpu + hdd) * 50))
}

# vmlt_check_load <level> <load average, as printed by /proc/loadavg>
vmlt_check_load() {
    local level=$1 avg=$2 floor got
    floor=$(vmlt_load_floor "$level") || return 1
    [ "$floor" -eq 0 ] && return 0
    got=$(printf '%s' "$avg" | awk -F. '{printf "%d", $1*100 + substr($2 "00", 1, 2)}')
    if [ -z "$got" ] || [ "$got" -lt "$floor" ]; then
        echo "vmlt_check_load: level $level ended at load average $avg, below the" \
             "floor $(printf '%s.%02d' $((floor / 100)) $((floor % 100)));" \
             "the background load was not running and this row is an idle row" >&2
        return 1
    fi
    return 0
}

# vmlt_mark_refused <refused bursts as l-<level>-n<n>-r<rep>, space separated>
# Filters section rows on stdin. A row of a refused burst is marked in
# every section the burst reached, so the refusal the table decided
# follows the burst to the orphan it produced; an operator cannot take
# the orphan sentence from a burst the table called idle. Rows are keyed
# on their first cell (the burst prefix) or, for the evidence table, on
# level, burst and run.
vmlt_mark_refused() {
    awk -F'|' -v refused=" $1 " '
    {
        key = $2; gsub(/ /, "", key)
        if (key !~ /^l-/) {
            l = $2; n = $3; r = $4; gsub(/ /, "", l); gsub(/ /, "", n); gsub(/ /, "", r)
            key = "l-" l "-n" n "-r" r
        }
        if (index(refused, " " key " ") > 0) sub(/^\| /, "| refused: ")
        print
    }'
}

# vmlt_table <tsv file>
# Columns in: level burst repeat started under1s budget slow completed
#             maxms maxrose failures aborted loadavg
# An empty table is refused. A renderer that prints a header and no rows
# reports "nothing failed" having measured nothing.
vmlt_table() {
    local f=$1
    [ -r "$f" ] || { echo "vmlt_table: cannot read $f" >&2; return 1; }
    if [ ! -s "$f" ]; then
        echo "vmlt_table: $f has no rows; a header with no rows is not a result" >&2
        return 1
    fi
    echo '| load | burst | run | started | under 1s | 1s to budget | slow | completed | max attach ms | start failures | container gone | load avg | steal % | iowait % |'
    echo '| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |'
    # The load floor is checked here, beside the row it refuses, so a
    # loaded row whose load average never left the floor is rendered
    # marked refused and the table says so in its own text, where the
    # markdown an operator pastes carries it; the run cannot print such
    # a row and forget to refuse it, because the same function does both.
    local -a col
    local nr=0 bad=0 refused=0 refusedlist="" maxcell mark
    VMLT_REFUSED=""
    while IFS=$'\t' read -r -a col; do
        nr=$((nr + 1))
        if [ "${#col[@]}" -ne 15 ]; then
            echo "vmlt_table: row $nr has ${#col[@]} fields, expected 15" >&2; bad=1; continue
        fi
        if vmlt_check_load "${col[0]}" "${col[12]}"; then
            mark=${col[0]}
        else
            refused=$((refused + 1))
            refusedlist="${refusedlist:+$refusedlist, }${col[0]} n${col[1]} r${col[2]}"
            VMLT_REFUSED="$VMLT_REFUSED l-${col[0]}-n${col[1]}-r${col[2]}"
            mark="refused: ${col[0]}"
        fi
        if [ "${col[9]}" = yes ]; then maxcell=${col[8]}; else maxcell="<= ${col[8]}"; fi
        printf '| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n' \
            "$mark" "${col[1]}" "${col[2]}" "${col[3]}" "${col[4]}" "${col[5]}" "${col[6]}" "${col[7]}" "$maxcell" \
            "${col[10]}" "${col[11]}" "${col[12]}" "${col[13]}" "${col[14]}"
    done < "$f"
    if [ "$refused" -gt 0 ]; then
        echo
        echo "$refused rows ran at a load average below their level's floor and are marked refused above and in every section below; those bursts were idle rows under another name and are not results: $refusedlist."
        echo "vmlt_table: $refused rows ran at a load average below their level's floor; they are marked refused in the table" >&2
        return 1
    fi
    [ "$bad" -eq 0 ]
}

# vmlt_seed_files <dir> <public key file> <instance id>
# The NoCloud pair. Root gets the key because every step this script runs
# in the VM is a root step, and a sudo hop would only add a place for the
# transport to fail.
vmlt_seed_files() {
    local dir=$1 keyfile=$2 iid=$3 key
    [ -r "$keyfile" ] || { echo "vmlt_seed_files: cannot read $keyfile" >&2; return 1; }
    key=$(cat "$keyfile")
    [ -n "$key" ] || { echo "vmlt_seed_files: $keyfile is empty" >&2; return 1; }
    mkdir -p "$dir" || return 1
    {
        echo '#cloud-config'
        echo 'disable_root: false'
        echo 'ssh_pwauth: false'
        echo 'users:'
        echo '  - name: root'
        echo '    ssh_authorized_keys:'
        echo "      - $key"
        echo 'growpart:'
        echo '  mode: auto'
        echo 'resize_rootfs: true'
        echo 'package_update: false'
    } > "$dir/user-data" || return 1
    {
        echo "instance-id: $iid"
        echo "local-hostname: $iid"
    } > "$dir/meta-data" || return 1
    return 0
}

# vmlt_seed_image <dir with the NoCloud pair> <image path>
# A FAT volume labelled cidata, which is the shape cloud-init's NoCloud
# datasource looks for when there is no ISO writer on the box.
vmlt_seed_image() {
    local dir=$1 img=$2
    command -v mformat >/dev/null 2>&1 || { echo "vmlt_seed_image: mformat is not installed" >&2; return 1; }
    command -v mcopy   >/dev/null 2>&1 || { echo "vmlt_seed_image: mcopy is not installed" >&2; return 1; }
    [ -r "$dir/user-data" ] && [ -r "$dir/meta-data" ] || {
        echo "vmlt_seed_image: $dir carries no user-data/meta-data pair" >&2; return 1; }
    rm -f "$img" || return 1
    mformat -C -f 1440 -v CIDATA -i "$img" :: || return 1
    mcopy -o -i "$img" "$dir/user-data" "$dir/meta-data" :: || return 1
    return 0
}

# --- the VM -------------------------------------------------------------

VMLT_BASE="$VMLT_WORK/base/debian-13-genericcloud-amd64.qcow2"
VMLT_DISK="$VMLT_WORK/disk.qcow2"
VMLT_SEED="$VMLT_WORK/seed.img"
VMLT_PIDFILE="$VMLT_WORK/qemu.pid"
VMLT_CONSOLE="$VMLT_WORK/console.log"
VMLT_KEY="$VMLT_WORK/id_ed25519"

ssh_opts() {
    printf '%s\n' -i "$VMLT_KEY" -p "$VMLT_SSH_PORT" \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes \
        -o ServerAliveInterval=15 -o ServerAliveCountMax=8
}

vm_ssh() {
    local opts=(); mapfile -t opts < <(ssh_opts)
    ssh "${opts[@]}" root@127.0.0.1 "$@"
}

vm_scp() {
    local src=$1 dst=$2
    scp -i "$VMLT_KEY" -P "$VMLT_SSH_PORT" \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes \
        "$src" "root@127.0.0.1:$dst"
}

host_preflight() {
    local missing=()
    local t
    for t in qemu-system-x86_64 qemu-img mformat mcopy ssh scp ssh-keygen curl sha512sum python3 awk docker git tar; do
        command -v "$t" >/dev/null 2>&1 || missing+=("$t")
    done
    [ "${#missing[@]}" -eq 0 ] || cannot "these are not installed: ${missing[*]}"
    [ -r /dev/kvm ] && [ -w /dev/kvm ] || cannot "/dev/kvm is not readable and writable by this user; this script runs qemu as an ordinary user and will not fall back to emulation, which would measure the emulator"
    docker info >/dev/null 2>&1 || cannot "the host Docker daemon is not reachable; the plugin rootfs is built here and not inside the 2 GB VM"
    if command -v ss >/dev/null 2>&1; then
        local listening
        listening=$(ss -ltn "sport = :$VMLT_SSH_PORT" 2>/dev/null | tail -n +2)
        [ -z "$listening" ] || cannot "port $VMLT_SSH_PORT is already listening; set VMLT_SSH_PORT"
    fi
}

fetch_image() {
    mkdir -p "$(dirname "$VMLT_BASE")" || cannot "cannot create $(dirname "$VMLT_BASE")"
    if [ ! -s "$VMLT_BASE" ]; then
        say "downloading the cloud image once into $VMLT_BASE"
        curl -fsSL -o "$VMLT_BASE.part" "$VMLT_IMAGE_URL" \
            || cannot "could not download $VMLT_IMAGE_URL"
        mv "$VMLT_BASE.part" "$VMLT_BASE" || cannot "could not place the image"
    fi
    local got
    got=$(sha512sum "$VMLT_BASE" | awk '{print $1}')
    if [ "$got" != "$VMLT_IMAGE_SHA512" ]; then
        die "$VMLT_BASE hashes to $got, not the expected $VMLT_IMAGE_SHA512. The \`latest\` directory moves: read SHA512SUMS beside the image and pass VMLT_IMAGE_SHA512, or point VMLT_IMAGE_URL at the build you mean."
    fi
    say "cloud image checksum verified"
}

vm_start() {
    mkdir -p "$VMLT_WORK" || cannot "cannot create $VMLT_WORK"
    [ -f "$VMLT_KEY" ] || ssh-keygen -q -t ed25519 -N '' -C vm-load-test -f "$VMLT_KEY" \
        || cannot "could not generate the ssh key"

    vmlt_seed_files "$VMLT_WORK/seed" "$VMLT_KEY.pub" vm-load-test \
        || cannot "could not write the NoCloud pair"
    vmlt_seed_image "$VMLT_WORK/seed" "$VMLT_SEED" \
        || cannot "could not build the cidata volume"

    rm -f "$VMLT_DISK"
    qemu-img create -q -f qcow2 -F qcow2 -b "$VMLT_BASE" "$VMLT_DISK" 24G \
        || cannot "could not create the overlay disk"

    rm -f "$VMLT_PIDFILE" "$VMLT_CONSOLE"
    say "starting the VM: 2 vCPU, 2 GB, ssh on 127.0.0.1:$VMLT_SSH_PORT"
    qemu-system-x86_64 \
        -machine q35,accel=kvm -enable-kvm -cpu host -smp 2 -m 2048 \
        -device virtio-rng-pci \
        -drive "file=$VMLT_DISK,if=virtio,format=qcow2,cache=writeback" \
        -drive "file=$VMLT_SEED,if=virtio,format=raw,readonly=on" \
        -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$VMLT_SSH_PORT-:22" \
        -device virtio-net-pci,netdev=n0 \
        -display none -serial "file:$VMLT_CONSOLE" \
        -pidfile "$VMLT_PIDFILE" -daemonize \
        || cannot "qemu would not start; the console log is $VMLT_CONSOLE"

    local waited=0
    while [ ! -s "$VMLT_PIDFILE" ] && [ "$waited" -lt 50 ]; do sleep 0.2; waited=$((waited + 1)); done
    [ -s "$VMLT_PIDFILE" ] || cannot "qemu wrote no pidfile; nothing can be stopped safely"
    say "qemu pid $(cat "$VMLT_PIDFILE")"
}

# vm_stop — by the pid qemu wrote, and only after this run's disk path is
# confirmed in that process's argv. A pid file outlives the process it
# named, and the number is reused.
vm_stop() {
    [ -s "$VMLT_PIDFILE" ] || { note "no pidfile at $VMLT_PIDFILE; nothing to stop"; return 0; }
    local pid
    pid=$(tr -cd '0-9' < "$VMLT_PIDFILE")
    [ -n "$pid" ] || { note "$VMLT_PIDFILE holds no pid"; return 0; }
    if [ ! -r "/proc/$pid/cmdline" ]; then
        note "pid $pid is gone; the VM is already stopped"
        rm -f "$VMLT_PIDFILE"
        return 0
    fi
    local argv
    argv=$(tr '\0' ' ' < "/proc/$pid/cmdline")
    case "$argv" in
        *"$VMLT_DISK"*) ;;
        *) note "pid $pid does not name $VMLT_DISK in its argv; refusing to signal it"
           return 1 ;;
    esac
    say "stopping the VM: pid $pid, argv confirmed"
    kill "$pid" 2>/dev/null
    local waited=0
    while [ -d "/proc/$pid" ] && [ "$waited" -lt 100 ]; do sleep 0.2; waited=$((waited + 1)); done
    if [ -d "/proc/$pid" ]; then
        note "pid $pid did not exit on SIGTERM; sending SIGKILL"
        kill -9 "$pid" 2>/dev/null
        sleep 1
    fi
    rm -f "$VMLT_PIDFILE"
    return 0
}

vm_wait_ssh() {
    local budget=${1:-600} waited=0
    say "waiting for ssh (cloud-init runs first)"
    while [ "$waited" -lt "$budget" ]; do
        if vm_ssh true >/dev/null 2>&1; then
            say "ssh is up after ${waited}s"
            return 0
        fi
        sleep 5; waited=$((waited + 5))
    done
    note "the console log is $VMLT_CONSOLE"
    return 1
}

# --- the rig, inside the VM ---------------------------------------------

# The rig script is written once into the VM and called per step, so the
# burst storm is fired from inside the guest. Firing it over N ssh
# connections would measure ssh.
vmlt_rig_script() {
    cat <<'RIG'
#!/bin/bash
# In-VM rig for the attach-budget load test. Written by scripts/vm-load-test.sh.
set -uo pipefail
# shellcheck source=/dev/null
. /root/vmlt-env

DHCPNET=dhcp-load
LEASES=/var/lib/vmlt/dnsmasq.leases
RIG
    # The lease join is one function: the host defines it, the self-test
    # drives it, and the rig runs that same text inside the VM.
    declare -f vmlt_lease_owner
    cat <<'RIG'
DNSMASQ_LOG=/var/log/vmlt-dnsmasq.log
DNSMASQ_PID=/run/vmlt-dnsmasq.pid
SOCK=/var/run/docker.sock
IMAGE=busybox:stable

plugin_sock() {
    local id
    id=$(docker plugin inspect -f '{{.Id}}' "$VMLT_PLUGIN_TAG" 2>/dev/null) || return 1
    [ -n "$id" ] || return 1
    printf '/run/docker/plugins/%s/net-dhcp.sock\n' "$id"
}

cmd_health() {
    local s
    s=$(plugin_sock) || { echo "the plugin is not installed" >&2; return 1; }
    curl -sf --unix-socket "$s" http://localhost/Plugin.Health
}

# The fixture shape: a plain Linux bridge with the gateway address,
# FORWARD accepted both ways (br_netfilter would otherwise drop DHCP
# under Docker's default-deny policy), and dnsmasq bound to the bridge.
cmd_up() {
    ip link add "$VMLT_BRIDGE" type bridge || return 1
    printf '0' > "/sys/class/net/$VMLT_BRIDGE/bridge/forward_delay" 2>/dev/null || true
    ip addr add "$VMLT_GATEWAY/${VMLT_SUBNET#*/}" dev "$VMLT_BRIDGE" || return 1
    ip link set "$VMLT_BRIDGE" up || return 1
    iptables  -I FORWARD -i "$VMLT_BRIDGE" -j ACCEPT || return 1
    iptables  -I FORWARD -o "$VMLT_BRIDGE" -j ACCEPT || return 1
    ip6tables -I FORWARD -i "$VMLT_BRIDGE" -j ACCEPT || return 1
    ip6tables -I FORWARD -o "$VMLT_BRIDGE" -j ACCEPT || return 1
    mkdir -p "$(dirname "$LEASES")" && rm -f "$LEASES" || return 1
    setsid dnsmasq --no-daemon --conf-file=/dev/null --port=0 \
        --interface="$VMLT_BRIDGE" --bind-interfaces --except-interface=lo \
        --dhcp-range="$VMLT_POOL_START,$VMLT_POOL_END,$VMLT_LEASE_TIME" \
        --dhcp-leasefile="$LEASES" --dhcp-no-override --dhcp-broadcast \
        --log-dhcp --log-facility="$DNSMASQ_LOG" > /var/log/vmlt-dnsmasq.out 2>&1 &
    echo $! > "$DNSMASQ_PID"
    local i
    for i in $(seq 1 40); do
        ss -lun 'sport = :67' 2>/dev/null | grep ':67' >/dev/null && break
        sleep 0.25
    done
    ss -lun 'sport = :67' 2>/dev/null | grep ':67' >/dev/null || { echo "dnsmasq did not bind udp/67" >&2; return 1; }
    docker pull -q "$IMAGE" >/dev/null || return 1
    docker network create -d "$VMLT_PLUGIN_TAG" --ipam-driver null \
        -o bridge="$VMLT_BRIDGE" "$DHCPNET" >/dev/null || return 1
    echo "rig up"
}

cmd_down() {
    docker network rm "$DHCPNET" >/dev/null 2>&1
    local pid argv
    if [ -s "$DNSMASQ_PID" ]; then
        pid=$(tr -cd '0-9' < "$DNSMASQ_PID")
        if [ -n "$pid" ] && [ -r "/proc/$pid/cmdline" ]; then
            argv=$(tr '\0' ' ' < "/proc/$pid/cmdline")
            case "$argv" in
                *dnsmasq*"$LEASES"*) kill "$pid" 2>/dev/null ;;
                *) echo "pid $pid is not this rig's dnsmasq; leaving it alone" >&2 ;;
            esac
        fi
        rm -f "$DNSMASQ_PID"
    fi
    iptables  -D FORWARD -i "$VMLT_BRIDGE" -j ACCEPT 2>/dev/null
    iptables  -D FORWARD -o "$VMLT_BRIDGE" -j ACCEPT 2>/dev/null
    ip6tables -D FORWARD -i "$VMLT_BRIDGE" -j ACCEPT 2>/dev/null
    ip6tables -D FORWARD -o "$VMLT_BRIDGE" -j ACCEPT 2>/dev/null
    ip link del "$VMLT_BRIDGE" 2>/dev/null
    echo "rig down"
}

cmd_create() {
    local n=$1 prefix=$2 i
    for i in $(seq 1 "$n"); do
        docker create --name "$prefix-$i" --network "$DHCPNET" "$IMAGE" sleep 900 >/dev/null &
        [ $((i % 8)) -eq 0 ] && wait
    done
    wait
    echo "created=$(docker ps -a --filter "name=^${prefix}-" --format '{{.ID}}' | wc -l)"
}

# The burst. One curl per container, all in flight at once, through the
# engine's socket, not the CLI: fifty docker CLIs in a 2 GB VM
# measure the CLI's resident set.
cmd_start() {
    local n=$1 prefix=$2 i codes=/tmp/vmlt-codes t0
    : > "$codes"
    t0=$(date +%s)
    for i in $(seq 1 "$n"); do
        (
            c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 900 \
                --unix-socket "$SOCK" -X POST \
                "http://localhost/containers/$prefix-$i/start")
            printf '%s\n' "$c" >> "$codes"
        ) &
    done
    wait
    local ok bad
    ok=$(grep -c '^20' "$codes"); [ -n "$ok" ] || ok=0
    bad=$(grep -vc '^20' "$codes"); [ -n "$bad" ] || bad=0
    echo "start_ok=$ok start_other=$bad burst_s=$(( $(date +%s) - t0 ))"
}

cmd_status() {
    local prefix=$1 running load
    running=$(docker ps --filter "name=^${prefix}-" --filter status=running --format '{{.ID}}' | wc -l)
    load=$(cut -d' ' -f1 /proc/loadavg)
    echo "running=$running load=$load"
}

# Outside evidence: the DHCP server's own lease file, joined to the
# burst's containers by the hostname the plugin sends as option 12,
# which for a Docker container is its short id.
cmd_leases() {
    local prefix=$1 ids leasefile matched=0 total id
    ids=$(docker ps --filter "name=^${prefix}-" --filter status=running --format '{{.ID}}')
    leasefile=$(cat "$LEASES" 2>/dev/null)
    for id in $ids; do
        case "$leasefile" in
            *" $id "*) matched=$((matched + 1)) ;;
        esac
    done
    total=$(printf '%s' "$leasefile" | grep -c . ); [ -n "$total" ] || total=0
    echo "leases_matched=$matched leases_total=$total"
}

cmd_freepool() {
    local total
    total=$(grep -c . "$LEASES" 2>/dev/null); [ -n "$total" ] || total=0
    echo "leases_total=$total free=$((VMLT_POOL_SIZE - total))"
}

# Every running container of the burst with the address it holds and
# whether the DHCP server still has a lease for it. Read after the lease
# time has passed, a configured address with no lease is a container
# nothing is renewing.
# The VM's own clock: steal is time the host took from the VM, iowait is
# time its one disk kept it waiting. Read on either side of a burst.
cmd_cpustat() {
    local _ u n s i w q sq st
    read -r _ u n s i w q sq st _ < /proc/stat
    echo "steal=$st iowait=$w total=$((u + n + s + i + w + q + sq + st))"
}

cmd_evidence() {
    local prefix=$1 id ip leasefile owner
    leasefile=$(cat "$LEASES" 2>/dev/null)
    for id in $(docker ps --filter "name=^${prefix}-" --filter status=running --format '{{.ID}}'); do
        ip=$(docker exec "$id" ip -4 -o addr show scope global 2>/dev/null | awk 'NR == 1 {print $4}')
        ip=${ip%%/*}
        owner=$(vmlt_lease_owner "$leasefile" "${ip:-none}" "$id")
        case "$owner" in
            own)     echo "$id ${ip:-none} lease=own entry=$id" ;;
            other:*) echo "$id ${ip:-none} lease=other entry=${owner#other:}" ;;
            *)       echo "$id ${ip:-none} lease=none entry=-" ;;
        esac
    done
}

cmd_rm() {
    local prefix=$1 ids
    ids=$(docker ps -a --filter "name=^${prefix}-" --format '{{.ID}}')
    [ -n "$ids" ] && printf '%s\n' "$ids" | xargs -r -P 8 -n 4 docker rm -f >/dev/null 2>&1
    echo "removed"
}

cmd_stress() {
    local args=$1
    if [ -z "$args" ]; then
        echo "stress=none"
        return 0
    fi
    # shellcheck disable=SC2086
    setsid stress-ng $args --timeout 0 > /var/log/vmlt-stress.log 2>&1 &
    echo $! > /run/vmlt-stress.pid
    sleep 20
    echo "stress=$args load=$(cut -d' ' -f1 /proc/loadavg)"
}

# By the pid this script recorded, after its argv is confirmed, never by
# name: the VM also runs the engine and the plugin.
cmd_stressoff() {
    [ -s /run/vmlt-stress.pid ] || { echo "stress=none"; return 0; }
    local pid argv
    pid=$(tr -cd '0-9' < /run/vmlt-stress.pid)
    if [ -n "$pid" ] && [ -r "/proc/$pid/cmdline" ]; then
        argv=$(tr '\0' ' ' < "/proc/$pid/cmdline")
        case "$argv" in
            *stress-ng*) kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null ;;
            *) echo "pid $pid is not stress-ng; leaving it alone" >&2 ;;
        esac
    fi
    rm -f /run/vmlt-stress.pid
    sleep 5
    echo "stress=off load=$(cut -d' ' -f1 /proc/loadavg)"
}

cmd_identity() {
    echo "engine=$(docker version -f '{{.Server.Version}}' 2>/dev/null)"
    echo "kernel=$(uname -r)"
    cmd_health | python3 -c 'import json,sys; d=json.load(sys.stdin); print("plugin_version=%s" % d.get("version")); print("plugin_commit=%s" % d.get("commit")); print("library=%s" % d.get("library"))'
}

case "${1:-}" in
    up)        cmd_up ;;
    down)      cmd_down ;;
    health)    cmd_health ;;
    identity)  cmd_identity ;;
    create)    cmd_create "$2" "$3" ;;
    start)     cmd_start "$2" "$3" ;;
    status)    cmd_status "$2" ;;
    cpustat)   cmd_cpustat ;;
    leases)    cmd_leases "$2" ;;
    freepool)  cmd_freepool ;;
    evidence)  cmd_evidence "$2" ;;
    rm)        cmd_rm "$2" ;;
    stress)    cmd_stress "${2:-}" ;;
    stressoff) cmd_stressoff ;;
    *) echo "vmlt-rig: unknown subcommand: ${1:-}" >&2; exit 2 ;;
esac
RIG
}

# --- host side: build, provision, measure -------------------------------

VMLT_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VMLT_LOG_LEVEL="${VMLT_LOG_LEVEL:-info}"

# vmlt_tree_identity <repo>
# `make plugin` builds the working tree and stamps it with HEAD, so a
# tree with changes under the build's inputs would be measured and then
# reported under a commit it is not. Prints HEAD when the inputs (what
# the Dockerfile copies, go.* as the glob it copies, the Makefile that
# stamps it, the plugin config)
# are at HEAD, tracked and untracked alike; refuses otherwise and names
# the paths.
vmlt_tree_identity() {
    local repo=$1 head dirty
    head=$(git -C "$repo" rev-parse HEAD) || return 1
    dirty=$(git -C "$repo" status --porcelain --untracked-files=all -- cmd pkg 'go.*' Dockerfile Makefile config.json) || return 1
    if [ -n "$dirty" ]; then
        printf 'vmlt_tree_identity: the build inputs differ from %s:\n%s\n' "${head:0:12}" "$dirty" >&2
        return 1
    fi
    printf '%s\n' "$head"
}

build_plugin() {
    local head
    head=$(vmlt_tree_identity "$VMLT_REPO") || cannot "$VMLT_REPO is not the commit the report would name; commit or stash the changes listed above"
    VMLT_HEAD=$head
    say "building the plugin rootfs on the host at ${head:0:12}"
    make -C "$VMLT_REPO" plugin > "$VMLT_WORK/build.log" 2>&1 \
        || cannot "the plugin build failed; see $VMLT_WORK/build.log"
    tar -C "$VMLT_REPO/plugin" -cf "$VMLT_WORK/plugin.tar" . \
        || cannot "could not pack the plugin rootfs"
}

provision() {
    say "installing the engine, stress-ng and the plugin in the VM"
    vm_ssh 'bash -s' <<'GUEST' || cannot "provisioning the VM failed"
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
    docker.io docker-cli dnsmasq-base stress-ng curl ca-certificates python3 iproute2 iptables
systemctl enable --now docker
mkdir -p /var/lib/net-dhcp /root/plugin
GUEST

    vm_scp "$VMLT_WORK/plugin.tar" /root/plugin.tar || cannot "could not copy the plugin rootfs"
    vm_ssh "set -e
        tar -C /root/plugin -xf /root/plugin.tar
        docker plugin create '$VMLT_PLUGIN_TAG' /root/plugin
        docker plugin set '$VMLT_PLUGIN_TAG' LOG_LEVEL='$VMLT_LOG_LEVEL'
        docker plugin enable '$VMLT_PLUGIN_TAG'" >/dev/null \
        || cannot "the plugin would not install in the VM"

    {
        printf 'VMLT_PLUGIN_TAG=%s\n' "$VMLT_PLUGIN_TAG"
        printf 'VMLT_BRIDGE=%s\n'     "$VMLT_BRIDGE"
        printf 'VMLT_SUBNET=%s\n'     "$VMLT_SUBNET"
        printf 'VMLT_GATEWAY=%s\n'    "$VMLT_GATEWAY"
        printf 'VMLT_POOL_START=%s\n' "$VMLT_POOL_START"
        printf 'VMLT_POOL_END=%s\n'   "$VMLT_POOL_END"
        printf 'VMLT_LEASE_TIME=%s\n' "$VMLT_LEASE_TIME"
        printf 'VMLT_POOL_SIZE=%s\n'  "$(vmlt_pool_size)"
    } | vm_ssh 'cat > /root/vmlt-env' || cannot "could not write the rig environment"

    vmlt_rig_script | vm_ssh 'cat > /root/vmlt-rig.sh && chmod +x /root/vmlt-rig.sh' \
        || cannot "could not write the rig script"

    local identity commit
    identity=$(vm_ssh /root/vmlt-rig.sh identity) || cannot "the plugin does not answer /Plugin.Health"
    say "$identity"
    commit=$(printf '%s\n' "$identity" | sed -n 's/^plugin_commit=//p')
    [ "$commit" = "$VMLT_HEAD" ] \
        || die "the plugin in the VM reports commit ${commit:-none}; this tree is at $VMLT_HEAD. The measurement would be of another build."
    VMLT_ENGINE=$(printf '%s\n' "$identity" | sed -n 's/^engine=//p')
}

# The DHCP pool's size, so a burst that fails for want of addresses is
# refused before it runs instead of being read as the finding.
vmlt_pool_size() {
    local a b
    a=${VMLT_POOL_START##*.}
    b=${VMLT_POOL_END##*.}
    echo $((b - a + 1))
}

vmlt_field() { sed -n "s/.*$1=\\([^ ]*\\).*/\\1/p" <<< "$2"; }

VMLT_ROWS=""
VMLT_EVIDENCE=""
VMLT_ORPHANS=""
VMLT_STALE=""
VMLT_KEY_PROVEN=0
VMLT_REFUSED=""

wait_for_pool() {
    local need=$1 waited=0 out free
    while [ "$waited" -lt 240 ]; do
        out=$(vm_ssh /root/vmlt-rig.sh freepool) || return 1
        free=$(vmlt_field free "$out")
        [ -n "$free" ] || return 1
        [ "$free" -ge "$need" ] && return 0
        [ "$waited" -eq 0 ] && say "  waiting for leases to expire: $free free, $need needed"
        sleep 10; waited=$((waited + 10))
    done
    return 1
}

burst() {
    local level=$1 n=$2 rep=$3
    local prefix="l-$level-n$n-r$rep"
    local before="$VMLT_WORK/$prefix.before.json" after="$VMLT_WORK/$prefix.after.json"

    wait_for_pool $((n + 10)) \
        || die "the DHCP pool has no room for a burst of $n; a burst that runs out of addresses measures the pool, not the budget"

    vm_ssh /root/vmlt-rig.sh create "$n" "$prefix" >/dev/null \
        || die "could not create the $n containers of $prefix"

    vm_ssh /root/vmlt-rig.sh health > "$before" || die "could not read /Plugin.Health before $prefix"
    local cpu0 cpu1
    cpu0=$(vm_ssh /root/vmlt-rig.sh cpustat) || die "could not read the VM's clock before $prefix"

    local started status running load
    started=$(vm_ssh /root/vmlt-rig.sh start "$n" "$prefix") || die "the burst $prefix did not run"

    # The starts have returned; the attaches have not necessarily landed.
    # Poll until every successful start has an outcome, bounded by the
    # plugin's own attach window plus a margin, so a burst whose attaches
    # are still in flight at the bound is read as it stands and said so.
    local start_ok settled=0 waited=0
    start_ok=$(vmlt_field start_ok "$started")
    [ -n "$start_ok" ] || die "the burst $prefix reported no start count"
    while [ "$waited" -le "$VMLT_SETTLE_SECONDS" ]; do
        vm_ssh /root/vmlt-rig.sh health > "$after" || die "could not read /Plugin.Health after $prefix"
        if vmlt_settled "$before" "$after" "$start_ok"; then settled=1; break; fi
        sleep 2; waited=$((waited + 2))
    done
    [ "$settled" = 1 ] || note "$prefix: not every attach had an outcome ${VMLT_SETTLE_SECONDS}s after the last start returned; the row is read as it stands"
    cpu1=$(vm_ssh /root/vmlt-rig.sh cpustat) || die "could not read the VM's clock after $prefix"
    local share steal iowait
    share=$(vmlt_cpu_share "$cpu0" "$cpu1") || die "the VM's clock over $prefix is not a measurement"
    steal=$(vmlt_field steal "$share"); iowait=$(vmlt_field iowait "$share")

    status=$(vm_ssh /root/vmlt-rig.sh status "$prefix") || die "could not read the state of $prefix"

    running=$(vmlt_field running "$status")
    load=$(vmlt_field load "$status")

    local delta
    delta=$(vmlt_delta "$before" "$after") || die "the counters for $prefix are not a measurement"
    # shellcheck disable=SC2086
    set -- $delta
    local under1s=$1 budget=$2 slow=$3 completed=$4 maxms=$5 maxrose=$6 failures=$7 aborted=$8

    vmlt_check_attached "$running" "$completed" "$failures" \
        || die "the burst $prefix exercised no attach"

    local leases matched total
    leases=$(vm_ssh /root/vmlt-rig.sh leases "$prefix")
    matched=$(vmlt_field leases_matched "$leases")
    total=$(vmlt_field leases_total "$leases")
    [ -n "$matched" ] && [ -n "$total" ] || die "could not read the lease file after $prefix"
    printf '%s\n' "$leases" > "$VMLT_WORK/$prefix.leases.txt"

    # A client that is not bound after the burst settled is the #969
    # shape only if the container's link still carries an address; that
    # is read from inside the container, against the server's lease file.
    local unbound stale=0 held
    unbound=$(vmlt_unbound "$after") || die "the health view after $prefix lists no endpoints"

    if [ "$VMLT_KEY_PROVEN" = 0 ]; then
        if vmlt_check_lease_key "$completed" "$matched" "$unbound"; then
            VMLT_KEY_PROVEN=1
        elif [ $? -eq 1 ]; then
            die "the outside evidence is blind; see $VMLT_WORK/$prefix.leases.txt"
        fi
    fi
    if [ "$unbound" -gt 0 ] || [ "$matched" -lt "$running" ]; then
        held=$(vm_ssh /root/vmlt-rig.sh evidence "$prefix")
        printf '%s\n' "$held" > "$VMLT_WORK/$prefix.addresses.txt"
        stale=$(vmlt_count_held "$held")
        VMLT_STALE="${VMLT_STALE}| ${prefix} | ${running} | ${unbound} | ${stale} | $prefix.addresses.txt |
"
    fi

    VMLT_ROWS="${VMLT_ROWS}${level}	${n}	${rep}	${running}	${under1s}	${budget}	${slow}	${completed}	${maxms}	${maxrose}	${failures}	${aborted}	${load}	${steal}	${iowait}
"
    VMLT_EVIDENCE="${VMLT_EVIDENCE}| ${level} | ${n} | ${rep} | ${running} | ${total} | ${matched} | $(vmlt_field start_ok "$started") | $(vmlt_field start_other "$started") | $(vmlt_field burst_s "$started") |
"
    say "  $prefix: running=$running completed=$completed slow=$slow failures=$failures max=${maxms}ms leases=$matched unbound=$unbound stale=$stale burst=$(vmlt_field burst_s "$started")s load=$load steal=${steal}% iowait=${iowait}%"

    if [ "$failures" -gt 0 ]; then
        lease_evidence "$prefix"
    fi

    vm_ssh /root/vmlt-rig.sh rm "$prefix" >/dev/null
}

# Runs only when the plugin counted a start failure, which is the only
# case this evidence has a subject. It waits out the lease and then asks
# the DHCP server, not the plugin, whether the addresses those containers
# are still holding are still leased.
lease_evidence() {
    local prefix=$1 wait_s=$((VMLT_LEASE_SECONDS + 60))
    say "  $prefix counted a start failure: holding its containers ${wait_s}s for the lease to expire"
    sleep "$wait_s"
    local out
    out=$(vm_ssh /root/vmlt-rig.sh evidence "$prefix")
    printf '%s\n' "$out" > "$VMLT_WORK/$prefix.evidence.txt"
    local orphans
    orphans=$(vmlt_count_held "$out")
    say "  $prefix: $orphans running containers hold an address the DHCP server no longer leases to them"
    VMLT_ORPHANS="${VMLT_ORPHANS}| ${prefix} | ${wait_s} | ${orphans} | $prefix.evidence.txt |
"
}

run_matrix() {
    local level n rep args
    for level in $VMLT_LEVELS; do
        args=$(vmlt_stress_args "$level") || die "no such load level: $level"
        say "load level $level"
        vm_ssh "/root/vmlt-rig.sh stress $(printf '%q' "$args")" || die "could not apply the $level load"
        for n in $VMLT_BURSTS; do
            for rep in $(seq 1 "$VMLT_REPEATS"); do
                burst "$level" "$n" "$rep"
            done
        done
        vm_ssh /root/vmlt-rig.sh stressoff || note "could not stop the $level load"
    done
}

report() {
    local rows="$VMLT_WORK/rows.tsv"
    printf '%s' "$VMLT_ROWS" > "$rows"
    echo
    echo "## Attach budget under load (#969)"
    echo
    echo "VM: 2 vCPU, 2 GB, one disk. Engine ${VMLT_ENGINE:-unknown}. Plugin ${VMLT_HEAD:0:12}, bridge mode, dnsmasq on an isolated bridge inside the same VM, plugin defaults (AWAIT_TIMEOUT 10s, conflict_check wait)."
    echo "Deltas are per burst. \`max attach ms\` is a running maximum: the value is printed when it rose inside that burst and \`<= N\` when it did not."
    echo "The burst's wall time includes the RFC 5227 conflict check every start pays inside CreateEndpoint (4 to 7 s); the attach buckets measure the Join window only."
    echo "\`steal %\` and \`iowait %\` are the VM's own counters over the burst as a share of its wall time: steal is what the host's other tenants took, so a row with steal well above zero measured the host as much as the level."
    echo
    # A refused row is a refusal of that row, not of the evidence the other
    # sections carry; the report prints every section and returns the
    # refusal at the end, so the orphan a matrix found is not discarded
    # because one of its bursts was idle.
    local rc=0
    vmlt_table "$rows" || rc=1
    echo
    echo "### Outside evidence"
    echo
    echo 'The dnsmasq lease file is read whole after each burst (entries of'
    echo 'earlier bursts that have not expired are in it) and joined to the'
    echo "burst's running containers by the hostname the persistent client"
    echo 'sends at Join as option 12, the short container id. A container whose'
    echo 'one-shot client got the address but whose persistent client never'
    echo 'sent that REQUEST is in the file under another name: an entry, not'
    echo 'a match.'
    echo
    echo '| load | burst | run | running | lease file entries | matched by container id | start 2xx | start other | burst wall s |'
    echo '| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |'
    printf '%s' "$VMLT_EVIDENCE" | vmlt_mark_refused "$VMLT_REFUSED"
    if [ "$VMLT_KEY_PROVEN" = 0 ]; then
        echo
        echo 'The container-id join key was never proven in this run: no burst had a bound endpoint whose lease carried its container id, so the matched column above is not evidence of anything.'
        rc=1
    fi
    echo
    echo '### Containers left without a renewal client'
    echo
    if [ -z "$VMLT_ORPHANS" ]; then
        echo 'No burst counted a start failure, so this reading has no subject.'
        echo 'It is not a pass: nothing was held past its lease.'
    else
        echo '| burst | held for (s) | running with an address the server no longer leases to it | detail |'
        echo '| --- | ---: | ---: | --- |'
        printf '%s' "$VMLT_ORPHANS" | vmlt_mark_refused "$VMLT_REFUSED"
    fi
    echo
    echo '### Address held while the client is not bound'
    echo
    if [ -z "$VMLT_STALE" ]; then
        echo 'After every burst settled, every endpoint the plugin listed was bound and every running container was leased, so this reading has no subject.'
    else
        echo 'Read right after the burst settled: endpoints the plugin listed in a state other than bound, and running containers whose link carries an address the DHCP server does not lease to that container: the lease file has no entry for the address, or its entry carries another name (a removed container not yet expired, or the one-shot client when the persistent client never started). The detail file names the entry.'
        echo
        echo '| burst | running | not bound | address held without its own lease | detail |'
        echo '| --- | ---: | ---: | ---: | --- |'
        printf '%s' "$VMLT_STALE" | vmlt_mark_refused "$VMLT_REFUSED"
    fi
    echo
    return "$rc"
}

# --- self-test ----------------------------------------------------------
# No KVM, no network, no Docker: the table and delta arithmetic, the
# refusals, and the seed builder, driven against fixtures. These are the
# functions the run calls, not a transcription of them.

VMLT_ST_PASS=0
VMLT_ST_FAIL=0
VMLT_ST_SKIP=0
st_ok() { VMLT_ST_PASS=$((VMLT_ST_PASS + 1)); printf '  ok    %s\n' "$1"; }
st_no() { VMLT_ST_FAIL=$((VMLT_ST_FAIL + 1)); printf '  FAIL  %s\n' "$1"; }
# st_skip <cases it stands for> <why>: a skip is counted, so the case
# population the gate test pins is the same on a box that lacks a tool.
st_skip() { VMLT_ST_SKIP=$((VMLT_ST_SKIP + $1)); printf '  SKIP  %s cases: %s\n' "$1" "$2"; }

st_health() {
    local iid=$1 u=$2 b=$3 s=$4 c=$5 m=$6 f=$7 g=$8
    cat <<JSON
{ "healthy": true, "instance_id": "$iid",
  "join_attach_under_1s": $u, "join_attach_1s_to_budget": $b,
  "join_attach_slow": $s, "join_attach_completed": $c,
  "join_attach_ms_max": $m, "join_start_failures": $f,
  "join_aborted_container_gone": $g,
  "join_aborted_no_container": 0, "join_aborted_endpoint_left": 0 }
JSON
}

self_test() {
    local here dir
    here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    # shellcheck source=scripts/tmpdir-guard.sh
    . "$here/tmpdir-guard.sh"
    guarded_tmpdir dir

    echo "vm-load-test --self-test"

    vmlt_check_lease_key 10 10 0 2>/dev/null; rc=$?
    [ $rc -eq 0 ] && st_ok "every completed attach has a lease: the key is proven" || st_no "a fully leased clean burst returned $rc"
    vmlt_check_lease_key 5 3 2 2>/dev/null; rc=$?
    [ $rc -eq 0 ] && st_ok "a loaded burst with two clients still acquiring proves the key by the three it matched" || st_no "a partly matched loaded burst returned $rc; the matrix would have been thrown away"
    vmlt_check_lease_key 0 0 0 2>/dev/null; rc=$?
    [ $rc -eq 2 ] && st_ok "a burst that completed nothing proves no key; it cannot latch the proof for the run" || st_no "an empty burst returned $rc instead of 2"
    vmlt_check_lease_key 10 0 0 2>/dev/null; rc=$?
    [ $rc -eq 1 ] && st_ok "ten bound clients and no lease under their ids is a blind key, refused" || st_no "a blind key returned $rc instead of 1"
    vmlt_check_lease_key 10 0 10 2>/dev/null; rc=$?
    [ $rc -eq 2 ] && st_ok "ten clients still acquiring and no lease under their ids is undecided, not blind" || st_no "an all-acquiring burst returned $rc instead of 2"

    [ "$(vmlt_stress_args idle)" = "" ] \
        && st_ok "the idle level runs no stress-ng" \
        || st_no "the idle level produced arguments: $(vmlt_stress_args idle)"
    [ "$(vmlt_stress_args light)" = "--cpu 1 --hdd 1 --cpu-load 40 --hdd-bytes 64M" ] \
        && st_ok "the light level carries its worker counts and its extra arguments" \
        || st_no "light: $(vmlt_stress_args light)"
    case "$(vmlt_stress_args heavy)" in
        "--cpu 4 --hdd 2 "*) st_ok "the heavy level's worker counts come from the spec, not from the extra arguments" ;;
        *) st_no "heavy: $(vmlt_stress_args heavy)" ;;
    esac
    if vmlt_stress_args nosuch >/dev/null 2>&1; then st_no "an unknown level produced stress-ng arguments"; else st_ok "an unknown level has no stress-ng arguments"; fi

    st_health s1 0 0 0 0 0 0 0 > "$dir/s0.json"
    st_health s1 2 0 0 2 300 1 0 > "$dir/s1.json"
    if vmlt_settled "$dir/s0.json" "$dir/s1.json" 3 2>/dev/null; then st_ok "two completions and a failure settle a burst of three starts"; else st_no "three outcomes for three starts did not settle"; fi
    if vmlt_settled "$dir/s0.json" "$dir/s1.json" 4 2>/dev/null; then st_no "three outcomes settled a burst of four starts"; else st_ok "an attach still in flight keeps the burst unsettled"; fi
    if vmlt_settled "$dir/s0.json" "$dir/s0.json" 0 2>/dev/null; then st_ok "a burst with no successful start is settled at once"; else st_no "zero starts did not settle"; fi
    printf '{"join_attach_completed": 1}\n' > "$dir/s2.json"
    vmlt_settled "$dir/s0.json" "$dir/s2.json" 1 2>/dev/null; rc=$?
    [ "$rc" -eq 2 ] && st_ok "a snapshot missing an outcome counter is refused, not read as settled" || st_no "a snapshot missing counters returned $rc"

    st_health i1 5 1 0 6 820 0 0 > "$dir/a.json"
    st_health i1 9 3 1 13 4100 2 1 > "$dir/b.json"

    local out rc

    out=$(vmlt_counters "$dir/a.json"); rc=$?
    if [ $rc -eq 0 ] && [ "$(printf '%s\n' "$out" | wc -l)" -eq 8 ]; then
        st_ok "vmlt_counters reads the seven readings and the instance id"
    else
        st_no "vmlt_counters over a complete document: rc=$rc out=$out"
    fi

    python3 - "$dir/a.json" "$dir/no-slow.json" <<'PRUNE'
import json, sys
d = json.load(open(sys.argv[1])); del d["join_attach_slow"]
json.dump(d, open(sys.argv[2], "w"))
PRUNE
    if vmlt_counters "$dir/no-slow.json" >/dev/null 2>&1; then
        st_no "a document missing join_attach_slow was read as a zero"
    else
        st_ok "a missing counter is refused, not defaulted to zero"
    fi

    python3 - "$dir/a.json" "$dir/no-iid.json" <<'PRUNE'
import json, sys
d = json.load(open(sys.argv[1])); del d["instance_id"]
json.dump(d, open(sys.argv[2], "w"))
PRUNE
    if vmlt_counters "$dir/no-iid.json" >/dev/null 2>&1; then
        st_no "a document with no instance_id was accepted"
    else
        st_ok "a document with no instance_id is refused"
    fi

    printf '%s' '{"join_attach_under_1s": "many", "join_attach_1s_to_budget": 0, "join_attach_slow": 0, "join_attach_completed": 0, "join_attach_ms_max": 0, "join_start_failures": 0, "join_aborted_container_gone": 0, "instance_id": "i"}' > "$dir/text.json"
    if vmlt_counters "$dir/text.json" >/dev/null 2>&1; then
        st_no "a non-integer counter was accepted"
    else
        st_ok "a non-integer counter is refused"
    fi

    echo 'not json at all' > "$dir/junk.json"
    if vmlt_counters "$dir/junk.json" >/dev/null 2>&1; then
        st_no "a non-JSON body was read as counters"
    else
        st_ok "a non-JSON body is refused"
    fi

    out=$(vmlt_delta "$dir/a.json" "$dir/b.json"); rc=$?
    if [ $rc -eq 0 ] && [ "$out" = "4 2 1 7 4100 yes 2 1" ]; then
        st_ok "vmlt_delta reports the burst's own numbers, not the running totals"
    else
        st_no "vmlt_delta: rc=$rc got '$out', wanted '4 2 1 7 4100 yes 2 1'"
    fi

    st_health i1 9 3 1 13 820 2 1 > "$dir/same-max.json"
    out=$(vmlt_delta "$dir/a.json" "$dir/same-max.json")
    case "$out" in
        *" 820 no "*) st_ok "an unchanged maximum is reported as not belonging to this burst" ;;
        *) st_no "an unchanged maximum: got '$out'" ;;
    esac

    if vmlt_delta "$dir/b.json" "$dir/a.json" >/dev/null 2>&1; then
        st_no "a counter that fell was reported as a delta"
    else
        st_ok "a counter that fell is refused"
    fi

    st_health i2 9 3 1 13 4100 2 1 > "$dir/restarted.json"
    if vmlt_delta "$dir/a.json" "$dir/restarted.json" >/dev/null 2>&1; then
        st_no "two different plugin processes were subtracted from each other"
    else
        st_ok "a changed instance_id is refused"
    fi

    if vmlt_check_attached 10 0 0 >/dev/null 2>&1; then
        st_no "a burst with ten running containers and no attach at all passed"
    else
        st_ok "a burst that exercised no attach is refused"
    fi
    vmlt_check_attached 10 0 10 >/dev/null 2>&1 \
        && st_ok "a burst whose attaches all failed is a measurement, not a broken rig" \
        || st_no "a burst of ten failures was refused"
    vmlt_check_attached 0 0 0 >/dev/null 2>&1 \
        && st_ok "a burst in which nothing reached running is not refused here" \
        || st_no "a burst with no running containers was refused"

    vmlt_check_load idle 0.00 >/dev/null 2>&1 \
        && st_ok "the unloaded level has no floor" \
        || st_no "the unloaded level was held to a floor"
    if vmlt_check_load heavy 0.31 >/dev/null 2>&1; then
        st_no "a heavy level that ended at load 0.31 was accepted as loaded"
    else
        st_ok "a loaded level whose load average never left the floor is refused"
    fi
    vmlt_check_load heavy 5.20 >/dev/null 2>&1 \
        && st_ok "a heavy level at load 5.20 passes its floor" \
        || st_no "load 5.20 was below the heavy floor"

    printf 'heavy\t50\t1\t50\t10\t30\t10\t50\t9800\tyes\t0\t0\t5.20\t1.5\t20.0\n' > "$dir/rows.tsv"
    printf 'idle\t10\t1\t10\t10\t0\t0\t10\t9800\tno\t0\t0\t0.10\t0.0\t0.3\n'     >> "$dir/rows.tsv"
    out=$(vmlt_table "$dir/rows.tsv"); rc=$?
    if [ $rc -eq 0 ] && [ "$(printf '%s\n' "$out" | wc -l)" -eq 4 ]; then
        st_ok "vmlt_table renders a header and one row per burst"
    else
        st_no "vmlt_table: rc=$rc, $(printf '%s\n' "$out" | wc -l) lines"
    fi
    case "$out" in
        *"| <= 9800 |"*) st_ok "a maximum that did not rise is printed as a bound" ;;
        *) st_no "a maximum that did not rise was printed as this burst's own" ;;
    esac

    : > "$dir/empty.tsv"
    if vmlt_table "$dir/empty.tsv" >/dev/null 2>&1; then
        st_no "a table with no rows was rendered as a result"
    else
        st_ok "a table with no rows is refused"
    fi

    printf 'heavy\t50\t1\t50\t10\t30\t10\t50\t9800\tyes\t0\t0\t0.31\t0.0\t0.1\n' > "$dir/floor.tsv"
    printf 'idle\t10\t1\t10\t10\t0\t0\t10\t9800\tno\t0\t0\t0.10\t0.0\t0.3\n'    >> "$dir/floor.tsv"
    out=$(vmlt_table "$dir/floor.tsv" 2>/dev/null); rc=$?
    if [ $rc -ne 0 ] && [ "$(printf '%s\n' "$out" | grep -c '^| refused: heavy | 50 | 1 |')" -eq 1 ] && [ "$(printf '%s\n' "$out" | grep -c '^| idle ')" -eq 1 ]; then
        st_ok "a loaded row whose load average sat below its floor is rendered marked refused, and the table returns the refusal"
    else
        st_no "vmlt_table rc=$rc: $(printf '%s\n' "$out" | grep -c '^| refused: heavy ') marked heavy, $(printf '%s\n' "$out" | grep -c '^| heavy ') unmarked heavy, $(printf '%s\n' "$out" | grep -c '^| idle ') idle rows"
    fi
    case "$out" in
        *"1 rows ran at a load average below their level's floor"*"heavy n50 r1"*) st_ok "the table's own text names the refused burst, so the pasted markdown carries the refusal" ;;
        *) st_no "the refusal is not in the table's stdout" ;;
    esac

    # report() over the same rows: a refused row must not take the
    # sections after the table with it, and the report still returns
    # the refusal. An unproven join key refuses the lease column.
    VMLT_WORK=$dir VMLT_ENGINE=st VMLT_HEAD=st
    VMLT_ROWS="$(cat "$dir/floor.tsv")"$'\n'
    VMLT_EVIDENCE='| heavy | 50 | 1 | 50 | 50 | 50 | 50 | 0 | 531 |
| heavy | 20 | 1 | 20 | 20 | 20 | 20 | 0 | 211 |
| idle | 10 | 1 | 10 | 10 | 10 | 10 | 0 | 108 |
'
    VMLT_ORPHANS='| l-heavy-n50-r1 | 130 | 1 | l-heavy-n50-r1.orphans.txt |
'
    VMLT_STALE='| l-heavy-n50-r1 | 50 | 9 | 3 | l-heavy-n50-r1.addresses.txt |
'
    VMLT_KEY_PROVEN=1
    out=$(report 2>/dev/null); rc=$?
    if [ $rc -ne 0 ] && grep -q '^| refused: l-heavy-n50-r1 | 130 |' <<< "$out" && grep -q '^| refused: heavy | 50 | 1 |' <<< "$out"; then
        st_ok "a refused row does not take the report with it: the orphan section follows the marked table and the report returns the refusal"
    else
        st_no "report rc=$rc; orphan rows printed: $(printf '%s\n' "$out" | grep -c 'l-heavy-n50-r1 | 130 '), refused rows printed: $(printf '%s\n' "$out" | grep -c '^| refused: heavy ')"
    fi
    if grep -q '^| refused: heavy | 50 | 1 | 50 | 50 |' <<< "$out" && grep -q '^| refused: l-heavy-n50-r1 | 50 | 9 |' <<< "$out" \
        && grep -q '^| idle | 10 | 1 | 10 |' <<< "$out" && ! grep -q 'refused: idle' <<< "$out" \
        && grep -q '^| heavy | 20 | 1 | 20 |' <<< "$out"; then
        st_ok "the refusal follows the burst into the evidence, orphan and held-address sections, and leaves the other bursts unmarked, the same level's other burst included"
    else
        st_no "sections marked: evidence $(printf '%s\n' "$out" | grep -c '^| refused: heavy | 50 | 1 | 50 | 50 |'), stale $(printf '%s\n' "$out" | grep -c '^| refused: l-heavy-n50-r1 | 50 | 9 |'), idle marked $(printf '%s\n' "$out" | grep -c 'refused: idle'), heavy n20 unmarked $(printf '%s\n' "$out" | grep -c '^| heavy | 20 | 1 | 20 |')"
    fi
    VMLT_ROWS="$(sed -n '2p' "$dir/floor.tsv")"$'\n'
    VMLT_KEY_PROVEN=0
    out=$(report 2>/dev/null); rc=$?
    if [ $rc -ne 0 ] && grep -q 'join key was never proven' <<< "$out"; then
        st_ok "a run in which no lease ever carried a container id refuses its matched column instead of printing zeros as evidence"
    else
        st_no "report rc=$rc with an unproven key; sentences: $(printf '%s\n' "$out" | grep -c 'never proven')"
    fi
    VMLT_KEY_PROVEN=1
    out=$(report 2>/dev/null); rc=$?
    if [ $rc -eq 0 ] && grep -q '^| idle | 10 | 1 |' <<< "$out"; then
        st_ok "a clean run's report returns 0 with its rows"
    else
        st_no "a clean report returned $rc"
    fi
    VMLT_ROWS="" VMLT_EVIDENCE="" VMLT_ORPHANS="" VMLT_STALE=""

    printf 'heavy\t50\t1\t50\t10\t30\t10\t50\t9800\tyes\t0\t0\t5.20\n' > "$dir/short.tsv"
    if vmlt_table "$dir/short.tsv" >/dev/null 2>&1; then
        st_no "a row with the wrong number of fields was rendered"
    else
        st_ok "a row with the wrong number of fields is refused"
    fi

    out=$(vmlt_cpu_share "steal=100 iowait=50 total=1000" "steal=200 iowait=150 total=2000"); rc=$?
    if [ $rc -eq 0 ] && [ "$out" = "steal=10.0 iowait=10.0" ]; then
        st_ok "steal and iowait are read as a share of the burst's own clock"
    else
        st_no "vmlt_cpu_share rc=$rc out=$out"
    fi
    if vmlt_cpu_share "steal=100 iowait=50 total=1000" "steal=100 iowait=50 total=1000" >/dev/null 2>&1; then
        st_no "a window in which the clock did not move was read as 0 %"
    else
        st_ok "a window in which the clock did not move is refused"
    fi
    if vmlt_cpu_share "steal=100 iowait=50 total=1000" "iowait=60 total=1100" >/dev/null 2>&1; then
        st_no "a missing steal counter was read as a share"
    else
        st_ok "a missing steal counter is refused"
    fi
    if vmlt_cpu_share "steal=100 iowait=50 total=1000" "steal=90 iowait=60 total=1100" >/dev/null 2>&1; then
        st_no "a steal counter that went backwards was read as a share"
    else
        st_ok "a steal counter that went backwards is refused"
    fi

    printf '{"endpoints":[{"endpoint":"a","lease_state":"bound"},{"endpoint":"b","lease_state":"acquiring"},{"endpoint":"c"}]}\n' > "$dir/u.json"
    out=$(vmlt_unbound "$dir/u.json")
    [ "$out" = "2" ] && st_ok "endpoints not bound are counted, one with no state among them" || st_no "vmlt_unbound counted '$out' of 2"
    printf '{"endpoints":[]}\n' > "$dir/u0.json"
    out=$(vmlt_unbound "$dir/u0.json")
    [ "$out" = "0" ] && st_ok "no endpoints, none unbound" || st_no "vmlt_unbound counted '$out' with no endpoints"
    printf '{"healthy":true}\n' > "$dir/u2.json"
    vmlt_unbound "$dir/u2.json" >/dev/null 2>&1; rc=$?
    [ $rc -eq 2 ] && st_ok "a health view without an endpoints list is refused, not read as none unbound" || st_no "a health view without endpoints returned $rc"

    if vmlt_level_field nosuchlevel 1 >/dev/null 2>&1; then
        st_no "an undefined load level was accepted"
    else
        st_ok "an undefined load level is refused"
    fi

    [ "$(vmlt_pool_size)" = "231" ] \
        && st_ok "the pool size is derived from the configured range" \
        || st_no "the pool size came out as $(vmlt_pool_size)"

    printf 'ssh-ed25519 AAAAFIXTUREKEY vm-load-test\n' > "$dir/key.pub"
    if vmlt_seed_files "$dir/seed" "$dir/key.pub" vmlt-fixture; then
        local ud="$dir/seed/user-data"
        [ "$(head -n1 "$ud")" = "#cloud-config" ] \
            && st_ok "user-data opens with the cloud-config header" \
            || st_no "user-data does not open with #cloud-config"
        case "$(cat "$ud")" in
            *"ssh-ed25519 AAAAFIXTUREKEY vm-load-test"*)
                st_ok "the public key reaches user-data verbatim" ;;
            *) st_no "the public key is not in user-data" ;;
        esac
        case "$(cat "$dir/seed/meta-data")" in
            *"instance-id: vmlt-fixture"*) st_ok "meta-data carries the instance id" ;;
            *) st_no "meta-data carries no instance id" ;;
        esac
    else
        st_no "vmlt_seed_files refused a complete input"
    fi

    if vmlt_seed_files "$dir/seed2" "$dir/no-such-key.pub" vmlt-fixture >/dev/null 2>&1; then
        st_no "a missing public key was accepted"
    else
        st_ok "a missing public key is refused"
    fi

    # The cidata cases: one function each, run from one array, and the
    # skipped count is that array's length. A case deleted here leaves
    # the count with it on a box that skips them, so the gate's pin
    # moves on both kinds of box; a literal beside the cases would not.
    st_cidata_user_data() { case "$1" in *user-data*) st_ok "the cidata volume carries user-data" ;; *) st_no "the cidata volume has no user-data" ;; esac; }
    st_cidata_meta_data() { case "$1" in *meta-data*) st_ok "the cidata volume carries meta-data" ;; *) st_no "the cidata volume has no meta-data" ;; esac; }
    st_cidata_label() { case "$1" in *CIDATA*) st_ok "the volume is labelled CIDATA, which is what NoCloud looks for" ;; *) st_no "the volume is not labelled CIDATA" ;; esac; }
    st_cidata_refusal() {
        if vmlt_seed_image "$2/nothing-here" "$2/seed3.img" >/dev/null 2>&1; then
            st_no "a directory with no NoCloud pair produced a seed volume"
        else
            st_ok "a directory with no NoCloud pair is refused"
        fi
    }
    local -a cidata_cases=(st_cidata_user_data st_cidata_meta_data st_cidata_label st_cidata_refusal)
    local cidata_case listing=""
    if command -v mformat >/dev/null 2>&1 && command -v mcopy >/dev/null 2>&1 && command -v mdir >/dev/null 2>&1; then
        if vmlt_seed_image "$dir/seed" "$dir/seed.img" >/dev/null 2>&1; then
            listing=$(mdir -i "$dir/seed.img" ::)
        fi
        for cidata_case in "${cidata_cases[@]}"; do "$cidata_case" "$listing" "$dir"; done
    else
        st_skip "${#cidata_cases[@]}" "the cidata volume cases need mtools (mformat, mcopy, mdir); they did not run here"
    fi

    # The build's identity: a tree whose build inputs differ from HEAD
    # is refused, tracked and untracked alike; a change outside them is
    # not the plugin and passes.
    local repo="$dir/repo" head
    git init -q "$repo" && mkdir -p "$repo/pkg" "$repo/scripts" && echo 'package pkg' > "$repo/pkg/a.go" \
        && git -C "$repo" config commit.gpgsign false && git -C "$repo" config user.name st \
        && git -C "$repo" config user.email st@example.invalid \
        && git -C "$repo" add pkg && git -C "$repo" commit -q -m st
    head=$(vmlt_tree_identity "$repo" 2>/dev/null); rc=$?
    if [ $rc -eq 0 ] && [ "$head" = "$(git -C "$repo" rev-parse HEAD)" ]; then
        st_ok "a clean tree's identity is its HEAD"
    else
        st_no "a clean tree returned $rc with '$head'"
    fi
    echo '// changed' >> "$repo/pkg/a.go"
    if vmlt_tree_identity "$repo" >/dev/null 2>&1; then st_no "a modified build input was reported as HEAD"; else st_ok "a modified file under the build inputs is refused"; fi
    git -C "$repo" checkout -q -- pkg/a.go
    echo 'package pkg' > "$repo/pkg/b.go"
    if vmlt_tree_identity "$repo" >/dev/null 2>&1; then st_no "an untracked file under the build inputs was reported as HEAD"; else st_ok "an untracked file under the build inputs is refused; go build would compile it"; fi
    rm -f "$repo/pkg/b.go"
    echo 'x' > "$repo/scripts/note"
    if vmlt_tree_identity "$repo" >/dev/null 2>&1; then st_ok "a change outside the build inputs is not the plugin and passes"; else st_no "a change outside the build inputs was refused"; fi
    echo 'go 1.25' > "$repo/go.work"
    if vmlt_tree_identity "$repo" >/dev/null 2>&1; then st_no "an untracked go.work was reported as HEAD; the Dockerfile copies go.*"; else st_ok "an untracked go.work is refused: the Dockerfile copies go.*"; fi
    # build_plugin itself, against that dirty tree: it refuses before make
    # runs, so no build log is written.
    ( VMLT_REPO=$repo VMLT_WORK=$dir build_plugin ) >/dev/null 2>&1; rc=$?
    if [ $rc -ne 0 ] && [ ! -e "$dir/build.log" ]; then
        st_ok "build_plugin refuses a dirty tree before make runs"
    else
        st_no "build_plugin rc=$rc on a dirty tree; build.log present: $([ -e "$dir/build.log" ] && echo yes || echo no)"
    fi
    rm -f "$repo/go.work"

    # The held-address join: the entry carrying the address is the
    # container's own only under its id, so a removed container's live
    # entry or the one-shot client's entry is not this container's lease.
    local leasefile='1758000000 aa:bb:cc:00:00:01 192.168.101.10 abc123def456 01:aa:bb:cc:00:00:01
1758000000 aa:bb:cc:00:00:02 192.168.101.11 0123456789ab 01:aa:bb:cc:00:00:02
1758000000 aa:bb:cc:00:00:03 192.168.101.12 * 01:aa:bb:cc:00:00:03'
    [ "$(vmlt_lease_owner "$leasefile" 192.168.101.10 abc123def456)" = own ] \
        && st_ok "an address whose entry carries the container id is the container's own lease" \
        || st_no "own lease read as: $(vmlt_lease_owner "$leasefile" 192.168.101.10 abc123def456)"
    [ "$(vmlt_lease_owner "$leasefile" 192.168.101.11 abc123def456)" = other:0123456789ab ] \
        && st_ok "an address whose entry carries another container's id is not this container's lease, and the entry is named" \
        || st_no "another container's entry read as: $(vmlt_lease_owner "$leasefile" 192.168.101.11 abc123def456)"
    [ "$(vmlt_lease_owner "$leasefile" 192.168.101.12 abc123def456)" = 'other:*' ] \
        && st_ok "an address under the one-shot client's name is held without the persistent client's lease" \
        || st_no "the one-shot entry read as: $(vmlt_lease_owner "$leasefile" 192.168.101.12 abc123def456)"
    [ "$(vmlt_lease_owner "$leasefile" 192.168.101.13 abc123def456)" = none ] \
        && st_ok "an address with no entry has no lease" \
        || st_no "a missing entry read as: $(vmlt_lease_owner "$leasefile" 192.168.101.13 abc123def456)"
    local held='abc123def456 192.168.101.10 lease=own entry=abc123def456
0123456789ab 192.168.101.11 lease=other entry=ep-old
fedcba987654 192.168.101.12 lease=none entry=-
1234567890ab none lease=none entry=-'
    [ "$(vmlt_count_held "$held")" -eq 2 ] \
        && st_ok "the held count is every container with an address that is not under its own id: the other-name entry and the missing entry, not the container with no address" \
        || st_no "held count $(vmlt_count_held "$held"), expected 2"
    [ "$(vmlt_count_held "")" -eq 0 ] \
        && st_ok "no evidence rows count as nothing held" \
        || st_no "empty evidence counted $(vmlt_count_held "")"

    rm -rf "$dir"
    echo
    printf 'passed %d, failed %d, skipped %d\n' "$VMLT_ST_PASS" "$VMLT_ST_FAIL" "$VMLT_ST_SKIP"
    [ "$VMLT_ST_FAIL" -eq 0 ]
}

# --- main ---------------------------------------------------------------

VMLT_HEAD=""
VMLT_ENGINE=""

vmlt_at_exit() {
    if [ "$VMLT_KEEP" = "1" ]; then
        note "the VM is still running: stop it with $0 --stop"
        return 0
    fi
    vm_stop
}

main() {
    case "${1:-}" in
        --self-test) self_test; exit $? ;;
        --stop)      vm_stop; exit $? ;;
        "")          ;;
        *) echo "usage: $0 [--self-test|--stop]" >&2; exit 2 ;;
    esac

    host_preflight
    mkdir -p "$VMLT_WORK" || cannot "cannot create $VMLT_WORK"
    fetch_image
    build_plugin
    vm_start
    trap vmlt_at_exit EXIT
    vm_wait_ssh 900 || cannot "the VM never answered ssh; the console log is $VMLT_CONSOLE"
    provision
    vm_ssh /root/vmlt-rig.sh up >/dev/null || cannot "the rig would not come up in the VM"
    run_matrix
    vm_ssh /root/vmlt-rig.sh down >/dev/null
    report || die "the report carries a refusal; the reason is above"
    say "work directory: $VMLT_WORK"
}

[ "${VMLT_LIB:-0}" = "1" ] || main "$@"
