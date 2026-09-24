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
# ci-pool-exempt: a historical shard count from run 31939915811
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
# Usage: bash scripts/check-registry-login.sh [workflow-dir]
# Exit:  0 every pool job that builds authenticates
#        1 at least one does not
#        2 the check could not run (missing dir, nothing discovered)

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

# What authenticating looks like: a step whose own `uses:` key is
# docker/login-action, or a step whose run: shell has `docker login` as
# a command. Named in a step name:, a with: or env: value, or an echo, it
# authenticates nothing, so the text only makes a step a candidate (#883).
LOGIN_ACTION_RE='^      (- |  )uses:[[:space:]]*["'"'"']?docker/login-action@'

# shellcheck source=scripts/workflow-shell-lines.sh
. "$HERE/workflow-shell-lines.sh"

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
scan_file() {
    awk -v pool="$POOL_LABEL" -v build_re="$BUILD_RE" -v action_re="$LOGIN_ACTION_RE" '
    function flush_step(   i, l, raw, at) {
        raw = ""; at = ""
        for (i = 1; i <= sn; i++) {
            if (sbuf[i] ~ action_re) { printf "login\t%s\t%d\t-\n", job, sline[i]; sn = 0; return }
            if (index(sbuf[i], "docker login")) at = at (at == "" ? "" : ",") (i - 1) ":" sline[i]
            l = sbuf[i]; gsub(/\t/, " ", l); raw = raw (i > 1 ? "\037" : "") l
        }
        if (at != "") printf "cand\t%s\t%s\t%s\n", job, at, raw
        sn = 0
    }
    function flush(   ) {
        if (job != "") {
            flush_step()
            printf "job\t%s\t%d\t%d\n", job, on_pool, build_line
        }
        job = ""; on_pool = 0; build_line = 0; sn = 0
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
        if ($0 ~ /^      - /) flush_step()
        sn++; sbuf[sn] = $0; sline[sn] = FNR
    }
    END { flush() }
    ' "$1"
}

logins() { awk '$1 == "docker" && $2 == "login"'; }

# login_line MENTIONS RAW: the line of the step's first `docker login`
# command, or nothing when its shell runs none. MENTIONS is idx:line per
# line naming it; a line counts only if, read alone, it runs a login the
# whole step runs, so an earlier echo or name: cannot date a later login
# (#883). No such line: the last mention, the later reading. Blind: a
# heredoc line identical to the login, above the build, dates it early.
login_line() {
    local m idx ln c
    local -a L
    local -A avail=()
    IFS=$'\037' read -r -a L <<< "$2"
    while IFS= read -r c; do avail["$c"]=1; done \
        < <(printf '%s\n' "${L[@]}" | workflow_shell_lines --raw - | shell_simple_commands | logins)
    [ "${#avail[@]}" -gt 0 ] || return 0
    for m in ${1//,/ }; do
        idx="${m%%:*}"; ln="${m#*:}"
        while IFS= read -r c; do
            [ -n "${avail["$c"]:-}" ] && { echo "$ln"; return 0; }
        done < <(printf '%s\n' "${L[$idx]}" | sed -E 's/^[[:space:]]*(-[[:space:]]+)?run:[[:space:]]*//' |
            shell_simple_commands | logins)
    done
    echo "$ln"
}

findings=()
examined=0
for f in "${files[@]}"; do
    examined=$((examined + 1))
    declare -A LOGIN=()
    while IFS=$'\t' read -r kind job a b; do
        case "$kind" in
            cand) a="$(login_line "$a" "$b")"; [ -n "$a" ] || continue ;&
            login) [ "${LOGIN[$job]:-0}" -eq 0 ] || [ "$a" -lt "${LOGIN[$job]}" ] && LOGIN[$job]="$a" ;;
            job)
                [ "$a" = 1 ] || continue; [ "$b" -gt 0 ] || continue
                login="${LOGIN[$job]:-0}"
                if [ "$login" -eq 0 ]; then
                    findings+=("$(basename "$f")	$job	no login step	line $b")
                elif [ "$login" -gt "$b" ]; then
                    findings+=("$(basename "$f")	$job	logs in at line $login, after the build	line $b")
                fi
                ;;
        esac
    done < <(scan_file "$f")
    unset LOGIN
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

echo "OK — examined $examined workflow file(s); every job building on the '$POOL_LABEL' pool logs in to Docker Hub first."
