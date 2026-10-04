#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for preflight-release-tooling.sh (#745).
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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

CHECK="$(cd "$(dirname "$0")" && pwd)/preflight-release-tooling.sh"
guarded_tmpdir TMP

failures=0

# Exactly the external commands preflight-release-tooling.sh invokes. Adding
# /usr/bin instead would smuggle in the real gh/cosign and make the
# "missing" cases pass without testing anything.
UTILS="$TMP/bin-utils"
mkdir -p "$UTILS"
for c in bash sed head git dirname; do
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
         bash "${CASE_CHECK:-$CHECK}" ) > "$TMP/out" 2>&1
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

# The major is matched whole: v30 is not major 3 (#745).
cosign30="$TMP/bin-cosign30"
stub "$cosign30" gh
stub "$cosign30" cosign 'echo "GitVersion:    v30.0.1"'
run_case "cosign v30 is not major 3" 1 "$cosign30" "ABC123"

# The major comes from scripts/release-tooling.env, the file release.yml and
# the cosign-major-stated invariant read too (#522, #745). A copy of the
# script beside its own data file proves the verdict follows that file and
# not a literal: with major 4 on file, the v3 stub fails and a v4 stub passes.
PF="$TMP/pf"
mkdir -p "$PF"
cp "$CHECK" "$PF/preflight-release-tooling.sh"
cp "$(dirname "$CHECK")/gatelib.sh" "$PF/"
printf 'COSIGN_MAJOR=4\n' > "$PF/release-tooling.env"
export CASE_CHECK="$PF/preflight-release-tooling.sh"
run_case "a v3 cosign fails when the data file says major 4" 1 "$complete" "ABC123"
cosign4="$TMP/bin-cosign4"
stub "$cosign4" gh
stub "$cosign4" cosign 'echo "GitVersion:    v4.0.1"'
run_case "a v4 cosign passes when the data file says major 4" 0 "$cosign4" "ABC123"
printf '# no major here\n' > "$PF/release-tooling.env"
run_case "a data file without COSIGN_MAJOR is exit 2" 2 "$complete" "ABC123"
rm -f "$PF/release-tooling.env"
run_case "a missing data file is exit 2" 2 "$complete" "ABC123"
unset CASE_CHECK

# The signing step and this preflight must read the same file, and no other
# tracked file may carry a major of its own (#745): two copies is how the
# docs, the workflow and the preflight come to name different majors.
REPO="$(cd "$(dirname "$CHECK")/.." && pwd)"
# The read itself (the sed and the file it is given), not the name in an error
# message: pointing the sed at another file must fail here.
# Comment lines are dropped first: a comment that quotes the read must not
# stand in for the line the step runs.
read_lines="$(grep -v '^[[:space:]]*#' "$REPO/.github/workflows/release.yml" | grep -A1 "sed -n 's/^COSIGN_MAJOR=" || true)"
if [[ "$read_lines" == *scripts/release-tooling.env* ]]; then
    echo "PASS: release.yml reads scripts/release-tooling.env"
else
    echo "FAIL: release.yml does not read scripts/release-tooling.env"
    failures=$((failures + 1))
fi
# Both read the major with one sed expression (the one each actually runs).
rel_expr="$(grep -v '^[[:space:]]*#' "$REPO/.github/workflows/release.yml" | grep -o 's/^COSIGN_MAJOR=[^'"'"']*' || true)"
pf_expr="$(grep -v '^[[:space:]]*#' "$CHECK" | grep -o 's/^COSIGN_MAJOR=[^'"'"']*' || true)"
if [ -n "$pf_expr" ] && [ "${rel_expr%%$'\n'*}" = "${pf_expr%%$'\n'*}" ]; then
    echo "PASS: release.yml and this preflight read the major with the same sed"
else
    echo "FAIL: release.yml ('$rel_expr') and the preflight ('$pf_expr') read the major differently"
    failures=$((failures + 1))
fi
literals="$(cd "$REPO" && git grep -lE 'COSIGN_MAJOR=[0-9]' -- . \
    ':(exclude)scripts/release-tooling.env' ':(exclude,glob)scripts/test-*.sh')"
if [ -z "$literals" ]; then
    echo "PASS: no tracked file but the data file assigns COSIGN_MAJOR"
else
    echo "FAIL: COSIGN_MAJOR=<n> is assigned outside scripts/release-tooling.env:"
    printf '      %s\n' "$literals"
    failures=$((failures + 1))
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures case(s) failed"
    exit 1
fi
echo "all cases passed"
