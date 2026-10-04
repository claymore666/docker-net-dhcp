#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-doc-opener.sh (#861).
#
# The defect is a comment, so every fixture is a small Go tree. Each rule
# case is paired with its absence: the opener naming another test fails, the
# same block opening with the function's own name, with prose, or with a
# different shape of func passes. The real tree is a case too, so the gate
# is shown clean for the reasons the repository is.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
GATE="$HERE/check-doc-opener.sh"

guarded_tmpdir TMP
cp "$HERE/gatelib.sh" "$TMP/"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# put <dir> <path>: stdin becomes the file, plus a clean doc'd func so a
# fixture is never refused as empty unless it means to be.
put() { mkdir -p "$(dirname "$1/$2")"; cat > "$1/$2"; }
anchor() {
    put "$1" pkg/anchor/anchor.go <<'GO'
package anchor

// Anchor gives every fixture one doc block the gate can read.
func Anchor() {}
GO
}

fx_real_tree() { mkdir -p "$1"; cp -r "$REPO/pkg" "$REPO/cmd" "$REPO/test" "$1/"; }

fx_other_test_missing() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// TestNewClient_ChildGetsItsOwnProcessGroup sets Setpgid and reads the group back.
func TestSweepOrphans_SparesALiveClient(t *testing.T) {}
GO
}

fx_other_test_exists() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// TestSweepOrphans_Unreadable checks the unreadable case.
func TestSweepOrphans_Unreadable(t *testing.T) {}

// TestSweepOrphans_Unreadable checks a missing field, not an unreadable one.
func TestSweepOrphans_Missing(t *testing.T) {}
GO
}

fx_own_name() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// TestSweepOrphans_Missing checks a missing field is not a match.
func TestSweepOrphans_Missing(t *testing.T) {}
GO
}

fx_prose_opener() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// A missing field is not a match; compare TestSweepOrphans_Unreadable for the other case.
func TestSweepOrphans_Missing(t *testing.T) {}

// Test that the sweep spares it.
func TestA(t *testing.T) {}

// Tests that the sweep spares it.
func TestB(t *testing.T) {}

// Testing the sweep.
func TestC(t *testing.T) {}
GO
}

fx_other_test_in_body() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// TestSweepOrphans_Missing checks a missing field.
// Unlike TestSweepOrphans_Unreadable it never reads the file.
func TestSweepOrphans_Missing(t *testing.T) {}
GO
}

fx_multiline_bad() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// TestNewClient_ChildGetsItsOwnProcessGroup sets Setpgid on the child.
// The group is read back through the child's pid, and the test
// kills it.
func TestSweepOrphans_Spares(t *testing.T) {}
GO
}

fx_opener_is_a_prefix() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// TestSweep checks the sweep.
func TestSweep_Missing(t *testing.T) {}
GO
}

fx_opener_extends_the_name() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// TestA_B checks the sweep.
func TestA(t *testing.T) {}
GO
}

fx_underscore_opener() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// Test_other checks the sweep.
func Test_x(t *testing.T) {}
GO
}

fx_digit_opener() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

import "testing"

// Test1Other checks the sweep.
func Test1x(t *testing.T) {}
GO
}

fx_method() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

type s struct{}

// TestOther checks something else.
func (r *s) TestHelper() {}
GO
}

fx_method_own() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

type s struct{}

// TestHelper checks the thing.
func (r *s) TestHelper() {}
GO
}

fx_generic() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

// TestOther checks something else.
func TestHelper[T any](v T) {}
GO
}

fx_helper_names_test() {
    anchor "$1"
    put "$1" pkg/x/a.go <<'GO'
package x

// TestSweepOrphans_Missing needs this to build the tree.
func buildTree() {}
GO
}

fx_directive_first_bad() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

//go:noinline
// TestOther checks something else.
func TestHelper() {}
GO
}

fx_directive_first_ok() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

// Helper keeps its frame, see TestOther.
//
//go:noinline
func TestHelper() {}

//go:noinline
// TestOwn keeps its frame.
func TestOwn() {}
GO
}

fx_blank_line() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

// TestOther is a floating comment, not a doc block.

func TestHelper() {}
GO
}

fx_no_space() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

//TestOther checks something else.
func TestHelper() {}
GO
}

fx_two_directives_bad() {
    anchor "$1"
    put "$1" pkg/x/a_test.go <<'GO'
package x

//go:noinline
//go:nosplit
// TestOther checks something else.
func TestHelper() {}
GO
}

fx_tab_opener() {
    anchor "$1"
    printf 'package x\n\n//\tTestOther checks something else.\nfunc TestHelper() {}\n' | put "$1" pkg/x/a_test.go
}

fx_in_non_test_file() {
    anchor "$1"
    put "$1" cmd/y/main.go <<'GO'
package main

// TestOther checks something else.
func TestHelper() {}
GO
}

fx_in_testdata_only() {
    anchor "$1"
    put "$1" pkg/x/testdata/a.go <<'GO'
package x

// TestOther checks something else.
func TestHelper() {}
GO
}

fx_real_tree_plus_hit() {
    fx_real_tree "$1"
    put "$1" pkg/plugin/zz_planted_test.go <<'GO'
package plugin

import "testing"

// TestSaveOptions_AtomicNoTornFile checks the rename.
func TestSaveOptions_LeavesNoTempFilesPlanted(t *testing.T) {}
GO
}

fx_no_go_files() { mkdir -p "$1/docs"; printf '# x\n' > "$1/docs/x.md"; }

fx_no_doc_blocks() {
    put "$1" pkg/x/a.go <<'GO'
package x

func Bare() {}
GO
}

fx_unreadable_file() {
    anchor "$1"
    printf 'package x\n// \377\376 not utf-8\nfunc F() {}\n' > "$1/pkg/x.go"
}

CASES=(
    "the repository as it stands is clean|fx_real_tree|0"
    "the repository plus one planted hit fails|fx_real_tree_plus_hit|1"
    "an opener naming a test that does not exist fails|fx_other_test_missing|1"
    "an opener naming another test that DOES exist fails|fx_other_test_exists|1"
    "an opener naming the func's own name passes|fx_own_name|0"
    "prose openers pass, including Test/Tests/Testing as words|fx_prose_opener|0"
    "another test named later in the block passes|fx_other_test_in_body|0"
    "a long block whose first line names another test fails|fx_multiline_bad|1"
    "an opener that is a prefix of the func's name fails|fx_opener_is_a_prefix|1"
    "an opener that extends the func's name fails|fx_opener_extends_the_name|1"
    "an opener Test_name fails|fx_underscore_opener|1"
    "an opener Test1Name fails|fx_digit_opener|1"
    "a method documented as another test fails|fx_method|1"
    "a method opening with its own name passes|fx_method_own|0"
    "a generic func documented as another test fails|fx_generic|1"
    "a helper that opens by naming a test fails|fx_helper_names_test|1"
    "a directive line before the opener is skipped, bad opener still fails|fx_directive_first_bad|1"
    "a directive line before or after a clean opener passes|fx_directive_first_ok|0"
    "a comment separated by a blank line is not a doc block|fx_blank_line|0"
    "an opener with no space after the slashes fails|fx_no_space|1"
    "two directive lines before the opener are both skipped, bad opener still fails|fx_two_directives_bad|1"
    "an opener separated from the slashes by a tab fails|fx_tab_opener|1"
    "a hit in a non-test file fails|fx_in_non_test_file|1"
    "a hit under testdata is out of scope|fx_in_testdata_only|0"
    "a tree with no Go files is refused|fx_no_go_files|2"
    "a tree with no doc-commented func is refused|fx_no_doc_blocks|2"
    "an unreadable file is refused, not passed|fx_unreadable_file|2"
)

# verdict <gate> <builder> -> exit code
verdict() {
    local gate="$1" builder="$2" d rc
    guarded_tmpdir d "$TMP/fx.XXXXXX"
    git init -q "$d"
    "$builder" "$d"
    bash "$gate" "$d" >/dev/null 2>&1
    rc=$?
    echo "$rc"
}

echo "--- the gate itself ---"
for c in "${CASES[@]}"; do
    IFS='|' read -r desc builder want <<< "$c"
    got=$(verdict "$GATE" "$builder")
    if [ "$got" = "$want" ]; then ok "$desc"; else no "$desc: want exit $want, got $got"; fi
done

# The report names file and line, so a hit is findable.
guarded_tmpdir d "$TMP/fx.XXXXXX"
git init -q "$d"
fx_other_test_missing "$d"
out=$(bash "$GATE" "$d" 2>&1)
if printf '%s' "$out" | grep 'pkg/x/a_test.go:6: TestSweepOrphans_SparesALiveClient is documented as TestNewClient_ChildGetsItsOwnProcessGroup' >/dev/null; then
    ok "the report names the file, line, func and the opener"
else
    no "the report does not name the hit: $out"
fi

echo "--- mutants ---"
mutant() {
    local path="$TMP/mutant-$1.sh"
    printf '#!/usr/bin/env bash\nexit %s\n' "$1" > "$path"
    echo "$path"
}
for code in 0 1; do
    m=$(mutant "$code")
    killed=0
    for c in "${CASES[@]}"; do
        IFS='|' read -r desc builder want <<< "$c"
        [ "$(verdict "$m" "$builder")" = "$want" ] || killed=$((killed + 1))
    done
    if [ "$killed" -gt 0 ]; then
        ok "the always-exit-$code mutant is killed by $killed of ${#CASES[@]} cases"
    else
        no "the always-exit-$code mutant survives every case"
    fi
done

echo
if [ "$fail" -gt 0 ]; then
    echo "check-doc-opener meta-test: $pass passed, $fail FAILED" >&2
    exit 1
fi
echo "check-doc-opener meta-test: $pass passed, 0 failed"
