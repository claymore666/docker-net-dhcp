#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for scripts/tmpdir-guard.sh (#963).
#
# The subject is a refusal, so the cases that matter DRIVE THE ABSENCE:
# with `mktemp -d` returning nothing, a real gate self-test must stop
# instead of building its fixture in whatever directory it was started
# from. Case 2 runs the shipped test-check-coverage-floor.sh, copied not
# reimplemented, both as it stands and mechanically reverted to the shape
# that shipped, so the rig is shown to see the damage before it is asked
# to certify its absence.
#
# Every refusal the guard can raise has a case of its own here, and each
# was driven by deleting that branch alone and watching this suite go
# red. A branch with no observer is a branch that can be deleted.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
LIB="$HERE/tmpdir-guard.sh"
REPO="$(cd "$HERE/.." && pwd)"

pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

[ -f "$LIB" ] || { echo "cannot find $LIB"; exit 2; }

# This suite's own sandbox does not use the subject: a broken guard must
# not be able to decide where this file writes.
WORK=$(mktemp -d) || WORK=
[ -n "$WORK" ] && [ -d "$WORK" ] || { echo "cannot make a sandbox"; exit 2; }
trap 'rm -rf "$WORK"' EXIT

# stub_mktemp <dir> <body> — a PATH directory holding one `mktemp`.
stub_mktemp() {
    mkdir -p "$1"
    printf '#!/bin/sh\n%s\n' "$2" > "$1/mktemp"
    chmod +x "$1/mktemp"
}

# An inert `mktemp` that succeeds and prints nothing: the failure mode
# measured on the session box, where /tmp had no inodes left. It is a
# stand-in for the binary because that is what the shipped scripts go
# through -- they call `mktemp`, not a shell function.
INERT="$WORK/bin-inert"
stub_mktemp "$INERT" 'exit 0'

# --- 1. the safe shape, first: the guard hands out a usable directory --

cat > "$WORK/normal.sh" <<SH
#!/usr/bin/env bash
set -u
. "$LIB"
guarded_tmpdir d
printf '%s\n' "\$d" > "$WORK/normal.path"
cd "\$d" || exit 2
touch inside
SH
( cd "$WORK" && bash "$WORK/normal.sh" ) >"$WORK/normal.out" 2>&1
rc=$?
np="$(cat "$WORK/normal.path" 2>/dev/null || true)"
if [ "$rc" -eq 0 ] && [ -n "$np" ]; then
    ok "an ordinary temp path is not refused"
else
    no "an ordinary temp path is not refused (exit $rc, path '$np')"
    sed 's/^/      /' "$WORK/normal.out" >&2
fi
if [ -n "$np" ] && [ ! -e "$np" ]; then
    ok "the directory is removed when the script exits"
else
    no "the directory is removed when the script exits ('$np' still there)"
fi
if [ ! -e "$WORK/inside" ]; then
    ok "the fixture was built in the temp directory, not the caller's cwd"
else
    no "the fixture was built in the temp directory, not the caller's cwd"
fi

# --- 2. the incident, driven through a shipped self-test ---------------
#
# A sandbox holding the real gate, the real self-test and the real
# library. Nothing here is a rewrite of the block under test.

sandbox() { # sandbox <dir> — populate a runnable copy
    local sb="$1"
    mkdir -p "$sb/scripts"
    cp "$REPO/scripts/check-coverage-floor.sh" \
       "$REPO/scripts/test-check-coverage-floor.sh" \
       "$REPO/scripts/tmpdir-guard.sh" "$sb/scripts/"
}

SB="$WORK/guarded"
sandbox "$SB"
( cd "$SB" && PATH="$INERT:$PATH" bash scripts/test-check-coverage-floor.sh ) \
    >"$WORK/guarded.out" 2>&1
grc=$?
if [ "$grc" -ne 0 ]; then
    ok "a shipped self-test stops when mktemp -d returns nothing"
else
    no "a shipped self-test stops when mktemp -d returns nothing (exit 0)"
fi
if grep -F 'Unsafe temp directory' "$WORK/guarded.out" >/dev/null; then
    ok "it stops naming the temp path, not something downstream of it"
else
    no "it stops naming the temp path, not something downstream of it"
    sed 's/^/      /' "$WORK/guarded.out" | head -5 >&2
fi
if [ ! -e "$SB/.git" ]; then
    ok "no fixture repository is created in the directory it was started from"
else
    no "no fixture repository is created in the directory it was started from"
fi

# The same rig against the shape that shipped, produced by reverting the
# real file rather than by writing a lookalike. If this does not damage
# the sandbox, the three assertions above are measuring nothing.
PRE="$WORK/prefix"
sandbox "$PRE"
sed -e '/tmpdir-guard\.sh/d' \
    -e 's/^\( *\)guarded_tmpdir \([A-Za-z_][A-Za-z0-9_]*\)$/\1\2=$(mktemp -d)/' \
    "$REPO/scripts/test-check-coverage-floor.sh" > "$PRE/scripts/test-check-coverage-floor.sh"
if grep -F '=$(mktemp -d)' "$PRE/scripts/test-check-coverage-floor.sh" >/dev/null; then
    ok "the control really is the pre-fix shape"
else
    no "the control really is the pre-fix shape (the revert matched nothing)"
fi
( cd "$PRE" && PATH="$INERT:$PATH" bash scripts/test-check-coverage-floor.sh ) \
    >"$WORK/prefix.out" 2>&1
if [ -e "$PRE/.git" ]; then
    ok "the pre-fix shape does build its fixture in that directory"
else
    no "the pre-fix shape does build its fixture in that directory, so this rig proves nothing"
fi

# --- 3. the refusals, one case per branch ------------------------------
#
# Each of these was driven by deleting its branch in tmpdir-guard.sh,
# alone, and watching the named case fail.

cat > "$WORK/ref.sh" <<SH
#!/usr/bin/env bash
set -u
. "$LIB"
guarded_tmpdir d
cd "\$d" || exit 2
touch "$WORK/REFUSAL-LEAKED"
SH

cat > "$WORK/ref-noarg.sh" <<SH
#!/usr/bin/env bash
set -u
. "$LIB"
guarded_tmpdir
touch "$WORK/REFUSAL-LEAKED"
SH

refuses() { # refuses <name> <fixture> <needle> <env assignment...>
    local name="$1" fixture="$2" needle="$3"; shift 3
    local out rc
    out=$( cd "$WORK" && env "$@" bash "$WORK/$fixture" 2>&1 ); rc=$?
    # 2 is this repository's "cannot tell", and a refusal must not be
    # confused with a gate's own verdict of 1.
    if [ "$rc" -ne 2 ]; then
        no "$name (exit $rc, want 2)"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
        return
    fi
    if printf '%s\n' "$out" | grep -F "$needle" >/dev/null; then
        ok "$name"
    else
        no "$name (exit 2, but the message does not mention '$needle')"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
    fi
}

refuses "an empty mktemp -d is a refusal" ref.sh "empty path" \
        "PATH=$INERT:$PATH"

# The same branch, reached the way a real box reaches it rather than
# through a stand-in: a TMPDIR that cannot be written makes the real
# mktemp print nothing. Named so it does not read as a second branch.
refuses "an unwritable TMPDIR reaches the empty-path refusal" ref.sh "empty path" \
        "TMPDIR=$WORK/no-such-dir"

: > "$WORK/a-file"
stub_mktemp "$WORK/bin-file" "printf %s '$WORK/a-file'"
refuses "a temp path that is not a directory is a refusal" ref.sh "not a directory" \
        "PATH=$WORK/bin-file:$PATH"

# With no resolvable guarded root, `/` is the only branch left that can
# refuse this, which is what makes the case an observer for it.
stub_mktemp "$WORK/bin-root" "printf %s /"
refuses "a temp path that resolves to / is a refusal" ref.sh "resolves to /" \
        "PATH=$WORK/bin-root:$PATH" "TMPDIR_GUARD_ROOT=$WORK/no-such-root"

# Through a stand-in that hands back a directory under this suite's own
# sandbox: a refused path is never removed by the guard (it is, by
# definition, a path the guard does not trust), so driving these cases
# through the real mktemp would leave a directory behind per run.
mkdir -p "$WORK/root/under" "$WORK/asroot" "$WORK/deep/root"
stub_mktemp "$WORK/bin-inside" "printf %s '$WORK/root/under'"
refuses "a temp path inside the guarded tree is a refusal" ref.sh "inside the repository" \
        "PATH=$WORK/bin-inside:$PATH" "TMPDIR_GUARD_ROOT=$WORK/root"

stub_mktemp "$WORK/bin-asroot" "printf %s '$WORK/asroot'"
refuses "a temp path that is the guarded tree is a refusal" ref.sh "repository root" \
        "PATH=$WORK/bin-asroot:$PATH" "TMPDIR_GUARD_ROOT=$WORK/asroot"

stub_mktemp "$WORK/bin-above" "printf %s '$WORK/deep'"
refuses "a temp path that contains the guarded tree is a refusal" ref.sh "contains the repository" \
        "PATH=$WORK/bin-above:$PATH" "TMPDIR_GUARD_ROOT=$WORK/deep/root"

# A directory the caller cannot enter passes `[ -d ]` and fails `cd`, so
# it has a branch of its own and needs a case of its own.
mkdir -p "$WORK/sealed"
chmod 000 "$WORK/sealed"
stub_mktemp "$WORK/bin-sealed" "printf %s '$WORK/sealed'"
refuses "a temp path that cannot be entered is a refusal" ref.sh "cannot resolve" \
        "PATH=$WORK/bin-sealed:$PATH"
chmod 755 "$WORK/sealed"

# A newline in the path would split one registry line into two, and the
# second half is whatever the path happens to end with.
NLDIR="$WORK/new
line"
mkdir -p "$NLDIR"
stub_mktemp "$WORK/bin-newline" "printf '%s' '$NLDIR'"
refuses "a temp path containing a newline is a refusal" ref.sh "contains a newline" \
        "PATH=$WORK/bin-newline:$PATH"

refuses "guarded_tmpdir without a variable name is a refusal" ref-noarg.sh \
        "needs the name of a variable" "TMPDIR=${TMPDIR:-/tmp}"

# The registry IS the cleanup for the 77 callers that gave up their own
# trap, so a registry that cannot be written is a refusal and not a shrug.
# TMPDIR points inside this suite's sandbox because this is the one case
# where the path passes every check and is then refused: the directory
# mktemp really made is not removed (see the library), so it has to be
# made somewhere this suite already sweeps.
mkdir -p "$WORK/registry-is-a-directory" "$WORK/reg-tmp"
refuses "a registry that cannot be written is a refusal" ref.sh "cleanup registry" \
        "TMPDIR=$WORK/reg-tmp" "TMPDIR_GUARD_REGISTRY=$WORK/registry-is-a-directory"

if [ ! -e "$WORK/REFUSAL-LEAKED" ]; then
    ok "a refusal writes nothing in the directory it was started from"
else
    no "a refusal writes nothing in the directory it was started from"
    rm -f "$WORK/REFUSAL-LEAKED"
fi

# --- 4. how far a refusal reaches, and where it stops -------------------

cat > "$WORK/sub.sh" <<SH
#!/usr/bin/env bash
set -u
. "$LIB"
mk() { local w; guarded_tmpdir w; printf '%s' "\$w"; }
ws=\$(mk)
cd "\$ws" || exit 2
touch "$WORK/SUBSHELL-LEAKED"
SH
( cd "$WORK" && PATH="$INERT:$PATH" bash "$WORK/sub.sh" ) >/dev/null 2>&1
src=$?
if [ "$src" -eq 2 ] && [ ! -e "$WORK/SUBSHELL-LEAKED" ]; then
    ok "a refusal raised in a command substitution ends the script"
else
    no "a refusal raised in a command substitution ends the script (exit $src, want 2)"
    rm -f "$WORK/SUBSHELL-LEAKED"
fi

# A caller with its own TERM handler must not be able to catch the
# refusal and carry on: that handler runs, and the exit still happens.
cat > "$WORK/sub-term.sh" <<SH
#!/usr/bin/env bash
set -u
trap 'printf caught > "$WORK/term-caught"' TERM
. "$LIB"
mk() { local w; guarded_tmpdir w; printf '%s' "\$w"; }
ws=\$(mk)
cd "\$ws" || exit 2
touch "$WORK/TERMTRAP-LEAKED"
SH
( cd "$WORK" && PATH="$INERT:$PATH" bash "$WORK/sub-term.sh" ) >/dev/null 2>&1
trc=$?
if [ "$trc" -eq 2 ] && [ ! -e "$WORK/TERMTRAP-LEAKED" ] && [ -s "$WORK/term-caught" ]; then
    ok "a caller's own TERM handler runs and does not rescue the caller"
else
    no "a caller's own TERM handler runs and does not rescue the caller (exit $trc)"
    rm -f "$WORK/TERMTRAP-LEAKED"
fi

# THE BOUNDARY, pinned rather than claimed. `exit` ends the subshell the
# refusal was raised in; the signal that stops the TOP-LEVEL shell is
# delivered between ITS commands. So in a subshell nested inside another
# subshell, the enclosing list runs to its end first -- measured, and the
# only one of four shapes (nested substitution, pipeline element,
# background job, plain `( ... )`) where anything after the refusal runs
# at all. The script still stops with 2 either way. Both halves are
# asserted so the docstring's boundary cannot quietly become false.
cat > "$WORK/sub-nested.sh" <<SH
#!/usr/bin/env bash
set -u
. "$LIB"
x=\$( ( guarded_tmpdir w ); touch "$WORK/NESTED-LIST-RAN" )
touch "$WORK/NESTED-AFTER-RAN"
SH
( cd "$WORK" && PATH="$INERT:$PATH" bash "$WORK/sub-nested.sh" ) >/dev/null 2>&1
erc=$?
if [ "$erc" -eq 2 ] && [ ! -e "$WORK/NESTED-AFTER-RAN" ]; then
    ok "a refusal in a nested subshell still stops the script"
else
    no "a refusal in a nested subshell still stops the script (exit $erc, want 2)"
    rm -f "$WORK/NESTED-AFTER-RAN"
fi
if [ -e "$WORK/NESTED-LIST-RAN" ]; then
    ok "and its boundary holds: the enclosing list ran to its end first"
else
    no "and its boundary holds: the enclosing list ran to its end first (the docstring says it does)"
fi

# --- 5. what the sweep has to reach ------------------------------------

cat > "$WORK/sweep.sh" <<SH
#!/usr/bin/env bash
set -u
trap 'printf own-trap-ran > "$WORK/own-trap"' EXIT
. "$LIB"
mk() { local w; guarded_tmpdir w; printf '%s' "\$w"; }
guarded_tmpdir top
sub=\$(mk)
printf '%s\n%s\n%s\n' "\$top" "\$sub" "\$_tmpdir_guard_registry" > "$WORK/sweep.paths"
exit 3
SH
( cd "$WORK" && bash "$WORK/sweep.sh" ) >/dev/null 2>&1
wrc=$?
top="$(sed -n 1p "$WORK/sweep.paths" 2>/dev/null || true)"
sub="$(sed -n 2p "$WORK/sweep.paths" 2>/dev/null || true)"
reg="$(sed -n 3p "$WORK/sweep.paths" 2>/dev/null || true)"
if [ "$wrc" -eq 3 ]; then
    ok "the trap does not swallow the script's exit status"
else
    no "the trap does not swallow the script's exit status (exit $wrc, want 3)"
fi
if [ -n "$top" ] && [ ! -e "$top" ]; then
    ok "a directory is swept when the script dies mid-run"
else
    no "a directory is swept when the script dies mid-run ('$top')"
fi
if [ -n "$sub" ] && [ ! -e "$sub" ]; then
    ok "a directory made inside a command substitution is swept too"
else
    no "a directory made inside a command substitution is swept too ('$sub')"
fi
if [ -s "$WORK/own-trap" ]; then
    ok "an EXIT trap the caller set before sourcing still runs"
else
    no "an EXIT trap the caller set before sourcing still runs"
fi
if [ -n "$reg" ] && [ ! -e "$reg" ]; then
    ok "that shell's own registry file is gone with it"
else
    no "that shell's own registry file is gone with it ('$reg')"
fi

# --- 6. the census: no self-test takes a directory from mktemp itself ---
#
# KEYED ON THE SOURCE, NOT ON THE USE. The first version of this asked
# "does a `cd` reach a variable assigned from `mktemp -d`", and a gate
# keyed on one spelling reproduces its own silence: `local dir; dir=$(...)`
# -- the idiom twelve sites in this tree already used -- walked straight
# past it, as did `declare`, `--directory`, an unquoted `cd $d`, a second
# variable holding the first, a helper function's stdout, `cd "$(mktemp
# -d)"` and `pushd`. Asking instead whether the file calls `mktemp` for a
# directory AT ALL closes every one of those at once, because a directory
# a self-test never made is a directory no spelling of `cd` can reach.
#
# Domain: every tracked `scripts/test-*.sh`. Not the gates: a self-test
# copies the gate it judges into a fixture tree, so a gate that sourced
# this library would break the moment it was copied without it. The nine
# directory sites in the eight non-test scripts were read instead --
# every one carries its own EXIT trap and none is ever `cd`-ed into
# (measured), as is the one in .github/workflows/test.yaml, which also
# runs under `set -euo pipefail`.
#
# This file is the one exemption, named rather than pattern-matched
# away: it has to contain the pre-fix shape to drive it.

raw_mktemp_sites() { # <file>... -> "file:line: text" per raw directory mktemp
    [ "$#" -gt 0 ] || return 0
    awk '
        { t = $0; sub(/^[ \t]+/, "", t) }
        t ~ /^#/ { next }
        /mktemp/ && /(^|[ \t])(-d|--directory)($|[^a-zA-Z0-9_-])/ {
            printf "%s:%d: %s\n", FILENAME, FNR, $0
        }
    ' "$@"
}

domain=()
while IFS= read -r f; do
    case "$f" in */test-tmpdir-guard.sh) continue ;; esac
    domain+=("$REPO/$f")
done < <(cd "$REPO" && git ls-files 'scripts/test-*.sh')

if [ "${#domain[@]}" -gt 0 ]; then
    ok "the census has a domain to inspect (${#domain[@]} self-test(s))"
else
    no "the census has a domain to inspect — an empty domain would pass having read nothing"
fi

census="$(raw_mktemp_sites ${domain+"${domain[@]}"})"
if [ -z "$census" ]; then
    ok "no gate self-test takes a directory from mktemp itself"
else
    no "no gate self-test takes a directory from mktemp itself"
    printf '%s\n' "$census" | sed "s|^$REPO/|      |" >&2
fi

# The nine shapes a use-keyed census missed, plus two that must NOT be
# reported. Every one is a whole file, so the detector is driven the way
# it is used above.
PROBE="$WORK/probe"
mkdir -p "$PROBE"
probe() { printf '%s\n' "$2" > "$PROBE/$1"; }
probe caught-bare.sh      'd=$(mktemp -d)
cd "$d" || exit 2'
probe caught-local.sh     'f() { local d; d=$(mktemp -d); cd "$d" || exit 2; }'
probe caught-localone.sh  'f() { local d=$(mktemp -d); cd "$d" || exit 2; }'
probe caught-declare.sh   'declare d=$(mktemp -d)
cd "$d" || exit 2'
probe caught-longopt.sh   'd=$(mktemp --directory)
cd "$d" || exit 2'
probe caught-unquoted.sh  'd=$(mktemp -d)
cd $d || exit 2'
probe caught-alias.sh     'd=$(mktemp -d)
work=$d
cd "$work" || exit 2'
probe caught-helper.sh    'mk() { mktemp -d; }
d=$(mk)
cd "$d" || exit 2'
probe caught-inline.sh    'cd "$(mktemp -d)" || exit 2'
probe caught-pushd.sh     'd=$(mktemp -d)
pushd "$d" || exit 2'
probe clean-guarded.sh    'guarded_tmpdir d
cd "$d" || exit 2'
probe clean-tempfile.sh   'f=$(mktemp)
printf x > "$f"'
probe clean-comment.sh    '# the old shape was d=$(mktemp -d) followed by cd "$d"
guarded_tmpdir d'

missed=(); wrong=()
for pf in "$PROBE"/*.sh; do
    hit="$(raw_mktemp_sites "$pf")"
    case "$(basename "$pf")" in
        caught-*) [ -n "$hit" ] || missed+=("$(basename "$pf")") ;;
        clean-*)  [ -z "$hit" ] || wrong+=("$(basename "$pf")") ;;
    esac
done
if [ "${#missed[@]}" -eq 0 ]; then
    ok "every shape of a raw directory mktemp is reported (10 probes)"
else
    no "every shape of a raw directory mktemp is reported — missed: ${missed[*]}"
fi
if [ "${#wrong[@]}" -eq 0 ]; then
    ok "and a guarded file, a temp FILE and a comment are not reported"
else
    no "and a guarded file, a temp FILE and a comment are not reported — reported: ${wrong[*]}"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
