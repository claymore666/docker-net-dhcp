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

GATE="$(dirname "$0")/check-doc-invariants.sh"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

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
