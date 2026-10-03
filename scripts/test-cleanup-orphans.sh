#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for test/integration/cleanup-orphans.sh, two phases, one verdict.
# Phase A: the state-record phase (#1174, #1165) against a stubbed engine, on a copy
# with the root check cut out (asserted, so a changed check cannot pass vacuously).
# Phase B: the process sweep (#1147) against fake survivors inside a user and pid
# namespace; it skips alone, with a reason, where no namespace can be made.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SWEEP="$REPO/test/integration/cleanup-orphans.sh"
guarded_tmpdir TMP
failures=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; failures=$((failures + 1)); }

# ---- phase A: state records (#1174, #1165) ----

BIN="$TMP/bin"
mkdir -p "$BIN"
# STUB_NETS: ids `docker network ls --no-trunc -q` prints; STUB_LS_EXIT: its exit status.
cat > "$BIN/docker" <<'STUB'
#!/usr/bin/env bash
if [ "$1 $2" = "network ls" ]; then
    case " $* " in
    *" --no-trunc "*)
        cat "$STUB_NETS"
        exit "${STUB_LS_EXIT:-0}"
        ;;
    esac
fi
exit 0
STUB
for tool in ip iptables pgrep kill sleep; do
    printf '#!/usr/bin/env bash\nexit 1\n' > "$BIN/$tool"
done
chmod +x "$BIN"/*

# The copy must differ from the original by the root check and nothing else
# that matters: if the cut matches nothing, the cases below would run the
# refusal path and pass or fail for the wrong reason.
COPY="$TMP/cleanup-orphans.sh"
# shellcheck disable=SC2016  # the pattern is for sed, not for this shell
sed '/^if \[\[ \$EUID -ne 0 \]\]; then$/,/^fi$/d' "$SWEEP" > "$COPY"
if grep -qF 'EUID' "$COPY" || cmp -s "$SWEEP" "$COPY"; then
    fail "the root check was not cut from the copy; the cases would test the refusal"
    echo "$failures failure(s)"; exit 1
fi

hex() { printf '%064d' 0 | tr 0 "$1"; }
A=$(hex a); B=$(hex b); C=$(hex c)
LIVE_PREFIX_TWIN="${A:0:12}$(printf '%052d' 0 | tr 0 d)"

# go LS_EXIT STATE_DIR: runs the copy against $TMP/nets; output in $TMP/out, status in $RC
go() {
    STUB_LS_EXIT="$1" STUB_NETS="$TMP/nets" NET_DHCP_STATE_DIR="$2" \
        PATH="$BIN:$PATH" bash "$COPY" > "$TMP/out" 2>&1
    RC=$?
}

verdict() {
    local name="$1" ok="$2"
    if [ "$ok" = 1 ]; then
        pass "$name"
    else
        fail "$name"; sed 's/^/    /' "$TMP/out"
    fi
}
absent() { [ ! -e "$STATE/$1" ]; }
present() { [ -e "$STATE/$1" ]; }
# the lines the phase printed: between its header and the next one
phase_lines() { awk -v h="$HEADER" '$0 == h {on=1; next} /^===/ {on=0} on' "$TMP/out"; }
said() { grep -qF -- "$1" "$TMP/out"; }

HEADER='=== removing plugin state records of networks the engine no longer has ==='

# (a) a record whose network the engine does not list is removed and reported
STATE="$TMP/state-stale"; mkdir -p "$STATE"
echo '{}' > "$STATE/$A.json"
printf '%s\n' "$B" > "$TMP/nets"
go 0 "$STATE"
verdict "a record of a network the engine does not list is removed and reported" \
    "$(absent "$A.json" && said "removed $A.json (network gone)" && said "$HEADER" && [ "$RC" -eq 0 ] && echo 1 || echo 0)"

# (b) a record of a listed network stays; (c) neighbours stay; (g) near-misses stay
STATE="$TMP/state-mixed"; mkdir -p "$STATE"
echo '{}' > "$STATE/$B.json"
echo '{}' > "$STATE/$A.json"
echo '{}' > "$STATE/tombstones.json"
echo '{}' > "$STATE/lease-records.jsonl"
echo '{}' > "$STATE/leases.jsonl"
echo '{}' > "$STATE/lock"
echo '{}' > "$STATE/${C:0:63}.json"
echo '{}' > "$STATE/$(hex A).json"
echo '{}' > "$STATE/$C.json.tmp"
mkdir "$STATE/$(hex e).json"
printf '%s\n' "$B" > "$TMP/nets"
go 0 "$STATE"
verdict "a record of a listed network stays" "$(present "$B.json" && echo 1 || echo 0)"
verdict "the stale record next to it is removed" "$(absent "$A.json" && echo 1 || echo 0)"
verdict "tombstones, lease records, leases and the lock stay" \
    "$(present tombstones.json && present lease-records.jsonl && present leases.jsonl && present lock && echo 1 || echo 0)"
verdict "a 63-hex name, an uppercase name, a .tmp name and a directory stay" \
    "$(present "${C:0:63}.json" && present "$(hex A).json" && present "$C.json.tmp" && present "$(hex e).json" && echo 1 || echo 0)"

# (f) a stale record sharing the 12-character prefix of a live one is not protected by it
STATE="$TMP/state-prefix"; mkdir -p "$STATE"
echo '{}' > "$STATE/$A.json"
echo '{}' > "$STATE/$LIVE_PREFIX_TWIN.json"
printf '%s\n' "$A" > "$TMP/nets"
go 0 "$STATE"
verdict "a live id protects only its own record, not a record with the same 12-character prefix" \
    "$(present "$A.json" && absent "$LIVE_PREFIX_TWIN.json" && echo 1 || echo 0)"

# (d) the engine is unreachable: nothing is removed and the phase says why
STATE="$TMP/state-down"; mkdir -p "$STATE"
echo '{}' > "$STATE/$A.json"
: > "$TMP/nets"
go 1 "$STATE"
verdict "an unreachable engine removes nothing and says so" \
    "$(present "$A.json" && said "  engine unreachable; state records left alone" && [ "$RC" -eq 0 ] && echo 1 || echo 0)"

# (e) no state directory, engine down as well: the phase has nothing to say and the script ends well
: > "$TMP/nets"
go 1 "$TMP/no-such-dir"
verdict "a missing state directory ends cleanly with no line after the header" \
    "$([ "$RC" -eq 0 ] && said "$HEADER" && [ -z "$(phase_lines)" ] && echo 1 || echo 0)"

# the phase sits after the network removal and before the namespace sweep
order_ok=1
h1=$(grep -n -F '=== removing dh-itest-* networks ===' "$SWEEP" | cut -d: -f1)
h2=$(grep -n -F "$HEADER" "$SWEEP" | cut -d: -f1)
h3=$(grep -n -F '=== removing dh-itest-* network namespaces ===' "$SWEEP" | cut -d: -f1)
if [ -z "$h1" ] || [ -z "$h2" ] || [ -z "$h3" ] || ! [ "$h1" -lt "$h2" ] || ! [ "$h2" -lt "$h3" ]; then order_ok=0; fi
verdict "the phase runs after the network removal and before the namespace sweep" "$order_ok"

# the process section runs before the first removal, or a dying suite deletes the links again (#1147)
k=$(grep -n -F '=== killing the previous job' "$SWEEP" | cut -d: -f1)
c=$(grep -n -F '=== removing dh-itest-* containers ===' "$SWEEP" | cut -d: -f1)
if [ -n "$k" ] && [ -n "$c" ] && [ "$k" -lt "$c" ]; then order_ok=1; else order_ok=0; fi
verdict "the process section runs before the first removal" "$order_ok"

# ---- phase B: process sweep (#1147) ----
phase_b() {
for tool in pgrep ps unshare; do
    command -v "$tool" >/dev/null 2>&1 || { echo "SKIP: $tool is not installed"; return 0; }
done
# docker exits 1, so the sweep reads "engine unreachable" and leaves any real state
# directory alone (#1174); a namespace shares the host filesystem (#1147).
mkdir -p "$TMP/nsbin"
printf '#!/bin/sh\nexit 1\n' > "$TMP/nsbin/docker"
printf '#!/bin/sh\nexit 0\n' > "$TMP/nsbin/ip"
printf '#!/bin/sh\nexit 1\n' > "$TMP/nsbin/iptables"
chmod +x "$TMP/nsbin/"*

# Unprivileged namespaces first; passwordless sudo where the distribution forbids them.
ns=()
if unshare -Urpf --mount-proc true 2>/dev/null; then
    ns=(unshare -Urpf --mount-proc)
elif sudo -n unshare -pf --mount-proc true 2>/dev/null; then
    ns=(sudo -n unshare -pf --mount-proc)
else
    echo "SKIP: neither an unprivileged user+pid namespace nor passwordless sudo is available"
    return 0
fi

cat > "$TMP/inner.sh" <<'INNER'
#!/usr/bin/env bash
# Runs inside the namespace: start fakes, run the sweep, print facts. $1 sweep, $2 mode, $3 stubs, $4 workdir.
sweep=$1 mode=$2 stubs=$3 work=$4
export PATH="$stubs:$PATH"
surv=() decoy=()
fake() { # ARGV0 [ign]
    if [[ "${2:-}" == ign ]]; then bash -c 'trap "" TERM; exec -a "$0" sleep 300' "$1" & else bash -c 'exec -a "$0" sleep 300' "$1" & fi
    last=$!
}
case $mode in
    full)
        fake "/tmp/go-build9/b001/integration.test -test.v" ign; surv+=("$last")
        fake "/tmp/go-build9/b001/integration.test -test.v"; surv+=("$last")
        fake "go test -v -tags integration -count=1 ./test/integration/"; surv+=("$last")
        fake "make integration-test-shard SHARD=1 OF=2"; surv+=("$last")
        fake "dnsmasq --interface=dh-itest-br2 --dhcp-range=x"; surv+=("$last")
        fake "kea-dhcp4 -c /tmp/dh-itest-ephemeral-77/kea-dhcp4.json"; surv+=("$last")
        fake "kea-dhcp6 -c /tmp/dh-itest-ephemeral-78/kea-dhcp6.json"; surv+=("$last") ;;
    nokill)
        fake "/tmp/go-build9/b001/integration.test -test.v"; surv+=("$last") ;;
esac
for d in "/home/runner/bin/Runner.Worker spawnclient 1 2" "bash test/integration/cleanup-orphans-notes.sh" \
         "/usr/sbin/dnsmasq --interface=eth0" "kea-dhcp4 -c /etc/kea/kea-dhcp4.conf" \
         "/tmp/go-build9/b001/harness.test -test.v" "make integration-cleanup" "go build ./cmd/net-dhcp"; do
    fake "$d"; decoy+=("$last")
done
sleep 0.5
alive() { local s; s=$(ps -o stat= -p "$1" 2>/dev/null); [[ -n "$s" && "$s" != Z* ]]; }
envp=()
[[ $mode == nokill ]] && { echo 'kill() { :; }' > "$work/nokill.env"; envp=(BASH_ENV="$work/nokill.env"); }
# The wrapper's command line matches `go test ... -tags integration`: it is an ancestor and must survive.
env "${envp[@]}" bash -c 'bash "$1" > "$2" 2>&1 < /dev/null; echo $? > "$3"; : > "$4"' \
    "go test -v -tags integration" "$sweep" "$work/out" "$work/rc" "$work/wrapper-alive"
n=0; for p in "${surv[@]}"; do alive "$p" && n=$((n + 1)); done
m=0; for p in "${decoy[@]}"; do alive "$p" && m=$((m + 1)); done
echo "RC=$(cat "$work/rc" 2>/dev/null)"
echo "SURV_TOTAL=${#surv[@]}"
echo "SURV_ALIVE=$n"
echo "DECOY_ALIVE=$m"
echo "WRAPPER=$([[ -e "$work/wrapper-alive" ]] && echo yes || echo no)"
INNER

run() { # NAME SWEEP MODE
    mkdir -p "$TMP/$1"
    "${ns[@]}" bash "$TMP/inner.sh" "$2" "$3" "$TMP/nsbin" "$TMP/$1" > "$TMP/$1/facts" 2>&1 < /dev/null
}
fact() { grep -m1 "^$2=" "$TMP/$1/facts" | cut -d= -f2; }
has() { grep -qE -- "$2" "$TMP/$1/out" 2>/dev/null; }

# A mutant: line LINE of the sweep either deleted (no replacement) or replaced.
mutant() { # NAME FIXED-STRING [REPLACEMENT]
    local n
    n=$(grep -nF -- "$2" "$SWEEP" | cut -d: -f1)
    if [[ -z "$n" || "$n" == *$'\n'* ]]; then
        fail "mutation target for $1 is not exactly one line in the sweep"
        return 1
    fi
    { head -n $((n - 1)) "$SWEEP"; [[ $# -ge 3 ]] && printf '%s\n' "$3"; tail -n +$((n + 1)) "$SWEEP"; } > "$TMP/$1.sh"
    bash -n "$TMP/$1.sh" || { fail "mutant $1 does not parse"; return 1; }
}

mutant mut-nokill 'kill -KILL $pids 2>/dev/null' || true
mutant mut-exactname "'(^|/)integration\\.test( |\$)'" "    '^integration\\.test\$'" || true
mutant mut-noprotect '[[ " $protected " == *" $p "* ]] && continue' || true

run control "$SWEEP" full &
run none "$SWEEP" none &
run survives-kill "$SWEEP" nokill &
[[ -f "$TMP/mut-nokill.sh" ]] && run mut-nokill "$TMP/mut-nokill.sh" full &
[[ -f "$TMP/mut-exactname.sh" ]] && run mut-exactname "$TMP/mut-exactname.sh" full &
[[ -f "$TMP/mut-noprotect.sh" ]] && run mut-noprotect "$TMP/mut-noprotect.sh" full &
wait

if [[ "$(fact control RC)" == 0 && "$(fact control SURV_TOTAL)" == 7 && "$(fact control SURV_ALIVE)" == 0 ]]; then
    pass "seven survivors, one ignoring TERM, are all gone and the sweep exits 0"
else
    fail "control: $(tr '\n' ' ' < "$TMP/control/facts")"
fi
[[ "$(fact control DECOY_ALIVE)" == 7 && "$(fact control WRAPPER)" == yes ]] \
    && pass "seven decoys and the calling wrapper whose command line matches go test are spared" \
    || fail "control spared: $(tr '\n' ' ' < "$TMP/control/facts")"
[[ "$(grep -c 'found pid=' "$TMP/control/out")" == 7 ]] && has control 'age=[0-9]+s' \
    && pass "each of the seven is reported with pid and age" || fail "control did not report seven found lines"
[[ "$(fact none RC)" == 0 && "$(fact none DECOY_ALIVE)" == 7 ]] && has none '^  none$' \
    && pass "nothing to kill: prints none, exits 0, decoys spared" || fail "none: $(tr '\n' ' ' < "$TMP/none/facts")"
[[ "$(fact survives-kill RC)" == 1 && "$(fact survives-kill SURV_ALIVE)" == 1 ]] && has survives-kill 'SURVIVOR pid=' \
    && pass "a process that survives KILL is printed and the sweep exits 1" || fail "survives-kill: $(tr '\n' ' ' < "$TMP/survives-kill/facts")"

[[ "$(fact mut-nokill RC)" == 1 && "$(fact mut-nokill SURV_ALIVE)" == 1 ]] \
    && pass "mutant without the KILL step is caught: the TERM-ignoring survivor stays and the exit is 1" \
    || fail "mutant without KILL was not caught: $(tr '\n' ' ' < "$TMP/mut-nokill/facts" 2>/dev/null)"
[[ "$(fact mut-exactname RC)" == 0 && "$(fact mut-exactname SURV_ALIVE)" -ge 2 ]] \
    && pass "mutant matching the bare binary name (the pgrep -x shape) is caught: both test binaries stay, exit 0" \
    || fail "mutant with the bare name was not caught: $(tr '\n' ' ' < "$TMP/mut-exactname/facts" 2>/dev/null)"
[[ "$(fact mut-noprotect WRAPPER)" == no ]] \
    && pass "mutant without the ancestor exclusion is caught: it kills its own caller" \
    || fail "mutant without the ancestor exclusion was not caught: $(tr '\n' ' ' < "$TMP/mut-noprotect/facts" 2>/dev/null)"
}
phase_b

if [ "$failures" -ne 0 ]; then echo "$failures check(s) failed"; exit 1; fi
echo "all cleanup-orphans checks passed"
