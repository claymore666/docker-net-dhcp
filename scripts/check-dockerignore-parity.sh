#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every directory .gitignore excludes, and every credential-shaped path it
# excludes, must also be excluded by .dockerignore.
#
# The defect this guards, first half: .dockerignore listed /plugin/ but not
# /plugin-cover/. Both are build output written by `sudo make create...`,
# so both end up root-owned; once one is in the build context, every
# unprivileged `docker build` from the repo root dies in the context walk
# with "permission denied" — in a place unrelated to whatever the
# developer changed. /logs/ (teed by the root-only integration targets)
# was missing for the same reason.
#
# The defect this guards, second half, and why the scope grew: this gate
# used to collect ROOT-ANCHORED directories only (`^/name/`), and said so
# in a comment that called everything else out of scope. .gitignore's
# credential block — secrets/, credentials/, .env, *.key, *.pem — is
# written unanchored, so the gate could not see it, and .dockerignore
# never carried a counterpart. A live SSH private key in ./secrets/ was
# therefore uploaded into the build context on every `docker build` from
# the repo root. It reached no image, because the Dockerfile copies go.*,
# cmd/ and pkg/ and nothing else — but the whole context is transferred to
# the daemon before any COPY is considered, and a gate whose pattern
# excludes the very lines that matter is the #487 shape a third time.
#
# check-build-context.sh already asserts the property for .claude/, but
# it can only see the defect on a tree that HAPPENS to have root-owned
# residue sitting there. On a clean checkout — every CI checkout — it
# passes no matter what .dockerignore says. That is a guard that fires by
# luck. This one is deterministic: it compares the two files and needs no
# residue to exist.
#
# Two classes are collected, and each is a superset that is then judged:
# every entry must be either excluded or listed in ALLOWED_ABSENT with a
# reason. A gate that only matched the entries it already knew about would
# go quiet exactly when a new one appeared.
#
#   directories   — /name/ or /name/*, and single-component name/. A
#                   directory ignored by git is build output, editor
#                   droppings or local state; nothing in the image is ever
#                   built from one, so shipping it is at best wasted
#                   context and at worst a failed build.
#
#   credentials   — .env, .env.*, .netrc, *.key, *.pem, *.p12, *.pfx and
#                   the like. Matched by shape rather than by a list of
#                   the lines that happen to be there today, so a newly
#                   added *.jks is caught by the gate rather than by
#                   whoever remembers to widen it.
#
# Ordinary file ignores (CLAUDE.md, code-review-report.md) stay out of
# scope: .dockerignore covers those its own way, and demanding parity for
# them would fail a correct file.
#
# Both classes are also asserted to be NON-EMPTY. A gitignore that yields
# nothing would otherwise report success having compared nothing at all —
# the failure this whole class of gate exists to prevent.
#
# On spelling: a bare `secrets` in .dockerignore excludes the context root
# only, while .gitignore's `secrets/` applies at any depth. Both spellings
# are ACCEPTED here — the entries this pair is about live at the context
# root, and a gate that demands one spelling fails correct files — but
# .dockerignore itself uses the `**/` form so that the two files mean the
# same thing rather than merely agreeing where it has been tested.
#
# Usage:
#   scripts/check-dockerignore-parity.sh [.gitignore] [.dockerignore]
# Exit: 0 pass, 1 drift, 2 usage error.

set -uo pipefail

GITIGNORE="${1:-.gitignore}"
DOCKERIGNORE="${2:-.dockerignore}"

for f in "$GITIGNORE" "$DOCKERIGNORE"; do
    if [ ! -r "$f" ]; then
        echo "cannot read $f" >&2
        exit 2
    fi
done

# Entries that are deliberately NOT in .dockerignore, each with the
# reason. Empty today: every known ignored directory and every
# credential-shaped path belongs out of the context. Add an entry rather
# than deleting a check.
declare -A ALLOWED_ABSENT=()

# Comments and trailing whitespace gone; negations (!/bin/.gitkeep) are
# not entries and must never be read as one.
strip() {
    sed -e 's/#.*//' -e 's/[[:space:]]*$//' "$1" | grep -vE '^[[:space:]]*(!|$)'
}

# --- class 1: directories ---------------------------------------------
# Root-anchored (/name/, /name/*) and single-component unanchored (name/).
# A multi-component path such as test/foo/bar is a FILE ignore and must
# not be read as the directory "test" — that would demand the whole test
# tree leave the build context.
mapfile -t dirs < <(
    strip "$GITIGNORE" |
        grep -E '^(/[^/]+/|[^/]+/$)' |
        sed -E -e 's#^/##' -e 's#/.*$##' |
        sort -u
)

if [ "${#dirs[@]}" -eq 0 ]; then
    echo "::error title=No directories matched::$GITIGNORE yielded no ignored" \
         "directory entries. This gate would otherwise pass having compared nothing." >&2
    exit 2
fi

# --- class 2: credential-shaped paths ----------------------------------
# Matched by shape. Extend the shape, never the list of known lines.
CREDENTIAL_RE='(^|/)(\.env(\..*)?|\.netrc|\.?[^/]*\.(key|pem|p12|pfx|jks|keystore|asc|gpg))$'
mapfile -t creds < <(strip "$GITIGNORE" | grep -E "$CREDENTIAL_RE" | sort -u)

if [ "${#creds[@]}" -eq 0 ]; then
    echo "::error title=No credential patterns matched::$GITIGNORE yielded no" \
         "credential-shaped ignores. That block was there when this gate was written;" \
         "if it is genuinely gone, remove this guard deliberately rather than letting" \
         "the gate pass having compared nothing." >&2
    exit 2
fi

# Does .dockerignore exclude ENTRY? Accepts a leading / or **/ and a
# trailing /, all of which exclude it at the context root.
excluded() {
    local bare="${1#/}"; bare="${bare%/}"
    local esc
    esc=$(printf '%s' "$bare" | sed -e 's/[][\\.^$*+?(){}|]/\\&/g')
    grep -qE "^(/|\*\*/)?${esc}/?[[:space:]]*$" "$DOCKERIGNORE"
}

fails=0
judge() {
    local entry="$1" kind="$2"
    if excluded "$entry"; then
        echo "ok    $entry excluded from the build context"
    elif [ -n "${ALLOWED_ABSENT[$entry]:-}" ]; then
        echo "ok    $entry deliberately in context — ${ALLOWED_ABSENT[$entry]}"
    else
        echo "FAIL  $entry is ignored by $GITIGNORE but not by $DOCKERIGNORE."
        if [ "$kind" = dir ]; then
            echo "      It is a directory git will not track. If it is ever written by a"
            echo "      root-only target, an unprivileged \`docker build\` from the repo root"
            echo "      will die walking it."
        else
            echo "      It is credential-shaped, and \`docker build\` uploads the whole"
            echo "      context to the daemon before any COPY is considered — so a local"
            echo "      copy is transferred even though no instruction names it."
        fi
        echo "      Add it to $DOCKERIGNORE (the \`**/\` form), or to ALLOWED_ABSENT"
        echo "      in this script with the reason."
        fails=1
    fi
}

for d in "${dirs[@]}"; do judge "$d" dir; done
for c in "${creds[@]}"; do judge "$c" cred; done

if [ "$fails" -ne 0 ]; then
    exit 1
fi
echo "OK: ${#dirs[@]} ignored directories and ${#creds[@]} credential-shaped ignores" \
     "are out of the build context."
