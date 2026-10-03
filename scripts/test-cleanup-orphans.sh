#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for the state-record phase of test/integration/cleanup-orphans.sh
# (#1174, #1165). The script refuses to run as non-root, so the test runs a
# copy with that check cut out (asserted, so a changed check cannot make the
# cases pass vacuously). docker, ip, iptables, pgrep, kill and sleep are
# stubs: nothing here touches the host's engine, interfaces or processes.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$REPO/test/integration/cleanup-orphans.sh"
guarded_tmpdir TMP
failures=0

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
sed '/^if \[\[ \$EUID -ne 0 \]\]; then$/,/^fi$/d' "$SCRIPT" > "$COPY"
if grep -qF 'EUID' "$COPY" || cmp -s "$SCRIPT" "$COPY"; then
    echo "FAIL: the root check was not cut from the copy; the cases would test the refusal"
    exit 1
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
        echo "PASS: $name"
    else
        echo "FAIL: $name"; sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
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
h1=$(grep -n -F '=== removing dh-itest-* networks ===' "$SCRIPT" | cut -d: -f1)
h2=$(grep -n -F "$HEADER" "$SCRIPT" | cut -d: -f1)
h3=$(grep -n -F '=== removing dh-itest-* network namespaces ===' "$SCRIPT" | cut -d: -f1)
if [ -z "$h1" ] || [ -z "$h2" ] || [ -z "$h3" ] || ! [ "$h1" -lt "$h2" ] || ! [ "$h2" -lt "$h3" ]; then order_ok=0; fi
verdict "the phase runs after the network removal and before the namespace sweep" "$order_ok"

if [ "$failures" -ne 0 ]; then echo "$failures failure(s)"; exit 1; fi
echo "all passed"
