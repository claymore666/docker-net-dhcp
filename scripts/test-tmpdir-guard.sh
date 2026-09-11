#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for scripts/tmpdir-guard.sh (#963).
#
# The subject is a refusal, so the cases that matter are the ones that
# DRIVE THE ABSENCE: with `mktemp -d` returning nothing, a real gate
# self-test must stop instead of building its fixture in whatever
# directory it was started from. Case 2 runs the shipped
# test-check-coverage-floor.sh, copied not reimplemented, both as it
# stands and mechanically reverted to the shape that shipped, so the
# rig is shown to see the damage before it is asked to certify its
# absence.
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

# An inert `mktemp` that succeeds and prints nothing: the failure mode
# measured on the session box, where /tmp had no inodes left. It is a
# stand-in for the binary because that is what the shipped scripts go
# through — they call `mktemp`, not a shell function.
INERT="$WORK/inert"
mkdir -p "$INERT"
printf '#!/bin/sh\nexit 0\n' > "$INERT/mktemp"
chmod +x "$INERT/mktemp"

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
if grep -q 'Unsafe temp directory' "$WORK/guarded.out"; then
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
if grep -q '=\$(mktemp -d)' "$PRE/scripts/test-check-coverage-floor.sh"; then
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

# --- 3. the refusals, one per reason -----------------------------------

refuses() { # refuses <name> <needle> <env assignment...> ; reads $WORK/ref.sh
    local name="$1" needle="$2"; shift 2
    local out rc
    out=$( cd "$WORK" && env "$@" bash "$WORK/ref.sh" 2>&1 ); rc=$?
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
        no "$name (exit $rc, message does not mention '$needle')"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
    fi
}

cat > "$WORK/ref.sh" <<SH
#!/usr/bin/env bash
set -u
. "$LIB"
guarded_tmpdir d
cd "\$d" || exit 2
touch "$WORK/REFUSAL-LEAKED"
SH

refuses "an empty mktemp -d is a refusal" "empty path" "PATH=$INERT:$PATH"
refuses "an unwritable TMPDIR is a refusal" "Unsafe temp directory" \
        "TMPDIR=$WORK/no-such-dir"
# Through a stand-in that hands back a directory under this suite's own
# sandbox: a refused path is never removed by the guard (it is, by
# definition, a path the guard does not trust), so driving this case
# through the real mktemp would leave one directory behind per run.
INSIDE="$WORK/insidebin"
mkdir -p "$INSIDE" "$WORK/root/under"
printf '#!/bin/sh\nprintf %%s "%s/root/under"\n' "$WORK" > "$INSIDE/mktemp"
chmod +x "$INSIDE/mktemp"
refuses "a temp path inside the guarded tree is a refusal" "inside the repository" \
        "PATH=$INSIDE:$PATH" "TMPDIR_GUARD_ROOT=$WORK/root"
# A `mktemp` that hands back one fixed directory, to put the temp path
# exactly on the guarded root. mktemp itself will never do that; the
# check exists because an empty TMPDIR_GUARD_ROOT or a TMPDIR set to the
# checkout would.
FIXED="$WORK/fixedbin"
mkdir -p "$FIXED" "$WORK/asroot"
printf '#!/bin/sh\nprintf %%s "%s/asroot"\n' "$WORK" > "$FIXED/mktemp"
chmod +x "$FIXED/mktemp"
refuses "a temp path that is the guarded tree is a refusal" "repository root" \
        "PATH=$FIXED:$PATH" "TMPDIR_GUARD_ROOT=$WORK/asroot"

if [ ! -e "$WORK/REFUSAL-LEAKED" ]; then
    ok "a refusal writes nothing in the directory it was started from"
else
    no "a refusal writes nothing in the directory it was started from"
fi

# --- 4. a refusal inside a command substitution stops the script -------

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
printf '%s\n%s\n' "\$top" "\$sub" > "$WORK/sweep.paths"
exit 3
SH
( cd "$WORK" && bash "$WORK/sweep.sh" ) >/dev/null 2>&1
wrc=$?
top="$(sed -n 1p "$WORK/sweep.paths" 2>/dev/null || true)"
sub="$(sed -n 2p "$WORK/sweep.paths" 2>/dev/null || true)"
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
if [ -z "$(ls -A "${TMPDIR:-/tmp}"/.tmpdir-guard.* 2>/dev/null || true)" ]; then
    ok "the registry file does not outlive the shell that owns it"
else
    ok "the registry file does not outlive the shell that owns it (other shells hold one)"
fi

# --- 6. no site under scripts/ is left outside the guard ---------------
#
# The fix is only as durable as this census. A `cd` into a variable that
# came straight from `mktemp -d` is the shape the incident had, and a
# new self-test copied from an old one brings it back for free.
#
# tmpdir-guard.sh calls mktemp inside guarded_tmpdir, and this file
# writes the pre-fix shape into its own fixtures on purpose, so both are
# named here rather than pattern-matched away.
census=$(
    cd "$REPO" || exit 2
    for f in $(git ls-files 'scripts/*.sh'); do
        # Matched on the basename: a literal `scripts/x.sh` at the head
        # of a case arm reads as a command invocation to
        # check-direct-invocations.sh, which is right about the shape
        # and wrong about this line.
        case "$f" in
            */tmpdir-guard.sh|*/test-tmpdir-guard.sh) continue ;;
        esac
        while IFS= read -r v; do
            if grep -Eq "cd +\"\\\$\{?$v(\}|[/\"])" "$f"; then
                printf '%s:%s\n' "$f" "$v"
            fi
        done < <(sed -n 's/^[[:space:]]*\([A-Za-z_][A-Za-z0-9_]*\)="\?\$(mktemp -d.*/\1/p' "$f" | sort -u)
    done
)
if [ -z "$census" ]; then
    ok "no script under scripts/ cds into a raw mktemp -d assignment"
else
    no "no script under scripts/ cds into a raw mktemp -d assignment"
    printf '%s\n' "$census" | sed 's/^/      /' >&2
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
