#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Self-test for capability-matrix.sh (#690). The verdict it must never give
# is a pass over a cell that measured nothing; each refusal has a green twin
# that differs in one field.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/capability-matrix.sh"
CELL="$HERE/capability-cell.sh"
pass=0
fail=0

ok() { echo "ok    $1"; pass=$((pass + 1)); }
bad() { echo "FAIL  $1"; fail=$((fail + 1)); }

tmp=
guarded_tmpdir tmp
mapfile -t COLS < <(bash "$GATE" --columns)
PASSROW="enables=yes capeff=n/a mount=private bridge=pass macvlan=pass dns=pass user=pass dns_user=pass renew=pass renew_user=pass restart=pass"
ROW_A="enables=yes capeff=dropped mount=private bridge=fail macvlan=fail dns=fail user=fail dns_user=fail renew=fail renew_user=fail restart=fail"
TBL_NONE="| none | yes | n/a | private | pass | pass | pass | pass | pass | pass | pass | pass |"
TBL_A='| `CAP_NET_ADMIN` | yes | dropped | private | fail | fail | fail | fail | fail | fail | fail | fail |'

# build <case> <config-caps-json> <table rows...>: a config, a doc and an
# empty rows directory under $tmp/<case>.
build() {
    local d="$tmp/$1" caps="$2"; shift 2
    mkdir -p "$d/rows"
    printf '{"linux":{"capabilities":%s}}\n' "$caps" > "$d/config.json"
    {
        echo "prose"
        echo "<!-- capability-matrix: begin -->"
        printf '| removed |%s\n' "$(printf ' %s |' "${COLS[@]}")"
        printf '|---|%s\n' "$(printf -- '---|%.0s' "${COLS[@]}")"
        printf '%s\n' "$@"
        echo "<!-- capability-matrix: end -->"
        echo "more prose"
    } > "$d/reference.md"
}
# row <case> <cell> <result> <fields>
row() { printf 'CAP_MATRIX_ROW removed=%s result=%s %s\n' "$2" "$3" "$4" >> "$tmp/$1/rows/$2.row"; }
run() {
    local d="$tmp/$1"; shift
    out="$(CAP_MATRIX_CONFIG="$d/config.json" CAP_MATRIX_DOC="$d/reference.md" bash "$GATE" "$@" 2>&1)"
    got=$?
}
# expect <case-label> <exit> <output-fragment>
expect() {
    if [ "$got" -eq "$2" ] && { [ -z "$3" ] || printf '%s' "$out" | grep -F -- "$3" >/dev/null; }; then
        ok "$1"
    else
        bad "$1: exit $got want $2, output: $(printf '%s' "$out" | tr '\n' ' ')"
    fi
}
# base <case>: the green twin every red case is one field away from.
base() {
    build "$1" '["CAP_NET_ADMIN"]' "$TBL_NONE" "$TBL_A"
    row "$1" none ok "$PASSROW"
    row "$1" CAP_NET_ADMIN ok "$ROW_A"
}

base match; run match --reconcile "$tmp/match/rows"
expect "measured rows equal to the table pass" 0 ""
run match --reconcile --strict "$tmp/match/rows"
expect "a fully measured table passes strict" 0 ""

base mismatch; sed -i 's/restart=fail/restart=pass/' "$tmp/mismatch/rows/CAP_NET_ADMIN.row"
run mismatch --reconcile "$tmp/mismatch/rows"
expect "a measured value unlike the table is red" 1 "restart measured pass, docs table says fail"

base missing; rm -f "$tmp/missing/rows/CAP_NET_ADMIN.row"
run missing --reconcile "$tmp/missing/rows"
expect "a cell that uploaded no row is red" 1 "cell CAP_NET_ADMIN produced 0 rows"

base dup; row dup CAP_NET_ADMIN ok "$ROW_A"
run dup --reconcile "$tmp/dup/rows"
expect "a cell with two rows is red" 1 "produced 2 rows"

base prefix; rm -f "$tmp/prefix/rows/CAP_NET_ADMIN.row"; row prefix CAP_NET_ADMINX ok "$ROW_A"
run prefix --reconcile "$tmp/prefix/rows"
expect "a row for a longer name does not stand in for the cell" 1 "cell CAP_NET_ADMIN produced 0 rows"

for r in no-verdict misconfigured no-fixture; do
    base "r-$r"; : > "$tmp/r-$r/rows/CAP_NET_ADMIN.row"; row "r-$r" CAP_NET_ADMIN "$r" "$ROW_A"
    run "r-$r" --reconcile "$tmp/r-$r/rows"
    expect "result=$r is red, not a measurement" 1 "result=$r: no measurement"
done

build fullfail '["CAP_NET_ADMIN"]' "${TBL_NONE/| pass | pass | pass | pass | pass | pass | pass | pass |/| pass | fail | pass | pass | pass | pass | pass | pass |}" "$TBL_A"
row fullfail none ok "${PASSROW/macvlan=pass/macvlan=fail}"; row fullfail CAP_NET_ADMIN ok "$ROW_A"
run fullfail --reconcile "$tmp/fullfail/rows"
expect "the full set failing a scenario is red even when the table agrees" 1 "the full set failed macvlan"

build fullnoen '["CAP_NET_ADMIN"]' "${TBL_NONE/| none | yes |/| none | no |}" "$TBL_A"
row fullnoen none ok "${PASSROW/enables=yes/enables=no}"; row fullnoen CAP_NET_ADMIN ok "$ROW_A"
run fullnoen --reconcile "$tmp/fullnoen/rows"
expect "the full set not enabling is red even when the table agrees" 1 "the full set did not enable"

build unmeasured '["CAP_NET_ADMIN"]' "$TBL_NONE" '| `CAP_NET_ADMIN` | ? | ? | ? | ? | ? | ? | ? | ? | ? | ? | ? |'
row unmeasured none ok "$PASSROW"; row unmeasured CAP_NET_ADMIN ok "$ROW_A"
run unmeasured --reconcile "$tmp/unmeasured/rows"
expect "a ? off dev and main is a warning naming the value" 0 "::warning title=capability matrix::cell CAP_NET_ADMIN restart is unmeasured in the docs table, measured fail"
run unmeasured --reconcile --strict "$tmp/unmeasured/rows"
expect "a ? on dev and main is red" 1 "::error title=capability matrix::cell CAP_NET_ADMIN enables is unmeasured"

base badval; sed -i 's/bridge=fail/bridge=maybe/' "$tmp/badval/rows/CAP_NET_ADMIN.row"
sed -i 's/| `CAP_NET_ADMIN` | yes | dropped | private | fail/| `CAP_NET_ADMIN` | yes | dropped | private | maybe/' "$tmp/badval/reference.md"
run badval --reconcile "$tmp/badval/rows"
expect "a value outside the column's vocabulary is red even when the table agrees" 1 "bridge='maybe' is not a measured value"

for kv in enables=maybe capeff=maybe mount=Private; do
    k="${kv%%=*}"; v="${kv#*=}"
    base "vocab-$k"; sed -i "s/ $k=[^ ]*/ $k=$v/" "$tmp/vocab-$k/rows/CAP_NET_ADMIN.row"
    run "vocab-$k" --reconcile "$tmp/vocab-$k/rows"
    expect "$k='$v' is outside the column's vocabulary and red" 1 "$k='$v' is not a measured value"
done

base dropcol; sed -i 's/ restart=fail//' "$tmp/dropcol/rows/CAP_NET_ADMIN.row"
run dropcol --reconcile "$tmp/dropcol/rows"
expect "a row missing a column is red" 1 "restart='' is not a measured value"

build stale '["CAP_NET_ADMIN"]' "$TBL_NONE" "$TBL_A" '| `CAP_GONE` | yes | dropped | private | fail | fail | fail | fail | fail | fail | fail | fail |'
row stale none ok "$PASSROW"; row stale CAP_NET_ADMIN ok "$ROW_A"
run stale --reconcile "$tmp/stale/rows"
expect "a table row naming no capability of config.json is red" 1 "docs table row CAP_GONE names no cell"

build newcap '["CAP_NET_ADMIN","CAP_SYS_ADMIN"]' "$TBL_NONE" "$TBL_A"
row newcap none ok "$PASSROW"; row newcap CAP_NET_ADMIN ok "$ROW_A"; row newcap CAP_SYS_ADMIN ok "$ROW_A"
run newcap --reconcile "$tmp/newcap/rows"
expect "a capability added to config.json without a table row is red" 1 "docs table has no single row for CAP_SYS_ADMIN"

base header; sed -i 's/| renew | renew_user |/| renew_user | renew |/' "$tmp/header/reference.md"
run header --reconcile "$tmp/header/rows"
expect "a table header out of column order cannot be checked" 2 "table header"

base noblock; sed -i '/capability-matrix: begin/d' "$tmp/noblock/reference.md"
run noblock --reconcile "$tmp/noblock/rows"
expect "a doc without the table cannot be checked" 2 "carries no capability-matrix block"

base norows; run norows --reconcile "$tmp/norows/nothing"
expect "a missing rows directory cannot be checked" 2 "does not exist"

build nocaps '[]' "$TBL_NONE"
run nocaps --cells-json
expect "a config.json with no capabilities cannot be checked" 2 "declares no capabilities"

build cells '["CAP_NET_ADMIN","CAP_SYS_PTRACE"]' "$TBL_NONE"
run cells --cells-json
expect "the cell list is none plus config.json's capabilities" 0 '["none","CAP_NET_ADMIN","CAP_SYS_PTRACE"]'

run cells --strip CAP_SYS_PTRACE "$tmp/cells/config.json"
if [ "$got" -eq 0 ] && [ "$(printf '%s' "$out" | jq -c .linux.capabilities)" = '["CAP_NET_ADMIN"]' ]; then
    ok "--strip removes exactly the named capability"
else
    bad "--strip: exit $got, output $out"
fi
run cells --strip CAP_SYS_ADMIN "$tmp/cells/config.json"
expect "--strip of a capability config.json does not request is refused" 2 "is not requested"

# The server-log count the cell's renewal and restart verdicts rest on.
log="$tmp/dnsmasq.log"
M1=02:42:0a:00:00:01 M2=02:42:0a:00:00:02
cat > "$log" <<EOF
dnsmasq-dhcp[1]: 111 DHCPDISCOVER(cm-mvp) $M1
dnsmasq-dhcp[1]: 111 DHCPACK(cm-mvp) 10.98.1.50 $M1 host
dnsmasq-dhcp[1]: 222 DHCPACK(cm-mvp) 10.98.1.51 $M2
dnsmasq-dhcp[1]: 333 DHCPREQUEST(cm-mvp) 10.98.1.50 $M1
dnsmasq-dhcp[1]: 333 DHCPACK(cm-mvp) 10.98.1.50 $M1
dnsmasq-dhcp[1]: 444 DHCPNAK(cm-mvp) 10.98.1.50 $M1 wrong server-ID
EOF
count_case() {
    local label="$1" want="$2"; shift 2
    out="$(bash "$GATE" --dhcp-count "$@" 2>&1)"; got=$?
    if [ "$got" -eq 0 ] && [ "$out" = "$want" ]; then ok "$label"; else bad "$label: exit $got, '$out' want '$want'"; fi
}
count_case "every ACK for a MAC is counted from the top" 2 "$log" 0 "$M1" DHCPACK
count_case "an ACK at or before the mark is not counted" 1 "$log" 2 "$M1" DHCPACK
count_case "nothing after the last ACK counts, a NAK is not an ACK" 0 "$log" 5 "$M1" DHCPACK
count_case "another MAC's ACK is not counted" 1 "$log" 0 "$M2" DHCPACK
count_case "a DISCOVER before the mark is not counted" 0 "$log" 1 "$M1" DHCPDISCOVER
count_case "a DISCOVER after the mark is counted" 1 "$log" 0 "$M1" DHCPDISCOVER
count_case "a MAC prefix is not the MAC" 0 "$log" 0 "${M1%1}" DHCPACK
out="$(bash "$GATE" --dhcp-count "$log" x "$M1" DHCPACK 2>&1)"; got=$?
expect "a mark that is not a line number cannot be counted" 2 "--dhcp-count"
out="$(bash "$GATE" --dhcp-count "$tmp/nolog" 0 "$M1" DHCPACK 2>&1)"; got=$?
expect "a missing server log cannot be counted" 2 "cannot read"

# Where a `?` is red: wherever a merge happens.
strict_case() {
    out="$(bash "$GATE" --strictness "$2" "$3" "$4" 2>&1)"; got=$?
    if [ "$got" -eq 0 ] && [ "$out" = "$1" ]; then ok "$2 $3 draft=$4 is $1"; else bad "$2 $3 draft=$4: exit $got, '$out' want $1"; fi
}
strict_case strict pull_request refs/pull/7/merge false
strict_case lenient pull_request refs/pull/7/merge true
strict_case strict push refs/heads/dev false
strict_case strict push refs/heads/main false
strict_case lenient push refs/heads/ci/x false
strict_case strict workflow_dispatch refs/heads/main false
strict_case lenient workflow_dispatch refs/heads/ci/x false
out="$(bash "$GATE" --strictness schedule refs/heads/main false 2>&1)"; got=$?
expect "an event the workflow does not declare has no strictness" 2 "no strictness"
if [ -z "$(bash "$GATE" --strictness schedule refs/heads/main false 2>/dev/null)" ]; then
    ok "a refused strictness prints no mode"
else
    bad "a refused strictness printed a mode"
fi

# The cell's renewal and restart checks over a scripted server log: docker
# and sleep are stubs, and each stub appends the lines the server would
# have logged by then (#690 D7, D8).
ACK1="dnsmasq-dhcp[1]: 555 DHCPACK(cm-mvp) 10.98.1.50 $M1"
ACK2="dnsmasq-dhcp[1]: 555 DHCPACK(cm-mvp) 10.98.1.51 $M2"
REQ1="dnsmasq-dhcp[1]: 555 DHCPREQUEST(cm-mvp) 10.98.1.50 $M1"
DIS1="dnsmasq-dhcp[1]: 555 DHCPDISCOVER(cm-mvp) $M1"
# drive <label> <exit> <output-fragment> <check> [args]; PRE, ON_STOP,
# ON_SLEEP1 (the first sleep) and ON_START hold the scripted lines; STOP_RC
# is the stub stop's exit.
drive() {
    local label="$1" want="$2" frag="$3"; shift 3
    out="$(
        DLOG="$tmp/drive.log" tick=0
        : > "$DLOG"
        put() { [ $# -eq 0 ] || printf '%s\n' "$@" >> "$DLOG"; }
        put "${PRE[@]}"
        # shellcheck source=scripts/capability-checks.sh
        . "$HERE/capability-checks.sh"
        say() { printf '%s\n' "$*"; }
        mac_of() { echo "$M1"; }
        attached() { say "attached $*"; }
        docker() { case "$1" in stop) put "${ON_STOP[@]}"; return "$STOP_RC" ;; start) put "${ON_START[@]}" ;; esac; }
        sleep() { tick=$((tick + 1)); [ "$tick" -ne 1 ] || put "${ON_SLEEP1[@]}"; }
        "$@" 2>&1
    )"
    got=$?
    expect "$label" "$want" "$frag"
}
PRE=() ON_STOP=("$ACK1") ON_SLEEP1=() ON_START=("$REQ1" "$ACK1") STOP_RC=0
drive "restart passes on a quiet stop and an ACK after the start" 0 "attached cm-c-mv" restarted
STOP_RC=1
drive "restart refuses a refused stop" 1 "stop refused" restarted
STOP_RC=0
ON_SLEEP1=("$ACK1")
drive "restart refuses an ACK for the MAC while stopped" 1 "acknowledged while stopped" restarted
ON_SLEEP1=("$ACK2")
drive "restart ignores another MAC's ACK while stopped" 0 "attached cm-c-mv" restarted
ON_SLEEP1=() ON_START=()
drive "restart refuses a start with no ACK after it" 1 "no ACK after the start" restarted
ON_START=("$REQ1")
drive "restart refuses a REQUEST with no ACK after the start" 1 "no ACK after the start" restarted
PRE=("$ACK1") ON_STOP=() ON_SLEEP1=("$REQ1" "$ACK1") ON_START=()
drive "renewal passes on an ACK after the mark with no DISCOVER" 0 "renewal ACK seen" renewed cm-c-mv "$M1"
ON_SLEEP1=("$DIS1" "$ACK1")
drive "renewal refuses a new DISCOVER before the ACK" 1 "a new DHCPDISCOVER" renewed cm-c-mv "$M1"
ON_SLEEP1=()
drive "renewal refuses an ACK logged before the check started" 1 "no renewal ACK within 30 s" renewed cm-c-mv "$M1"
ON_SLEEP1=("$REQ1")
drive "renewal refuses a REQUEST with no ACK" 1 "no renewal ACK within 30 s" renewed cm-c-mv "$M1"
ON_SLEEP1=("$ACK2")
drive "renewal refuses another MAC's ACK" 1 "no renewal ACK within 30 s" renewed cm-c-mv "$M1"

# The attach check over a scripted container: docker is a stub answering
# inspect, the container's ip output (all links, or one by name) and the
# MAC of one link and lo. Bridge mode names the link after the bridge, which
# the draft run 37294535723 read as a failed attach on eth0 (#690).
attach_case() {
    local label="$1" want="$2" frag="$3"; shift 3
    out="$(
        LEASES="$tmp/attach.leases"
        printf '%s\n' "${LEASE_LINES[@]}" > "$LEASES"
        # shellcheck source=scripts/capability-checks.sh
        . "$HERE/capability-checks.sh"
        say() { printf '%s\n' "$*"; }
        docker() {
            case "$1" in
                inspect) echo "$RUNNING" ;;
                exec)
                    shift 4
                    case "$*" in
                        "ip -4 -o addr show") printf '%s\n' "${IP_LINES[@]}" ;;
                        "ip -4 -o addr show dev "*)
                            printf '%s\n' "${IP_LINES[@]}" | awk -v l="${*: -1}" '$2 == l {f=1; print} END {exit !f}' ;;
                        "cat /sys/class/net/$LINK/address") echo "$LINK_MAC" ;;
                        "cat /sys/class/net/lo/address") echo 00:00:00:00:00:00 ;;
                        *) return 1 ;;
                    esac ;;
                *) return 1 ;;
            esac
        }
        "$@" 2>&1
    )"
    got=$?
    expect "$label" "$want" "$frag"
}
MB=ae:72:73:22:2f:20
LO_L='1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever'
BR_L='7: cm-br00    inet 10.98.2.74/24 brd 10.98.2.255 scope global cm-br00\       valid_lft forever preferred_lft forever'
MV_L='9: eth0    inet 10.98.1.50/24 brd 10.98.1.255 scope global eth0\       valid_lft forever preferred_lft forever'
FAR_L='7: cm-br00    inet 110.98.2.74/24 brd 110.98.2.255 scope global cm-br00\       valid_lft forever preferred_lft forever'
RUNNING=true LINK=cm-br00 LINK_MAC=$MB IP_LINES=("$LO_L" "$BR_L") LEASE_LINES=("1 $MB 10.98.2.74 c1 *")
attach_case "attach passes on a bridge-mode link named after the bridge" 0 "cm-br00 $MB -> 10.98.2.74, leased" attached cm-c-bridge 10.98.2.
attach_case "the MAC is read from the link carrying the address" 0 "$MB" mac_of cm-c-bridge 10.98.2.
LEASE_LINES=()
attach_case "attach refuses an address the lease file does not hold" 1 "lease file holds no $MB -> 10.98.2.74" attached cm-c-bridge 10.98.2.
LEASE_LINES=("1 $MB 10.98.2.75 c1 *")
attach_case "attach refuses a lease for that MAC on another address" 1 "lease file holds no" attached cm-c-bridge 10.98.2.
LEASE_LINES=("1 $M1 10.98.2.74 c1 *")
attach_case "attach refuses a lease for that address under another MAC" 1 "lease file holds no" attached cm-c-bridge 10.98.2.
LEASE_LINES=("1 $MB 110.98.2.74 c1 *") IP_LINES=("$LO_L" "$FAR_L")
attach_case "attach refuses an address that only contains the prefix" 1 "no 10.98.2. address on any link" attached cm-c-bridge 10.98.2.
attach_case "mac_of refuses a container with no address under the prefix" 1 "" mac_of cm-c-bridge 10.98.2.
IP_LINES=("$LO_L" "$BR_L")
attach_case "mac_of refuses an empty prefix" 1 "" mac_of cm-c-bridge ""
LEASE_LINES=("1 $MB 10.98.2.74 c1 *") IP_LINES=("$LO_L" "$BR_L") LINK=eth1
attach_case "attach refuses when the address's own link has no MAC" 1 "no MAC on cm-br00" attached cm-c-bridge 10.98.2.
LINK=cm-br00 RUNNING=false
attach_case "attach refuses a container that is not running" 1 "is not running" attached cm-c-bridge 10.98.2.
RUNNING=true LINK=eth0 LINK_MAC=$M1 IP_LINES=("$LO_L" "$MV_L") LEASE_LINES=("1 $M1 10.98.1.50 c1 *")
attach_case "attach passes on a macvlan link named eth0" 0 "eth0 $M1 -> 10.98.1.50, leased" attached cm-c-mv 10.98.1.
attach_case "attach refuses a macvlan address under the bridge prefix" 1 "no 10.98.2. address" attached cm-c-mv 10.98.2.

# The cell prints the keys this script declares, so neither can drift alone.
keys="$(sed -n 's/.*for k in "\${\(COLUMNS\)\[@\]}".*/\1/p' "$CELL")"
src="$(grep -c 'capability-matrix.sh" --columns' "$CELL")"
if [ "$keys" = COLUMNS ] && [ "$src" -eq 1 ]; then
    ok "capability-cell.sh takes its row keys from --columns"
else
    bad "capability-cell.sh row keys are not read from capability-matrix.sh --columns"
fi

# The draft state is read live, not taken from the payload (#690, run
# 37301682056: the payload said draft after the ready click). gh is stubbed
# to answer STUB_GH_OUT with STUB_GH_RC and to record its arguments.
mkdir -p "$tmp/bin"
cat > "$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$GH_ARGS"
printf '%s' "${STUB_GH_OUT-}"
exit "${STUB_GH_RC:-0}"
STUB
chmod +x "$tmp/bin/gh"
# live_case <label> <event> <live-answer> <gh-rc> <want-exit> <want-output>
live_case() {
    : > "$tmp/gh.args"
    out="$(PATH="$tmp/bin:$PATH" GH_ARGS="$tmp/gh.args" STUB_GH_OUT="$3" STUB_GH_RC="$4" \
        bash "$GATE" --draft-now "$2" owner/repo 7 2>&1)"; got=$?
    if [ "$got" -eq "$5" ] && { [ "$5" -ne 0 ] || [ "$out" = "$6" ]; }; then ok "$1"; else bad "$1: exit $got want $5, output '$out'"; fi
}
live_case "a pull request that is ready now is not a draft" pull_request false 0 0 false
live_case "a pull request that is a draft now is a draft" pull_request true 0 0 true
if grep -qx 'api repos/owner/repo/pulls/7 --jq .draft' "$tmp/gh.args"; then ok "the live read asks the pull request's own endpoint"; else bad "live read asked: $(cat "$tmp/gh.args")"; fi
live_case "a refused read cannot be judged" pull_request true 1 2 ""
live_case "an empty answer cannot be judged" pull_request "" 0 2 ""
live_case "a null answer cannot be judged" pull_request null 0 2 ""
live_case "a push is never a draft and asks nothing" push true 1 0 false
if [ ! -s "$tmp/gh.args" ]; then ok "a push does not call gh"; else bad "a push called gh: $(cat "$tmp/gh.args")"; fi
# The race itself: the strictness handed the live answer, not the payload's.
live="$(PATH="$tmp/bin:$PATH" GH_ARGS="$tmp/gh.args" STUB_GH_OUT=false bash "$GATE" --draft-now pull_request owner/repo 7)"
out="$(bash "$GATE" --strictness pull_request refs/pull/7/merge "$live" 2>&1)"; got=$?
if [ "$got" -eq 0 ] && [ "$out" = strict ]; then ok "ready after the event fired: the run is strict"; else bad "ready race: exit $got '$out'"; fi
live="$(PATH="$tmp/bin:$PATH" GH_ARGS="$tmp/gh.args" STUB_GH_OUT=true bash "$GATE" --draft-now pull_request owner/repo 7)"
out="$(bash "$GATE" --strictness pull_request refs/pull/7/merge "$live" 2>&1)"; got=$?
if [ "$got" -eq 0 ] && [ "$out" = lenient ]; then ok "converted back to a draft: the run is lenient"; else bad "draft twin: exit $got '$out'"; fi
out="$(bash "$GATE" --draft-now pull_request owner/repo x 2>&1)"; got=$?
expect "a pull request number that is not a number cannot be read" 2 "--draft-now"

# The workflow takes the draft state from --draft-now and never from the payload.
WF="$HERE/../.github/workflows/capability-matrix.yml"
if grep -q 'pull_request\.draft' "$WF"; then bad "the workflow reads the draft state from the event payload"; else ok "the workflow does not read the draft state from the event payload"; fi
wf_ok=1
for want in '--draft-now "\$EVENT" "\$REPO" "\$PR")" || exit 2' 'pull-requests: read' 'GH_TOKEN: \${{ github.token }}' \
    'REPO: \${{ github.repository }}' 'PR: \${{ github.event.pull_request.number'; do
    grep -q -- "$want" "$WF" || { wf_ok=0; echo "missing in the workflow: $want"; }
done
if [ "$wf_ok" -eq 1 ]; then ok "the reconcile step reads the draft state live, with the token, repo, number and permission to"; else bad "the workflow does not read the draft state live"; fi

# The shipped table parses and names every cell of the shipped config.json.
d="$tmp/shipped"; mkdir -p "$d"
mapfile -t cells < <(bash "$GATE" --cells-json | jq -r '.[]')
for c in "${cells[@]}"; do printf 'CAP_MATRIX_ROW removed=%s result=ok\n' "$c" > "$d/$c.row"; done
out="$(bash "$GATE" --reconcile "$d" 2>&1)"; got=$?
if [ "$got" -eq 1 ] && ! printf '%s' "$out" | grep -E 'no single row|names no cell|table header|no capability-matrix block' >/dev/null; then
    ok "the shipped docs table has one row per cell of the shipped config.json"
else
    bad "shipped table: exit $got, output: $(printf '%s' "$out" | grep -E 'no single row|names no cell|table header|block' | tr '\n' ' ')"
fi

echo
echo "capability-matrix self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
