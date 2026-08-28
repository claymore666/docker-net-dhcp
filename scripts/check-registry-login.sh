#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert that every workflow job which builds container images on the
# self-hosted pool authenticates to Docker Hub first (#562).
#
# THE FAILURE THIS PREVENTS. The plugin build pulls two Hub base images.
# Pulled anonymously they are billed to a per-ADDRESS quota shared by
# the whole runner pool, and when it is spent `make plugin` dies fifteen
# seconds in with `toomanyrequests`. No test binary runs. The suite job
# goes red, the aggregate check goes red, and it looks exactly like a
# sharded test failure — on run 31939915811 two of four suite jobs died
# this way while the other two passed, and the tree under test was fine.
#
# WHY A GATE AND NOT A COMMENT. The login is one step in one job, easy
# to omit when a job is added or a matrix is split, and its absence is
# invisible until an unrelated burst of activity from the same address
# happens to spend the quota. That is the shape of every blind spot this
# project has been bitten by: the rule was written down, prose decayed,
# and nothing went red. So it is checked.
#
# WHAT IT DOES NOT CLAIM. It reads the workflow text; it cannot know
# whether the credential is valid, present as a secret, or accepted by
# the registry. It answers one question — "does this job try to log in
# before it builds" — which is the question that was answered wrong.
#
# THE SCAN IS PROVED TO HAVE HAPPENED, NOT ASSUMED FROM THE FILE LIST.
# `examined` counted loop visits, and `scan_file()` had no readability
# guard and its exit status was discarded, so a workflow the parser
# never opened was reported as a workflow with nothing wrong in it.
# Measured 2026-08-28 with a real #562 violation planted -- a job on the
# pool running `docker build` with no login step: exit 1 while readable,
# and exit 0 at mode 000 AND exit 0 as a directory named `*.yml`,
# printing `OK -- examined 2 workflow file(s)` and counting the file it
# could not read. That is non-vacuity evidence derived from the file
# list rather than from what the parser actually read, which is the
# defect class this gate belongs to one level up.
#
# So readability is decided in the SHELL before awk (`-f` and `-r` are
# the same on every awk and every uid, and the two awks disagree about a
# directory: mawk cannot open it and exits 2, gawk SKIPS it with a
# warning and exits 0), awk exit status is read beside it, and an
# unreadable workflow is a refusal rather than a silent zero findings.
#
# Usage: bash scripts/check-registry-login.sh [workflow-dir]
# Env:   REGISTRY_LOGIN_AWK  the awk to scan with. A test seam, and it
#                            exists so the STATUS half of the refusal
#                            has a case: with `-f` and `-r` asked first
#                            no workflow fixture can make awk fail while
#                            still producing output, so without the seam
#                            that half would be a branch nothing drives.
# Exit:  0 every pool job that builds authenticates
#        1 at least one does not
#        2 the check could not run (missing dir, nothing discovered, a
#          workflow that could not be read)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
DIR="${1:-$ROOT/.github/workflows}"

if [ ! -d "$DIR" ]; then
    echo "::error title=Workflow directory missing::$DIR is not a directory" >&2
    exit 2
fi

# The label that identifies the shared-address pool. A job on a
# GitHub-hosted runner is out of scope: those pull from addresses this
# project neither controls nor shares with itself.
POOL_LABEL="dhcp-ci"

# Anything that pulls a base image. `make plugin` / `make plugin-cover`
# / `make create` all run `docker build`, so the Make targets count as
# builds even though the word does not appear.
BUILD_RE='docker build|docker buildx build|make [^|;&]*plugin|make [^|;&]*create'

# What authenticating looks like. Either the action or a raw CLI login.
LOGIN_RE='docker/login-action|docker login'

shopt -s nullglob
files=("$DIR"/*.yml "$DIR"/*.yaml)
if [ "${#files[@]}" -eq 0 ]; then
    echo "::error title=No workflows found::$DIR matched no *.yml or *.yaml." \
         "This check would otherwise pass having examined nothing." >&2
    exit 2
fi

# Split a workflow into jobs by indentation, and report the ones that
# build on the pool without logging in first.
#
# ORDER IS PART OF THE ASSERTION, not a detail. A login that appears
# after the build authenticates nothing — the pull has already happened
# and already failed. So the login must be seen at a lower line number
# than the first build, and a job that logs in only afterwards is
# reported as if it had no login at all, with the reason named.
AWK="${REGISTRY_LOGIN_AWK:-awk}"

scan_file() {
    "$AWK" -v pool="$POOL_LABEL" -v build_re="$BUILD_RE" -v login_re="$LOGIN_RE" '
    function flush(   ) {
        if (job != "" && on_pool && build_line > 0) {
            if (login_line == 0)
                printf "%s\t%s\t%s\t%d\n", FILENAME, job, "no login step", build_line
            else if (login_line > build_line)
                printf "%s\t%s\t%s\t%d\n", FILENAME, job, "logs in at line " login_line ", after the build", build_line
        }
        job = ""; on_pool = 0; build_line = 0; login_line = 0
    }
    # A job header is exactly two spaces of indent followed by a name
    # and a colon, inside the top-level `jobs:` block.
    /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
    in_jobs && /^[^[:space:]#]/ { flush(); in_jobs = 0; next }
    in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
        flush()
        job = $1; sub(/:$/, "", job)
        next
    }
    !in_jobs { next }
    # Comments never carry behaviour, and this file is heavily
    # commented — counting a comment as a build would make the check
    # fire on its own explanation.
    /^[[:space:]]*#/ { next }
    {
        if ($0 ~ pool) on_pool = 1
        if (build_line == 0 && $0 ~ build_re) build_line = FNR
        if (login_line == 0 && $0 ~ login_re) login_line = FNR
    }
    END { flush() }
    ' "$1"
}

findings=()
examined=0
unreadable=0
for f in "${files[@]}"; do
    # Asked BEFORE awk, and asked of the shell, because the answer must
    # not depend on which awk the runner happens to have. `examined` is
    # incremented only after the file has actually been read, so the
    # number in the OK line counts files SCANNED rather than loop
    # iterations -- a non-vacuity witness has to come from the parser,
    # not from the glob that fed it.
    if [ ! -f "$f" ] || [ ! -r "$f" ]; then
        echo "::error file=$f,title=Cannot read workflow::$(basename "$f") is not a" \
             "readable regular file, so no job in it was scanned. Reporting that as" \
             "\"no unauthenticated build found\" would drop the file out of this" \
             "check's domain in silence, which is how the outage in #562 looked from" \
             "the outside." >&2
        unreadable=1
        continue
    fi
    # Captured whole rather than streamed, so awk's exit status can be
    # read at all: a process substitution consumed by `while read` hides
    # it, and an awk that dies part way through a file leaves partial
    # output that is not empty.
    scan_out="$(scan_file "$f")"
    scan_rc=$?
    if [ "$scan_rc" -ne 0 ]; then
        echo "::error file=$f,title=Cannot read workflow::scanning" \
             "$(basename "$f") failed (awk exit $scan_rc), so its jobs were not" \
             "judged. A parser that stopped is not a file with nothing wrong in it." >&2
        unreadable=1
        continue
    fi
    examined=$((examined + 1))
    [ -n "$scan_out" ] || continue
    while IFS=$'\t' read -r file job why line; do
        [ -n "${file:-}" ] || continue
        findings+=("$(basename "$file")	$job	$why	line $line")
    done <<< "$scan_out"
done

if [ "${#findings[@]}" -ne 0 ]; then
    echo "::error title=Unauthenticated registry pulls on the shared pool::the" \
         "following jobs build container images on the '$POOL_LABEL' pool without" \
         "authenticating to Docker Hub first (#562):" >&2
    for f in "${findings[@]}"; do
        IFS=$'\t' read -r file job why where <<<"$f"
        printf '  %s: job %s — %s (build at %s)\n' "$file" "$job" "$why" "$where" >&2
    done
    echo >&2
    echo "Anonymous pulls are billed to a per-address quota shared by the whole" >&2
    echo "pool. When it is spent the build dies with 'toomanyrequests' before any" >&2
    echo "test runs, and the red is indistinguishable from a test failure." >&2
    echo >&2
    echo "Add, before the first build step:" >&2
    echo >&2
    echo "  - name: Log in to Docker Hub" >&2
    echo "    if: env.HAS_HUB_CREDS == 'true'" >&2
    echo "    uses: docker/login-action@<pinned-sha> # v4" >&2
    echo "    with:" >&2
    echo "      username: \${{ secrets.DOCKERHUB_USERNAME }}" >&2
    echo "      password: \${{ secrets.DOCKERHUB_TOKEN }}" >&2
    echo >&2
    echo "with HAS_HUB_CREDS materialized in the job's env, as release.yml and" >&2
    echo "integration.yml both do. Gate it on the secrets being present: fork" >&2
    echo "pull requests are never given them, and a mandatory login would turn" >&2
    echo "every external contribution red." >&2
    exit 1
fi

# A violation outranks a refusal -- the block above has already exited 1
# if there was one -- but a refusal outranks a pass.
#
# There is deliberately NO second `examined -eq 0` backstop here, and
# that is a decision rather than an omission. Every path through the
# loop either increments `examined` or sets `unreadable`, so
# `examined == 0` with a non-empty file list implies `unreadable == 1`
# and this refusal has already fired. A zero-check below it would be a
# branch no fixture can reach -- the shape this branch is fixing one
# level up, arriving inside the fix for it. What makes `examined`
# trustworthy is not a backstop, it is that it is incremented AFTER the
# read rather than at the top of the loop: the number in the OK line
# now counts files the parser actually read, not loop iterations.
if [ "$unreadable" -ne 0 ]; then
    exit 2
fi

echo "OK — examined $examined workflow file(s); every job building on the '$POOL_LABEL' pool logs in to Docker Hub first."
