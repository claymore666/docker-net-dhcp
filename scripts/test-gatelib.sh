#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for scripts/gatelib.sh (#744): collation, refusal, subject
# discovery, and that every scripts/check-*.sh sources the library.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
LIB="$HERE/gatelib.sh"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"

[ -f "$LIB" ] || { echo "cannot find $LIB"; exit 2; }

pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }
expect() { # desc want got
    if [ "$2" = "$3" ]; then ok "$1"; else no "$1: want '$2', got '$3'"; fi
}

guarded_tmpdir T

# A gate-shaped script: the library beside it, the body from stdin.
gate() { # name body
    mkdir -p "$T/bin"
    cp "$LIB" "$T/bin/gatelib.sh"
    { printf '#!/usr/bin/env bash\nset -uo pipefail\n'
      # shellcheck disable=SC2016 # the source line is written literally
      printf '. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2\n'
      cat; } > "$T/bin/$1.sh"
}
run() { # name [dir] -> stdout+stderr in $T/out, rc echoed
    ( cd "${2:-$T}" && bash "$T/bin/$1.sh" ) > "$T/out" 2>&1
    echo $?
}

# --- collation ----------------------------------------------------------
gate check-collate <<'EOF'
echo "LC_ALL=$LC_ALL"
printf 'check-good-first-issues.sh\ncheck-go-pins.sh\n' | sort | head -n 1
EOF
rc=$(LC_ALL=de_DE.UTF-8 LANG=de_DE.UTF-8 run check-collate)
expect "a gate runs under LC_ALL=C.UTF-8 whatever the caller exported" "0|LC_ALL=C.UTF-8" "$rc|$(head -n 1 "$T/out")"
expect "sort inside a gate puts check-go-pins.sh first (byte order)" "check-go-pins.sh" "$(sed -n 2p "$T/out")"
# C.UTF-8 reads text as characters, as the hosted lanes do: under C the
# arrow's first byte matches the bracket class check-docs-drift.sh uses.
gate check-reading <<'EOF'
s=$'\303\251'
echo "len=${#s}"
printf -- '- `x` \342\206\222 y\n' | grep -c -- '^- `x` [—:-]'
EOF
rc=$(LC_ALL=C LANG=C run check-reading)
expect "a gate reads UTF-8 as characters even when the caller runs under C" "1|len=1|0" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"
mkdir -p "$T/noloc"
cp "$T/bin/check-reading.sh" "$T/noloc/check-reading.sh"
sed 's/LC_ALL=C\.UTF-8/LC_ALL=xx_NONE.UTF-8/' "$LIB" > "$T/noloc/gatelib.sh"
for loc in C de_DE.UTF-8; do
    rc=0; ( cd "$T" && LC_ALL=$loc bash "$T/noloc/check-reading.sh" ) > "$T/out" 2>&1 || rc=$?
    expect "a box without the locale refuses (caller $loc)" "2|1" "$rc|$(grep -c 'C.UTF-8 locale is missing' "$T/out")"
done
# The pair differs only where a collating locale exists; the issue measured it under de_DE.UTF-8.
control=""
for loc in de_DE.UTF-8 en_US.UTF-8; do
    first=$(printf 'check-good-first-issues.sh\ncheck-go-pins.sh\n' | LC_ALL=$loc sort 2>/dev/null | head -n 1)
    if [ "$first" = check-good-first-issues.sh ]; then control=$loc; break; fi
done
if [ -n "$control" ]; then
    ok "control: $control orders the pair the other way, so the case above can fail"
else
    echo "NOTE  no collating locale here; the LC_ALL=C case above still holds"
fi

# --- refusal ------------------------------------------------------------
gate check-refuse <<'EOF'
gate_refuse "the input is gone"
echo "AFTER REFUSAL"
EOF
rc=$(run check-refuse)
expect "gate_refuse exits 2" 2 "$rc"
expect "gate_refuse names the gate and the reason" "::error title=check-refuse cannot judge::the input is gone" "$(cat "$T/out")"

gate check-title <<'EOF'
GATE_TITLE='Nothing to inspect' gate_refuse "kept title"
EOF
rc=$(run check-title)
expect "GATE_TITLE keeps an established annotation title" "2|::error title=Nothing to inspect::kept title" "$rc|$(cat "$T/out")"

# --- subject discovery ----------------------------------------------------
R="$T/repo"
mkdir -p "$R/sub" "$R/testdata" "$R/deep/testdata" "$R/docs/deep" "$R/fix/deep"
git init -q "$R"
for f in a.go a_test.go sub/b.go sub/b_test.go testdata/c.go deep/testdata/x.go ignored.go gone.go README.md sub/notes.md docs/guide.md docs/deep/x.md testdata/n.md check-a.sh sub/check-b.sh manifest.json fix/manifest.json fix/deep/manifest.json; do
    echo "package x" > "$R/$f"
done
printf 'ignored.go\n' > "$R/.gitignore"
git -C "$R" add a.go gone.go sub/b.go
rm "$R/gone.go"

gate check-subjects <<'EOF'
gate_subjects ${OPTS:-} files "$CLASS" ${DIR:+"$DIR"}
echo "n=${#files[@]}"
printf '%s\n' ${files[@]+"${files[@]}"}
EOF
subjects() { # class [dir] [opts]
    ( export CLASS="$1" DIR="${2:-}" OPTS="${3:-}"; run check-subjects "$R" )
}

rc=$(subjects go-src)
expect "go-src: tracked and untracked, not test, testdata, ignored or deleted" \
    "0|n=2|a.go|sub/b.go" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"
rc=$(subjects go)
expect "go: test files included, testdata at any depth excluded" \
    "0|n=4|a.go|a_test.go|sub/b.go|sub/b_test.go" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"
rc=$(subjects go-src "" --shallow)
expect "--shallow keeps the directory's own files only" "0|n=1|a.go" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"
rc=$(subjects go-src "$R/sub")
expect "a <dir> narrows the walk and prefixes the paths as find did" \
    "0|n=1|$R/sub/b.go" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"
rc=$(subjects md)
expect "md lists every markdown copy, nested ones too, testdata excluded" "0|n=4|README.md|docs/deep/x.md|docs/guide.md|sub/notes.md" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"
rc=$(subjects docs)
expect "docs is the README and the top-level docs pages" "0|n=2|README.md|docs/guide.md" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"
rc=$(subjects gates)
expect "gates is the directory's own check-*.sh only" "0|n=1|check-a.sh" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"
rc=$(subjects manifest)
expect "manifest is one directory level deep only" "0|n=1|fix/manifest.json" "$rc|$(tr '\n' '|' < "$T/out" | sed 's/|$//')"

rc=$(subjects go-test "$R/docs")
expect "an empty class refuses by default" 2 "$rc"
if grep -q "no 'go-test' file under" "$T/out"; then ok "the empty refusal names the class"; else no "the empty refusal names the class: $(cat "$T/out")"; fi
if grep -q '^n=' "$T/out"; then no "the gate went on after the empty refusal"; else ok "the gate stops at the empty refusal"; fi
rc=$(subjects go-test "$R/docs" --may-be-empty)
expect "--may-be-empty is the explicit opt-out" "0|n=0" "$rc|$(cat "$T/out")"
rc=$(subjects nosuchclass)
expect "an unknown class refuses" 2 "$rc"

mkdir -p "$T/plain"; echo "package x" > "$T/plain/a.go"
rc=$(subjects go "$T/plain")
expect "a directory outside any git work tree refuses, and says so" "2|1" "$rc|$(grep -c 'is not in a git work tree' "$T/out")"
rc=$(subjects go "$T/missing")
expect "a missing directory refuses, and says so" "2|1" "$rc|$(grep -c 'is not a directory' "$T/out")"

mkdir -p "$T/fakebin"
printf '#!/bin/sh\ncase "$*" in *ls-files*) exit 128 ;; esac\nexec %s "$@"\n' "$(command -v git)" > "$T/fakebin/git"
chmod +x "$T/fakebin/git"
rc=$(PATH="$T/fakebin:$PATH" subjects go)
expect "a failing git ls-files refuses, not an empty pass" 2 "$rc"
rc=$(PATH="$T/fakebin:$PATH" subjects go "" --may-be-empty)
expect "a failing git ls-files refuses even where empty is allowed" "2|1" "$rc|$(grep -c 'git ls-files failed' "$T/out")"

# --- adoption -------------------------------------------------------------
missing=""
n=0
for g in "$HERE"/check-*.sh; do
    n=$((n + 1))
    # shellcheck disable=SC2016 # the source line is matched literally
    grep -qxF '. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2' "$g" || missing="$missing ${g##*/}"
done
[ "$n" -gt 0 ] || no "no scripts/check-*.sh found beside the library"
# A refusal inside $(...) or <(...) ends only the subshell, and the gate goes on.
wrapped=$(grep -lE '[$<]\([[:space:]]*gate_(subjects|refuse)' "$HERE"/check-*.sh | sed 's|.*/||' | tr '\n' ' ')
expect "no gate calls gate_subjects or gate_refuse inside a subshell" "" "$wrapped"
expect "every scripts/check-*.sh sources gatelib.sh ($n gates)" "" "$missing"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
