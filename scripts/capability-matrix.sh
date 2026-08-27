#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# What each capability in config.json actually buys (#690).
#
# config.json asks operators to grant CAP_NET_ADMIN, CAP_SYS_ADMIN and
# CAP_SYS_PTRACE. Until this ran, the repo could say what the first two
# are for and asserted the third. This installs the plugin once per
# capability set -- the full set, then each capability removed in turn --
# runs a small scenario set against each, and compares the result against
# a RECORDED expectation. A change that quietly starts needing a fourth
# capability, or stops needing one, fails here instead of being found by
# a user whose containers stop getting addresses.
#
# ONE JOB, NOT A MATRIX OF FOUR, and the reason is the pool rather than
# the suite. integration.yml already puts four privileged jobs on a pool
# of eight and documents (#430, and the 2026-08-02 measurement in that
# file) that two concurrent runs is the ceiling. Four more privileged
# jobs would make a capability run alone saturate it. The variants share
# one rootfs build and differ only in config.json, so running them in
# sequence in a single job costs one slot and a few minutes.
#
# THE VARIANTS ARE DERIVED FROM config.json, never typed. A fourth
# capability added there grows this matrix by itself; a list here would
# leave the new one unmeasured while the job still reported a clean pass.
#
# THE SCENARIO NAMES ARE TYPED, so they are checked. `go test -run` over
# a name that matches nothing exits 0 and prints ok -- a scenario that
# has been renamed would read as a passing cell forever. Every name below
# must match exactly one test function in the integration suite, or this
# refuses before installing anything.
#
# A VARIANT THAT WILL NOT BUILD OR ENABLE IS exit 2, NOT A ROW OF FAILS.
# "the plugin could not be created" and "this capability is required"
# produce the same-looking row, and the second is the answer the job
# exists to find -- so the difference has to be kept, or a broken build
# reads as a discovery.
#
# Seams, so the self-test drives THIS script rather than a copy:
#   CAPMATRIX_DOCKER   the docker command            (default: docker)
#   CAPMATRIX_RUNNER   run one scenario:  <runner> <plugin-ref> <test>
#                      exit 0 pass, 1 fail, anything else cannot-judge
#   CAPMATRIX_CONFIG   the manifest to read capabilities from
#   CAPMATRIX_EXPECT   the recorded expectation table
#   CAPMATRIX_TESTDIR  where scenario names are verified to exist
#   CAPMATRIX_ROOTFS   the built plugin rootfs directory
#   CAPMATRIX_RECORD   non-empty: write the measured table to CAPMATRIX_EXPECT
#                      instead of comparing against it
#
# THE EXPECTATION FILE IS NOT SHIPPED PRE-FILLED, on purpose. The obvious
# thing to do here was to write down what the capabilities are believed to
# buy and let the first run confirm it. That is a prediction wearing the
# clothes of a measurement: whoever reads the file next cannot tell which
# it is, and if the first run had disagreed the temptation would have been
# to fix the run. So the file starts absent, this refuses until a run
# records it, and the recorded table is reviewed as a diff by a human who
# can see it was produced rather than typed.
#
# Exit: 0 every cell matches the recorded expectation
#       1 a cell diverged -- what a capability buys has changed
#       2 CANNOT JUDGE -- nothing to vary, nothing to run, a scenario
#         name that matches no test, or a variant that would not install
set -uo pipefail
export LC_ALL=C

CONFIG="${CAPMATRIX_CONFIG:-config.json}"
EXPECT="${CAPMATRIX_EXPECT:-.github/capability-matrix.txt}"
TESTDIR="${CAPMATRIX_TESTDIR:-test/integration}"
ROOTFS="${CAPMATRIX_ROOTFS:-plugin}"
DOCKER="${CAPMATRIX_DOCKER:-docker}"
RUNNER="${CAPMATRIX_RUNNER:-}"
TAG_PREFIX="${CAPMATRIX_TAG_PREFIX:-dnd-capmatrix}"

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
refuse() { echo "::error title=Capability matrix cannot be judged::$*" >&2; exit 2; }

# The five scenarios #690 names: bridge attach, macvlan attach, a
# --user 1000 container, a renewal, and a container restart. One test
# each, chosen because each is the thinnest thing that exercises the
# path rather than the fastest thing that passes.
SCENARIOS=(
    TestLifecycleBridge_GoldenPath
    TestLifecycleMacvlan_GoldenPath
    TestNonRootContainer_PersistentClientStarts
    TestLeaseRenew_HonorsT1
    TestTombstoneRestart_PreservesMACAndIP
)

[ -n "$RUNNER" ] || refuse "CAPMATRIX_RUNNER is unset. There is no way to run a scenario, and a matrix that runs nothing reports a clean pass."
[ -r "$CONFIG" ] || refuse "$CONFIG is not readable, so the capability set cannot be derived."
[ -d "$TESTDIR" ] || refuse "$TESTDIR is not a directory, so no scenario name can be verified to exist."

# --- derive the capability set -----------------------------------------
mapfile -t CAPS < <(python3 - "$CONFIG" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for c in (d.get("linux") or {}).get("capabilities") or []:
    print(c)
PY
)
[ "${#CAPS[@]}" -gt 0 ] || refuse "$CONFIG declares no linux.capabilities. There is nothing to vary, and a matrix over one row would report that every capability is unnecessary."

# --- verify every scenario names a real test ---------------------------
missing=()
for s in "${SCENARIOS[@]}"; do
    n=$(grep -rhcE "^func ${s}\(" "$TESTDIR"/*_test.go 2>/dev/null | awk '{t+=$1} END{print t+0}')
    [ "$n" -eq 1 ] || missing+=("$s (matched $n test functions)")
done
if [ "${#missing[@]}" -gt 0 ]; then
    refuse "these scenario names do not each match exactly one test in $TESTDIR: ${missing[*]}. \`go test -run\` over a name that matches nothing exits 0, so a renamed scenario would read as a passing cell forever."
fi

# --- the recorded expectation ------------------------------------------
RECORD="${CAPMATRIX_RECORD:-}"
if [ -z "$RECORD" ]; then
    [ -r "$EXPECT" ] || refuse "$EXPECT does not exist. No run has recorded what these capabilities buy yet, and this will not guess. Dispatch capability-matrix.yml with record=true, review the table it produces as a diff, and commit it."
    grep -vE '^[[:space:]]*(#|$)' "$EXPECT" > "$TMP/expect"
    [ -s "$TMP/expect" ] || refuse "$EXPECT holds no data rows. An expectation over no cells is met by any result."
fi

# --- the variants -------------------------------------------------------
# full, then each capability removed in turn.
VARIANTS=("full")
for c in "${CAPS[@]}"; do VARIANTS+=("minus-$c"); done

variant_caps() { # variant -> the capability list for it, one per line
    local v="$1" c
    for c in "${CAPS[@]}"; do
        [ "$v" = "minus-$c" ] && continue
        echo "$c"
    done
}

install_variant() { # variant -> plugin ref on stdout, or non-zero
    local v="$1"
    local ref="$TAG_PREFIX:$v"
    local dir="$TMP/$v"
    mkdir -p "$dir"
    cp -a "$ROOTFS/." "$dir/" 2>/dev/null || return 1
    variant_caps "$v" > "$TMP/caps.$v"
    python3 - "$dir/config.json" "$TMP/caps.$v" <<'PY' || return 1
import json, sys
p, capfile = sys.argv[1], sys.argv[2]
d = json.load(open(p))
d.setdefault("linux", {})["capabilities"] = [l.strip() for l in open(capfile) if l.strip()]
json.dump(d, open(p, "w"), indent=2)
PY
    "$DOCKER" plugin rm -f "$ref" >/dev/null 2>&1
    "$DOCKER" plugin create "$ref" "$dir" >/dev/null 2>&1 || return 1
    "$DOCKER" plugin enable "$ref" >/dev/null 2>&1 || return 1
    echo "$ref"
}

# --- run ----------------------------------------------------------------
: > "$TMP/got"
for v in "${VARIANTS[@]}"; do
    if ! ref=$(install_variant "$v"); then
        refuse "variant '$v' could not be created and enabled. That is not the same as its scenarios failing, and recording it as a row of FAILs would turn a broken build into a finding about capabilities."
    fi
    for s in "${SCENARIOS[@]}"; do
        "$RUNNER" "$ref" "$s" >/dev/null 2>&1
        case $? in
            0) echo "$v $s PASS" >> "$TMP/got" ;;
            1) echo "$v $s FAIL" >> "$TMP/got" ;;
            *) refuse "scenario '$s' on variant '$v' returned neither pass nor fail. A third outcome recorded as either one is a guess." ;;
        esac
    done
    "$DOCKER" plugin disable -f "$ref" >/dev/null 2>&1
    "$DOCKER" plugin rm -f "$ref" >/dev/null 2>&1
done

# AN INTERNAL INVARIANT, AND NO FIXTURE CAN VIOLATE IT. Stated because
# scripts/test-capability-matrix.sh cannot kill a mutation of the next
# two lines, and a reader counting them as tested would be wrong: every
# path that produces fewer cells than variants x scenarios already
# refuses above, so nothing driveable from the seams gets here short. It
# is kept because record mode has no comparison to catch a short table --
# the recorded file simply BECOMES the expectation -- and that is the one
# place where a future refactor could lose cells in silence.
cells=$(wc -l < "$TMP/got" | tr -d ' ')
want_cells=$(( ${#VARIANTS[@]} * ${#SCENARIOS[@]} ))
[ "$cells" -eq "$want_cells" ] || refuse "recorded $cells cell(s), expected ${#VARIANTS[@]} variant(s) x ${#SCENARIOS[@]} scenario(s) = $want_cells."

# --- report and compare -------------------------------------------------
printf '%-28s' "scenario"
for v in "${VARIANTS[@]}"; do printf '%-22s' "$v"; done
echo
for s in "${SCENARIOS[@]}"; do
    printf '%-28s' "${s#Test}"
    for v in "${VARIANTS[@]}"; do
        printf '%-22s' "$(awk -v v="$v" -v s="$s" '$1==v && $2==s {print $3}' "$TMP/got")"
    done
    echo
done
echo

if [ -n "$RECORD" ]; then
    {
        echo "# What each capability in config.json buys (#690)."
        echo "#"
        echo "# MEASURED by scripts/capability-matrix.sh, not predicted. Each row is"
        echo "# <variant> <scenario> <PASS|FAIL>; a variant named minus-CAP_X is the"
        echo "# plugin installed with every capability except that one."
        echo "#"
        echo "# Do not edit a row to make a red run green. A diverging cell means the"
        echo "# plugin's capability requirements changed; say which, in the PR body."
        sort "$TMP/got"
    } > "$EXPECT"
    echo "recorded $cells cell(s) to $EXPECT. Review it as a diff before committing: it is now the answer everything else is judged against."
    exit 0
fi

sort "$TMP/got" > "$TMP/got.s"
sort "$TMP/expect" > "$TMP/expect.s"
if diff -q "$TMP/got.s" "$TMP/expect.s" >/dev/null; then
    echo "capability matrix: $cells cell(s), all as recorded in $EXPECT."
    exit 0
fi

echo "::error title=What a capability buys has changed::the matrix diverges from $EXPECT." >&2
join -j 1 -o 0,1.2,2.2 \
    <(awk '{print $1"/"$2, $3}' "$TMP/got.s"    | sort) \
    <(awk '{print $1"/"$2, $3}' "$TMP/expect.s" | sort) \
  | awk '$2 != $3 { printf "  %-60s measured %s, recorded %s\n", $1, $2, $3 }'
comm -23 <(awk '{print $1"/"$2}' "$TMP/got.s") <(awk '{print $1"/"$2}' "$TMP/expect.s") \
  | sed 's/^/  measured but not recorded: /'
comm -13 <(awk '{print $1"/"$2}' "$TMP/got.s") <(awk '{print $1"/"$2}' "$TMP/expect.s") \
  | sed 's/^/  recorded but not measured: /'
echo
echo "If the change is intended, update $EXPECT in the same PR and say in the body what capability" >&2
echo "the plugin now needs or no longer needs. Do not update it to make this pass." >&2
exit 1
