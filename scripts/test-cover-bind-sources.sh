#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for the bind-source creation in the Makefile's `create-cover`.
#
# THE FAILURE THIS GUARDS. config-cover.json declares four bind sources;
# the recipe used to create exactly one of them by name (/var/lib/net-dhcp)
# and left /var/lib/dh-cover to a comment that told the operator to run
# `mkdir -p` once, by hand. Nothing reconciled the list against the
# manifest, so the manifest grew a source the recipe did not know about
# and `make capture-fixtures` — advertised as one command — died on
#
#   failed to fulfil mount request: open /var/lib/dh-cover: no such file
#
# That is the third instance of one shape here: #588 (first install on a
# host that had never run a container), #660 (a recreated CI runner), and
# this one. A missing bind source does not degrade dockerd — the mount
# fails, and an already-enabled plugin takes the daemon down with it.
#
# So the recipe now DERIVES the directories from the manifest. This test
# exists because a derivation can be quietly reverted to a list, and a
# list looks correct right up until the manifest changes.
#
# THE /var/lib FILTER IS LOAD-BEARING, in the opposite direction:
# /var/run/docker.sock is a bind source too, and `mkdir -p` on a socket
# path replaces it with a directory. A future "just create them all"
# simplification breaks the plugin's Docker connection instead of a
# coverage run, so the socket is asserted to be left alone.
#
# HOW IT RUNS. The real recipe is extracted from the real Makefile and
# executed with `mkdir` shadowed by a stub that records its arguments
# instead of creating anything. Nothing here writes outside its tempdir,
# and a copy of the logic in this file could not rot apart from the
# shipped one, because there is no copy.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
MK="$REPO/Makefile"
MANIFEST="$REPO/config-cover.json"
[ -f "$MK" ] || { echo "FAIL: $MK does not exist"; exit 2; }
[ -f "$MANIFEST" ] || { echo "FAIL: $MANIFEST does not exist"; exit 2; }

command -v jq >/dev/null || { echo "FAIL: this self-test needs jq"; exit 2; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

# --- extract the shipped recipe ----------------------------------------
# Everything between the target line and the first `docker plugin` line:
# that is the preparation, which is what this test is about. Make joins
# backslash continuations into one shell command, so we do too.
extract_recipe() {
    sed -n '/^create-cover:/,/^\tdocker plugin/p' "$MK" \
        | sed -e '1d' -e '/^\tdocker plugin/d' \
        | sed -e 's/^\t//' -e 's/^@//' \
        | sed -e ':a' -e '/\\$/{N;s/\\\n[[:space:]]*/ /;ba' -e '}'
}

RECIPE="$(extract_recipe)"
if [ -z "${RECIPE//[[:space:]]/}" ]; then
    echo "FAIL: extracted no recipe from create-cover — the target's shape changed"
    exit 2
fi

# The regression itself: a hardcoded directory name in the recipe is the
# thing that rotted. Derivation is the fix, so name it directly.
if printf '%s\n' "$RECIPE" | grep -E 'mkdir -p +/var/' >/dev/null; then
    echo "FAIL: create-cover hardcodes a bind-source path — derive it from the manifest instead"
    fails=1
else
    echo "PASS: create-cover names no bind-source path of its own"
fi

# --- run it with mkdir shadowed ----------------------------------------
mkdir -p "$TMP/bin"
cat > "$TMP/bin/mkdir" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do
    [ "$a" = "-p" ] && continue
    echo "$a" >> "$MKDIR_LOG"
done
STUB
chmod +x "$TMP/bin/mkdir"

# Returns the sorted set of paths the recipe would have created.
run_recipe() {
    local manifest="$1" dir="$TMP/run"
    rm -rf "$dir"; command mkdir -p "$dir"
    cp "$manifest" "$dir/config-cover.json"
    MKDIR_LOG="$dir/log" : > "$dir/log"
    (
        cd "$dir" || exit 9
        export MKDIR_LOG="$dir/log"
        export PATH="$TMP/bin:$PATH"
        eval "$RECIPE"
    )
    rc=$?
    sort -u "$dir/log" 2>/dev/null | tr '\n' ' ' | sed 's/ $//'
    return $rc
}

# 1. The shipped manifest: BOTH /var/lib sources, not just the one the
#    recipe used to name.
got=$(run_recipe "$MANIFEST")
want=$(jq -r '.mounts[]? | select(.type=="bind") | .source | select(startswith("/var/lib/"))' \
        "$MANIFEST" | sort -u | tr '\n' ' ' | sed 's/ $//')
check "shipped manifest: every /var/lib bind source is created" "$want" "$got"
case "$got" in
    *"/var/lib/dh-cover"*) echo "PASS: /var/lib/dh-cover specifically — the one that was missed" ;;
    *) echo "FAIL: /var/lib/dh-cover specifically — the one that was missed"; fails=1 ;;
esac

# 2. The socket is never handed to mkdir. This is the check that stops a
#    "create them all" simplification from replacing docker.sock with a
#    directory.
case " $got " in
    *docker.sock*) echo "FAIL: /var/run/docker.sock was passed to mkdir — that replaces the socket"; fails=1 ;;
    *) echo "PASS: /var/run/docker.sock is left alone" ;;
esac

# 3. A source added to the manifest tomorrow is picked up with no edit to
#    the recipe. A hardcoded list fails exactly here, which is the point.
jq '.mounts += [{"source":"/var/lib/dh-newthing","destination":"/newthing","type":"bind","options":["bind","rw"]}]' \
    "$MANIFEST" > "$TMP/grown.json"
got=$(run_recipe "$TMP/grown.json")
case " $got " in
    *"/var/lib/dh-newthing"*) echo "PASS: a newly declared /var/lib source needs no recipe edit" ;;
    *) echo "FAIL: a newly declared /var/lib source needs no recipe edit — the recipe is not deriving"; fails=1 ;;
esac

# 4. A manifest with no bind mounts must succeed having created nothing,
#    not fail — `xargs -r` is what makes that true.
jq 'del(.mounts)' "$MANIFEST" > "$TMP/nomounts.json"
got=$(run_recipe "$TMP/nomounts.json"); rc=$?
check "manifest with no mounts creates nothing" "" "$got"
check "manifest with no mounts still succeeds" "0" "$rc"

# 5. A missing manifest must fail rather than quietly create nothing —
#    "no directories needed" and "I could not read the manifest" must not
#    look the same, which is the failure mode this repo keeps finding.
rm -rf "$TMP/run"; command mkdir -p "$TMP/run"
(
    cd "$TMP/run" || exit 9
    export MKDIR_LOG="$TMP/run/log"; : > "$MKDIR_LOG"
    export PATH="$TMP/bin:$PATH"
    eval "$RECIPE"
) >/dev/null 2>&1
check "absent manifest fails rather than passing over nothing" "1" "$([ $? -ne 0 ] && echo 1 || echo 0)"

echo
if [ "$fails" -eq 0 ]; then
    echo "create-cover bind-source self-test: all cases passed"
else
    echo "create-cover bind-source self-test: FAILURES above"
fi
exit "$fails"
