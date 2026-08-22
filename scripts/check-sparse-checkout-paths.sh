#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert that no `actions/checkout` step which sets `sparse-checkout:`
# shares its workspace path with another checkout in the same job (#736).
#
# THE BUG THIS IS WRITTEN FOR, WHICH SHIPPED.
#
# actions/checkout writes the sparse-checkout configuration into the
# repository it creates and does NOT clear a configuration it finds
# already there. So a second checkout at the SAME path — one that
# passes no `sparse-checkout` of its own, and therefore looks like it
# asks for a full tree — inherits the first one's cone and populates
# exactly the handful of files the first step named.
#
# dev @ 9245f09 did this twice:
#
#   pages.yml    deploy job    a resolver script checked out sparsely,
#                              then the docs tree checked out full at
#                              the same path. The deploy ran
#                              `pip install -r docs/requirements.txt`
#                              against a working tree containing one
#                              shell script and nothing else, and died
#                              on a file that had been in git the whole
#                              time.
#
#   release.yml  release job   the same pair, seventy lines and five
#                              unrelated steps apart. This one has no
#                              observer: release.yml runs on a tag push
#                              or a dispatch and on nothing else, so its
#                              first execution would have been tag day,
#                              in the job that holds the signing
#                              identity and both registry tokens, with
#                              `make push` reading a working tree of one
#                              shell script.
#
# WHY A GATE AND NOT JUST THE FIX. Neither instance is reachable by any
# pull request. `Docs site` at least fires on a push to dev, which is
# how the first one was caught within the hour; release.yml fires on
# nothing a pull request or a dev push can produce. No amount of CI on
# the change itself would have shown either. A check that reads the
# workflow text is the only observer these two files have before the
# tag exists, which makes this script the deliverable and the two
# `path:` lines the smaller half of the fix.
#
# THE RULE IS DIRECTIONLESS ON PURPOSE. It is not "a sparse checkout
# followed by a full one" — it is "shares its path with ANY other
# checkout in the same job". The mirror image (full first, sparse
# second) leaves the workspace sparse for every step AFTER it, which is
# a different symptom with the same cause, and a gate that passes the
# mirror image of the bug it was written for is not a gate.
#
# It is also job-scoped rather than adjacency-scoped, because
# release.yml's pair is not adjacent and an adjacency check would call
# it clean. Steps in different jobs run on different runners with
# different workspaces and cannot collide.
#
# THE FIX IS ALWAYS `path:`, never `git sparse-checkout disable`. The
# two checkouts are two different things — in both cases one is our
# trusted copy of a resolver and the other is the tree being built —
# and giving them separate paths says so. Clearing the config afterwards
# leaves them sharing a directory and relies on every future step
# remembering why.
#
# Usage: bash scripts/check-sparse-checkout-paths.sh [workflow-dir]
# Exit:  0 no sparse checkout shares a path
#        1 at least one does
#        2 the check could not run (missing dir, no checkouts discovered)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
DIR="${1:-$ROOT/.github/workflows}"

if [ ! -d "$DIR" ]; then
    echo "::error title=Workflow directory missing::$DIR is not a directory" >&2
    exit 2
fi

shopt -s nullglob
files=("$DIR"/*.yml "$DIR"/*.yaml)
if [ "${#files[@]}" -eq 0 ]; then
    echo "::error title=No workflows found::$DIR matched no *.yml or *.yaml." \
         "This check would otherwise pass having examined nothing." >&2
    exit 2
fi

# One record per actions/checkout step:
#
#   <job> <TAB> <line> <TAB> <path> <TAB> <sparse 0|1>
#
# `path` is the normalised workspace directory: absent, empty, `.` and
# `./` all mean the default workspace and are reported as `.`, so the
# collision is visible whichever way the two steps happen to spell it.
#
# Steps are buffered because the three facts that matter — that it IS a
# checkout, where it lands, and whether it configures sparse mode — sit
# on separate lines that only mean anything together.
scan_file() {
    awk '
    function reset_step() { sn = 0 }
    function flush_step(   i, l, isco, path, sparse, coline) {
        if (sn == 0) return
        isco = 0; path = "."; sparse = 0; coline = 0
        for (i = 1; i <= sn; i++) {
            l = sbuf[i]
            # A commented-out line carries no behaviour, and these
            # workflows explain this very rule in prose that quotes
            # `sparse-checkout:` and `path:`. Counting comments would
            # make the check fire on its own documentation — and, worse,
            # would let a commented-out `path:` answer for a real one.
            if (l ~ /^[[:space:]]*#/) continue
            # The first line of a step carries the leading "- ", so
            # `- uses: actions/checkout@...` and an indented `uses:`
            # are the same fact written two ways.
            sub(/^([[:space:]]*)-[[:space:]]+/, "  ", l)
            if (l ~ /uses:[[:space:]]*actions\/checkout/) {
                isco = 1
                coline = sline[i]
            }
            if (l ~ /^[[:space:]]*sparse-checkout:/) sparse = 1
            if (l ~ /^[[:space:]]*path:[[:space:]]*[^[:space:]]/) {
                path = l
                sub(/^[[:space:]]*path:[[:space:]]*/, "", path)
                sub(/[[:space:]]*$/, "", path)
                # A trailing comment is not part of the value.
                sub(/[[:space:]]+#.*$/, "", path)
                gsub(/^["]|["]$/, "", path)
                sub(/\/+$/, "", path)
                sub(/^\.\//, "", path)
                if (path == "") path = "."
            }
        }
        if (isco) printf "%s\t%d\t%s\t%d\n", job, coline, path, sparse
        reset_step()
    }
    function flush_job() {
        if (job != "") flush_step()
        job = ""; reset_step()
    }

    /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
    in_jobs && /^[^[:space:]#]/ { flush_job(); in_jobs = 0; next }
    !in_jobs { next }

    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { flush_job(); job = $1; sub(/:$/, "", job); next }

    {
        if ($0 ~ /^      - /) flush_step()
        sn++; sbuf[sn] = $0; sline[sn] = FNR
    }
    END { flush_job() }
    ' "$1"
}

findings=()
examined=0
checkouts=0
sparse_total=0

for f in "${files[@]}"; do
    examined=$((examined + 1))
    base="$(basename "$f")"

    # `job<TAB>path` -> how many checkouts land there, how many of those
    # are sparse, and which lines they are on.
    declare -A COUNT=() SPARSE=() LINES=()
    order=()

    while IFS=$'\t' read -r job line path sparse; do
        [ -n "${job:-}" ] || continue
        checkouts=$((checkouts + 1))
        [ "$sparse" = "1" ] && sparse_total=$((sparse_total + 1))
        key="${job}	${path}"
        [ -z "${COUNT[$key]:-}" ] && order+=("$key")
        COUNT["$key"]=$(( ${COUNT[$key]:-0} + 1 ))
        SPARSE["$key"]=$(( ${SPARSE[$key]:-0} + sparse ))
        if [ "$sparse" = "1" ]; then
            LINES["$key"]="${LINES[$key]:-}${LINES[$key]:+ }${line}(sparse)"
        else
            LINES["$key"]="${LINES[$key]:-}${LINES[$key]:+ }${line}"
        fi
    done < <(scan_file "$f")

    for key in "${order[@]}"; do
        [ "${COUNT[$key]}" -gt 1 ] || continue
        [ "${SPARSE[$key]}" -gt 0 ] || continue
        IFS=$'\t' read -r job path <<<"$key"
        findings+=("$base	$job	$path	${LINES[$key]}")
    done

    unset COUNT SPARSE LINES
done

# The failure this whole file is about is a step reporting success over
# something it never looked at. If the parser stops recognising
# checkouts, every workflow trivially satisfies the rule.
if [ "$checkouts" -eq 0 ]; then
    echo "::error title=No checkouts found::examined $examined workflow file(s)" \
         "and recognised no actions/checkout step in any of them. This check" \
         "would otherwise pass having examined nothing." >&2
    exit 2
fi

if [ "${#findings[@]}" -ne 0 ]; then
    echo "::error title=Sparse checkout shares a path::the following jobs run" \
         "more than one actions/checkout into the same workspace path, and at" \
         "least one of them sets sparse-checkout (#736):" >&2
    for entry in "${findings[@]}"; do
        IFS=$'\t' read -r file job path lines <<<"$entry"
        printf '  %s: job %s — path %s, checkouts at lines %s\n' \
               "$file" "$job" "$path" "$lines" >&2
    done
    echo >&2
    echo "actions/checkout does not clear a sparse configuration it finds" >&2
    echo "already in the workspace. The second checkout inherits the first" >&2
    echo "one's cone and populates only those files, however full a tree it" >&2
    echo "looks like it is asking for. That shipped on dev @ 9245f09, and the" >&2
    echo "release job would have hit it first on tag day." >&2
    echo >&2
    echo "Give the sparse checkout its own path. They are two different trees;" >&2
    echo "saying so is the fix:" >&2
    echo >&2
    echo "      - uses: actions/checkout@<pinned-sha> # v7" >&2
    echo "        with:" >&2
    echo "          path: .resolver" >&2
    echo "          sparse-checkout: scripts/some-script.sh" >&2
    echo "          sparse-checkout-cone-mode: false" >&2
    echo >&2
    echo "Do NOT reach for 'git sparse-checkout disable' between them. That" >&2
    echo "leaves the two trees sharing a directory and makes every later step" >&2
    echo "depend on a cleanup nobody can see from where they are reading." >&2
    exit 1
fi

echo "OK: $checkouts actions/checkout step(s) across $examined workflow" \
     "file(s), $sparse_total of them sparse; no sparse checkout shares a" \
     "path with another checkout in its job."
exit 0
