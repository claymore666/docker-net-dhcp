#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every raw file descriptor the PLUGIN opens must be close-on-exec
# (#729).
#
# "Raw" and "the plugin" are both load-bearing and both narrower than
# "every fd": descriptors opened through Go's os package already carry
# the flag, and test files are deliberately out of scope (see below).
# Said in the first sentence rather than only in WHAT IT CHECKS, because
# a header that claims more than the pattern matches is the bug #758
# fixed in three gates at once.
#
# WHY THIS EXISTS
#
# Go's os package sets O_CLOEXEC on everything it opens. The unix and
# syscall packages do not: the flag word is whatever the caller passes.
# So a descriptor opened through unix.Openat with plain O_RDONLY is
# INHERITED by every child process forked afterwards, and Go's os/exec
# does not sweep foreign descriptors on the way out -- it closes only the
# ones it was given.
#
# This plugin forks constantly and it forks privileged: unshare, sh, and
# a root dhcpcd, spawned per endpoint, per lease, per renewal. The
# descriptors in question are not ordinary files. One is an open handle
# on a CONTAINER'S MOUNT NAMESPACE; another is a handle on a container's
# procfs entry, which exists precisely because the PID it came from
# cannot be trusted a second time (#688). Leaking either into a child
# hands that child a reference the parent went to some length to pin.
#
# WHY A GATE AND NOT A TEST
#
# There is nothing to observe. Every one of these descriptors is closed
# before the function that opened it returns, so no Go test can ask
# whether the flag was set -- the fd is gone by the time a caller could
# look. The rule is a property of the SOURCE, universally true here, and
# a source rule with no runtime observable is what a gate is for.
#
# It is also the shape the mistake has taken: container_netns.go got
# O_CLOEXEC right and the two calls in resolvconf.go, written for the
# same reason in the same release, did not. One fix does not reach the
# copies. This is what stops the next one.
#
# WHAT IT CHECKS
#
# Every call to Open / Openat on the unix or syscall package, in
# non-test Go source under pkg/ and cmd/, must name O_CLOEXEC in its
# flag argument.
#
# The package name is read from each file's own import block rather than
# assumed to be "unix", so `import sysunix "golang.org/x/sys/unix"`
# followed by sysunix.Openat is found. That is not hypothetical
# tidiness: a gate that greps for a literal "unix.Openat" goes silently
# blind the day someone aliases the import, and a silently blind gate is
# worse than none.
#
# WHAT IT REFUSES TO JUDGE (exit 2, never a clean pass)
#
#   - a dot-import of unix or syscall: the call site is then a bare
#     Openat( and cannot be told from any other function of that name;
#   - Openat2, whose flags live in a *unix.OpenHow struct field rather
#     than in an argument, so this gate's argument scan does not apply;
#   - an empty subject set, which would otherwise report a clean pass
#     over nothing at all.
#
# Test files are out of scope on purpose: a test descriptor lives for
# microseconds in a process that spawns nothing, and demanding the flag
# there would teach the next reader that this gate is boilerplate.
#
# Usage: check-openat-cloexec.sh [<tree>]
# Exit:  0 clean, 1 a call site lacks O_CLOEXEC, 2 cannot check.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2
cd "${1:-.}" || exit 2

command -v python3 >/dev/null 2>&1 || {
    echo "::error title=Cannot check::python3 is required by check-openat-cloexec." >&2
    exit 2
}

[ -d pkg ] || [ -d cmd ] || {
    echo "::error title=Nothing to inspect::no pkg/ or cmd/ directory in '${1:-.}'." >&2
    exit 2
}

# Enumerated from the filesystem rather than from `git ls-files`, and
# that is the untracked-file question answered rather than skipped: a
# file being written is not tracked yet, and that is exactly when this
# gate is worth asking. `find` sees it either way. The sibling
# check-proc-path-discipline.sh reads the tree the same way, and the
# directories in scope hold no build output for a git listing to filter.
FILES=$(find pkg cmd -type f -name '*.go' ! -name '*_test.go' 2>/dev/null | sort)

if [ -z "$FILES" ]; then
    echo "::error title=Nothing to inspect::no non-test Go files under pkg/ or cmd/ in '${1:-.}'." \
         "This gate would otherwise report a clean pass having read nothing." >&2
    exit 2
fi

FILES="$FILES" python3 -c '
import os
import re
import sys

# The calls whose flag word is the caller argument this gate scans.
# Openat2 is deliberately absent: its flags live in a struct field, so
# the scan below does not apply to it and pretending otherwise would
# pass it silently.
SCANNED = ("Open", "Openat")
UNJUDGEABLE = ("Openat2",)

# Both import forms. The single-line `import "x"` form was the one the
# meta-test caught: without the optional `import` prefix the regex read
# the keyword itself as the alias, so every file that imported unix
# outside a parenthesised block was scanned for calls on a package named
# "import" -- and reported clean.
IMPORT_LINE = re.compile(
    r"^\s*(?:import\s+)?(?:(\.|_|[A-Za-z_]\w*)\s+)?\"(golang\.org/x/sys/unix|syscall)\"")

files = [f for f in os.environ["FILES"].splitlines() if f.strip()]

violations = []
refusals = []
call_sites = 0
files_with_calls = 0


def arguments(text, open_paren):
    """Return the argument text of a call whose ( is at open_paren.

    Balanced so a call broken across lines, or one carrying a nested
    call in its flags, is read whole. A line-based scan would read the
    first line of a wrapped call and answer about half a call site.
    """
    depth = 0
    for i in range(open_paren, len(text)):
        c = text[i]
        if c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
            if depth == 0:
                return text[open_paren + 1:i]
    return None


for path in files:
    try:
        with open(path, encoding="utf-8") as fh:
            src = fh.read()
    except (OSError, UnicodeDecodeError) as exc:
        refusals.append(f"{path}: cannot read: {exc}")
        continue

    # Which local names refer to a package that opens raw descriptors.
    names = set()
    for line in src.splitlines():
        m = IMPORT_LINE.match(line)
        if not m:
            continue
        alias, pkg = m.group(1), m.group(2)
        if alias == ".":
            refusals.append(
                f"{path}: dot-imports {pkg}. A call site is then a bare Openat( "
                "that cannot be told from any other function of that name, so "
                "this gate cannot answer for this file."
            )
            names = set()
            break
        if alias == "_":
            continue
        names.add(alias or ("unix" if pkg.endswith("unix") else "syscall"))

    if not names:
        continue

    found_here = 0
    for name in sorted(names):
        for bad in UNJUDGEABLE:
            if re.search(rf"\b{re.escape(name)}\.{bad}\s*\(", src):
                refusals.append(
                    f"{path}: calls {name}.{bad}, whose flags live in a struct "
                    "field rather than in an argument. This gate scans arguments, "
                    "so it would report a clean pass over a call it never read."
                )

        for fn in SCANNED:
            # \b...\. and the trailing (?!\w) keep Openat out of the
            # match for Open, and keep Openat2 out of the match for
            # Openat -- it is refused above, not judged here.
            for m in re.finditer(rf"\b{re.escape(name)}\.{fn}(?!\w)\s*\(", src):
                open_paren = src.index("(", m.end() - 1)
                args = arguments(src, open_paren)
                if args is None:
                    refusals.append(f"{path}: unbalanced parentheses at {name}.{fn}(")
                    continue
                found_here += 1
                if "O_CLOEXEC" not in args:
                    line_no = src.count("\n", 0, m.start()) + 1
                    violations.append((path, line_no, f"{name}.{fn}"))

    call_sites += found_here
    if found_here:
        files_with_calls += 1

if refusals:
    for r in refusals:
        print(f"::error title=Cannot check::{r}", file=sys.stderr)
    sys.exit(2)

if violations:
    print(f"FAIL  {len(violations)} descriptor(s) opened without O_CLOEXEC:", file=sys.stderr)
    for path, line_no, call in violations:
        print(f"  {path}:{line_no}: {call}", file=sys.stderr)
    print("", file=sys.stderr)
    print("Go os/exec does not sweep foreign descriptors, so each of these is", file=sys.stderr)
    print("inherited by the next unshare / sh / dhcpcd this process spawns.", file=sys.stderr)
    print("Add unix.O_CLOEXEC to the flag argument (#729).", file=sys.stderr)
    sys.exit(1)

print(f"O_CLOEXEC OK — {call_sites} raw open(2) call site(s) in "
      f"{files_with_calls} file(s), {len(files)} non-test Go file(s) scanned.")
'
