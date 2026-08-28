#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# gate-selftest-runs-in: staticcheck
#
# Why one invocation of staticcheck is not enough (#871), driven on a
# CONSTRUCTED fixture rather than on this repository.
#
# THE TREE IS THE WRONG WITNESS, and that is the whole reason this file
# exists. When #871 was filed there was a live example in the tree: a
# symbol defined in an untagged harness file whose only caller was
# tagged, reported as unused on an open PR. The #869 work gave that
# symbol a unit test, and the tree now has ZERO symbols crossing the
# tag boundary: `staticcheck ./...` and `staticcheck -tags integration
# ./...` are both rc=0, measured on this branch's base.
#
# No commit id is quoted for that, deliberately. An earlier draft cited
# one; the branch it was on was rebased before merging, the id it
# named exists in no merged history, and the claim could then only be
# checked by the person who wrote it. The property is what matters and
# the property is measurable by anyone, in two commands, on whatever
# tree they are holding.
#
# A self-test run over the real tree would therefore pass, and it would
# pass for a reason with nothing to do with the fix. That is the exact
# failure this repository keeps rediscovering: a check whose green says
# only that the tree happens to be clean today. So every case below
# builds its own module and asserts a verdict that MOVES.
#
# EACH BAD FIXTURE IS THE GOOD ONE PLUS ONE DEFECT. A fixture written
# from scratch tends to be red for a reason the author did not intend,
# and the assertion then passes on the wrong finding. So each case
# builds a clean control first, requires BOTH views to be silent on it,
# and only then adds the single defect — and the assertion names the
# symbol, not just the exit code.
#
# IT REFUSES RATHER THAN SKIPS. Without the staticcheck binary this
# exits 2. A skip here would be indistinguishable from a pass while
# exercising nothing, which is the bug class the file is about. The
# gate-self-test runner is told by the marker above that this belongs
# to the staticcheck job, which is the job that installs the binary,
# and the runner then requires a workflow to actually RUN this file.
#
# That last sentence read "to actually NAME this file" and it was
# false: naming was all the runner checked, and a comment naming the
# file satisfied it, so the step running this suite could be deleted
# with nothing going red. #872 hardened the runner to look only at
# shell a workflow executes; the sentence is now true, and it is true
# because the mechanism changed rather than because the wording did.
#
# Usage: bash scripts/test-staticcheck-tag-views.sh
# Exit:  0 all cases behaved as measured, 1 a case did not, 2 cannot run.

set -uo pipefail

SC="${STATICCHECK:-staticcheck}"
if ! command -v "$SC" >/dev/null 2>&1; then
    echo "FAIL  no staticcheck binary on PATH (tried '$SC'). This test refuses" >&2
    echo "      rather than skipping: a skip would be indistinguishable from a" >&2
    echo "      pass while exercising nothing, which is the bug class it covers." >&2
    exit 2
fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok   $1"; }
bad() { fail=$((fail+1)); echo "  FAIL $1"; }

# Build a module holding one package, from the files named as
# NAME=CONTENT pairs on stdin.
mkmod() { # <dir>
    local d="$1"
    mkdir -p "$d/pkg"
    cat > "$d/go.mod" <<EOF
module fixture

go 1.27
EOF
}

# Run one view and report "rc|output".
view() { # <dir> [tags]
    local d="$1" tags="${2-}" out rc
    if [ -n "$tags" ]; then
        out=$(cd "$d" && "$SC" -tags "$tags" ./... 2>&1); rc=$?
    else
        out=$(cd "$d" && "$SC" ./... 2>&1); rc=$?
    fi
    printf '%s|%s' "$rc" "$(printf '%s' "$out" | tr '\n' ';')"
}

# Assert a view is silent, and say what it found if it is not. A control
# that is red for an unintended reason makes every later assertion in
# the case meaningless, so this is checked before the defect is added.
want_clean() { # <desc> <dir> [tags]
    local r; r=$(view "$2" "${3-}")
    if [ "${r%%|*}" = "0" ]; then ok "$1"
    else bad "$1 — control is not clean, so nothing below it means anything: ${r#*|}"; fi
}

# Assert a view reports a finding NAMING the symbol. Exit code alone
# would pass on any other finding the fixture happened to contain.
want_finds() { # <desc> <symbol> <dir> [tags]
    local r; r=$(view "$3" "${4-}")
    if [ "${r%%|*}" != "0" ] && [[ "${r#*|}" == *"$2"* ]]; then ok "$1"
    else bad "$1 — expected a finding naming '$2', got rc=${r%%|*}: ${r#*|}"; fi
}

# Assert a view is silent WHERE A FINDING EXISTS in the other view.
want_blind() { # <desc> <symbol> <dir> [tags]
    local r; r=$(view "$3" "${4-}")
    if [ "${r%%|*}" = "0" ]; then ok "$1"
    else bad "$1 — expected silence, got: ${r#*|}"; fi
}

echo "1..N staticcheck tag views ($("$SC" -version 2>&1 | head -1))"

# --- 1. THE HOLE: a defect inside a tagged file ----------------------
#
# This is #871 itself. The default invocation never compiles the file,
# so it never parses it, and reports success over it. 64 of 196 tracked
# .go files were in exactly this position at 0b73c46 on dev.
echo "case 1: a defect inside a tagged file"
d="$TMP/hole"; mkmod "$d"
cat > "$d/pkg/plain.go" <<'EOF'
package pkg

func Used() {}
EOF
cat > "$d/pkg/tagged.go" <<'EOF'
//go:build integration

package pkg

func Used2() {}
EOF
want_clean "control: both views silent before the defect" "$d"
want_clean "control: tagged view silent too" "$d" integration
cat > "$d/pkg/tagged.go" <<'EOF'
//go:build integration

package pkg

func unusedInTaggedFile() {}
EOF
want_blind "the default view is BLIND to it" unusedInTaggedFile "$d"
want_finds "the integration view catches it" unusedInTaggedFile "$d" integration

# --- 2. PRESERVATION: the default view loses nothing -----------------
#
# The opposite failure, and the one a widening is most likely to cause.
# A fix that silenced ordinary untagged findings would look identical
# to this one from the outside — case 1 would still be green.
echo "case 2: a defect in an untagged file"
d="$TMP/keep"; mkmod "$d"
cat > "$d/pkg/plain.go" <<'EOF'
package pkg

func Used() {}
EOF
want_clean "control: silent before the defect" "$d"
cat > "$d/pkg/plain.go" <<'EOF'
package pkg

func unusedEverywhere() {}

func Used() {}
EOF
want_finds "the default view still catches it" unusedEverywhere "$d"
want_finds "and so does the integration view" unusedEverywhere "$d" integration

# --- 3. WHY THE DEFAULT VIEW MUST STAY -------------------------------
#
# The argument for two invocations rather than one widened flag, and it
# is measured here rather than asserted. A file carrying a NEGATED
# constraint compiles only without the tag, so a tag-only run is blind
# to it in the same way the default run was blind to case 1.
#
# There is no such file in this repository. That is precisely why its
# disappearance from CI would go unnoticed, and why the case is built
# rather than looked for.
echo "case 3: a defect behind a negated constraint"
d="$TMP/neg"; mkmod "$d"
cat > "$d/pkg/plain.go" <<'EOF'
package pkg

func Used() {}
EOF
cat > "$d/pkg/notint.go" <<'EOF'
//go:build !integration

package pkg

func Used2() {}
EOF
want_clean "control: default view silent before the defect" "$d"
want_clean "control: integration view silent too" "$d" integration
cat > "$d/pkg/notint.go" <<'EOF'
//go:build !integration

package pkg

func unusedWhenNotIntegration() {}
EOF
want_finds "the default view catches it" unusedWhenNotIntegration "$d"
want_blind "the integration view is BLIND to it — one flag would not do" \
    unusedWhenNotIntegration "$d" integration

# --- 4. THE TAG BOUNDARY, pinned rather than fixed -------------------
#
# A symbol defined in an untagged file whose only caller is tagged is
# reported as unused by the default view. That was the live report on
# an open PR when #871 was filed.
#
# RUNNING BOTH VIEWS DOES NOT SILENCE IT, and this case exists to say
# so in an executable place. The finding is true of the build it is
# made against: in the default build nothing calls that symbol. The
# resolution is in the code — tag the file, or give the symbol a caller
# that is not tagged, which is what 3c64b8f did.
#
# It is pinned because the tempting "fix" is to drop the default view,
# and that would silently reintroduce case 3.
echo "case 4: a symbol defined untagged and used only from tagged code"
d="$TMP/cross"; mkmod "$d"
cat > "$d/pkg/plain.go" <<'EOF'
package pkg

func helper() string { return "x" }

func Used() string { return helper() }
EOF
want_clean "control: silent while an untagged caller exists" "$d"
cat > "$d/pkg/plain.go" <<'EOF'
package pkg

func helper() string { return "x" }

func Used() {}
EOF
cat > "$d/pkg/tagged.go" <<'EOF'
//go:build integration

package pkg

func Caller() string { return helper() }
EOF
want_finds "the default view reports it unused" helper "$d"
want_blind "the integration view does not" helper "$d" integration

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
