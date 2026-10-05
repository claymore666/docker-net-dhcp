#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# A func's doc block must not open by naming a different Test function (#861).
#
# Expires-when: a linter in the lint job rejects a doc comment whose first word
#   is another Test symbol, or the tree stops writing Test names in doc
#   openers (#861).
#
# WHY. A comment block copied above a new test, or left behind by a rename,
# keeps its old first word and tells the reader the test does something it does
# not. Measured on 944e6e6: 4 blocks, two naming a test that still existed, so
# an "is this a declared symbol" check passes them. The rule asks only whether
# the first word is this function's own name.
#
# RULE. For every top-level `func` (methods and generics included) with a doc
# block, the first doc line after any `//go:` style directive lines, when its
# first word is `Test` followed by a non-lowercase character, names the
# function it documents. Prose openers and a bare "Test" pass, as do doc
# blocks that open with the function's own name. The Go convention "every
# doc opens with its own name" is NOT enforced (117 deliberate prose openers
# at 944e6e6).
#
# BOUNDS. Not seen: `/* */` doc blocks (none in the tree), a doc block
# separated from its func by a blank line (not a doc block to Go either), a
# column-0 `func` inside a raw string, and a body comment naming a test that
# does not exist (the oracle for that one needs an allowlist, #861).
# Refused (exit 2): no Go file, an unreadable file, no doc-commented func at all.
#
# Usage: check-doc-opener.sh [<tree>]
# Exit:  0 clean, 1 an opener names another test, 2 cannot check.
set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

cd "$(dirname "$0")/.." || exit 2
cd "${1:-.}" || exit 2

command -v python3 >/dev/null 2>&1 || gate_refuse "python3 is required by check-doc-opener"

gofiles=()
gate_subjects gofiles go

FILES="$(printf '%s\n' "${gofiles[@]}")" python3 -c '
import os
import re
import sys

FUNC = re.compile(r"^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)")
DIRECTIVE = re.compile(r"^//[a-z][a-z0-9]*:")
OPENER = re.compile(r"^//\s*(Test[A-Z0-9_]\w*)")

hits, refusals = [], []
docs = own = 0
files = [f for f in os.environ["FILES"].splitlines() if f]
for path in files:
    try:
        with open(path, encoding="utf-8") as fh:
            lines = fh.read().split("\n")
    except (OSError, UnicodeDecodeError) as exc:
        refusals.append(f"{path}: cannot read: {exc}")
        continue
    for i, line in enumerate(lines):
        m = FUNC.match(line)
        if not m:
            continue
        j = i
        while j > 0 and lines[j - 1].startswith("//"):
            j -= 1
        while j < i and DIRECTIVE.match(lines[j]):
            j += 1
        if j == i:
            continue
        docs += 1
        o = OPENER.match(lines[j])
        if not o:
            continue
        if o.group(1) == m.group(1):
            own += 1
        else:
            hits.append((path, i + 1, m.group(1), o.group(1)))

if not refusals and docs == 0:
    refusals.append(f"no doc-commented func in {len(files)} Go file(s); a pass would have read nothing")
if refusals:
    for r in refusals:
        print(f"::error title=check-doc-opener cannot judge::{r}", file=sys.stderr)
    sys.exit(2)
if hits:
    print(f"FAIL  {len(hits)} doc block(s) open by naming a different Test function:", file=sys.stderr)
    for path, ln, fn, op in hits:
        print(f"  {path}:{ln}: {fn} is documented as {op}", file=sys.stderr)
    print("Rewrite the opener to say what this function does (#861).", file=sys.stderr)
    sys.exit(1)
print(f"OK: {docs} doc-commented func(s) in {len(files)} Go file(s), {own} open with their own Test name, none with another.")
'
