#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# A dispatch-supplied ref must never be a pull-request head (#593).
#
# THE FAILURE THIS PREVENTS. `workflow_dispatch` hands the caller a free
# text `ref` input that goes straight to `actions/checkout`. The API
# will not let you *dispatch* a workflow at `refs/pull/N/head` — that is
# neither a branch nor a tag — but a fork's PR head is fetchable from
# the BASE repository, with no `repository:` argument:
#
#     $ git ls-remote https://github.com/claymore666/docker-net-dhcp 'refs/pull/*/*'
#         refs/pull/593/head
#
# so `-f ref=refs/pull/593/head` walks around the restriction. Unlike a
# `pull_request` event from a fork, a dispatch run HAS the repository
# secrets. On integration.yml that put outside code in a job that runs
# as root on the self-hosted pool holding the Docker Hub credential, and
# in the gate job whose script executes with the default branch's Actions
# cache scope — CodeQL's `actions/cache-poisoning/poisonable-step`, open
# high, integration.yml:165.
#
# WHY A GATE AND NOT A COMMENT. integration.yml already carried the rule
# in prose: "checking out a PR head in a job that has the credential …
# must not be done here". It was written down correctly, and a route it
# did not anticipate walked around it anyway. That is this project's
# recurring failure shape, so the rule is executable now.
#
# TWO MODES, one authority for what a legitimate ref looks like.
#
#   --validate REF   Runtime. Called by the workflows themselves, from a
#                    TRUSTED checkout, before anything consumes REF. An
#                    empty REF is fine — that is the ordinary case, where
#                    checkout falls back to the triggering ref.
#
#   [workflow-dir]   Static. Called by test.yaml. Asserts that every job
#                    which hands a dispatch-influenced value to
#                    `actions/checkout` is covered by a --validate call
#                    first, in the same job above the checkout or in a
#                    job it (transitively) needs. The second half is the
#                    load-bearing one: a guard wired into the first
#                    consumer and not the second is how #593 happened.
#
# WHAT IT DOES NOT CLAIM. The static half reads workflow text. It cannot
# know whether a ref resolves, whether the runner is privileged, or
# whether a secret is really in scope. It answers one question — "can an
# unvalidated dispatch ref reach a checkout" — which is the question
# that was answered wrong.
#
# Usage: bash scripts/check-dispatch-ref.sh --validate <ref>
#        bash scripts/check-dispatch-ref.sh [workflow-dir]
# Exit:  0 ref is acceptable / every consumer is gated
#        1 ref is rejected / at least one consumer is ungated
#        2 the check could not run (bad usage, missing dir, nothing found)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"

# ---------------------------------------------------------------- runtime

# The documented purpose of these inputs is re-running "a tree that has
# not changed" — our own branches, tags and SHAs (#419). Everything else
# is refused by an ALLOWLIST rather than by blocking `refs/pull/`
# specifically: a deny list only ever knows the routes somebody already
# thought of, and this issue exists because one nobody thought of was
# taken.
validate_ref() {
    local ref="$1"

    # Blank is the ordinary case: every event except workflow_dispatch
    # leaves the input empty and checkout uses the triggering ref.
    if [ -z "$ref" ]; then
        echo "OK — no dispatch ref supplied; checkout will use the triggering ref."
        return 0
    fi

    local reason=""

    # A leading dash would reach git as an option rather than a ref.
    case "$ref" in
    -*) reason="starts with '-', which reaches git as an option, not a ref" ;;
    esac

    # Keep the character set to what a git ref can legitimately hold and
    # what is safe to interpolate into a workflow. This also removes any
    # question about quoting downstream.
    if [ -z "$reason" ] && printf '%s' "$ref" | LC_ALL=C grep -q '[^A-Za-z0-9._/-]'; then
        reason="contains characters outside [A-Za-z0-9._/-]"
    fi

    if [ -z "$reason" ]; then
        case "$ref" in
        *..*) reason="contains '..', which git rejects in a ref name" ;;
        esac
    fi

    if [ -z "$reason" ]; then
        case "$ref" in
        # The whole point. Both the fully-qualified form checkout
        # honours and the bare form somebody will try next.
        refs/pull/* | refs/remotes/pull/* | pull/*)
            reason="is a pull-request head. A dispatch run carries the repository secrets, so checking out a PR head hands them to code that has not been reviewed (#593). Re-run the PR's own checks instead"
            ;;
        # Fully-qualified refs are fine as long as they name a branch or
        # a tag in this repository.
        refs/heads/* | refs/tags/*) ;;
        refs/*)
            reason="is a fully-qualified ref that is neither refs/heads/* nor refs/tags/*"
            ;;
        esac
    fi

    if [ -n "$reason" ]; then
        echo "::error title=Rejected dispatch ref::'$ref' $reason." >&2
        echo >&2
        echo "This input takes a branch, a tag or a commit SHA belonging to this" >&2
        echo "repository. It exists to re-run a tree that has not changed (#419)," >&2
        echo "not to run somebody else's tree with this repository's credentials." >&2
        return 1
    fi

    echo "OK — dispatch ref '$ref' is a branch, tag or SHA."
    return 0
}

if [ "${1:-}" = "--validate" ]; then
    if [ "$#" -ne 2 ]; then
        echo "::error title=Bad usage::--validate takes exactly one ref (pass '' for none)." >&2
        exit 2
    fi
    validate_ref "$2"
    exit $?
fi

case "${1:-}" in
--*)
    echo "::error title=Bad usage::unknown option '$1'." >&2
    echo "Usage: $0 --validate <ref> | $0 [workflow-dir]" >&2
    exit 2
    ;;
esac

# ----------------------------------------------------------------- static

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

# Flatten each workflow into facts, one per line, so the graph work below
# is ordinary shell rather than more awk. Emitted per job:
#
#   JOB       <job>                  the job exists
#   NEEDS     <job> <dep>            job depends on dep
#   VALIDATES <job> <line>           a --validate call, at this line
#   CHECKOUT  <job> <line> <expr>    an actions/checkout `ref:`, at this line
#   STEPINPUT <job> <step-id>        a step whose body reads an input
#   JOBINPUT  <job>                  the job reads an input anywhere
#
# Comment lines are skipped throughout. These workflows carry more
# commentary than code, and a check that reads its own explanation as
# behaviour reports the opposite of the truth.
scan_file() {
    awk '
    function endstep(   ) {
        if (in_step && is_checkout && ref_expr != "")
            printf "CHECKOUT\t%s\t%d\t%s\n", job, ref_line, ref_expr
        if (in_step && step_id != "" && step_reads_input)
            printf "STEPINPUT\t%s\t%s\n", job, step_id
        in_step = 0; is_checkout = 0; step_id = ""
        ref_expr = ""; ref_line = 0; step_reads_input = 0
    }
    function endjob(   ) {
        endstep()
        if (job != "" && job_reads_input) printf "JOBINPUT\t%s\n", job
        job = ""; job_reads_input = 0; needs_block = 0
    }

    /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
    in_jobs && /^[^[:space:]#]/ { endjob(); in_jobs = 0; next }
    !in_jobs { next }
    /^[[:space:]]*#/ { next }

    # A job header: exactly two spaces, a name, a colon, nothing else.
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
        endjob()
        job = $1; sub(/:$/, "", job)
        printf "JOB\t%s\n", job
        next
    }
    job == "" { next }

    # `needs:` in all three spellings — scalar, flow sequence, block.
    /^    needs:/ {
        needs_block = 0
        rest = $0
        sub(/^    needs:[[:space:]]*/, "", rest)
        sub(/[[:space:]]*(#.*)?$/, "", rest)
        if (rest == "") { needs_block = 1; next }
        gsub(/[\[\]",]/, " ", rest)
        n = split(rest, parts, /[[:space:]]+/)
        for (i = 1; i <= n; i++)
            if (parts[i] != "") printf "NEEDS\t%s\t%s\n", job, parts[i]
        next
    }
    needs_block && /^      - / {
        dep = $0
        sub(/^      -[[:space:]]*/, "", dep)
        sub(/[[:space:]]*(#.*)?$/, "", dep)
        if (dep != "") printf "NEEDS\t%s\t%s\n", job, dep
        next
    }
    needs_block { needs_block = 0 }

    # A step boundary. `steps:` items sit at six spaces in these files.
    /^      - / { endstep(); in_step = 1 }

    {
        if ($0 ~ /check-dispatch-ref\.sh[^\n]*--validate/)
            printf "VALIDATES\t%s\t%d\n", job, FNR
        if ($0 ~ /inputs\./) {
            job_reads_input = 1
            if (in_step) step_reads_input = 1
        }
        if (in_step) {
            if ($0 ~ /uses:[[:space:]]*actions\/checkout/) is_checkout = 1
            if ($0 ~ /^[[:space:]]*id:[[:space:]]/) {
                step_id = $0
                sub(/^[[:space:]]*id:[[:space:]]*/, "", step_id)
                sub(/[[:space:]]*(#.*)?$/, "", step_id)
            }
            if (ref_expr == "" && $0 ~ /^[[:space:]]*ref:[[:space:]]/) {
                ref_expr = $0
                sub(/^[[:space:]]*ref:[[:space:]]*/, "", ref_expr)
                sub(/[[:space:]]*$/, "", ref_expr)
                ref_line = FNR
            }
        }
    }
    END { endjob() }
    ' "$1"
}

findings=()
examined=0
checkouts=0
gated=0

for f in "${files[@]}"; do
    examined=$((examined + 1))
    base="$(basename "$f")"

    declare -A NEEDS=() VALIDATES=() STEPINPUT=() JOBINPUT=()
    checkout_jobs=()
    checkout_lines=()
    checkout_exprs=()

    while IFS=$'\t' read -r kind a b c; do
        case "$kind" in
        JOB) : ;;
        NEEDS) NEEDS[$a]="${NEEDS[$a]:-} $b" ;;
        # First one wins: the earliest validation in the job is the one
        # that has to precede a checkout for the ordering test below.
        VALIDATES) [ -n "${VALIDATES[$a]:-}" ] || VALIDATES[$a]="$b" ;;
        STEPINPUT) STEPINPUT["$a/$b"]=1 ;;
        JOBINPUT) JOBINPUT[$a]=1 ;;
        CHECKOUT)
            checkout_jobs+=("$a")
            checkout_lines+=("$b")
            checkout_exprs+=("$c")
            ;;
        esac
    done < <(scan_file "$f")

    # Is this checkout's ref influenced by a workflow_dispatch input?
    # Directly, through a step output produced in the same job, or
    # through another job's output.
    influenced() {
        local job="$1" expr="$2" id dep
        case "$expr" in
        *'${{'*) ;;
        *) return 1 ;; # a literal ref cannot carry an input
        esac
        case "$expr" in
        *inputs.*) return 0 ;;
        esac
        if [[ "$expr" =~ steps\.([A-Za-z0-9_-]+)\.outputs\. ]]; then
            id="${BASH_REMATCH[1]}"
            [ -n "${STEPINPUT["$job/$id"]:-}" ] && return 0
        fi
        if [[ "$expr" =~ needs\.([A-Za-z0-9_-]+)\.outputs\. ]]; then
            dep="${BASH_REMATCH[1]}"
            [ -n "${JOBINPUT[$dep]:-}" ] && return 0
        fi
        return 1
    }

    # Walk the needs graph. A job is covered if it validates, or if
    # anything it depends on does.
    gated_by() {
        # Separate statements on purpose: `local a=$1 b=$a` expands
        # every right-hand side before assigning any of them, so `b`
        # would be unbound under `set -u`.
        local seen="" job dep
        local queue="$1"
        while [ -n "$queue" ]; do
            read -r job queue <<<"$queue"
            case " $seen " in *" $job "*) continue ;; esac
            seen="$seen $job"
            [ -n "${VALIDATES[$job]:-}" ] && { echo "$job"; return 0; }
            for dep in ${NEEDS[$job]:-}; do queue="$queue $dep"; done
        done
        return 1
    }

    for i in "${!checkout_jobs[@]}"; do
        job="${checkout_jobs[$i]}"
        line="${checkout_lines[$i]}"
        expr="${checkout_exprs[$i]}"

        influenced "$job" "$expr" || continue
        checkouts=$((checkouts + 1))

        # In-job validation counts only if it runs BEFORE the checkout.
        # A guard below the thing it guards is decoration: the untrusted
        # tree is already on disk by the time it speaks.
        if [ -n "${VALIDATES[$job]:-}" ] && [ "${VALIDATES[$job]}" -lt "$line" ]; then
            gated=$((gated + 1))
            continue
        fi

        if [ -n "${VALIDATES[$job]:-}" ]; then
            findings+=("$base	$job	line $line	validates at line ${VALIDATES[$job]}, after the checkout")
            continue
        fi

        if gated_by "$job" >/dev/null; then
            gated=$((gated + 1))
            continue
        fi

        findings+=("$base	$job	line $line	no --validate call in this job or anything it needs")
    done

    unset NEEDS VALIDATES STEPINPUT JOBINPUT
    unset -f influenced gated_by
done

if [ "${#findings[@]}" -ne 0 ]; then
    echo "::error title=Unvalidated dispatch ref reaches a checkout::the following" \
        "jobs hand a workflow_dispatch-supplied ref to actions/checkout without" \
        "validating it first (#593):" >&2
    for f in "${findings[@]}"; do
        IFS=$'\t' read -r file job where why <<<"$f"
        printf '  %s: job %s — %s (checkout ref at %s)\n' "$file" "$job" "$why" "$where" >&2
    done
    echo >&2
    echo "A dispatch run carries the repository secrets, and a fork's PR head is" >&2
    echo "fetchable from this repository as refs/pull/N/head. Together those let a" >&2
    echo "dispatch put unreviewed code in a credentialed job." >&2
    echo >&2
    echo "Add, in the job or in one it needs, from a TRUSTED checkout (one with no" >&2
    echo "'ref:' of its own) and above the checkout being guarded:" >&2
    echo >&2
    echo "  - name: Reject a dispatch ref that is not ours" >&2
    echo "    run: bash scripts/check-dispatch-ref.sh --validate \"\${{ inputs.ref }}\"" >&2
    exit 1
fi

echo "OK — examined $examined workflow file(s); $checkouts dispatch-influenced checkout(s), all $gated of them validated first."
