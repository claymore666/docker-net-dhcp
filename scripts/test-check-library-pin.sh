#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-library-pin.sh.
#
# Every case builds a REAL binary from a throwaway module that requires
# the library, and judges that binary. A fixture that only wrote text
# files could not test half 2 at all: the thing the gate reads is the
# module record the linker writes into the file, and only a linker
# writes one.
#
# The library comes from the local module cache, served to the fixture
# as a file:// proxy. That keeps these cases off the network and makes
# them independent of whether the gate's own tree is currently pinned
# the way the fixture is.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-library-pin.sh"
MODULE="github.com/claymore666/dhcp-golib"
VERSION="v0.1.0"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
n=0

MODCACHE="$(go env GOMODCACHE)"
export GOPROXY="file://${MODCACHE}/cache/download"
export GOFLAGS=-mod=mod
export GOSUMDB=off
export GOWORK=off

# The library source, writable, for the replacement cases. Copying the
# module cache rather than pointing at it directly matters: the cache is
# read-only and `go build` refuses to use it as a replacement target.
LIBCOPY="$TMP/libcopy"
mkdir -p "$LIBCOPY"
if ! cp -r "$MODCACHE/$MODULE@$VERSION/." "$LIBCOPY/" 2>/dev/null; then
    echo "FAIL: $MODULE@$VERSION is not in the module cache; run 'go mod download' first" >&2
    exit 1
fi
chmod -R u+w "$LIBCOPY"

# make_module <dir> — a module that requires the library and uses it, so
# the linker records the dependency.
make_module() {
    local d="$1"
    mkdir -p "$d/cmd/net-dhcp"
    cat > "$d/go.mod" <<EOF
module fixture

go 1.24.0

require $MODULE $VERSION
EOF
    cat > "$d/cmd/net-dhcp/main.go" <<EOF
package main

import (
	"fmt"

	"$MODULE/wire"
)

func main() { fmt.Println(wire.OptHostName) }
EOF
    (cd "$d" && go mod tidy) >/dev/null 2>&1
}

build_into() { # build_into <moduledir> <out>
    (cd "$1" && go build -o "$2" ./cmd/net-dhcp) >/dev/null 2>&1
}

run_case() { # run_case <name> <want_exit> <tree> <binary-or-empty>
    n=$((n + 1))
    local name="$1" want="$2" tree="$3" bin="${4:-}" got
    if [ -n "$bin" ]; then
        bash "$CHECK" --tree "$tree" --binary "$bin" > "$TMP/out" 2>&1
    else
        bash "$CHECK" --tree "$tree" > "$TMP/out" 2>&1
    fi
    got=$?
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name — wanted exit $want, got $got"
        sed 's/^/      /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# --- the honest build --------------------------------------------------
#
# The baseline AND the gate's other direction: a tree with the tag
# required, no replacement anywhere, and a binary built from it is NOT
# refused. Without this every refusal below would be satisfiable by a
# gate that refuses everything.
CLEAN="$TMP/clean"
make_module "$CLEAN"
build_into "$CLEAN" "$TMP/bin-clean" || { echo "FAIL: could not build the clean fixture" >&2; exit 1; }
run_case "the tag pinned, no replacement, is accepted" 0 "$CLEAN" "$TMP/bin-clean"

# The same tree with no --binary: the gate builds one itself, which is
# how the local lane runs it.
run_case "the gate builds its own binary when given none" 0 "$CLEAN" ""

# --- half 2: the bytes -------------------------------------------------
#
# The scenario the deleted copy check used to cover, and the reason this
# gate reads the binary at all. The DECLARATION here is clean -- the tree
# is $CLEAN, go.mod requires the tag, go.sum covers it -- and only the
# bytes were built from somewhere else. Half 1 sees nothing; half 2 must
# refuse.
REPLACED="$TMP/replaced"
make_module "$REPLACED"
printf '\nreplace %s => %s\n' "$MODULE" "$LIBCOPY" >> "$REPLACED/go.mod"
build_into "$REPLACED" "$TMP/bin-replaced" || { echo "FAIL: could not build the replaced fixture" >&2; exit 1; }
run_case "a clean tree judging a binary built from a directory replacement is refused" 1 "$CLEAN" "$TMP/bin-replaced"

# --- half 1: the declaration -------------------------------------------
#
# The other order: the tree carries the replacement, so the refusal must
# come before any binary is read. Judged with the CLEAN binary so the
# only thing that can refuse is half 1.
run_case "a tree that replaces the library is refused" 1 "$REPLACED" "$TMP/bin-clean"

# --- the sum chain -----------------------------------------------------
NOSUM="$TMP/nosum"
make_module "$NOSUM"
grep -v "^$MODULE $VERSION h1:" "$NOSUM/go.sum" > "$NOSUM/go.sum.new" && mv "$NOSUM/go.sum.new" "$NOSUM/go.sum"
run_case "a tree with no h1: entry for the library is refused" 1 "$NOSUM" "$TMP/bin-clean"

# --- the emptied domain ------------------------------------------------
#
# A gate that reads binaries is satisfied by handing it a binary with
# nothing in it. `go version -m` on a file with no module record prints
# no dep lines at all, and the answer to that is a refusal, not silence.
NODEP="$TMP/nodep"
mkdir -p "$NODEP/cmd/net-dhcp"
printf 'module fixture\n\ngo 1.24.0\n' > "$NODEP/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$NODEP/cmd/net-dhcp/main.go"
build_into "$NODEP" "$TMP/bin-nodep"
run_case "a binary that links no library at all is refused" 1 "$CLEAN" "$TMP/bin-nodep"

# A path that is not there is not a pass either.
run_case "a binary that does not exist is refused" 1 "$CLEAN" "$TMP/bin-missing"

# --- the gate's own domain ---------------------------------------------
#
# A tree that does not depend on the library cannot be judged, and
# saying nothing about it would be the same emptied-domain answer.
run_case "a tree that does not require the library is refused" 1 "$NODEP" "$TMP/bin-clean"

echo
if [ "$failures" -eq 0 ]; then
    echo "all $n cases passed"
else
    echo "$failures of $n cases FAILED"
    exit 1
fi
