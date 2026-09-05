#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# EVERY reader of GATE_SCOPE_BRANCHES must reach the same verdict from any
# working directory.
#
# WHY THIS EXISTS
#
# `GATE_SCOPE_BRANCHES="dev main 2.*"` is a word list, and a scope word may
# be a filter pattern. Splitting it with `for w in $value` runs PATHNAME
# EXPANSION first: the shell replaces `2.*` with whatever the CURRENT
# DIRECTORY happens to contain, and only then splits. So the same tree, the
# same remote and the same scope file produce different verdicts depending
# on where the gate was invoked from -- and in the direction that hurts,
# because a cwd holding a match silently drops the
# pattern-must-match-a-branch obligation.
#
# `check-missing-runs.sh` and `purge-workflow-runs.sh` were guarded with
# `set -f` at each of their splitting sites, and the scope file's own prose
# said so and counted them. `check-branch-refs.sh` was added later, split
# the same value, and was not guarded -- and the count in the prose did not
# notice, because a count in prose cannot.
#
# WHAT THIS IS
#
# The instrument that replaces the count. It DISCOVERS the readers (any
# non-test script that names the variable or the scope file), runs each one
# twice with identical stubs -- once from an empty directory, once from a
# directory seeded with file names the scope's own patterns match -- and
# requires the two runs to be identical in exit code and in output.
#
# A reader it does not know how to run is a FAILURE, not a skip. That is the
# discovery half: a fourth reader appearing tomorrow makes this suite red
# until somebody teaches it how to drive it, which is exactly the event the
# prose count missed.
#
# The verdicts are not required to be PASS. Several of these runs refuse
# under the stubs, and that is fine: the property is that the working
# directory does not change the answer, not what the answer is.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
SCOPE="$ROOT/.github/gate-branch-scope.env"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

[ -f "$SCOPE" ] || { echo "FAIL  ${SCOPE} does not exist; there is no value to split" >&2; exit 1; }

# --- the seed, derived from the scope file's own words ------------------
#
# Not a hand-written list of file names: the names come from the patterns
# the scope actually carries, so a scope that stops using patterns makes
# this suite refuse rather than pass on an empty seed.
scope_value=$(sed -n 's/^[[:space:]]*GATE_SCOPE_BRANCHES=//p' "$SCOPE" | tr -d '"' | tail -1)
mkdir -p "$WORK/empty" "$WORK/seeded"
seeded=0
patterns=0
set -f
for w in $scope_value; do
    [ -n "$w" ] || continue
    case "$w" in
        *[*?]*)
            patterns=$((patterns + 1))
            # Two matches for each pattern: one that is also a real branch,
            # one that is emphatically not. The second is the one that turned
            # a PASS into "'2.zzz-not-a-branch' is not a branch on origin".
            for sub in "0.0" "zzz-not-a-branch"; do
                name=$(printf '%s' "$w" | sed -e "s/\*/${sub}/g" -e "s/?/z/g")
                : > "$WORK/seeded/$name" && seeded=$((seeded + 1))
            done ;;
        *)
            : > "$WORK/seeded/$w" && seeded=$((seeded + 1)) ;;
    esac
done
set +f

if [ "$patterns" -eq 0 ]; then
    no "GATE_SCOPE_BRANCHES carries no pattern word, so nothing here can distinguish a guarded split from an unguarded one. This suite is not allowed to pass on an empty domain."
    printf '\n%d passed, %d failed\n' "$pass" "$fail"
    exit 1
fi

# --- the stubs ---------------------------------------------------------
mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'GHEOF'
#!/bin/sh
# The TRANSPORT, not the verdict. Enough of an answer that each reader gets
# past its own network calls and reaches the code that splits the scope.
case "$*" in
    *"repo view"*)  echo "claymore666/docker-net-dhcp" ;;
    *branches*)     printf 'dev\nmain\n2.0.0\n' ;;
    *commits*)      printf 'aaaaaaaaaaaa\n' ;;
    *)              : ;;
esac
exit 0
GHEOF
chmod +x "$WORK/bin/gh"

cat > "$WORK/heads.txt" <<'HEADSEOF'
1111111111111111111111111111111111111111	refs/heads/dev
2222222222222222222222222222222222222222	refs/heads/main
3333333333333333333333333333333333333333	refs/heads/2.0.0
HEADSEOF

# --- the population, discovered ----------------------------------------
readers=$(grep -lE 'GATE_SCOPE_BRANCHES|gate-branch-scope\.env' "$ROOT"/scripts/*.sh 2>/dev/null |
          grep -v '/test-' | sort)
n_readers=$(printf '%s\n' "$readers" | grep -c .)
if [ "$n_readers" -eq 0 ]; then
    no "no script in scripts/ reads GATE_SCOPE_BRANCHES. Either the scope moved or the discovery broke; an empty population is not a pass."
    printf '\n%d passed, %d failed\n' "$pass" "$fail"
    exit 1
fi

# Run one reader from one directory. The recipe per reader is the minimum
# that gets it past its transport; NOW_EPOCH and the fixed stubs keep two
# runs comparable.
drive() { # <script> <cwd> -> "<rc>\n<output>"
    local script="$1" dir="$2" out rc
    case "$(basename "$script")" in
        check-branch-refs.sh)
            out=$( cd "$dir" && BRANCH_REFS_HEADS_FILE="$WORK/heads.txt" \
                   bash "$script" 2>&1 )
            rc=$? ;;
        check-missing-runs.sh)
            out=$( cd "$dir" && PATH="$WORK/bin:$PATH" GATE_REPO=claymore666/docker-net-dhcp \
                   bash "$script" 2>&1 )
            rc=$? ;;
        branch-glob.sh)
            # A sourced library, so it is driven by calling the two functions
            # that TAKE the word list. They carry their own `set -f`; this is
            # what checks that they still do, from a cwd where it matters.
            out=$( cd "$dir" && SCOPE_VALUE="$scope_value" HEADS_FILE="$WORK/heads.txt" \
                   bash -c '
                       . "$0"
                       heads=$(sed "s|.*refs/heads/||" "$HEADS_FILE")
                       if branch_glob_list_has_pattern "$SCOPE_VALUE"; then
                           echo "has-pattern: yes"
                       else
                           echo "has-pattern: no"
                       fi
                       echo "scope: $SCOPE_VALUE"
                       branch_glob_expand_list "$SCOPE_VALUE" "$heads"
                       echo "expand rc=$?"
                   ' "$script" 2>&1 )
            rc=$? ;;
        purge-workflow-runs.sh)
            out=$( cd "$dir" && PATH="$WORK/bin:$PATH" REPO=claymore666/docker-net-dhcp \
                   NOW_EPOCH=1767225600 DRY_RUN=1 bash "$script" 2>&1 )
            rc=$? ;;
        *)
            printf 'UNKNOWN\n'
            return ;;
    esac
    printf '%s\n%s\n' "$rc" "$out"
}

reached=0
for r in $readers; do
    name=$(basename "$r")
    a=$(drive "$r" "$WORK/empty")
    if [ "$a" = "UNKNOWN" ]; then
        no "$name reads GATE_SCOPE_BRANCHES and this instrument does not know how to run it. A reader that cannot be driven is a reader that is not covered — teach drive() how to invoke it."
        continue
    fi
    b=$(drive "$r" "$WORK/seeded")
    if [ "$a" != "$b" ]; then
        no "$name gives a different verdict from a directory holding branch-shaped file names"
        diff <(printf '%s\n' "$a") <(printf '%s\n' "$b") | head -8 >&2
        continue
    fi
    # The run has to have REACHED the scope, or "identical" is measuring two
    # runs that both died before the split.
    if printf '%s' "$a" | grep -F -- "$scope_value" >/dev/null ||
       printf '%s' "$a" | grep -E 'scope|GATE_SCOPE_BRANCHES' >/dev/null; then
        reached=$((reached + 1))
        ok "$name: same verdict from an empty cwd and from one seeded with ${seeded} branch-shaped file(s)"
    else
        no "$name: the two runs agree, but neither one reached the branch scope, so they agree about nothing"
        printf '      %s\n' "$a" | head -4 >&2
    fi
done

if [ "$reached" -eq 0 ]; then
    no "no reader reached its branch scope under these stubs; the suite proved nothing"
fi

printf '\n%s\n' "readers discovered: $(printf '%s\n' "$readers" | sed "s|$ROOT/||" | tr '\n' ' ')"
printf '%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
