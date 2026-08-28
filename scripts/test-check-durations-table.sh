#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-durations-table.sh (#877), through the --root seam.
#
# Red cases first, and the emptiable ones before the interesting ones: a
# gate over a universally-quantified set is satisfied by emptying the
# set, so "no test files", "no table", "a table of nothing but comments"
# and "a partitioner that refuses" must each be exit 2. A gate that
# reports success having compared nothing is the defect this file is
# here to make impossible.
#
# The DRIVE-THE-ABSENCE case is `real tree minus one row`: it takes the
# shipped suite and the shipped table, deletes one row that the shipped
# table actually has, and requires the gate to name that test. A green
# self-test over synthetic trees would not tell you the gate works on
# the thing it guards.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-durations-table.sh"
REPO="$(cd "$HERE/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
failures=0

# A synthetic tree: three main-suite tests, one failure-suite test, the
# real partitioner. Table contents are written per-case.
tree() {
    local d="$TMP/$1"
    mkdir -p "$d/scripts" "$d/test/integration/testdata"
    cp "$HERE/integration-shard.sh" "$d/scripts/"
    cat > "$d/test/integration/alpha_test.go" <<'GO'
package integration

func TestAlpha_One(t *testing.T) {}
func TestAlpha_Two(t *testing.T) {}
GO
    cat > "$d/test/integration/beta_test.go" <<'GO'
package integration

func TestBeta_One(t *testing.T) {}
func TestFailure_Excluded(t *testing.T) {}
GO
    echo "$d"
}

table() { # <tree> <lines...>
    local d="$1"; shift
    { echo "# synthetic"; printf '%s\n' "$@"; } > "$d/test/integration/testdata/main-suite-durations.tsv"
}

check() { # <name> <want-exit> <root> <want-grep>
    local name="$1" want_exit="$2" root="$3" want_grep="$4"
    bash "$GATE" --root "$root" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && { [ -z "$want_grep" ] || grep -q -- "$want_grep" "$TMP/out"; }; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want_exit${want_grep:+, wanted /$want_grep/})"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# --- the gate must not be satisfied by an empty domain -----------------

d=$(tree nosuite); rm -rf "$d/test/integration"
check "no suite directory is 'cannot see', not a pass" 2 "$d" "not a directory"

d=$(tree nosharder); table "$d" "TestAlpha_One	1.00"; rm "$d/scripts/integration-shard.sh"
check "no partitioner is 'cannot see', not a pass" 2 "$d" "does not exist"

d=$(tree notable); rm -f "$d/test/integration/testdata/main-suite-durations.tsv"
check "no table is 'cannot see', not a pass" 2 "$d" "missing, unreadable"

d=$(tree dirtable); rm -f "$d/test/integration/testdata/main-suite-durations.tsv"
mkdir -p "$d/test/integration/testdata/main-suite-durations.tsv"
check "an unreadable table (a directory) is 'cannot see'" 2 "$d" "not a regular file"

d=$(tree commentsonly); printf '# nothing but prose\n# and more prose\n' > "$d/test/integration/testdata/main-suite-durations.tsv"
check "a table of only comments is 'cannot see', not a pass" 2 "$d" "holds no rows"

d=$(tree notests); table "$d" "TestAlpha_One	1.00"; rm -f "$d"/test/integration/*_test.go
check "no test files at all is 'cannot see', not a pass" 2 "$d" "refused"

d=$(tree refuser); table "$d" "TestAlpha_One	1.00"
printf '#!/usr/bin/env bash\nexit 3\n' > "$d/scripts/integration-shard.sh"
check "a partitioner that refuses is 'cannot see', not a pass" 2 "$d" "refused"

d=$(tree silent); table "$d" "TestAlpha_One	1.00"
printf '#!/usr/bin/env bash\necho "^()$"\n' > "$d/scripts/integration-shard.sh"
check "a partitioner that names no test is 'cannot see', not a pass" 2 "$d" "named no tests"

# --- rule 1: a partitioned test with no row ----------------------------

d=$(tree missing); table "$d" "TestAlpha_One	1.00" "TestAlpha_Two	2.00"
check "a main-suite test with no row is red, and is named" 1 "$d" "TestBeta_One"

# --- rule 2: a row naming nothing the partitioner places ---------------

d=$(tree stray); table "$d" "TestAlpha_One	1.00" "TestAlpha_Two	2.00" "TestBeta_One	3.00" "TestGone_Renamed	4.00"
check "a row naming a test that does not exist is red, and is named" 1 "$d" "TestGone_Renamed"

d=$(tree strayfail); table "$d" "TestAlpha_One	1.00" "TestAlpha_Two	2.00" "TestBeta_One	3.00" "TestFailure_Excluded	9.00"
check "a failure-suite row is red — that job is not sharded" 1 "$d" "TestFailure_Excluded"

d=$(tree straypkg); table "$d" "TestAlpha_One	1.00" "TestAlpha_Two	2.00" "TestBeta_One	3.00" "TestHostConfig_EnablesInit	0.00"
check "a row from another package is red — it moves the mean" 1 "$d" "TestHostConfig_EnablesInit"

# --- rule 3: malformed rows --------------------------------------------

d=$(tree onefield); table "$d" "TestAlpha_One" "TestAlpha_Two	2.00" "TestBeta_One	3.00"
check "a row with no duration is red" 1 "$d" "field(s), want 2"

d=$(tree notanumber); table "$d" "TestAlpha_One	fast" "TestAlpha_Two	2.00" "TestBeta_One	3.00"
check "a non-numeric duration is red" 1 "$d" "want a non-negative number"

d=$(tree negative); table "$d" "TestAlpha_One	-1.00" "TestAlpha_Two	2.00" "TestBeta_One	3.00"
check "a negative duration is red" 1 "$d" "want a non-negative number"

d=$(tree comma); table "$d" "TestAlpha_One	1,00" "TestAlpha_Two	2.00" "TestBeta_One	3.00"
check "a comma-decimal duration is red — awk would read it as 1 (#554)" 1 "$d" "want a non-negative number"

# --- rule 4: duplicates -------------------------------------------------

d=$(tree dupe); table "$d" "TestAlpha_One	1.00" "TestAlpha_One	9.00" "TestAlpha_Two	2.00" "TestBeta_One	3.00"
check "a duplicated row is red" 1 "$d" "TestAlpha_One"

# --- green --------------------------------------------------------------

d=$(tree ok); table "$d" "TestAlpha_One	1.00" "TestAlpha_Two	2.00" "TestBeta_One	3.00"
check "exactly the partitioned set passes" 0 "$d" "name exactly the 3 test(s)"

d=$(tree zerocost); table "$d" "TestAlpha_One	0.00" "TestAlpha_Two	2.00" "TestBeta_One	3.00"
check "a measured 0.00 is legitimate and stays green" 0 "$d" "name exactly the 3 test(s)"

# --- drive the absence, against the tree that actually ships ------------

real="$TMP/real"
mkdir -p "$real/scripts" "$real/test/integration/testdata"
cp "$HERE/integration-shard.sh" "$real/scripts/"
cp "$REPO"/test/integration/*_test.go "$real/test/integration/"
cp "$REPO/test/integration/testdata/main-suite-durations.tsv" "$real/test/integration/testdata/"
check "a copy of the shipped tree passes" 0 "$real" "name exactly the"

victim=$(awk -F'\t' '$1 !~ /^#/ && NF == 2 { print $1 }' \
    "$real/test/integration/testdata/main-suite-durations.tsv" | sed -n '5p')
if [ -z "$victim" ]; then
    echo "FAIL: could not pick a row to delete — the shipped table has fewer than five rows"
    failures=$((failures + 1))
else
    cp "$real/test/integration/testdata/main-suite-durations.tsv" "$TMP/table.bak"
    grep -v "^${victim}	" "$TMP/table.bak" > "$real/test/integration/testdata/main-suite-durations.tsv"
    check "deleting $victim's row from the shipped table goes red" 1 "$real" "$victim"
    cp "$TMP/table.bak" "$real/test/integration/testdata/main-suite-durations.tsv"
    check "restoring it goes green again" 0 "$real" "name exactly the"
fi

# --- the real tree, and the wiring --------------------------------------

check "the real tree passes" 0 "$REPO" "name exactly the"

for wired in .github/workflows/test.yaml scripts/local-lane.sh; do
    if grep -q "check-durations-table.sh" "$REPO/$wired"; then
        echo "PASS: gate is wired into $wired"
    else
        echo "FAIL: gate is not wired into $wired"
        failures=$((failures + 1))
    fi
done

if [ "$failures" -ne 0 ]; then echo "$failures failure(s)"; exit 1; fi
echo "all passed"
