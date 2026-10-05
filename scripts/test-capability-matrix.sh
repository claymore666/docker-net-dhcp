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

# The cell prints the keys this script declares, so neither can drift alone.
keys="$(sed -n 's/.*for k in "\${\(COLUMNS\)\[@\]}".*/\1/p' "$CELL")"
src="$(grep -c 'capability-matrix.sh" --columns' "$CELL")"
if [ "$keys" = COLUMNS ] && [ "$src" -eq 1 ]; then
    ok "capability-cell.sh takes its row keys from --columns"
else
    bad "capability-cell.sh row keys are not read from capability-matrix.sh --columns"
fi

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
