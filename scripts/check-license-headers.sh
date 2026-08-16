#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Per-file copyright and licence headers (#454). Two OpenSSF Best
# Practices gold MUSTs, `copyright_per_file` and `license_per_file`, ask
# for the licence to be stated in each source file rather than only once
# at repository level. Measured before this gate existed: 0 of 102
# tracked Go files carried either.
#
# WHY THE EXPRESSION IS `GPL-3.0-only` AND NOT `-or-later`. The issue
# that asked for this drafted `-or-later`, and that would have been a
# licence change. LICENSE.md here — and in upstream, which this is a
# fork of — is the bare GPLv3 text. Neither repository anywhere says
# "or any later version". GPLv3 section 14 is explicit about what that
# silence means: the option to follow a later version exists only if the
# program *specifies* it. Writing `-or-later` into 136 files would
# therefore hand out a permission upstream's authors never granted, on
# their code. `-only` is both the correct reading and the conservative
# one.
#
# The copyright line names the contributors rather than any individual
# or the fork owner: the file histories are the record of who wrote
# what, and upstream's authors are contributors to this codebase too.
#
# SCOPE: Go, shell and Python files that git can see and does not
# ignore — tracked or not, since a file being written is not yet tracked
# and is exactly when this gate is worth asking. Workflow YAML,
# Dockerfiles and JSON manifests are
# configuration and are deliberately left alone; the criteria are about
# source files, and two comment lines at the top of a workflow would
# push the explanation of *why the workflow exists* below the fold for
# no licensing gain.
#
# Usage:
#   check-license-headers.sh          report files missing the header
#   check-license-headers.sh --fix    insert it where it is missing
#
# --fix exists so that adding a file does not mean hand-copying a
# header, which is how a gate like this starts getting worked around.
#
# Exit: 0 clean, 1 a file is missing the header, 2 cannot check.
set -u

MODE="check"
case "${1:-}" in
    "") ;;
    --fix) MODE="fix" ;;
    -h|--help)
        sed -n '/^# Usage:/,/^# Exit:/p' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "usage: $0 [--fix]" >&2
        exit 2
        ;;
esac

command -v python3 >/dev/null 2>&1 || {
    echo "FAIL  python3 is required" >&2
    exit 2
}
command -v git >/dev/null 2>&1 || {
    echo "FAIL  git is required" >&2
    exit 2
}

# The file list comes from git, not from find: git already knows what is
# build output and what is a nested worktree, and `find` would have to be
# taught both by hand.
#
# UNTRACKED FILES COUNT TOO, and leaving them out was a real hole. A file
# that has been written but not yet `git add`ed is invisible to
# `ls-files` alone — and that is precisely the state a source file is in
# for the whole time someone is writing it. So the gate answered "OK —
# 188 files" in a working tree that contained two new files with no
# header at all. It was right about the 188 and useless about the two.
#
# CI never saw the hole, which is why it survived: a fresh checkout has
# everything committed, so tracked and present mean the same thing there.
# It only misleads in a working checkout — the one place a person runs
# this gate by hand, expecting it to tell them whether they are ready to
# commit. A gate that is honest only where nobody consults it is not a
# gate (#454).
#
# --exclude-standard is what keeps the enumeration honest in the other
# direction: it applies .gitignore, so build output under plugin/ and the
# nested worktrees under .claude/ stay out, exactly as before. Untracked
# and ignored are different questions and only the first one is being
# reversed here.
FILES="${LICENSE_HEADER_FILES:-}"
if [ -z "$FILES" ]; then
    tracked=$(git ls-files '*.go' '*.sh' '*.py') || exit 2
    untracked=$(git ls-files --others --exclude-standard '*.go' '*.sh' '*.py') || exit 2
    FILES=$(printf '%s\n%s\n' "$tracked" "$untracked" | grep -v '^$' | sort -u)
fi

MODE="$MODE" FILES="$FILES" python3 -c '
import os
import sys

COPYRIGHT = "Copyright the docker-net-dhcp contributors."
SPDX = "SPDX-License-Identifier: GPL-3.0-only"

# How far into a file the header may sit. Generous enough for a shebang
# plus an encoding line, tight enough that a header buried under a page
# of prose does not count -- it has to be findable.
WINDOW = 6

mode = os.environ["MODE"]
files = [f for f in os.environ["FILES"].splitlines() if f.strip()]

missing = []
fixed = []
unreadable = []

for path in files:
    try:
        with open(path, encoding="utf-8") as fh:
            lines = fh.read().splitlines(keepends=True)
    except (OSError, UnicodeDecodeError) as exc:
        unreadable.append(f"{path}: {exc}")
        continue

    head = "".join(lines[:WINDOW])
    if COPYRIGHT in head and SPDX in head:
        continue

    if mode != "fix":
        missing.append(path)
        continue

    comment = "//" if path.endswith(".go") else "#"
    header = [f"{comment} {COPYRIGHT}\n", f"{comment} {SPDX}\n", "\n"]

    # A shebang must stay on line 1. Everything else -- including a Go
    # build constraint, which may legally be preceded by line comments
    # -- takes the header above it.
    at = 1 if lines and lines[0].startswith("#!") else 0

    # Do not leave two blank lines where the file already had one.
    if at < len(lines) and lines[at].strip() == "":
        header = header[:2]

    lines[at:at] = header
    # Write-then-rename rather than truncate-in-place. This fixer edits
    # shell scripts, and one of them is the script currently running:
    # bash reads its input lazily by byte offset, so rewriting the file
    # underneath it makes the shell resume mid-token and die on garbage.
    # os.replace swaps the directory entry, leaving the running process
    # holding a valid fd on the old inode. It also means a crash cannot
    # leave a half-written source file behind.
    tmp = f"{path}.license-header.tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write("".join(lines))
    # A fresh file is 0644; carry the original mode over or every
    # script this touches silently loses its executable bit.
    os.chmod(tmp, os.stat(path).st_mode & 0o7777)
    os.replace(tmp, path)
    fixed.append(path)

if unreadable:
    for u in unreadable:
        print(f"FAIL  {u}", file=sys.stderr)
    sys.exit(2)

if mode == "fix":
    print(f"License headers: added to {len(fixed)} file(s), {len(files)} checked.")
    sys.exit(0)

if missing:
    print(f"FAIL  {len(missing)} of {len(files)} source file(s) lack the licence header:", file=sys.stderr)
    for path in missing:
        print(f"  {path}", file=sys.stderr)
    print("", file=sys.stderr)
    print("Each file needs these two lines within its first "
          f"{WINDOW} lines (// in Go, # in shell/Python):", file=sys.stderr)
    print(f"  {COPYRIGHT}", file=sys.stderr)
    print(f"  {SPDX}", file=sys.stderr)
    print("", file=sys.stderr)
    print("Run: bash scripts/check-license-headers.sh --fix", file=sys.stderr)
    sys.exit(1)

print(f"License headers OK — {len(files)} source file(s).")
'
