#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-doc-invariants.sh (#579), driven through
# the --root and --manifest seams against synthetic trees.
#
# The RED cases are the whole point. A gate of this shape — grep some
# files for some strings — passes trivially, and a gate that checks
# nothing at all passes identically. So every way it could pass while
# seeing nothing gets a case: an empty manifest, an entry with no
# marker, an entry with no file, and a declared file that does not
# exist.
#
# The green case is checked for the COUNT it reports, not merely for
# exit 0, for the same reason.
#
# The last two cases are different in kind: they assert the gate is
# wired into a workflow, and that the real committed tree still carries
# the invariant it was written for. #567's lesson was that a check can
# sit green for a project's entire life with its input missing.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(dirname "$0")/check-doc-invariants.sh"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
guarded_tmpdir TMP

failures=0

# check NAME WANT_EXIT ROOT MANIFEST GREP
check() {
    local name="$1" want_exit="$2" root="$3" manifest="$4" want_grep="$5"
    bash "$GATE" --root "$root" --manifest "$manifest" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# mkdocs DIR — two files carrying the same instruction, wrapped
# differently, which is the real shape: the marker has to survive
# re-wrapping and different markup, so it must be a single line.
mkdocs() {
    mkdir -p "$1/docs"
    cat > "$1/README.md" <<'EOF'
# Project

> **DO THIS FIRST**
>
> ```bash
> sudo mkdir -p /var/lib/net-dhcp
> ```
>
> Docker will not create a missing
> bind source.
EOF
    cat > "$1/docs/index.md" <<'EOF'
!!! danger "DO THIS FIRST"

    ```bash
    sudo mkdir -p /var/lib/net-dhcp
    ```

    Docker will not
    create a missing bind source.
EOF
}

# ---------------------------------------------------------------- green

mkdocs "$TMP/ok"
cat > "$TMP/ok/manifest.txt" <<'EOF'
# A comment, and the blank line below it, are both ignored.

state-dir
    file: README.md
    file: docs/index.md
    marker: sudo mkdir -p /var/lib/net-dhcp
    marker: DO THIS FIRST
    A precondition for every install, on every host, indefinitely.
EOF
check "an intact tree passes, and says how much it checked" 0 "$TMP/ok" "$TMP/ok/manifest.txt" \
    "1 invariant(s), 4 marker/file check(s) passed"

# ------------------------------------------------------------------ red

# The case the issue was filed for: someone deletes the block during a
# release documentation pass because it names an old version.
mkdocs "$TMP/deleted"
cp "$TMP/ok/manifest.txt" "$TMP/deleted/manifest.txt"
grep -v 'sudo mkdir -p /var/lib/net-dhcp' "$TMP/deleted/docs/index.md" > "$TMP/deleted/docs/index.md.new"
mv "$TMP/deleted/docs/index.md.new" "$TMP/deleted/docs/index.md"
check "a marker deleted from ONE of several files is red" 1 "$TMP/deleted" "$TMP/deleted/manifest.txt" \
    "docs/index.md no longer contains: sudo mkdir -p /var/lib/net-dhcp"

# The heading survives, the command goes. Both markers exist precisely
# so this is caught: the warning without the instruction is worse than
# nothing, because it reads as covered.
mkdocs "$TMP/partial"
cp "$TMP/ok/manifest.txt" "$TMP/partial/manifest.txt"
grep -v 'sudo mkdir' "$TMP/partial/README.md" > "$TMP/partial/README.md.new"
mv "$TMP/partial/README.md.new" "$TMP/partial/README.md"
check "the heading surviving without the command is still red" 1 "$TMP/partial" "$TMP/partial/manifest.txt" \
    "README.md no longer contains"

# A manifest pointing at a file that is gone must go red, not silently
# check zero files.
mkdocs "$TMP/moved"
cat > "$TMP/moved/manifest.txt" <<'EOF'
state-dir
    file: docs/install.md
    marker: sudo mkdir -p /var/lib/net-dhcp
    The file was renamed and nobody updated this entry.
EOF
check "a declared file that does not exist is red" 1 "$TMP/moved" "$TMP/moved/manifest.txt" \
    "declares docs/install.md, which does not exist"

# An entry with no marker would pass vacuously for every file it lists.
mkdocs "$TMP/nomarker"
cat > "$TMP/nomarker/manifest.txt" <<'EOF'
state-dir
    file: README.md
    Justified, but there is nothing to look for.
EOF
check "an entry with no marker is red" 1 "$TMP/nomarker" "$TMP/nomarker/manifest.txt" \
    "declares no marker"

# An entry with no file is checked against nothing.
mkdocs "$TMP/nofile"
cat > "$TMP/nofile/manifest.txt" <<'EOF'
state-dir
    marker: sudo mkdir -p /var/lib/net-dhcp
    Justified, but pointed at nothing.
EOF
check "an entry with no file is red" 1 "$TMP/nofile" "$TMP/nofile/manifest.txt" \
    "declares no file"

# A bare entry becomes a list somebody appends to — same rule as
# linkadd-accounting and the vuln allowlist.
mkdocs "$TMP/bare"
cat > "$TMP/bare/manifest.txt" <<'EOF'
state-dir
    file: README.md
    marker: DO THIS FIRST
EOF
check "an entry with no justification is red" 1 "$TMP/bare" "$TMP/bare/manifest.txt" \
    "has no justification"

# ------------------------------------------------- cannot see (exit 2)

# Distinct from a violated invariant: the gate is reporting that it is
# blind, not that the docs are wrong. Conflating the two is how a
# broken gate reads as a broken tree.
mkdocs "$TMP/empty"
cat > "$TMP/empty/manifest.txt" <<'EOF'
# Every entry was removed, leaving only this comment.
EOF
check "a manifest declaring nothing is exit 2, not a pass" 2 "$TMP/empty" "$TMP/empty/manifest.txt" \
    "would otherwise pass having checked nothing"

check "a missing manifest is exit 2" 2 "$TMP/ok" "$TMP/nope/manifest.txt" \
    "Doc-invariant manifest missing"

check "a missing root is exit 2" 2 "$TMP/nope-root" "$TMP/ok/manifest.txt" \
    "is not a directory"

# --------------------- scope, var and match (the former cosign gate, #745)
#
# The separate cosign-docs gate is gone; its rules are the declarations `scope:`,
# `var:` and `match: text` of the real cosign-major-stated entry. The
# fixtures run that very entry, cut out of the committed manifest, so a
# case here cannot pass against a copy that has drifted from it.
REAL_ENTRY="$TMP/real-entry.txt"
awk '/^cosign-major-stated$/ { p = 1 } p && /^$/ { exit } p' "$REPO/.github/doc-invariants.txt" > "$REAL_ENTRY"
if [ -s "$REAL_ENTRY" ]; then
    echo "PASS: the committed manifest declares cosign-major-stated"
else
    echo "FAIL: .github/doc-invariants.txt has no cosign-major-stated entry"
    failures=$((failures + 1))
fi

DOC_WITH_VERSION=$'# Verify\n\nYou need **cosign v3 or newer**.\n\n```sh\ncosign verify-blob --bundle b.json checksums.txt\n```'
DOC_WITHOUT=$'# Verify\n\n```sh\ncosign verify-blob --bundle b.json checksums.txt\n```'

# cosign_root NAME ENVLINE — a git tree with the data file and one good
# page; the caller adds or breaks what it needs.
cosign_root() {
    local r="$TMP/$1"
    mkdir -p "$r/scripts" "$r/docs"
    git init -q "$r"
    if [ -n "$2" ]; then printf '%s\n' "$2" > "$r/scripts/release-tooling.env"; fi
    printf '%s\n' "$DOC_WITH_VERSION" > "$r/docs/verifying-releases.md"
    cp "$REAL_ENTRY" "$r/manifest.txt"
}
cosign_check() { # NAME WANT_EXIT ROOTNAME GREP
    check "$1" "$2" "$TMP/$3" "$TMP/$3/manifest.txt" "$4"
}

cosign_root c-ok 'COSIGN_MAJOR=3'
cosign_check "a page that states the required major passes, and the count says one page was judged" 0 c-ok \
    "1 invariant(s), 1 marker/file check(s) passed"

cosign_root c-none 'COSIGN_MAJOR=3'
printf '%s\n' "$DOC_WITHOUT" > "$TMP/c-none/docs/verifying-releases.md"
cosign_check "a page that prints the command and states nothing is red" 1 c-none \
    "docs/verifying-releases.md no longer contains: cosign v3 or newer"

# The drift this exists for: the data file moves to a new major and the
# page still names the old one.
cosign_root c-bump 'COSIGN_MAJOR=4'
cosign_check "the data file bumped and a page left behind is red" 1 c-bump \
    "no longer contains: cosign v4 or newer"

cosign_root c-old 'COSIGN_MAJOR=3'
printf '%s\n' $'You need `cosign v2 or newer`.\n\n    cosign verify-blob x' > "$TMP/c-old/docs/verifying-releases.md"
cosign_check "a page naming an older major is red" 1 c-old "no longer contains: cosign v3 or newer"

# match: text. Emphasis inside the phrase and a different case count; the
# same page fails under the default raw match, so the option is what
# carries the pass and no other entry is loosened by it.
cosign_root c-emph 'COSIGN_MAJOR=3'
printf '%s\n' $'Needs Cosign `v3` or newer.\n\n    cosign verify x' > "$TMP/c-emph/docs/verifying-releases.md"
cosign_check "emphasis inside the phrase and a different case still count" 0 c-emph "1 invariant(s)"
grep -v '^    match: text$' "$REAL_ENTRY" > "$TMP/c-emph/manifest.txt"
cosign_check "the same page is red under a raw match" 1 c-emph "no longer contains: cosign v3 or newer"

# Source-of-truth failures are exit 2, never an empty substitution.
cosign_root c-noline 'something else'
cosign_check "a data file with no COSIGN_MAJOR line is exit 2" 2 c-noline "no COSIGN_MAJOR=<value> line in scripts/release-tooling.env"
cosign_root c-empty 'COSIGN_MAJOR='
cosign_check "an empty COSIGN_MAJOR is exit 2, not a marker that matches everything" 2 c-empty "no COSIGN_MAJOR=<value> line"
cosign_root c-nofile ''
cosign_check "a missing data file is exit 2" 2 c-nofile "no COSIGN_MAJOR=<value> line"
cosign_root c-typo 'COSIGN_MAJOR=3'
sed -i 's/\${COSIGN_MAJOR}/${COSIGN_MAJR}/' "$TMP/c-typo/manifest.txt"
cosign_check "a marker naming an undeclared variable is exit 2" 2 c-typo "unresolved variable"
cosign_root c-badvar 'COSIGN_MAJOR=3'
sed -i 's|^    var: .*|    var: scripts/release-tooling.env|' "$TMP/c-badvar/manifest.txt"
cosign_check "a var: that is not NAME=<path> is exit 2" 2 c-badvar "is not NAME=<path>"
cosign_root c-badmatch 'COSIGN_MAJOR=3'
sed -i 's/^    match: text$/    match: fuzzy/' "$TMP/c-badmatch/manifest.txt"
cosign_check "an unknown match: mode is exit 2" 2 c-badmatch "is not raw or text"

# A broken search is not a pass: the real repo always has such a page.
cosign_root c-nopage 'COSIGN_MAJOR=3'
printf '%s\n' $'# Nothing to see\n' > "$TMP/c-nopage/docs/verifying-releases.md"
cosign_check "no page printing a cosign command is exit 2" 2 c-nopage "no tracked Markdown page matches scope"

# A real violation and a blind entry together: the violation wins.
cosign_root c-both 'COSIGN_MAJOR=3'
printf '%s\n' "$DOC_WITHOUT" > "$TMP/c-both/docs/verifying-releases.md"
{ cat "$REAL_ENTRY"; printf '\nsecond\n    scope: ^no such line anywhere$\n    marker: x\n    Blind on purpose.\n'; } > "$TMP/c-both/manifest.txt"
cosign_check "a violation and a blind entry in one manifest is exit 1" 1 c-both "no longer contains: cosign v3 or newer"

# Scope: git's view, every page, and nothing else.
# A worktree under .claude/ is a nested repository: the walk must not
# judge this tree by another branch's copy of the pages (#530, #744).
cosign_root c-wt 'COSIGN_MAJOR=3'
mkdir -p "$TMP/c-wt/.claude/worktrees/other/docs"
printf '%s\n' "$DOC_WITHOUT" > "$TMP/c-wt/.claude/worktrees/other/docs/verifying-releases.md"
git init -q "$TMP/c-wt/.claude/worktrees/other"
cosign_check "a stale page inside a .claude worktree is not walked" 0 c-wt "1 invariant(s), 1 marker/file check(s)"

cosign_root c-dotgit 'COSIGN_MAJOR=3'
printf '%s\n' "$DOC_WITHOUT" > "$TMP/c-dotgit/.git/verifying-releases.md"
cosign_check "a page inside .git is not walked" 0 c-dotgit "1 invariant(s), 1 marker/file check(s)"

cosign_root c-deep 'COSIGN_MAJOR=3'
mkdir -p "$TMP/c-deep/deploy/notes"
printf '%s\n' "$DOC_WITHOUT" > "$TMP/c-deep/deploy/notes/verify.md"
cosign_check "a page outside docs/ is judged" 1 c-deep "deploy/notes/verify.md no longer contains"

cosign_root c-ign 'COSIGN_MAJOR=3'
mkdir -p "$TMP/c-ign/site"
printf '%s\n' "$DOC_WITHOUT" > "$TMP/c-ign/site/verifying-releases.md"
printf 'site/\n' > "$TMP/c-ign/.gitignore"
cosign_check "an ignored page is not walked" 0 c-ign "1 invariant(s), 1 marker/file check(s)"

# Declarations belong to their own entry: the entry after the cosign one
# keeps the raw match and its own file list.
cosign_root c-leak 'COSIGN_MAJOR=3'
printf '%s\n' $'Needs Cosign `v3` or newer.\n\n    cosign verify x' > "$TMP/c-leak/docs/verifying-releases.md"
{ cat "$REAL_ENTRY"; printf '\nplain\n    file: docs/verifying-releases.md\n    marker: cosign v3 or newer\n    A raw entry that follows.\n'; } > "$TMP/c-leak/manifest.txt"
cosign_check "match: text does not leak into the next entry" 1 c-leak "plain: docs/verifying-releases.md no longer contains"

# A scope adds to the declared files, it does not replace them.
cosign_root c-add 'COSIGN_MAJOR=3'
printf '%s\n' '# Readme with no cosign command and no phrase' > "$TMP/c-add/README.md"
sed -i 's|^    scope: .*|    file: README.md\n&|' "$TMP/c-add/manifest.txt"
cosign_check "file: and scope: are both judged" 1 c-add "README.md no longer contains: cosign v3 or newer"

# The committed data file and the signing step must name the same file.
read_lines="$(grep -A1 "sed -n 's/^COSIGN_MAJOR=" "$REPO/.github/workflows/release.yml" || true)"
if [[ "$read_lines" == *scripts/release-tooling.env* ]] \
    && ! grep -q 'COSIGN_MAJOR=[0-9]' "$REPO/.github/workflows/release.yml"; then
    echo "PASS: release.yml reads the data file and holds no major of its own"
else
    echo "FAIL: release.yml must read scripts/release-tooling.env and never assign COSIGN_MAJOR itself"
    failures=$((failures + 1))
fi

# ------------------------------------- the real tree, and the wiring

# The gate above ran entirely against synthetic trees. This asserts the
# committed manifest describes the committed docs — without it, every
# case here could pass while the real invariant was already gone.
if bash "$GATE" > "$TMP/real" 2>&1; then
    echo "PASS: the committed tree satisfies its own manifest"
else
    echo "FAIL: check-doc-invariants.sh is red against the committed tree"
    sed 's/^/    /' "$TMP/real"
    failures=$((failures + 1))
fi

# #567: a check whose input was missing sat green for the project's
# entire life. A gate with no step in any workflow is that same shape —
# it passes locally, it is referenced in a PR body, and it never runs.
if grep -rq -- "check-doc-invariants.sh" "$REPO/.github/workflows" 2>/dev/null; then
    echo "PASS: a workflow runs the gate"
else
    echo "FAIL: no workflow under .github/workflows runs check-doc-invariants.sh, so the"
    echo "      gate is committed but dead. Add a step; do not describe it in a PR body."
    failures=$((failures + 1))
fi

if [ "$failures" -eq 0 ]; then
    echo "all check-doc-invariants tests passed"
    exit 0
fi
echo "$failures failed"
exit 1
