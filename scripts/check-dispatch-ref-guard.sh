#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert that no `workflow_dispatch` input reaches an `actions/checkout`
# `ref:` without having been constrained first (#593, #738).
#
# WHY THIS EXISTS SEPARATELY FROM THE GUARDS THEMSELVES. A guard is
# worth exactly its coverage. check-dispatch-ref.sh can be perfect and
# still protect nothing the moment a new job — or a new matrix leg, or a
# copy-pasted checkout — takes a dispatch input without depending on it.
#
# WHY IT WAS REWRITTEN. The first version matched the literal string
# `inputs.ref`. That is the input name integration.yml happens to use.
# Two other workflows take a dispatch input into a checkout under a
# different name:
#
#   integration.yml        inputs.ref     ← was seen
#   release.yml            inputs.tag     ← was NOT seen
#   pages.yml              inputs.tag     ← was NOT seen
#
# so the check reported "No job consumes inputs.ref … nothing to guard"
# and exited 0 — the "green having examined nothing" outcome its own
# header condemned. pages.yml's deploy job holds contents: write and
# checks the input out, then runs `pip install` from that tree and
# `mike`, which executes the hooks named in the checked-out mkdocs.yml.
# The shape check that would have rejected a foreign ref lived inside
# the mike step, after both.
#
# So the rule is now keyed on the SINK, not on an input name. Every
# dispatch input is traced to `actions/checkout … ref:` — directly, or
# through a `steps.<id>.outputs.<n>`, a `needs.<job>.outputs.<n>`, or an
# `env:` variable — and each such checkout must be constrained one of
# two ways:
#
#   (a) the job reaches, through `needs:` and transitively, a job that
#       runs check-dispatch-ref.sh. That script answers "is this commit
#       ours?", which needs a clone, so it runs as its own gate job and
#       consumers sit behind it. Transitive counts: a failed gate skips
#       everything downstream, which IS the protection.
#
#   (b) the value is produced by a step running
#       resolve-dispatch-ref.sh, which emits `refs/heads/dev` or
#       `refs/tags/vX.Y.Z[-rcN]` and nothing else. That is for the job
#       that cannot use (a) because its checkout IS the thing being
#       protected — there is no earlier point to gate.
#
# Keying on a helper script rather than on "some validation happened
# nearby" is deliberate. A checker that tried to recognise an anchored
# regex would be guessing, and the first person to write a correct check
# it did not recognise would delete the rule rather than the code.
#
# WHAT IT DOES NOT CLAIM. It reads workflow text. It cannot tell whether
# a guard job's `if:` leaves it skipped — which is exactly why the guard
# job is written to run unconditionally and to pass trivially on a blank
# ref, rather than being conditioned on the event name.
#
# Usage: bash scripts/check-dispatch-ref-guard.sh [workflow-dir]
# Exit:  0 every dispatch input reaching a checkout is constrained
#        1 at least one is not
#        2 the check could not run (missing dir, nothing discovered)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
DIR="${1:-$ROOT/.github/workflows}"

GUARD_SCRIPT="check-dispatch-ref.sh"
RESOLVER_SCRIPT="resolve-dispatch-ref.sh"

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

# One pass per file, emitting typed tab-separated records:
#
#   input <TAB> -   <TAB> <name>    <TAB> -        <TAB> -
#   env   <TAB> job <TAB> <VAR>     <TAB> -        <TAB> -     (job "-" = workflow level)
#   needs <TAB> job <TAB> <csv>     <TAB> -        <TAB> -
#   guard <TAB> job <TAB> -         <TAB> -        <TAB> -
#   step  <TAB> job <TAB> <id>      <TAB> <input?> <TAB> <resolver?>
#   sink  <TAB> job <TAB> <line>    <TAB> <expr>   <TAB> -
#
# Steps are buffered because the facts about a step (its `id:`, whether
# it names an input, whether it runs the resolver, and the `ref:` it
# passes to checkout) are spread over lines that only make sense
# together.
scan_file() {
    awk -v guard="$GUARD_SCRIPT" -v resolver="$RESOLVER_SCRIPT" '
    function reset_step() { sn = 0 }
    function flush_step(   i, id, usesin, res, isco, refexpr, refline, l) {
        if (sn == 0) return
        id = ""; usesin = 0; res = 0; isco = 0; refexpr = ""; refline = 0
        for (i = 1; i <= sn; i++) {
            l = sbuf[i]
            if (l ~ /^[[:space:]]*#/) continue
            # The first line of a step carries the leading "- ", so
            # `- id: ref` and `        id: ref` are the same fact
            # written two ways. Normalising it away is not cosmetic:
            # without it a step whose id sits on the dash line parsed as
            # having no id, the checkout consuming its output traced to
            # nothing, and the file reported "reaches no checkout" —
            # the exact silent miss this rewrite exists to end.
            # (No apostrophes in here: the whole program is inside a
            # single-quoted shell string.)
            sub(/^([[:space:]]*)-[[:space:]]+/, "  ", l)
            if (l ~ /^[[:space:]]*id:[[:space:]]*[A-Za-z0-9_-]+/) {
                id = l
                sub(/^[[:space:]]*id:[[:space:]]*/, "", id)
                sub(/[[:space:]]*$/, "", id)
            }
            if (l ~ /inputs\.[A-Za-z0-9_-]+/) usesin = 1
            if (index(l, resolver) > 0) res = 1
            if (l ~ /uses:[[:space:]]*actions\/checkout/) isco = 1
            # POSIX ERE only: awk has no \S, and a silently
            # non-matching class here is exactly how a checker reports
            # clean having recognised nothing.
            if (l ~ /^[[:space:]]*ref:[[:space:]]*[^[:space:]]/) {
                refexpr = l
                sub(/^[[:space:]]*ref:[[:space:]]*/, "", refexpr)
                sub(/[[:space:]]*$/, "", refexpr)
                refline = sline[i]
            }
        }
        printf "step\t%s\t%s\t%d\t%d\n", job, (id == "" ? "-" : id), usesin, res
        if (isco && refexpr != "")
            printf "sink\t%s\t%d\t%s\t-\n", job, refline, refexpr
        reset_step()
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
    function flush_job() {
        if (job != "") {
            flush_step()
            printf "needs\t%s\t%s\t-\t-\n", job, (needs == "" ? "-" : needs)
            if (isguard) printf "guard\t%s\t-\t-\t-\n", job
        }
        job = ""; needs = ""; isguard = 0; reset_step()
    }

    # --- the `on:` block, for the dispatch input names -----------------
    /^on:[[:space:]]*$/ { in_on = 1; next }
    in_on && /^[^[:space:]#]/ { in_on = 0 }
    in_on {
        if ($0 ~ /^[[:space:]]*#/) next
        if ($0 ~ /^  workflow_dispatch:/) { in_wd = 1; wd_inputs = 0; next }
        if (in_wd && $0 ~ /^  [A-Za-z_]/) { in_wd = 0; wd_inputs = 0 }
        if (in_wd && $0 ~ /^    inputs:[[:space:]]*$/) { wd_inputs = 1; next }
        if (wd_inputs && $0 ~ /^    [A-Za-z_]/) { wd_inputs = 0 }
        if (wd_inputs && $0 ~ /^      [A-Za-z0-9_-]+:[[:space:]]*$/) {
            nm = $0
            sub(/^[[:space:]]*/, "", nm); sub(/:[[:space:]]*$/, "", nm)
            printf "input\t-\t%s\t-\t-\n", nm
        }
        next
    }

    # --- workflow-level env -------------------------------------------
    /^env:[[:space:]]*$/ { in_fenv = 1; next }
    in_fenv && /^[^[:space:]#]/ { in_fenv = 0 }
    in_fenv {
        if ($0 ~ /^  [A-Za-z_][A-Za-z0-9_]*:/ && $0 ~ /inputs\./) {
            v = $0; sub(/^[[:space:]]*/, "", v); sub(/:.*$/, "", v)
            printf "env\t-\t%s\t-\t-\n", v
        }
        next
    }

    /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
    in_jobs && /^[^[:space:]#]/ { flush_job(); in_jobs = 0; next }
    !in_jobs { next }

    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { flush_job(); job = $1; sub(/:$/, "", job); next }

    # Comments never carry behaviour, and these files are heavily
    # commented — this rule is explained in prose that names both
    # scripts and quotes the unsafe shape. Counting that would make the
    # check fire on its own documentation.
    /^[[:space:]]*#/ { in_needs = 0; in_jenv = 0; next }

    {
        # job-level env
        if ($0 ~ /^    env:[[:space:]]*$/) { in_jenv = 1; next }
        if (in_jenv) {
            if ($0 ~ /^      [A-Za-z_][A-Za-z0-9_]*:/) {
                if ($0 ~ /inputs\./ || $0 ~ /needs\.[A-Za-z0-9_-]+\.outputs\./) {
                    v = $0; sub(/^[[:space:]]*/, "", v); sub(/:.*$/, "", v)
                    printf "env\t%s\t%s\t-\t-\n", job, v
                }
                next
            }
            in_jenv = 0
        }
        if ($0 ~ /^    needs:/) {
            rest = $0
            sub(/^    needs:[[:space:]]*/, "", rest)
            if (rest == "") { in_needs = 1 } else { addneed(rest); in_needs = 0 }
            next
        }
        if (in_needs) {
            if ($0 ~ /^      -[[:space:]]*[A-Za-z0-9_-]+[[:space:]]*$/) {
                rest = $0
                sub(/^      -[[:space:]]*/, "", rest)
                addneed(rest)
                next
            }
            in_needs = 0
        }
        if (index($0, guard) > 0 && $0 !~ /^[[:space:]]*#/) isguard = 1
        if ($0 ~ /^      - /) flush_step()
        sn++; sbuf[sn] = $0; sline[sn] = FNR
    }
    END { flush_job() }
    ' "$1"
}

findings=()
examined=0
dispatch_files=0
sinks=0
constrained=0

for f in "${files[@]}"; do
    examined=$((examined + 1))
    base="$(basename "$f")"

    declare -A NEEDS=() GUARDJOB=() STEP_IN=() STEP_RES=() ENVTAINT=()
    declare -A JOB_HAS_RESOLVER=() JOB_USES_INPUT=()
    inputs=()
    sink_rows=()
    jobs=()

    while IFS=$'\t' read -r kind job a b c; do
        [ -n "${kind:-}" ] || continue
        case "$kind" in
            input)  inputs+=("$a") ;;
            env)    ENVTAINT["${job}/${a}"]=1 ;;
            needs)
                jobs+=("$job")
                [ "$a" = "-" ] && a=""
                NEEDS["$job"]="$a"
                ;;
            guard)  GUARDJOB["$job"]=1 ;;
            step)
                [ "$a" != "-" ] && { STEP_IN["${job}/${a}"]="$b"; STEP_RES["${job}/${a}"]="$c"; }
                [ "$b" = "1" ] && JOB_USES_INPUT["$job"]=1
                [ "$c" = "1" ] && JOB_HAS_RESOLVER["$job"]=1
                ;;
            sink)   sink_rows+=("$job	$a	$b") ;;
        esac
    done < <(scan_file "$f")

    if [ "${#inputs[@]}" -eq 0 ]; then
        unset NEEDS GUARDJOB STEP_IN STEP_RES ENVTAINT JOB_HAS_RESOLVER JOB_USES_INPUT
        continue
    fi
    dispatch_files=$((dispatch_files + 1))

    reaches_guard() {
        local start="$1"
        local -A seen=()
        local queue=("$start") cur next deps
        while [ "${#queue[@]}" -gt 0 ]; do
            cur="${queue[0]}"
            queue=("${queue[@]:1}")
            [ -n "${seen[$cur]:-}" ] && continue
            seen["$cur"]=1
            [ -n "${GUARDJOB[$cur]:-}" ] && return 0
            IFS=',' read -r -a deps <<< "${NEEDS[$cur]:-}"
            for next in "${deps[@]}"; do
                [ -n "$next" ] && queue+=("$next")
            done
        done
        return 1
    }

    for row in "${sink_rows[@]}"; do
        IFS=$'\t' read -r job line expr <<<"$row"

        tainted=0
        resolved=0
        how=""

        if [[ "$expr" == *"inputs."* ]]; then
            tainted=1
            how="the input is passed straight to checkout"
        elif [[ "$expr" =~ steps\.([A-Za-z0-9_-]+)\.outputs\. ]]; then
            sid="${BASH_REMATCH[1]}"
            if [ "${STEP_IN[${job}/${sid}]:-0}" = "1" ]; then
                tainted=1
                how="through step '${sid}'"
                [ "${STEP_RES[${job}/${sid}]:-0}" = "1" ] && resolved=1
            fi
        elif [[ "$expr" =~ needs\.([A-Za-z0-9_-]+)\.outputs\. ]]; then
            pjob="${BASH_REMATCH[1]}"
            if [ -n "${JOB_USES_INPUT[$pjob]:-}" ]; then
                tainted=1
                how="through job '${pjob}'"
                [ -n "${JOB_HAS_RESOLVER[$pjob]:-}" ] && resolved=1
            fi
        elif [[ "$expr" =~ env\.([A-Za-z_][A-Za-z0-9_]*) ]]; then
            var="${BASH_REMATCH[1]}"
            if [ -n "${ENVTAINT[${job}/${var}]:-}" ] || [ -n "${ENVTAINT[-/${var}]:-}" ]; then
                tainted=1
                how="through env '${var}'"
            fi
        fi

        [ "$tainted" = "1" ] || continue
        sinks=$((sinks + 1))

        if [ "$resolved" = "1" ] || reaches_guard "$job"; then
            constrained=$((constrained + 1))
        else
            findings+=("$base	$job	$line	$how")
        fi
    done

    unset NEEDS GUARDJOB STEP_IN STEP_RES ENVTAINT JOB_HAS_RESOLVER JOB_USES_INPUT
done

# #738's own lesson, made mechanical: the predecessor exited 0 having
# matched nothing at all. If no workflow in this tree declares a
# dispatch input, either they were all removed or the parser stopped
# working — and only one of those is good news.
if [ "$dispatch_files" -eq 0 ]; then
    echo "::error title=No dispatch inputs found::examined $examined workflow" \
         "file(s) and found no workflow_dispatch input in any of them. This" \
         "check would otherwise pass having examined nothing, which is how it" \
         "missed release.yml and pages.yml for as long as it did (#738)." >&2
    exit 2
fi

if [ "${#findings[@]}" -ne 0 ]; then
    echo "::error title=Unconstrained workflow_dispatch ref::the following" \
         "checkouts take a dispatch input without it being constrained first" \
         "(#593, #738):" >&2
    for entry in "${findings[@]}"; do
        IFS=$'\t' read -r file job line how <<<"$entry"
        printf '  %s: job %s — checkout ref at line %s, %s\n' "$file" "$job" "$line" "$how" >&2
    done
    echo >&2
    echo "A fork's pull-request head is fetchable from this repository, so an" >&2
    echo "unvalidated dispatch ref checks outside code out into a context that" >&2
    echo "HAS the repository secrets — and, on the self-hosted pool, runs it as" >&2
    echo "root with the registry credential in the job." >&2
    echo >&2
    echo "Constrain it one of two ways." >&2
    echo >&2
    echo "  (a) a gate job the consumer needs:" >&2
    echo >&2
    echo "      dispatch-ref:" >&2
    echo "        steps:" >&2
    echo "          - uses: actions/checkout@<pinned-sha> # v7" >&2
    echo "            with:" >&2
    echo "              fetch-depth: 0   # reachability needs the whole graph" >&2
    echo "          - env:" >&2
    echo "              INPUT_REF: \${{ inputs.ref }}" >&2
    echo "            run: bash scripts/$GUARD_SCRIPT \"\$INPUT_REF\"" >&2
    echo >&2
    echo "  (b) resolve it to a qualified ref before the checkout, for a job" >&2
    echo "      whose checkout IS the thing being protected:" >&2
    echo >&2
    echo "          - id: ref" >&2
    echo "            env:" >&2
    echo "              INPUT_TAG: \${{ inputs.tag }}" >&2
    echo "            run: |" >&2
    echo "              echo \"ref=\$(bash scripts/$RESOLVER_SCRIPT \"\$INPUT_TAG\")\" >> \"\$GITHUB_OUTPUT\"" >&2
    echo "          - uses: actions/checkout@<pinned-sha> # v7" >&2
    echo "            with:" >&2
    echo "              ref: \${{ steps.ref.outputs.ref }}" >&2
    exit 1
fi

if [ "$sinks" -eq 0 ]; then
    echo "OK: $dispatch_files of $examined workflow file(s) declare a dispatch" \
         "input, and none of them reaches an actions/checkout ref."
    exit 0
fi

echo "OK: all $constrained checkout(s) fed by a dispatch input across" \
     "$dispatch_files workflow file(s) are constrained by $GUARD_SCRIPT or" \
     "$RESOLVER_SCRIPT."
exit 0
