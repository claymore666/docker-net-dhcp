#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-release-tooling.sh.
#
# The check itself cannot run in CI usefully — hosted runners have no
# cosign and no signing key, and the thing it describes is a maintainer's
# box. What can be tested, and is tested here, is its verdict: build a
# synthetic PATH holding stub binaries, control the git config, and
# assert the check passes and fails where it should.
#
# Everything runs from a temp directory with GIT_CONFIG_GLOBAL and
# GIT_CONFIG_SYSTEM pointed at temp files, so the repository's own config
# and the real PATH cannot influence the result — otherwise a box that
# happens to have cosign installed would pass the "cosign missing" case.
#
# The PATH is built from a minimal directory of symlinks to exactly the
# external commands the check uses, never /usr/bin wholesale. The first
# draft of this file did include /usr/bin, and the "gh missing" case
# passed vacuously because it found the real gh — the same shape of
# vacuous pass that let #517 reach a tag.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-release-tooling.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

# Exactly the external commands check-release-tooling.sh invokes. Adding
# /usr/bin instead would smuggle in the real gh/cosign and make the
# "missing" cases pass without testing anything.
UTILS="$TMP/bin-utils"
mkdir -p "$UTILS"
for c in bash sed head git; do
    src="$(command -v "$c")" || { echo "cannot locate $c to build the test PATH"; exit 1; }
    ln -sf "$src" "$UTILS/$c"
done

stub() { # stub <dir> <name> [body]
    local dir="$1" name="$2" body="${3:-exit 0}"
    mkdir -p "$dir"
    printf '#!/bin/sh\n%s\n' "$body" > "$dir/$name"
    chmod +x "$dir/$name"
}

run_case() { # run_case <name> <want_exit> <bindir> <signingkey|"">
    local name="$1" want="$2" bindir="$3" key="$4" got
    local gitcfg="$TMP/gitconfig.$$"
    : > "$gitcfg"
    if [ -n "$key" ]; then
        printf '[user]\n\tsigningkey = %s\n' "$key" > "$gitcfg"
    fi
    ( cd "$TMP" \
      && PATH="$bindir:$UTILS" \
         GIT_CONFIG_GLOBAL="$gitcfg" \
         GIT_CONFIG_SYSTEM=/dev/null \
         bash "$CHECK" ) > "$TMP/out" 2>&1
    got=$?
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name — wanted exit $want, got $got"
        sed 's/^/      /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# A complete box: gh, cosign v3, a signing key.
complete="$TMP/bin-complete"
stub "$complete" gh
stub "$complete" cosign 'echo "GitVersion:    v3.1.3"'
run_case "complete tooling passes" 0 "$complete" "ABC123"

# The case that actually bit: cosign absent.
nocosign="$TMP/bin-nocosign"
stub "$nocosign" gh
run_case "cosign missing fails" 1 "$nocosign" "ABC123"

# Wrong major — present, on PATH, and still unable to verify the bundle.
oldcosign="$TMP/bin-oldcosign"
stub "$oldcosign" gh
stub "$oldcosign" cosign 'echo "GitVersion:    v2.4.1"'
run_case "cosign v2 fails" 1 "$oldcosign" "ABC123"

# Present but unreadable version — must not be treated as fine.
mutecosign="$TMP/bin-mutecosign"
stub "$mutecosign" gh
stub "$mutecosign" cosign 'echo "some other output"'
run_case "cosign with unreadable version fails" 1 "$mutecosign" "ABC123"

# gh absent.
nogh="$TMP/bin-nogh"
stub "$nogh" cosign 'echo "GitVersion:    v3.1.3"'
run_case "gh missing fails" 1 "$nogh" "ABC123"

# No signing key: step 9 tags with -s, so this is a real blocker.
run_case "missing signing key fails" 1 "$complete" ""

# crane is optional and its absence must not fail the check — the
# complete case above has no crane stub and passes, which is the
# assertion. Its presence must not change the verdict either.
withcrane="$TMP/bin-withcrane"
stub "$withcrane" gh
stub "$withcrane" cosign 'echo "GitVersion:    v3.1.3"'
stub "$withcrane" crane
run_case "crane present still passes" 0 "$withcrane" "ABC123"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures case(s) failed"
    exit 1
fi
echo "all cases passed"
