#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for capability-matrix.sh (#690).
#
# THE CASE THAT CARRIES THE WEIGHT is `create-fails`. A variant whose
# plugin will not build or enable produces the same-looking output as a
# variant whose capability is genuinely required -- five FAILs in a
# column -- and the second is the answer this job exists to find. A run
# that recorded the first as the second would report a discovery about
# capabilities from a broken build, and the recorded table would then be
# the thing everything else is judged against. It must exit 2.
#
# THE OTHER LOAD-BEARING ONE is `derives-variants`: adding a capability
# to the manifest must grow the matrix by itself. A typed variant list
# would leave a newly granted capability unmeasured while the job kept
# reporting a clean pass -- the shape where a gate's domain shrinks and
# nothing goes red.
#
# Everything here drives the shipped script through its seams. Nothing
# reimplements its loop.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
SUT=$HERE/capability-matrix.sh
D=$(mktemp -d); trap 'rm -rf "$D"' EXIT

pass=0; fail=0
eq()  { if [ "$2" = "$3" ]; then echo "ok   $1"; pass=$((pass+1)); else echo "FAIL $1 (want '$3', got '$2')"; fail=$((fail+1)); fi; }
has() { if grep -F "$3" "$2" > /dev/null; then echo "ok   $1"; pass=$((pass+1)); else echo "FAIL $1 (wanted: $3)"; sed 's/^/    /' "$2"; fail=$((fail+1)); fi; }
hasnt() { if grep -F "$3" "$2" > /dev/null; then echo "FAIL $1 (must NOT contain: $3)"; fail=$((fail+1)); else echo "ok   $1"; pass=$((pass+1)); fi; }

SCEN=(TestLifecycleBridge_GoldenPath TestLifecycleMacvlan_GoldenPath
      TestNonRootContainer_PersistentClientStarts TestLeaseRenew_HonorsT1
      TestTombstoneRestart_PreservesMACAndIP)

# --- fixtures ----------------------------------------------------------
mkcfg() { # $1 = file, rest = capabilities
    local f="$1"; shift
    python3 - "$f" "$@" <<'PY'
import json, sys
json.dump({"description": "fake", "linux": {"capabilities": sys.argv[2:]}}, open(sys.argv[1], "w"))
PY
}
mkdir -p "$D/testdir" "$D/rootfs"
{ echo "package integration"; for s in "${SCEN[@]}"; do echo "func $s(t *testing.T) {}"; done; } > "$D/testdir/fake_test.go"
mkcfg "$D/rootfs/config.json" CAP_NET_ADMIN CAP_SYS_ADMIN CAP_SYS_PTRACE
cp "$D/rootfs/config.json" "$D/config.json"

# A docker that records what it was asked and succeeds, unless asked to
# fail `plugin create` for one named variant.
cat > "$D/docker" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$DOCKER_CALLS"
if [ "${1:-}" = "plugin" ] && [ "${2:-}" = "create" ] && [ -n "${CREATE_FAILS_FOR:-}" ]; then
    case "$3" in *"$CREATE_FAILS_FOR") exit 1 ;; esac
fi
exit 0
EOF
chmod +x "$D/docker"

# A runner whose verdict comes from a table keyed on <variant>/<test>.
# Default PASS; anything listed in $RUNNER_FAILS fails; anything in
# $RUNNER_WEIRD returns a third status.
cat > "$D/runner" <<'EOF'
#!/usr/bin/env bash
ref="$1"; test="$2"; v="${ref#*:}"
case " ${RUNNER_WEIRD:-} " in *" $v/$test "*) exit 7 ;; esac
case " ${RUNNER_FAILS:-} " in *" $v/$test "*) exit 1 ;; esac
exit 0
EOF
chmod +x "$D/runner"

run() { # -> exit code; output in $D/out
    DOCKER_CALLS="$D/calls" \
    CAPMATRIX_DOCKER="$D/docker" CAPMATRIX_RUNNER="$D/runner" \
    CAPMATRIX_CONFIG="${CFG:-$D/config.json}" CAPMATRIX_EXPECT="${EXP:-$D/expect.txt}" \
    CAPMATRIX_TESTDIR="$D/testdir" CAPMATRIX_ROOTFS="$D/rootfs" \
    bash "$SUT" > "$D/out" 2>&1
    echo $?
}

# --- record mode, then the round trip ----------------------------------
: > "$D/calls"
rm -f "$D/expect.txt"
X=$(CAPMATRIX_RECORD=1 RUNNER_FAILS="minus-CAP_SYS_PTRACE/TestNonRootContainer_PersistentClientStarts" run)
eq  "record: exit 0" "$X" "0"
has "record: says what it wrote" "$D/out" "recorded 20 cell(s)"
eq  "record: 20 data rows" "$(grep -cvE '^[[:space:]]*(#|$)' "$D/expect.txt")" "20"
has "record: marks the file as measured" "$D/expect.txt" "MEASURED by scripts/capability-matrix.sh, not predicted"

X=$(RUNNER_FAILS="minus-CAP_SYS_PTRACE/TestNonRootContainer_PersistentClientStarts" run)
eq  "match: exit 0" "$X" "0"
has "match: says all cells match" "$D/out" "20 cell(s), all as recorded"

# --- a cell diverges ---------------------------------------------------
X=$(RUNNER_FAILS="minus-CAP_SYS_PTRACE/TestNonRootContainer_PersistentClientStarts minus-CAP_NET_ADMIN/TestLeaseRenew_HonorsT1" run)
eq  "diverge: exit 1" "$X" "1"
has "diverge: names the cell" "$D/out" "minus-CAP_NET_ADMIN/TestLeaseRenew_HonorsT1"
has "diverge: gives both readings" "$D/out" "measured FAIL, recorded PASS"
has "diverge: forbids editing the file to pass" "$D/out" "Do not update it to make this pass."

# The other direction: a capability that USED to be required no longer
# is. The good-news direction still has to fail, or the file rots into a
# record of what someone once believed.
X=$(run)
eq  "diverge-good-news: exit 1" "$X" "1"
has "diverge-good-news: names it" "$D/out" "measured PASS, recorded FAIL"

# --- a variant that will not install -----------------------------------
# The load-bearing case. Five FAILs in a column is the answer this job
# looks for; a failed create must not be able to produce it.
X=$(CREATE_FAILS_FOR="minus-CAP_SYS_ADMIN" RUNNER_FAILS="minus-CAP_SYS_PTRACE/TestNonRootContainer_PersistentClientStarts" run)
eq    "create-fails: exit 2" "$X" "2"
has   "create-fails: names the variant" "$D/out" "variant 'minus-CAP_SYS_ADMIN' could not be created"
has   "create-fails: says why it is not a finding" "$D/out" "recording it as a row of FAILs would turn a broken build into a finding"
hasnt "create-fails: records nothing" "$D/out" "all as recorded"

# --- a scenario that returns neither pass nor fail ----------------------
X=$(RUNNER_WEIRD="full/TestLeaseRenew_HonorsT1" run)
eq  "third-outcome: exit 2" "$X" "2"
has "third-outcome: refuses to guess" "$D/out" "A third outcome recorded as either one is a guess."

# --- the variants are DERIVED, not typed -------------------------------
# A fourth capability in the manifest must grow the matrix by itself.
mkcfg "$D/cfg4.json" CAP_NET_ADMIN CAP_SYS_ADMIN CAP_SYS_PTRACE CAP_DAC_OVERRIDE
cp "$D/cfg4.json" "$D/rootfs/config.json"
X=$(CFG="$D/cfg4.json" EXP="$D/expect4.txt" CAPMATRIX_RECORD=1 run)
eq  "derives-variants: exit 0" "$X" "0"
has "derives-variants: 5 variants x 5 scenarios" "$D/out" "recorded 25 cell(s)"
has "derives-variants: measured the new capability" "$D/expect4.txt" "minus-CAP_DAC_OVERRIDE"
# ...and against the THREE-capability expectation the four-capability
# manifest must not quietly pass on the twenty cells they share.
X=$(CFG="$D/cfg4.json" RUNNER_FAILS="minus-CAP_SYS_PTRACE/TestNonRootContainer_PersistentClientStarts" run)
eq  "derives-variants: extra rows are a divergence" "$X" "1"
has "derives-variants: names them" "$D/out" "measured but not recorded: minus-CAP_DAC_OVERRIDE"
cp "$D/config.json" "$D/rootfs/config.json"

# --- refusals ----------------------------------------------------------
mkcfg "$D/nocaps.json"
X=$(CFG="$D/nocaps.json" run)
eq  "nocaps: exit 2" "$X" "2"
has "nocaps: says the matrix would be one row" "$D/out" "nothing to vary"

rm -f "$D/gone.txt"
X=$(EXP="$D/gone.txt" run)
eq  "no-expectation: exit 2" "$X" "2"
has "no-expectation: names the record dispatch" "$D/out" "record=true"
hasnt "no-expectation: does not guess" "$D/out" "all as recorded"

printf '# only commentary\n\n' > "$D/comments.txt"
X=$(EXP="$D/comments.txt" run)
eq  "empty-expectation: exit 2" "$X" "2"
has "empty-expectation: says a universal over nothing is met" "$D/out" "met by any result"

# A scenario name that matches no test. `go test -run` over it exits 0,
# so this must refuse rather than report a passing column.
mkdir -p "$D/emptytests" && echo "package integration" > "$D/emptytests/x_test.go"
X=$(CAPMATRIX_DOCKER="$D/docker" CAPMATRIX_RUNNER="$D/runner" CAPMATRIX_CONFIG="$D/config.json" \
    CAPMATRIX_EXPECT="$D/expect.txt" CAPMATRIX_TESTDIR="$D/emptytests" CAPMATRIX_ROOTFS="$D/rootfs" \
    DOCKER_CALLS="$D/calls" bash "$SUT" > "$D/out" 2>&1; echo $?)
eq  "missing-scenario: exit 2" "$X" "2"
has "missing-scenario: says -run over nothing exits 0" "$D/out" "exits 0"
has "missing-scenario: names one" "$D/out" "TestLeaseRenew_HonorsT1"

X=$(CAPMATRIX_DOCKER="$D/docker" CAPMATRIX_CONFIG="$D/config.json" CAPMATRIX_EXPECT="$D/expect.txt" \
    CAPMATRIX_TESTDIR="$D/testdir" CAPMATRIX_ROOTFS="$D/rootfs" bash "$SUT" > "$D/out" 2>&1; echo $?)
eq  "no-runner: exit 2" "$X" "2"
has "no-runner: says a matrix that runs nothing passes" "$D/out" "reports a clean pass"

# --- the plugin is torn down between variants --------------------------
# Left enabled, the next variant's `plugin create` would collide or, worse,
# the scenarios would run against the previous variant's capabilities and
# every column would read the same.
: > "$D/calls"
X=$(RUNNER_FAILS="minus-CAP_SYS_PTRACE/TestNonRootContainer_PersistentClientStarts" run)
eq  "teardown: run still passes" "$X" "0"
eq  "teardown: one disable per variant" "$(grep -c '^plugin disable' "$D/calls")" "4"
eq  "teardown: one enable per variant"  "$(grep -c '^plugin enable' "$D/calls")" "4"

echo; echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
