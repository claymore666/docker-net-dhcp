#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert that every workflow job consuming `inputs.ref` is gated behind
# the job that validates it (#593).
#
# WHY THIS EXISTS SEPARATELY FROM THE GUARD ITSELF. A guard is worth
# exactly its coverage. check-dispatch-ref.sh can be perfect and still
# protect nothing the moment a new job — or a new matrix leg, or a
# copy-pasted checkout — takes `inputs.ref` without depending on it.
# That is not hypothetical: the rule this implements already existed in
# integration.yml as a SECURITY comment, and a second route
# (`workflow_dispatch` rather than `pull_request_target`) walked around
# it because the note named one route and not the other.
#
# So the wiring is checked, not remembered. This is the "and, since a
# guard is only worth what its coverage is" half of #593.
#
# WHAT IT ASSERTS. For each job whose body references `inputs.ref` or
# `github.event.inputs.ref`, the job must reach — through `needs:`,
# directly or transitively — a job that runs check-dispatch-ref.sh.
# Transitive counts because that is how the dependency actually works:
# if the guard fails, every job downstream of it is skipped and the
# untrusted ref is never checked out.
#
# A job that runs the guard script is itself considered guarded: it
# necessarily mentions the ref in order to hand it over.
#
# WHAT IT DOES NOT CLAIM. It reads the workflow text. It cannot tell
# whether the guard job's `if:` condition leaves it skipped, and a
# SKIPPED guard job does not protect anything — which is exactly why
# the guard job is written to run unconditionally and to pass trivially
# on a blank ref, rather than being conditioned on the event name.
#
# Usage: bash scripts/check-dispatch-ref-guard.sh [workflow-dir]
# Exit:  0 every consumer is behind the guard
#        1 at least one is not
#        2 the check could not run (missing dir, nothing discovered)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
DIR="${1:-$ROOT/.github/workflows}"

GUARD_SCRIPT="check-dispatch-ref.sh"

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

# Split a workflow into jobs by indentation and report, per job:
#
#   job <TAB> needs(comma-separated) <TAB> uses_ref <TAB> is_guard <TAB> line
#
# `needs:` appears in three shapes in this repo's workflows and all
# three are parsed: `needs: gate`, `needs: [gate, suite]`, and the
# block form with `- gate` on following lines.
scan_file() {
    awk -v guard="$GUARD_SCRIPT" '
    # "-" rather than an empty needs field, deliberately: tab is an IFS
    # whitespace character, so bash `read` collapses a run of them and
    # an empty field would shift every later field left by one. A job
    # with no `needs:` — which is exactly the shape of the unguarded
    # first job — then read as if it had no consumers, and the check
    # passed over the very job it exists to catch.
    function flush(   ) {
        if (job != "")
            printf "%s\t%s\t%d\t%d\t%d\n", job, (needs == "" ? "-" : needs),
                   uses_ref, is_guard, ref_line
        job = ""; needs = ""; uses_ref = 0; is_guard = 0; ref_line = 0
    }
    function addneed(s,   parts, i, t) {
        gsub(/[\[\]",]/, " ", s)
        n = split(s, parts, /[[:space:]]+/)
        for (i = 1; i <= n; i++) {
            t = parts[i]
            if (t == "") continue
            needs = (needs == "" ? t : needs "," t)
        }
    }
    /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
    in_jobs && /^[^[:space:]#]/ { flush(); in_jobs = 0; next }
    # A reference OUTSIDE any job taints the whole file. A workflow-level
    # `env:` (or `defaults:`) can carry the input into every job without
    # a single job naming it:
    #
    #     env:
    #       TARGET_REF: ${{ inputs.ref }}
    #     jobs:
    #       suite:
    #         steps:
    #           - uses: actions/checkout@...
    #             with:
    #               ref: ${{ env.TARGET_REF }}
    #
    # Scanning job bodies alone reports "nothing to guard" on that file
    # and exits 0 while an unguarded job checks the untrusted ref out.
    # A check that reports green having examined nothing is worse than
    # no check, so the laundering path is treated as consumption by
    # every job in the file.
    !in_jobs && /inputs\.ref/ && $0 !~ /^[[:space:]]*#/ {
        file_level = 1
        if (file_line == 0) file_line = FNR
    }
    in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
        flush()
        job = $1; sub(/:$/, "", job)
        next
    }
    !in_jobs { next }
    # Comments never carry behaviour, and these files are heavily
    # commented — this very check is explained in prose that names
    # inputs.ref, and counting that as a consumer would make it fire on
    # its own documentation.
    /^[[:space:]]*#/ { in_needs = 0; next }
    {
        if ($0 ~ /^    needs:/) {
            rest = $0
            sub(/^    needs:[[:space:]]*/, "", rest)
            if (rest == "") { in_needs = 1 } else { addneed(rest); in_needs = 0 }
            next
        }
        if (in_needs) {
            if ($0 ~ /^      -[[:space:]]*[A-Za-z0-9_-]+/) {
                rest = $0
                sub(/^      -[[:space:]]*/, "", rest)
                addneed(rest)
                next
            }
            in_needs = 0
        }
        if ($0 ~ /inputs\.ref/) {
            uses_ref = 1
            if (ref_line == 0) ref_line = FNR
        }
        if (index($0, guard) > 0) is_guard = 1
    }
    END {
        flush()
        if (file_level) printf "!file\t-\t0\t0\t%d\n", file_line
    }
    ' "$1"
}

findings=()
examined=0
consumers=0

for f in "${files[@]}"; do
    examined=$((examined + 1))
    declare -A NEEDS=() USES=() GUARDJOB=() LINE=()
    jobs=()
    file_level=0
    file_line=0
    while IFS=$'\t' read -r job needs uses guard line; do
        [ -n "${job:-}" ] || continue
        if [ "$job" = "!file" ]; then
            file_level=1
            file_line="$line"
            continue
        fi
        jobs+=("$job")
        [ "$needs" = "-" ] && needs=""
        NEEDS["$job"]="$needs"
        USES["$job"]="$uses"
        GUARDJOB["$job"]="$guard"
        LINE["$job"]="$line"
    done < <(scan_file "$f")

    # Workflow-level reference: every job in the file can reach the
    # input without naming it, so every job must be behind the guard.
    if [ "$file_level" = "1" ]; then
        for job in "${jobs[@]}"; do
            USES["$job"]=1
            [ "${LINE[$job]}" = "0" ] && LINE["$job"]="$file_line (workflow-level)"
        done
    fi

    # reaches_guard JOB — true if JOB is the guard, or any job it needs
    # (transitively) is. Visited-set guards against a cycle in `needs:`,
    # which GitHub itself rejects but this parser must survive.
    reaches_guard() {
        local start="$1"
        local -A seen=()
        local queue=("$start") cur next
        while [ "${#queue[@]}" -gt 0 ]; do
            cur="${queue[0]}"
            queue=("${queue[@]:1}")
            [ -n "${seen[$cur]:-}" ] && continue
            seen["$cur"]=1
            [ "${GUARDJOB[$cur]:-0}" = "1" ] && return 0
            IFS=',' read -r -a deps <<< "${NEEDS[$cur]:-}"
            for next in "${deps[@]}"; do
                [ -n "$next" ] && queue+=("$next")
            done
        done
        return 1
    }

    for job in "${jobs[@]}"; do
        [ "${USES[$job]}" = "1" ] || continue
        consumers=$((consumers + 1))
        if ! reaches_guard "$job"; then
            findings+=("$(basename "$f")	$job	${LINE[$job]}")
        fi
    done
    unset NEEDS USES GUARDJOB LINE
done

if [ "${#findings[@]}" -ne 0 ]; then
    echo "::error title=Unguarded workflow_dispatch ref::the following jobs" \
         "consume 'inputs.ref' without depending on a job that validates it" \
         "(#593):" >&2
    for entry in "${findings[@]}"; do
        IFS=$'\t' read -r file job line <<<"$entry"
        printf '  %s: job %s — first use at line %s\n' "$file" "$job" "$line" >&2
    done
    echo >&2
    echo "A fork's pull-request head is fetchable from this repository, so an" >&2
    echo "unvalidated dispatch ref checks outside code out into a context that" >&2
    echo "HAS the repository secrets — and, on the self-hosted pool, runs it as" >&2
    echo "root with the registry credential in the job." >&2
    echo >&2
    echo "Add a guard job:" >&2
    echo >&2
    echo "  dispatch-ref:" >&2
    echo "    runs-on: ubuntu-latest" >&2
    echo "    steps:" >&2
    echo "      - uses: actions/checkout@<pinned-sha> # v7" >&2
    echo "        with:" >&2
    echo "          fetch-depth: 0   # reachability needs the whole graph" >&2
    echo "      - run: bash scripts/$GUARD_SCRIPT \"\${{ inputs.ref }}\"" >&2
    echo >&2
    echo "and put it in the consumer's 'needs:', directly or transitively." >&2
    exit 1
fi

if [ "$consumers" -eq 0 ]; then
    echo "No job consumes inputs.ref in $examined workflow file(s) — nothing to guard."
    exit 0
fi

echo "OK: all $consumers job(s) consuming inputs.ref across $examined workflow" \
     "file(s) are behind $GUARD_SCRIPT."
exit 0
