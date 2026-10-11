#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for the per-function floor branch of coverage-ratchet.sh
# (#1117), in the shape of test-coverage-ratchet.sh: synthesized
# `go tool cover -func` tables and floor files, a verdict and an exit code per
# case, the arms that must refuse (exit 2), a pre-fix control built from the
# real script, and a check that the suite left the working tree alone.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
RATCHET="$HERE/coverage-ratchet.sh"
REPO_UNDER_TEST=$(cd "$HERE/.." && pwd)
guarded_tmpdir TMP
TREE_BEFORE=$(git -C "$REPO_UNDER_TEST" status --porcelain 2>/dev/null)

# The package half of the ratchet needs a percent file and a baseline; the
# cases below are about the function half, so the package half always holds.
PKG_BASE="$TMP/pkg-baseline.txt"
printf 'toolchain go1.99.0\nexample.com/mod/pkg/a 80.0\n' > "$PKG_BASE"
PKG_PCT="$TMP/pkg-percent.txt"
printf '\texample.com/mod/pkg/a\t\tcoverage: 85.0%% of statements\n' > "$PKG_PCT"

P=example.com/mod/pkg/a

# Every RATCHET_FUNC_* is set here, so a variable left in the caller's
# environment (a lane that exports one) cannot change a case.
run_ratchet() { # run_ratchet <script> [VAR=value ...]; output in $TMP/out
    local script="$1"
    shift
    env RATCHET_FUNC_PROFILE= RATCHET_FUNC_REQUIRED= RATCHET_FUNC_BASE_REF= \
        RATCHET_FUNC_REPO= RATCHET_FUNC_FLOOR_PATH= RATCHET_FUNC_HEAD_BASELINE= \
        RATCHET_REPORT= RATCHET_HEAD_BASELINE="$PKG_BASE" "$@" \
        bash "$script" "$PKG_PCT" "$PKG_BASE" > "$TMP/out" 2>&1
}

failures=0
expect() { # expect <name> <want-exit> <got-exit> [<must-contain>...]
    local name="$1" want="$2" got="$3" needle
    shift 3
    if [ "$got" -ne "$want" ]; then
        echo "FAIL: $name (want exit $want, got $got)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
        return
    fi
    for needle in "$@"; do
        if ! grep -F -e "$needle" "$TMP/out" > /dev/null; then
            echo "FAIL: $name (output lacks: $needle)"
            sed 's/^/    /' "$TMP/out"
            failures=$((failures + 1))
            return
        fi
    done
    echo "PASS: $name"
}

case_run() { # case_run <name> <want-exit> <env...> -- <must-contain...>
    local name="$1" want="$2" got
    shift 2
    local envs=()
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
        envs+=("$1")
        shift
    done
    [ "$#" -eq 0 ] || shift
    run_ratchet "$RATCHET" ${envs[@]+"${envs[@]}"}
    got=$?
    expect "$name" "$want" "$got" "$@"
}

table() { # table <file> <row>...   row = "<file>.go <line> <name> <pct>"
    local f="$1" r
    shift
    : > "$f"
    for r in "$@"; do
        # shellcheck disable=SC2086
        set -- $r
        printf '%s/%s:%s:\t\t%s\t\t%s%%\n' "$P" "$1" "$2" "$3" "$4" >> "$f"
    done
    printf 'total:\t\t\t(statements)\t\t90.0%%\n' >> "$f"
}

# Floor files carry the toolchain every run is told it is on (#1301);
# FLOORS_TC names another line, or none when empty.
export RATCHET_GO_VERSION=go1.99.0
floors() { # floors <file> <key floor>...
    local f="$1" r tc="${FLOORS_TC-toolchain go1.99.0}"
    shift
    { echo '# function floors'; echo; [ -n "$tc" ] && echo "$tc"; for r in "$@"; do echo "$r"; done; } > "$f"
}

FLOORS="$TMP/floors.txt"
floors "$FLOORS" "$P.Foo 90.0" "$P.Bar 50.0"

table "$TMP/hold.txt" "a.go 10 Foo 90.0" "b.go 20 Bar 50.0" "c.go 30 Other 1.0"
case_run "exact hold passes" 0 RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" \
    -- "FUNC-OK $P.Foo measured 90.0% floor 90.0%" "FUNC-CHECK 2 floors" "0 failed"

table "$TMP/up.txt" "a.go 10 Foo 99.0" "b.go 20 Bar 80.0"
case_run "improvement passes" 0 RATCHET_FUNC_PROFILE="$TMP/up.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-OK"

table "$TMP/noise.txt" "a.go 10 Foo 89.6" "b.go 20 Bar 50.0"
case_run "a drop within epsilon passes" 0 RATCHET_FUNC_PROFILE="$TMP/noise.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-OK $P.Foo"

table "$TMP/down.txt" "a.go 10 Foo 40.0" "b.go 20 Bar 50.0"
case_run "a regression fails, names the function and both numbers" 1 RATCHET_FUNC_PROFILE="$TMP/down.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-FAIL $P.Foo measured 40.0% is below its floor 90.0%" "FUNC-OK $P.Bar"

table "$TMP/edge.txt" "a.go 10 Foo 89.5" "b.go 20 Bar 50.0"
case_run "exactly epsilon under the floor passes" 0 RATCHET_FUNC_PROFILE="$TMP/edge.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-OK $P.Foo"
table "$TMP/hair.txt" "a.go 10 Foo 89.4" "b.go 20 Bar 50.0"
case_run "a hair past epsilon fails" 1 RATCHET_FUNC_PROFILE="$TMP/hair.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-FAIL $P.Foo"

# ---- the skip, and where it must not be allowed ------------------------
case_run "no merged profile in this run skips with the reason and is not red" 0 RATCHET_FUNC_HEAD_BASELINE="$FLOORS" \
    -- "FUNC-SKIP per-function floors not checked" "RATCHET_FUNC_PROFILE is not set"
case_run "a named table that does not exist skips the same way" 0 RATCHET_FUNC_PROFILE="$TMP/nosuch.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-SKIP" "missing or empty"
: > "$TMP/empty.txt"
mkdir -p "$TMP/adir"
case_run "a table named as a directory skips the same way" 0 RATCHET_FUNC_PROFILE="$TMP/adir" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-SKIP" "missing or empty"
case_run "an empty table skips the same way" 0 RATCHET_FUNC_PROFILE="$TMP/empty.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-SKIP"
case_run "REQUIRED with no profile named refuses" 2 RATCHET_FUNC_REQUIRED=1 RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "No function table"
case_run "REQUIRED with a missing table refuses" 2 RATCHET_FUNC_REQUIRED=1 RATCHET_FUNC_PROFILE="$TMP/nosuch.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "No function table"
case_run "REQUIRED with an empty table refuses" 2 RATCHET_FUNC_REQUIRED=1 RATCHET_FUNC_PROFILE="$TMP/empty.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "No function table"
case_run "REQUIRED with a good table passes" 0 RATCHET_FUNC_REQUIRED=1 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-CHECK 2 floors"

# ---- absence is a failure ---------------------------------------------
table "$TMP/renamed.txt" "a.go 10 FooRenamed 99.0" "b.go 20 Bar 50.0"
case_run "a floored function renamed away fails on absence" 1 RATCHET_FUNC_PROFILE="$TMP/renamed.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-FAIL $P.Foo is floored at 90.0% but has no row"
table "$TMP/moved.txt" "z_moved.go 999 Foo 90.0" "b.go 20 Bar 50.0"
case_run "a function moved to another file of its package keeps its row" 0 RATCHET_FUNC_PROFILE="$TMP/moved.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-OK $P.Foo"
{ printf 'example.com/mod/pkg/other/o.go:5:\t\tFoo\t\t100.0%%\n'; cat "$TMP/renamed.txt"; } > "$TMP/otherpkg.txt"
case_run "the same name in another package is not the floored function" 1 RATCHET_FUNC_PROFILE="$TMP/otherpkg.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-FAIL $P.Foo is floored at 90.0% but has no row"

# ---- refusals ----------------------------------------------------------
table "$TMP/dupfn.txt" "a.go 10 Foo 90.0" "a2.go 11 Foo 10.0" "b.go 20 Bar 50.0"
case_run "a floored name found twice in the table refuses" 2 RATCHET_FUNC_PROFILE="$TMP/dupfn.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "Function name ambiguous"
floors "$TMP/floors-dup.txt" "$P.Foo 90.0" "$P.Foo 10.0" "$P.Bar 50.0"
case_run "a floor key listed twice refuses" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$TMP/floors-dup.txt" \
    -- "Function floor duplicated"
floors "$TMP/floors-nonum.txt" "$P.Foo" "$P.Bar 50.0"
case_run "a floor with no number refuses (it would read as 0)" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/floors-nonum.txt" -- "Function floor unreadable"
floors "$TMP/floors-nan.txt" "$P.Foo 9x0" "$P.Bar 50.0"
floors "$TMP/floors-dots.txt" "$P.Foo 9.0.1" "$P.Bar 50.0"
case_run "a floor with two dots refuses" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/floors-dots.txt" -- "Function floor unreadable"
floors "$TMP/floors-trail.txt" "$P.Foo 90." "$P.Bar 50.0"
case_run "a floor with a trailing dot refuses" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/floors-trail.txt" -- "Function floor unreadable"
case_run "a floor that is not a number refuses" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/floors-nan.txt" -- "Function floor unreadable"
floors "$TMP/floors-extra.txt" "$P.Foo 90.0 extra" "$P.Bar 50.0"
case_run "a floor row with a third field refuses" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/floors-extra.txt" -- "Function floor unreadable"
floors "$TMP/floors-nodot.txt" "Foo 90.0"
case_run "a key that is not <import path>.<Func> refuses" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/floors-nodot.txt" -- "Function floor unreadable"
floors "$TMP/floors-none.txt"
case_run "a floor file with comments only refuses instead of comparing nothing" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/floors-none.txt" -- "No function floors"
case_run "a floor file that cannot be read refuses" 2 RATCHET_FUNC_PROFILE="$TMP/hold.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/no-such-floors.txt" -- "No function floors" "is not a readable file"

# `go tool cover -func` layout changes: the percent column gone, the row
# shape gone, a table that is not that output at all.
sed 's/%//' "$TMP/hold.txt" > "$TMP/nopct.txt"
case_run "a table whose rows lost their percent sign refuses" 2 RATCHET_FUNC_PROFILE="$TMP/nopct.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "Function table unreadable"
sed 's/\.go:\([0-9]*\):/.go (\1)/' "$TMP/hold.txt" > "$TMP/noshape.txt"
case_run "a table whose rows lost the file:line shape refuses" 2 RATCHET_FUNC_PROFILE="$TMP/noshape.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "Function table unreadable"
sed 's/\.go:\([0-9]*\):/.go:x\1:/' "$TMP/hold.txt" > "$TMP/nolineno.txt"
case_run "a table whose line numbers are not numbers refuses" 2 RATCHET_FUNC_PROFILE="$TMP/nolineno.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "Function table unreadable"
printf 'PASS\nok  \texample.com/mod/pkg/a\t1.2s\n' > "$TMP/notatable.txt"
case_run "a file that is not the -func output refuses" 2 RATCHET_FUNC_PROFILE="$TMP/notatable.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "Function table unreadable"
tr '\t' ' ' < "$TMP/hold.txt" > "$TMP/spaces.txt"
case_run "a table separated by spaces instead of tabs still parses" 0 RATCHET_FUNC_PROFILE="$TMP/spaces.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-OK"
: > "$TMP/gone.txt"
printf 'example.com/mod/pkg/zz/z.go:1:\t\tOnly\t\t100.0%%\n' > "$TMP/gone.txt"
case_run "a table with rows for other packages only fails every floor rather than passing" 1 RATCHET_FUNC_PROFILE="$TMP/gone.txt" \
    RATCHET_FUNC_HEAD_BASELINE="$FLOORS" -- "FUNC-FAIL $P.Foo" "FUNC-FAIL $P.Bar"

# ---- the locale: a comma-decimal awk must not truncate 90.5 to 90 -------
LC_ALL=de_DE.UTF-8 run_ratchet "$RATCHET" RATCHET_FUNC_PROFILE="$TMP/down.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS"
expect "a regression still fails under a comma-decimal locale" 1 $? "FUNC-FAIL $P.Foo"
LC_ALL=de_DE.UTF-8 run_ratchet "$RATCHET" RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS"
expect "a hold still passes under a comma-decimal locale" 0 $? "FUNC-OK $P.Foo"
LC_ALL=de_DE.UTF-8 run_ratchet "$RATCHET" RATCHET_FUNC_PROFILE="$TMP/edge.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS"
expect "a fractional measurement inside epsilon is not truncated under a comma-decimal locale" 0 $? "FUNC-OK $P.Foo measured 89.5%"

# An awk that reads "88.2" as 88 unless LC_ALL=C, the way a comma-decimal
# locale does on an awk that honours it (#1117). This box's mawk does not, so
# the shim is what makes the LC_ALL=C in the ratchet observable here.
make_truncating_awk() { # make_truncating_awk <dir>
    mkdir -p "$1"
    cat > "$1/awk" <<'SHIM_EOF'
#!/usr/bin/env bash
if [ "${LC_ALL-}" = C ]; then exec "$REAL_AWK" "$@"; fi
args=()
while [ "$#" -gt 0 ]; do
    if [ "$1" = -v ] && [ "$#" -ge 2 ]; then
        v="$2"
        case "$v" in *=[0-9]*.[0-9]*) v="${v%.*}" ;; esac
        args+=(-v "$v")
        shift 2
    else
        args+=("$1")
        shift
    fi
done
exec "$REAL_AWK" "${args[@]}"
SHIM_EOF
    chmod +x "$1/awk"
}
REAL_AWK=$(command -v awk)
export REAL_AWK
make_truncating_awk "$TMP/shim"
run_ratchet "$RATCHET" LC_ALL=de_DE.UTF-8 PATH="$TMP/shim:$PATH" RATCHET_FUNC_PROFILE="$TMP/edge.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS"
expect "...and under an awk that truncates, which is what the shim models" 0 $? "FUNC-OK $P.Foo measured 89.5%"

# ---- the package half is undisturbed ------------------------------------
printf '\texample.com/mod/pkg/a\t\tcoverage: 10.0%% of statements\n' > "$TMP/pkg-low.txt"
env RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" RATCHET_REPORT= RATCHET_HEAD_BASELINE="$PKG_BASE" \
    bash "$RATCHET" "$TMP/pkg-low.txt" "$PKG_BASE" > "$TMP/out" 2>&1
expect "a package regression is still exit 1 next to passing function floors" 1 $? "FAIL  example.com/mod/pkg/a" "FUNC-OK"

# ---- the floors come from the merge base on a PR ------------------------
# A throwaway repository: the base commit floors Foo at 90, the head commit
# lowers it to 10 in the same change. On a PR the ratchet must read the base.
REPO="$TMP/repo"
mkdir -p "$REPO/scripts" "$REPO/.github"
cp "$RATCHET" "$REPO/scripts/coverage-ratchet.sh"
printf 'module example.com/mod\n\ngo 1.22\n' > "$REPO/go.mod"
g() { git -C "$REPO" -c user.email=t@t -c user.name=t -c commit.gpgsign=false "$@"; }
g init -q -b main .
floors "$REPO/.github/coverage-func-baseline.txt" "$P.Foo 90.0" "$P.Bar 50.0"
g add -A && g commit -q -m base
BASE_SHA=$(g rev-parse HEAD)
g checkout -q -b feature
floors "$REPO/.github/coverage-func-baseline.txt" "$P.Foo 10.0" "$P.Bar 50.0"
g add -A && g commit -q -m lower
table "$TMP/half.txt" "a.go 10 Foo 40.0" "b.go 20 Bar 50.0"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/half.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF="$BASE_SHA"
expect "a floor lowered in the PR is judged against the merge base's number" 1 $? "FUNC-FAIL $P.Foo measured 40.0% is below its floor 90.0%" "the merge base"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/half.txt" RATCHET_FUNC_REPO="$REPO"
expect "...while a run with no PR context reads the head copy and passes" 0 $? "the working copy"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/half.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF=deadbeefdeadbeef
expect "a base that does not resolve refuses" 2 $? "Unknown base"

# A base that shares no history with the head has no merge base.
g checkout -q --orphan unrelated
g rm -q -rf . > /dev/null 2>&1
printf 'x\n' > "$REPO/x.txt"
g add -A && g commit -q -m unrelated
UNREL_SHA=$(g rev-parse HEAD)
g checkout -q feature
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/half.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF="$UNREL_SHA"
expect "a base with no merge base refuses" 2 $? "No merge base"

# The head drops the row of a function that is also gone from the table: the
# base still floors it, so that is a failure and not a pass on absence.
g checkout -q -b droprow "$BASE_SHA"
floors "$REPO/.github/coverage-func-baseline.txt" "$P.Bar 50.0"
g add -A && g commit -q -m "drop the Foo row"
table "$TMP/renamed.txt" "a.go 10 FooRenamed 99.0" "b.go 20 Bar 50.0"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/renamed.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF="$BASE_SHA"
expect "a renamed function whose row the PR also removed still fails on the base's floor" 1 $? "FUNC-FAIL $P.Foo is floored at 90.0% but has no row"

# The first PR that adds the file: the merge base has none, so the head copy
# is read and the run says so.
g checkout -q -b first "$BASE_SHA"
g rm -q .github/coverage-func-baseline.txt && g commit -q -m "drop floors on the base"
BARE_SHA=$(g rev-parse HEAD)
g checkout -q -b introduces
mkdir -p "$REPO/.github"
floors "$REPO/.github/coverage-func-baseline.txt" "$P.Foo 90.0" "$P.Bar 50.0"
g add -A && g commit -q -m "add floors"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/half.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF="$BARE_SHA"
expect "a base with no floor file falls back to the head copy and says so" 1 $? "the merge base has no" "FUNC-FAIL $P.Foo"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/half.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF="$BARE_SHA" \
    RATCHET_FUNC_HEAD_BASELINE="$TMP/no-such-floors.txt"
expect "a base with no floor file and no readable head copy refuses" 2 $? "No function floors" "is absent at the merge base"

# ---- the unit is compared before the numbers (#1301) ---------------------
# The v2.6.0 release PR: the merge base holds main's floors, measured on Go
# 1.27.1 with no toolchain line, and the head copy carries the run's
# toolchain. Only pull_request context reads the merge base, and the
# Coverage dispatch has none, so this is where that path is proved.
g checkout -q -b tc-old "$BASE_SHA"
FLOORS_TC='' floors "$REPO/.github/coverage-func-baseline.txt" "$P.Foo 90.0" "$P.Bar 50.0"
g add -A && g commit -q -m "floors with no toolchain line"
TC_OLD_SHA=$(g rev-parse HEAD)
g checkout -q -b tc-release
floors "$REPO/.github/coverage-func-baseline.txt" "$P.Foo 35.0" "$P.Bar 50.0"
g add -A && g commit -q -m "re-baselined floors"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/half.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF="$TC_OLD_SHA"
expect "merge-base floors from an unrecorded toolchain give way to the matching head copy" 0 $? \
    "function floors in the merge base $TC_OLD_SHA were measured on an unrecorded toolchain, this run on go1.99.0" \
    "FUNC-OK $P.Foo measured 40.0%"
table "$TMP/tc-low.txt" "a.go 10 Foo 30.0" "b.go 20 Bar 50.0"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/tc-low.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF="$TC_OLD_SHA"
expect "...and the head copy is compared, not waved through" 1 $? "FUNC-FAIL $P.Foo measured 30.0% is below its floor 35.0%"
FLOORS_TC='toolchain go1.27.1' floors "$REPO/.github/coverage-func-baseline.txt" "$P.Foo 35.0" "$P.Bar 50.0"
g add -A && g commit -q -m "head floors from another toolchain"
run_ratchet "$REPO/scripts/coverage-ratchet.sh" RATCHET_FUNC_PROFILE="$TMP/half.txt" RATCHET_FUNC_REPO="$REPO" RATCHET_FUNC_BASE_REF="$TC_OLD_SHA"
expect "a head copy from another toolchain too is a re-baseline red" 1 $? \
    "FAIL  re-baseline needed: function floors measured on go1.27.1"
if grep -F 'FUNC-OK' "$TMP/out" > /dev/null; then
    echo "FAIL: ...yet a function floor was compared"; failures=$((failures + 1))
else
    echo "PASS: ...and no function floor is compared in the wrong unit"
fi
g checkout -q introduces

FLOORS_TC='toolchain go1.27.1' floors "$TMP/tc-old-floors.txt" "$P.Foo 90.0" "$P.Bar 50.0"
run_ratchet "$RATCHET" RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$TMP/tc-old-floors.txt"
expect "with no PR context, a working copy from another toolchain is a re-baseline red" 1 $? \
    "FAIL  re-baseline needed: function floors measured on go1.27.1 in $TMP/tc-old-floors.txt, run on go1.99.0"
run_ratchet "$RATCHET" RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS"
expect "matching floors compare as before, the toolchain line read as no function" 0 $? "FUNC-OK $P.Foo" "FUNC-OK $P.Bar"
if grep -E 'FUNC-[A-Z]+ +toolchain' "$TMP/out" > /dev/null; then
    echo "FAIL: ...yet the toolchain line was judged as a function"; failures=$((failures + 1))
else
    echo "PASS: ...and no verdict names the toolchain line"
fi
FLOORS_TC='toolchain go1.27.1' floors "$TMP/tc-only-floors.txt"
run_ratchet "$RATCHET" RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$TMP/tc-only-floors.txt"
expect "function floors holding only a toolchain line keep their own refusal" 2 $? "No function floors"
printf 'toolchain go1.99.0\ntoolchain go1.27.1\n%s.Foo 90.0\n' "$P" > "$TMP/tc-dup-floors.txt"
run_ratchet "$RATCHET" RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$TMP/tc-dup-floors.txt"
expect "two toolchain lines in the function floors are refused" 2 $? "Unreadable toolchain line"
printf 'toolchain go1.27.1\nexample.com/mod/pkg/a 95.0\n' > "$TMP/tc-old-pkg.txt"
env RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" RATCHET_REPORT= \
    RATCHET_HEAD_BASELINE="$TMP/tc-old-pkg.txt" bash "$RATCHET" "$PKG_PCT" "$TMP/tc-old-pkg.txt" > "$TMP/out" 2>&1
expect "package floors needing a re-baseline stop the run before the function floors" 1 $? \
    "FAIL  re-baseline needed: package floors" "per-function floors are not compared"
if grep -E 'FUNC-(OK|FAIL)' "$TMP/out" > /dev/null; then
    echo "FAIL: ...yet the function floors were compared"; failures=$((failures + 1))
else
    echo "PASS: ...and none of them is"
fi
mkdir -p "$TMP/tcgo"; printf '#!/bin/sh\nexit 1\n' > "$TMP/tcgo/go"; chmod +x "$TMP/tcgo/go"
env -u RATCHET_GO_VERSION PATH="$TMP/tcgo:$PATH" RATCHET_FUNC_PROFILE="$TMP/hold.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS" \
    RATCHET_REPORT= RATCHET_HEAD_BASELINE="$PKG_BASE" bash "$RATCHET" "$PKG_PCT" "$PKG_BASE" > "$TMP/out" 2>&1
expect "a go that cannot name its version is exit 2, not a verdict" 2 $? "Unknown toolchain"

# A major-version rename: the floor is written under the old module path, the
# head module carries /v2 and the table is spelled with it.
REPO2="$TMP/repo2"
mkdir -p "$REPO2/scripts" "$REPO2/.github"
cp "$RATCHET" "$REPO2/scripts/coverage-ratchet.sh"
printf 'module example.com/mod/v2\n\ngo 1.22\n' > "$REPO2/go.mod"
printf 'toolchain go1.99.0\nexample.com/mod/v2/pkg/a 80.0\n' > "$TMP/pkg-baseline-v2.txt"
printf '\texample.com/mod/v2/pkg/a\t\tcoverage: 85.0%% of statements\n' > "$TMP/pkg-percent-v2.txt"
floors "$REPO2/.github/coverage-func-baseline.txt" "example.com/mod/pkg/a.Foo 90.0"
printf 'example.com/mod/v2/pkg/a/a.go:1:\t\tFoo\t\t95.0%%\n' > "$TMP/v2-hold.txt"
printf 'example.com/mod/v2/pkg/a/a.go:1:\t\tFoo\t\t20.0%%\n' > "$TMP/v2-low.txt"
env RATCHET_FUNC_PROFILE="$TMP/v2-hold.txt" RATCHET_REPORT= RATCHET_HEAD_BASELINE="$TMP/pkg-baseline-v2.txt" \
    bash "$REPO2/scripts/coverage-ratchet.sh" "$TMP/pkg-percent-v2.txt" "$TMP/pkg-baseline-v2.txt" > "$TMP/out" 2>&1
expect "a floor under the old module path is re-spelled to the head module and held" 0 $? "FUNC-OK example.com/mod/pkg/a.Foo measured 95.0%"
env RATCHET_FUNC_PROFILE="$TMP/v2-low.txt" RATCHET_REPORT= RATCHET_HEAD_BASELINE="$TMP/pkg-baseline-v2.txt" \
    bash "$REPO2/scripts/coverage-ratchet.sh" "$TMP/pkg-percent-v2.txt" "$TMP/pkg-baseline-v2.txt" > "$TMP/out" 2>&1
expect "...and judged there, not skipped by the rename" 1 $? "FUNC-FAIL example.com/mod/pkg/a.Foo measured 20.0%"

# ---- the workflow feeds the check what the check reads ------------------
WF="$REPO_UNDER_TEST/.github/workflows/coverage.yml"
step_block() { # step_block <name fragment>: the workflow step whose name contains it
    awk -v n="$1" '
        /^      - name:/ { on = (index($0, n) > 0) }
        on { print }' "$WF"
}
RATCHET_STEP=$(step_block "Coverage ratchet")
TABLE=$(printf '%s\n' "$RATCHET_STEP" | sed -n 's/.*RATCHET_FUNC_PROFILE=\([^ "]*\).*/\1/p' | head -1)
if [ -n "$TABLE" ]; then
    echo "PASS: the ratchet step names the function table ($TABLE)"
else
    echo "FAIL: the Coverage ratchet step does not set RATCHET_FUNC_PROFILE"
    failures=$((failures + 1))
    TABLE="__no_table_named__"
fi
if printf '%s\n' "$RATCHET_STEP" | grep -F 'RATCHET_FUNC_REQUIRED=1' > /dev/null; then
    echo "PASS: the ratchet step requires the function check (a missing table is red there)"
else
    echo "FAIL: the ratchet step does not set RATCHET_FUNC_REQUIRED=1"
    failures=$((failures + 1))
fi
if printf '%s\n' "$RATCHET_STEP" | grep -F 'RATCHET_FUNC_BASE_REF' > /dev/null; then
    echo "PASS: the ratchet step passes the merge base for the function floors"
else
    echo "FAIL: the ratchet step does not set RATCHET_FUNC_BASE_REF"
    failures=$((failures + 1))
fi
UPLOAD=$(awk -v t="$TABLE" '
    /^      - name:/ { blk = ""; }
    { blk = blk $0 "\n" }
    /if-no-files-found: error/ { if (index(blk, "actions/upload-artifact") && index(blk, t)) found = 1 }
    END { exit !found }' "$WF" && echo yes || echo no)
if [ "$UPLOAD" = yes ]; then
    echo "PASS: an upload step carries $TABLE with if-no-files-found: error"
else
    echo "FAIL: no upload-artifact step lists $TABLE under if-no-files-found: error"
    failures=$((failures + 1))
fi
if step_block "function" | grep -F "GITHUB_STEP_SUMMARY" | grep -F -e '>>' > /dev/null && step_block "function" | grep -F "$TABLE" > /dev/null; then
    echo "PASS: a step writes the table into the job summary"
else
    echo "FAIL: no step named for the function table writes $TABLE to GITHUB_STEP_SUMMARY"
    failures=$((failures + 1))
fi

# ---- the pre-fix control: the script without the function branch --------
PRE="$TMP/pre-fix.sh"
grep -v -x 'func_check' "$RATCHET" > "$PRE"
if [ "$(grep -c -x 'func_check' "$RATCHET")" -eq 1 ]; then
    run_ratchet "$PRE" RATCHET_FUNC_PROFILE="$TMP/down.txt" RATCHET_FUNC_HEAD_BASELINE="$FLOORS"
    got=$?
    if [ "$got" -eq 0 ] && ! grep -F 'FUNC-' "$TMP/out" > /dev/null; then
        echo "PASS: the script without the function branch exits 0 on a 40 % function under a 90 % floor (the defect)"
    else
        echo "FAIL: the pre-fix control did not reproduce the defect (exit $got)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
else
    echo "FAIL: the pre-fix control could not be built (func_check call site not found exactly once)"
    failures=$((failures + 1))
fi

tree_after=$(git -C "$REPO_UNDER_TEST" status --porcelain 2>/dev/null)
if [ "$tree_after" = "$TREE_BEFORE" ]; then
    echo "PASS: the suite wrote nothing into the working tree"
else
    echo "FAIL: this suite changed the working tree; a self-test must not write into it"
    diff <(printf '%s\n' "$TREE_BEFORE") <(printf '%s\n' "$tree_after") | sed 's/^/    /'
    failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures function-ratchet test(s) failed"
    exit 1
fi
echo "All function-ratchet tests passed"
