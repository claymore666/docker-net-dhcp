#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-license-headers.sh (#454).
#
# The gate's job is to notice an absence, which is the failure mode
# least likely to be noticed by anything else: a new file without a
# header breaks no build and fails no test. So the cases below are
# mostly about the gate staying capable of saying no — including when
# the licence expression has quietly drifted to something the project
# has no right to grant.
#
# --fix is tested as carefully as --check, because a fixer that mangles
# a file is worse than no fixer: it would put a shebang on line 3 or a
# Go build constraint after the package clause and break the build in a
# way the header itself did not.
set -u

# Absolute: the cases below run from inside $TMP so the file list can be
# relative, and a relative path here would silently resolve to nothing —
# every assertion would then "pass" against exit 127.
CHECK="$(cd "$(dirname "$0")" && pwd)/check-license-headers.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

COPY='Copyright the docker-net-dhcp contributors.'
SPDX='SPDX-License-Identifier: GPL-3.0-only'

failures=0

pass() { echo "PASS: $1"; }
fail() {
    echo "FAIL: $1"
    shift
    for line in "$@"; do echo "    $line"; done
    failures=$((failures + 1))
}

# run MODE FILES...  -> sets $out and $rc
run() {
    local mode="$1"
    shift
    local list
    list=$(printf '%s\n' "$@")
    out=$(cd "$TMP" && LICENSE_HEADER_FILES="$list" bash "$CHECK" $mode 2>&1)
    rc=$?
    # 127 means the script was never found, and every assertion built on
    # that run would be vacuous. Say so rather than letting it read as a
    # verdict.
    if [ "$rc" -eq 127 ]; then
        fail "harness: could not run $CHECK" "$out"
    fi
}

# check NAME WANT_EXIT MODE GREP FILES...
check() {
    local name="$1" want="$2" mode="$3" want_grep="$4"
    shift 4
    run "$mode" "$@"
    local ok=1
    [ "$rc" -eq "$want" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -qF "$want_grep" <<<"$out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        pass "$name"
    else
        fail "$name" "want exit $want / grep '$want_grep', got exit $rc" "$out"
    fi
}

# --- detection ---------------------------------------------------------
printf '%s\n%s\n\npackage main\n' "// $COPY" "// $SPDX" > "$TMP/good.go"
check "a file with the header passes" 0 "" "License headers OK" good.go

printf 'package main\n' > "$TMP/bare.go"
check "a file without the header fails" 1 "" "bare.go" bare.go

# The drift that matters most. -or-later is a licence this project has
# no standing to grant on a fork of someone else's GPLv3 code, so the
# gate must reject it rather than accept any SPDX line.
printf '// %s\n// SPDX-License-Identifier: GPL-3.0-or-later\n\npackage main\n' "$COPY" > "$TMP/orlater.go"
check "the wrong SPDX expression fails" 1 "" "orlater.go" orlater.go

printf '// %s\n\npackage main\n' "$COPY" > "$TMP/nospdx.go"
check "copyright without SPDX fails" 1 "" "nospdx.go" nospdx.go

printf '// %s\n\npackage main\n' "$SPDX" > "$TMP/nocopy.go"
check "SPDX without copyright fails" 1 "" "nocopy.go" nocopy.go

# A header has to be findable, not merely present somewhere in the file.
{
    echo "package main"
    for i in 1 2 3 4 5 6 7 8; do echo "// filler $i"; done
    echo "// $COPY"
    echo "// $SPDX"
} > "$TMP/buried.go"
check "a header buried past the window fails" 1 "" "buried.go" buried.go

# --- --fix -------------------------------------------------------------
printf 'package main\n\nfunc main() {}\n' > "$TMP/fixme.go"
check "--fix reports what it changed" 0 --fix "added to 1 file" fixme.go
check "and the file then passes" 0 "" "License headers OK" fixme.go

check "--fix is idempotent" 0 --fix "added to 0 file" fixme.go

# Shebang must stay on line 1 or the script stops being executable.
printf '#!/usr/bin/env bash\nset -u\necho hi\n' > "$TMP/fixme.sh"
run --fix fixme.sh
if [ "$(head -1 "$TMP/fixme.sh")" = "#!/usr/bin/env bash" ]; then
    pass "--fix keeps the shebang on line 1"
else
    fail "--fix keeps the shebang on line 1" "$(head -3 "$TMP/fixme.sh")"
fi
if [ "$(sed -n '2p' "$TMP/fixme.sh")" = "# $COPY" ]; then
    pass "--fix uses # for shell, not //"
else
    fail "--fix uses # for shell, not //" "$(sed -n '2p' "$TMP/fixme.sh")"
fi
check "the fixed script passes" 0 "" "License headers OK" fixme.sh

# A Go build constraint may be preceded by line comments but must still
# come before the package clause. Getting this wrong drops a whole file
# from its build tag silently.
printf '//go:build integration\n\npackage harness\n' > "$TMP/tagged.go"
run --fix tagged.go
tag_line=$(grep -n '^//go:build' "$TMP/tagged.go" | cut -d: -f1)
pkg_line=$(grep -n '^package ' "$TMP/tagged.go" | cut -d: -f1)
if [ -n "$tag_line" ] && [ -n "$pkg_line" ] && [ "$tag_line" -lt "$pkg_line" ]; then
    pass "--fix keeps //go:build before the package clause"
else
    fail "--fix keeps //go:build before the package clause" "$(cat "$TMP/tagged.go")"
fi
if [ "$(sed -n "$((tag_line + 1))p" "$TMP/tagged.go")" = "" ]; then
    pass "--fix leaves the blank line //go:build requires"
else
    fail "--fix leaves the blank line //go:build requires" "$(cat "$TMP/tagged.go")"
fi

# The fixer edits shell scripts, and in the real run one of them is the
# script doing the editing. bash reads its input lazily by byte offset,
# so rewriting that file in place makes the shell resume mid-token and
# die on garbage — which is exactly what happened the first time this
# ran against the repo: "added to 4 file(s)" followed by a syntax error
# from a line the author never wrote. Reproduced here by handing a copy
# of the checker its own path.
SELF="$TMP/self-copy.sh"
# Strip only the header itself — lines 2 and 3. A blanket delete would
# also remove the constants the checker compares against, which live in
# its own source.
awk 'NR==2 && /Copyright the docker-net-dhcp/ {next}
     NR==3 && /SPDX-License-Identifier/ {next}
     {print}' "$CHECK" > "$SELF"
chmod +x "$SELF"
out=$(cd "$TMP" && LICENSE_HEADER_FILES="self-copy.sh" bash "$SELF" --fix 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && ! grep -qi "syntax\|unexpected" <<<"$out"; then
    pass "--fix does not corrupt the script it is running from"
else
    fail "--fix does not corrupt the script it is running from" "exit $rc" "$out"
fi
if [ -x "$SELF" ]; then
    pass "--fix preserves the executable bit"
else
    fail "--fix preserves the executable bit" "$(ls -l "$SELF")"
fi
if [ -z "$(find "$TMP" -name '*.license-header.tmp' -print -quit)" ]; then
    pass "--fix leaves no temporary file behind"
else
    fail "--fix leaves no temporary file behind" "$(find "$TMP" -name '*.license-header.tmp')"
fi

# gofmt is the arbiter of whether the result is still well-formed Go.
if command -v gofmt >/dev/null 2>&1; then
    for f in fixme.go tagged.go; do
        if gofmt -l "$TMP/$f" 2>&1 | grep . >/dev/null; then
            fail "gofmt accepts the fixed $f" "$(gofmt -l "$TMP/$f" 2>&1)"
        else
            pass "gofmt accepts the fixed $f"
        fi
    done
else
    echo "SKIP: gofmt not on PATH"
fi

# --- guard the guard ---------------------------------------------------
# Broken file discovery must not read as success. With no explicit list
# the gate falls back to `git ls-files`, and $TMP is not a repository —
# so this is the real "the gate cannot see the files" case, and the only
# acceptable answer is a refusal. Reporting a clean tree here is how a
# gate goes green over an entirely unlicensed checkout.
out=$(cd "$TMP" && bash "$CHECK" 2>&1)
rc=$?
if [ "$rc" -eq 2 ]; then
    pass "unusable file discovery refuses rather than passing"
else
    fail "unusable file discovery refuses rather than passing" "exit $rc" "$out"
fi

check "an unreadable file is exit 2, not a pass" 2 "" "" no-such-file.go
check "an unknown flag is a usage error" 2 --nonsense "" good.go

# --- file discovery ----------------------------------------------------
# Everything above drives the gate through LICENSE_HEADER_FILES, which
# hands it a list and therefore never exercises the code that BUILDS the
# list. That is where the bug was: the gate enumerated tracked files
# only, so a new unheadered file sat in the tree while it reported a
# clean run, and no test above could have noticed because none of them
# reach that line.
#
# These run against a throwaway repository so the enumeration is the
# thing under test.
REPO="$TMP/discovery"
mkdir -p "$REPO"
(
    cd "$REPO" || exit 1
    git init -q .
    git config user.email t@example.com
    git config user.name t
    # Without this the fixture inherits the developer's global signing
    # config. On a machine that signs with a hardware key that is not a
    # failure but a HANG: `git commit` blocks forever waiting for a
    # touch nobody is there to give, and `make check` stops with no
    # output at all. Hosted CI has no signing config, so this was green
    # there and stuck in a working checkout.
    git config commit.gpgsign false
    printf '// Copyright the docker-net-dhcp contributors.\n// SPDX-License-Identifier: GPL-3.0-only\n\npackage x\n' > tracked.go
    git add tracked.go
    git commit -qm init
) >/dev/null 2>&1

out=$(cd "$REPO" && bash "$CHECK" 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
    pass "a repository whose tracked files all carry the header is clean"
else
    fail "a repository whose tracked files all carry the header is clean" "exit $rc" "$out"
fi

# THE REGRESSION. Written, not yet added — the state a file is in for the
# entire time it is being worked on, and the state the gate used to be
# blind to.
printf 'package y\n' > "$REPO/untracked.go"
out=$(cd "$REPO" && bash "$CHECK" 2>&1)
rc=$?
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep "untracked.go" >/dev/null; then
    pass "an untracked file with no header is found, not silently skipped"
else
    fail "an untracked file with no header is found, not silently skipped" "exit $rc" "$out"
fi

# The other direction, and the reason --exclude-standard is not optional.
# Ignored paths are build output and nested worktrees; judging them would
# make the gate fail on other branches' files and on generated code.
mkdir -p "$REPO/build"
printf 'build/\n' > "$REPO/.gitignore"
printf 'package z\n' > "$REPO/build/generated.go"
rm -f "$REPO/untracked.go"
out=$(cd "$REPO" && bash "$CHECK" 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
    pass "an ignored file is not judged"
else
    fail "an ignored file is not judged" "exit $rc" "$out"
fi

# --fix has to reach the same files the check does, or the gate reports a
# problem it cannot fix and the fixer becomes something to work around.
printf 'package w\n' > "$REPO/newly-written.go"
(cd "$REPO" && bash "$CHECK" --fix) >/dev/null 2>&1
if head -2 "$REPO/newly-written.go" | grep "SPDX-License-Identifier: GPL-3.0-only" >/dev/null; then
    pass "--fix reaches an untracked file too"
else
    fail "--fix reaches an untracked file too" "header not inserted" "$(cat "$REPO/newly-written.go")"
fi

# --- the pinned-library exemption (D21) --------------------------------
#
# internal/dhcp-golib/ is a copy of another repository at a fixed SHA;
# writing a header into it would falsify internal/dhcp-golib/SOURCE and
# the next sync would revert it. The exemption is on the DISCOVERY path
# only -- an explicit LICENSE_HEADER_FILES list is a deliberate request
# and is still honoured -- so these cases build a real git tree.
vrepo() {
    local d="$TMP/$1"; rm -rf "$d"; mkdir -p "$d/internal/dhcp-golib/wire"
    (
        cd "$d" || exit 1
        git init -q .; git config user.email t@example.com; git config user.name t
        git config commit.gpgsign false
        printf 'package wire\n' > internal/dhcp-golib/wire/codec.go
        shift
        git add -A; git commit -qm fixture
    ) >/dev/null 2>&1
    echo "$d"
}

# 1. It applies: a headerless file under the prefix is not a finding.
D=$(vrepo vend-ok)
printf '// %s\n// %s\n\npackage x\n' "$COPY" "$SPDX" > "$D/ours.go"
(cd "$D" && git add -A && git commit -qm ours) >/dev/null 2>&1
vout=$(cd "$D" && bash "$CHECK" 2>&1); vrc=$?
if [ "$vrc" -eq 0 ]; then pass "a headerless file in the pinned library copy is exempt"
else fail "a headerless file in the pinned library copy is exempt" "exit $vrc" "$vout"; fi

# 2. And it ANNOUNCES the hole on the run that passed.
if grep -qF 'not inspected here' <<<"$vout"; then
    pass "the exempt count is announced on a passing run"
else
    fail "the exempt count is announced on a passing run" "$vout"
fi

# 3. Anchored at the repository root: the same directory name deeper in
#    the tree is this repo's source and is still inspected.
D=$(vrepo vend-deep)
mkdir -p "$D/pkg/internal/dhcp-golib"
printf 'package y\n' > "$D/pkg/internal/dhcp-golib/t.go"
printf '// %s\n// %s\n\npackage x\n' "$COPY" "$SPDX" > "$D/ours.go"
(cd "$D" && git add -A && git commit -qm deep) >/dev/null 2>&1
vout=$(cd "$D" && bash "$CHECK" 2>&1); vrc=$?
if [ "$vrc" -eq 1 ] && grep -qF 'pkg/internal/dhcp-golib/t.go' <<<"$vout"; then
    pass "the same path deeper in the tree is NOT exempt"
else
    fail "the same path deeper in the tree is NOT exempt" "exit $vrc" "$vout"
fi

# 4. THE ONE THAT MATTERS. A rule over an empty population is satisfied
#    by emptying the population, so a tree whose only source files are
#    exempt must REFUSE (exit 2), not pass.
D=$(vrepo vend-only)
vout=$(cd "$D" && bash "$CHECK" 2>&1); vrc=$?
if [ "$vrc" -eq 2 ]; then
    pass "a tree whose only source files are exempt is a refusal, not a pass"
else
    fail "a tree whose only source files are exempt is a refusal, not a pass" "exit $vrc" "$vout"
fi

# 5. --fix must not write into the pinned copy either.
D=$(vrepo vend-fix)
printf '// %s\n// %s\n\npackage x\n' "$COPY" "$SPDX" > "$D/ours.go"
(cd "$D" && git add -A && git commit -qm ours) >/dev/null 2>&1
(cd "$D" && bash "$CHECK" --fix) >/dev/null 2>&1
if [ "$(head -1 "$D/internal/dhcp-golib/wire/codec.go")" = "package wire" ]; then
    pass "--fix does not write a header into the pinned library copy"
else
    fail "--fix does not write a header into the pinned library copy" \
        "$(head -3 "$D/internal/dhcp-golib/wire/codec.go")"
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures test(s) failed" >&2
    exit 1
fi
echo "All check-license-headers.sh tests passed."
