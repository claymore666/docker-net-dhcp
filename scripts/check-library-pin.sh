#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The DHCP library in the BINARY is the module go.mod pins.
#
# WHY THIS EXISTS
#
# Until 2.0 the library travelled as a directory under internal/, and
# check-dhcp-golib-copy.sh tied the `library` build label to the bytes
# that directory held. The module import deleted the directory and the
# gate with it, and what replaced the label's derivation --
# `go list -m -f '{{.Version}}' <module>` in Makefile and Dockerfile --
# answers a different question than the one anybody reads it as.
#
# It reports the REQUIRED version. A `replace` to a local directory
# leaves that answer completely unchanged: `.Version` still says
# v0.1.0, `.Replace.Version` is empty, and the build takes its bytes
# from the directory. So the image gets labelled with a tag it was not
# built from. The integration lane's health cell derives its expectation
# with the same command, so it agrees with the wrong answer and stays
# green -- one fact derived twice, both times by the looser derivation.
#
# A directory replacement is also outside the sum chain: go.sum is not
# consulted for it, so the h1: hash in go.sum stops covering what was
# compiled.
#
# WHAT IT CHECKS
#
# Two halves, and the second is the one that matters.
#
#   1. DECLARATION. go.mod requires the library at a real version and
#      carries no `replace` for it, and go.sum has an h1: line for that
#      version.
#   2. BYTES. The compiled binary's own module record -- what
#      `go version -m` reads out of the file -- names the library at
#      that same version, with that same h1: hash, and with no `=>`
#      replacement line. That record is written by the linker from what
#      was actually built, so no declaration can talk it out of the
#      truth.
#
# Half 2 is why this gate is not a grep for `replace` in go.mod. A grep
# judges the file that was READ; this judges the file that was WRITTEN.
#
# WHAT IT CANNOT DO
#
# It cannot tell whether the bytes behind the h1: hash are the bytes the
# tag names -- only that the build and go.sum agree on one hash. The
# proxy and the checksum database are what stand behind that, and a
# GONOSUMDB or GOFLAGS=-mod=mod on a poisoned cache is outside what any
# check in this tree can see. Stated so nobody reads more into a green
# run than it earned.
#
# It also judges a binary compiled from this tree, and not the one
# inside the published image. That is the same reach the copy check it
# replaces had -- a test.yaml step over the checkout -- and the reason
# it is not extended into the integration lane is written down rather
# than left to be rediscovered: the suite and build jobs there check out
# `inputs.ref`, so a step added to either is a poisonable step in a job
# holding the default branch's cache scope. CodeQL says so, and the same
# alert is already open on that dispatch path. The image's own claim is
# checked by the health document cell and by reproducible-build.yml.
#
# USAGE
#
#   check-library-pin.sh [--tree <dir>] --binary <path> [--binary <path>]...
#
# IT BUILDS NOTHING. Whoever calls it supplies the binary: the local
# lane and test.yaml compile cmd/net-dhcp into a temporary directory
# first. A checking script that also compiles is a build step, and
# where it runs decides whether that matters -- see WHAT IT CANNOT DO.
set -uo pipefail

MODULE="github.com/claymore666/dhcp-golib"

TREE="$(cd "$(dirname "$0")/.." && pwd)"
BINARIES=()

while [ $# -gt 0 ]; do
    case "$1" in
        --tree)   TREE="$2"; shift 2 ;;
        --binary) BINARIES+=("$2"); shift 2 ;;
        *) echo "usage: $0 [--tree <dir>] [--binary <path>]..." >&2; exit 2 ;;
    esac
done

fail() { echo "check-library-pin: $*" >&2; exit 1; }

[ -f "$TREE/go.mod" ] || fail "no go.mod under $TREE; there is no pin to judge"

# --- half 1: the declaration ------------------------------------------

listed="$(cd "$TREE" && go list -m -f '{{.Version}}|{{with .Replace}}{{.Path}}@{{.Version}}{{end}}' "$MODULE" 2>&1)" || {
    echo "$listed" >&2
    fail "go list -m $MODULE failed in $TREE. The module is not a dependency of this tree, or the tree does not build"
}

version="${listed%%|*}"
replacement="${listed#*|}"

[ -n "$version" ] || fail "go.mod names no version for $MODULE"

if [ -n "$replacement" ]; then
    fail "the module graph replaces $MODULE with ${replacement%@}.
The replacement can come from go.mod or from a go.work; either way it is
not what the label says and not what go.sum covers:
\`go list -m -f '{{.Version}}'\` still prints $version, so the image would
be labelled with a tag it was not built from."
fi

sumline="$(grep -F "$MODULE $version h1:" "$TREE/go.sum" 2>/dev/null | head -1)"
[ -n "$sumline" ] || fail "go.sum carries no h1: entry for $MODULE $version. Nothing covers the bytes"
want_sum="${sumline##* }"

# --- half 2: the bytes ------------------------------------------------

# Half 2 is the half that matters, so having nothing to run it on is a
# refusal and not a quiet success on half 1 alone.
[ "${#BINARIES[@]}" -gt 0 ] || fail "no --binary given. Half 1 alone judges the declaration, and the declaration is not what gets linked"

for bin in "${BINARIES[@]}"; do
    [ -f "$bin" ] || fail "$bin does not exist; there are no bytes to judge"

    record="$(go version -m "$bin" 2>&1)" || {
        echo "$record" >&2
        fail "go version -m $bin failed. A binary with no module record cannot be judged, and passing it would be the emptied-domain answer"
    }

    # The record is tab-separated. A dep line is `\tdep\t<path>\t<version>[\t<sum>]`
    # and a replacement for it is the NEXT line, `\t=>\t<path>\t<version>[\t<sum>]`.
    dep_idx="$(printf '%s\n' "$record" | grep -n -P "^\tdep\t\Q$MODULE\E\t" | head -1 | cut -d: -f1)"
    [ -n "$dep_idx" ] || fail "$bin carries no module record for $MODULE.
It was not built from this module graph, or it was built with the library
vendored into the main module -- which is the shape the import removed."

    dep_line="$(printf '%s\n' "$record" | sed -n "${dep_idx}p")"
    next_line="$(printf '%s\n' "$record" | sed -n "$((dep_idx + 1))p")"

    case "$next_line" in
        "	=>"*)
            fail "$bin was built with $MODULE REPLACED: ${next_line#	=>	}
go.mod in $TREE declares no replacement, so the binary and the tree
disagree about what was compiled. The label derived from go.mod would
name a version these bytes are not."
            ;;
    esac

    got_version="$(printf '%s' "$dep_line" | cut -f4)"
    got_sum="$(printf '%s' "$dep_line" | cut -f5)"

    [ "$got_version" = "$version" ] || fail "$bin was built against $MODULE $got_version and $TREE/go.mod pins $version"

    case "$got_sum" in
        h1:*) ;;
        *) fail "$bin carries no h1: hash for $MODULE $got_version. A dependency outside the sum chain was linked in" ;;
    esac

    [ "$got_sum" = "$want_sum" ] || fail "$bin was built from $MODULE $got_version $got_sum and go.sum records $want_sum"

    echo "ok: $(basename "$bin") built from $MODULE $got_version ($got_sum)"
done

echo "check-library-pin: the pin, the sum and the linked bytes agree on $version"
