#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Git-fixture hygiene gate.
#
# A gate self-test that builds a throwaway repository has to commit into
# it, and `git commit` reads the DEVELOPER'S global config. On a machine
# that signs commits with a hardware key, that is not a failure — it is
# a hang: the commit blocks forever waiting for a touch nobody is there
# to give, `make check` stops with no output at all, and the run has to
# be diagnosed with `ps`. Hosted CI carries no signing config and no
# identity, so this stays green there and is stuck only in a working
# checkout — the same shape as #569 and #564.
#
# Found in test-check-license-headers.sh, which configured user.name and
# user.email and not commit.gpgsign; the two fixtures written beside it
# had all three. So the convention already existed and was enforced by
# nobody, which is how it goes missing in the first place.
#
# The rule: a script that MAKES A COMMIT must pin all three settings it
# would otherwise inherit — identity is what makes the commit possible
# at all, and gpgsign is what keeps it from blocking. A fixture that
# only runs `git init`, to give a gate an index to read, commits
# nothing and needs none of them; demanding config there would fire on
# a script with nothing to fix, which is how a gate stops being read.
#
# WHAT THIS DOES NOT COVER: it matches the settings textually in the
# same file, so it cannot tell that they are applied to the right
# repository, nor catch a fixture that clones instead of init-ing. It
# is the presence half of the claim; running the suite is the other
# half, and both are in the lane.
#
# Usage: check-selftest-fixtures.sh
# Env:   FIXTURE_ROOT  repository to inspect (default: the repo this
#                      script lives in) — the seam the self-test drives.
# Exit:  0 clean, 1 a fixture inherits the developer's config, 2 cannot check.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${FIXTURE_ROOT:-$(cd "$HERE/.." && pwd)}"

if ! git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "::error title=Not a git repository::$ROOT — this gate discovers" \
         "files through the git index, so it cannot inspect anything here." >&2
    exit 2
fi

# Every tracked shell script that commits into a fixture, found through
# the index rather than a filesystem walk (#639).
#
# MATCH THE SUPERSET, THEN JUDGE. The first version of this looked for
# the literal string `git commit`, which is one of at least three ways
# to write it: `git -C "$d" commit` and a `git_q "$d" commit` wrapper
# both do the same thing and neither contains it. Two of the three
# fixtures with this defect were written the wrapper way and were
# invisible to the gate meant to find them — the same blind spot
# check-version-pins had when it matched only well-formed pins (#487).
#
# So: any line naming a git-ish command with `commit` as its own word.
# `git config commit.gpgsign false` does not match, because there the
# word is followed by a dot rather than whitespace — the setting must
# not make a script look like its own consumer. Comments are stripped
# first: a script that only TALKS about committing has no fixture to
# configure, and a gate that fires on prose is waived on its first run.
mapfile -t CANDIDATES < <(git -C "$ROOT" ls-files -- '*.sh' | sort)

FIXTURES=()
for f in "${CANDIDATES[@]}"; do
    if grep -vE '^[[:space:]]*#' "$ROOT/$f" 2>/dev/null \
         | grep -E 'git[A-Za-z0-9_]*.*[[:space:]]commit([[:space:]]|$)' >/dev/null; then
        FIXTURES+=("$f")
    fi
done

if [ "${#FIXTURES[@]}" -eq 0 ]; then
    echo "::error title=No git fixtures found::no tracked *.sh makes a commit" \
         "in $ROOT. Either the pattern went stale or the scripts moved; this" \
         "gate would otherwise pass having inspected nothing." >&2
    exit 2
fi

# Matched case-insensitively. git config keys are case-insensitive and
# one of these fixtures spells it `commit.gpgSign`, which is correct and
# which a case-sensitive gate reports as missing. A gate that flags a
# working script is waived, and a waived gate is not a gate.
has() { grep -qi -- "$1" "$ROOT/$2"; }

findings=0
for f in "${FIXTURES[@]}"; do
    missing=()
    has 'commit\.gpgsign' "$f" || missing+=("commit.gpgsign false")
    has 'user\.email'     "$f" || missing+=("user.email")
    has 'user\.name'      "$f" || missing+=("user.name")

    # An ANNOTATED or signed tag is signed under `tag.gpgsign true` and
    # blocks exactly as a commit does; a lightweight `git tag v1` is
    # not. Demanded only from the scripts that make one, so this stays
    # a rule about what a fixture actually does.
    if grep -vE '^[[:space:]]*#' "$ROOT/$f" 2>/dev/null \
         | grep -E 'git[A-Za-z0-9_]*.*[[:space:]]tag[[:space:]].*[[:space:]]-[ams]' >/dev/null; then
        has 'tag\.gpgsign' "$f" || missing+=("tag.gpgsign false")
    fi
    if [ "${#missing[@]}" -ne 0 ]; then
        findings=$((findings + 1))
        printf '  %-38s inherits: %s\n' "$f" "${missing[*]}" >&2
    fi
done

if [ "$findings" -ne 0 ]; then
    echo >&2
    echo "::error title=Fixture inherits the developer's git config::${findings}" \
         "script(s). Set them on the fixture repo — an unset commit.gpgsign" \
         "makes the suite HANG on a signing machine rather than fail on it." >&2
    exit 1
fi

echo "PASS  git fixtures pin their own config: ${#FIXTURES[@]} committing script(s) inspected"
