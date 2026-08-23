#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert that the floating registry tag (`:latest`) is moved LAST, and
# that a pre-release run still exercises the code that moves it (#736).
#
# WHY THIS EXISTS. `crane tag <version> latest` used to run inside the
# `release` job — before `cosign sign`, before the SBOM, before the
# attestation, and a whole job before `verify-install` proved the plugin
# installs at all. Every failure in that window left `:latest` publicly
# resolving to an image that was unsigned, unattested, or never proven
# installable. The v1.7.0-rc2 run took exactly that path: it failed in
# `release-arm64` after `make push` had already published.
#
# That ordering cannot be defended by a comment, because the failure is
# silent. Nothing goes red when a new step is appended after the retag;
# the release simply publishes a tip that nothing verified. So the
# ordering is asserted here instead.
#
# WHAT IT ASSERTS, over .github/workflows/release.yml:
#
#   1. Every step that runs `crane tag` lives in a job that reaches —
#      through `needs:`, directly or transitively — BOTH verify-install
#      jobs. A retag in the publishing job itself is the pre-#736 shape
#      and fails here.
#   2. No `crane tag` step is conditioned on `prerelease`. A pre-release
#      must run the promotion code, not skip it: skipping is what made
#      the promote path the one part of release.yml an rc could never
#      exercise, so a change to it would first execute on a real tag —
#      on a workflow with no rollback.
#   3. The destination tag is an expansion, never the literal `latest`.
#      Redirecting the rc to `latest-rc` is the whole reason (2) is safe;
#      a hardcoded `latest` re-breaks it while still passing (2).
#   4. The promotion job carries at least one `prerelease`-conditional
#      step. That is the rc-immutability assertion — the check that
#      `:latest` really did not move — and without it (2) and (3) rest
#      on reading an expression correctly rather than on evidence.
#   5. The promotion job runs scripts/assert-newest-release-tag.sh.
#      `crane tag` does not know what the floating tag currently points
#      at, so a dispatch of an OLD tag moves `:latest` backwards — and
#      the runbook offers exactly that dispatch as its recovery step for
#      a failed release. Nothing about (1)-(4) stops it: the retag is
#      correctly ordered, correctly unconditional and correctly aimed;
#      it is just aimed at the wrong release.
#
# (2) and (4) point in opposite directions on purpose: (2) forbids the
# promotion from being skipped on an rc, (4) requires the run to prove
# the rc left `:latest` alone. Either alone is satisfiable by the wrong
# workflow.
#
# WHAT IT DOES NOT CLAIM. It reads workflow text. It cannot prove the
# registry ended up in the right state — that is the job of the digest
# comparison and the immutability assertion inside the workflow itself,
# which read the registry. This check only keeps those wired in.
#
# Usage: bash scripts/check-latest-promotion.sh [workflow-file]
# Exit:  0 promotion is ordered last and rc-exercisable
#        1 at least one assertion failed
#        2 the check could not run (missing file, nothing discovered)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
FILE="${1:-$ROOT/.github/workflows/release.yml}"

# The jobs that must stand between a build and its floating tag.
#
# All four install proofs, not two. The Hub pair was split out of the
# GHCR pair in #776 so that each registry's install lands on a daemon
# that has never created a network sandbox (#588) -- and a proof that is
# not named here is a proof somebody can quietly drop from `needs:` with
# nothing going red, which is the exact failure mode rule (1) exists for.
# `:latest` resolves for Docker Hub users too.
REQUIRED_GATES=(verify-install verify-install-arm64
                verify-install-hub verify-install-hub-arm64)

if [ ! -f "$FILE" ]; then
    echo "::error title=Release workflow missing::$FILE is not a file." \
         "This check would otherwise pass having examined nothing." >&2
    exit 2
fi

# Emit one record per interesting line:
#
#   job <TAB> needs <TAB> kind <TAB> line <TAB> detail
#
# kind is one of:
#   needs      — the job's dependency list (emitted once per job)
#   cranetag   — a step running `crane tag`; detail is the destination
#   crane_if   — a `crane tag` step whose own `if:` names prerelease
#   pre_if     — any step in the job whose `if:` names prerelease
#   recency    — a step running assert-newest-release-tag.sh
#
# Steps are buffered rather than scanned line by line, because the
# question "is THIS retag conditional?" is about the step the line sits
# in, and the `if:` precedes the `run:` by several lines.
scan() {
    awk '
    function flush_step(   i, dest, has_crane, cond) {
        has_crane = 0; cond = 0; dest = ""
        for (i = 1; i <= sn; i++) {
            if (sbuf[i] ~ /assert-newest-release-tag\.sh/)
                printf "%s\t-\trecency\t%d\t-\n", job, sline[i]
            if (sbuf[i] ~ /crane[[:space:]]+tag/) {
                has_crane = 1
                if (dest == "") {
                    dest = sbuf[i]
                    sub(/[[:space:]]+$/, "", dest)
                    sub(/^.*[[:space:]]/, "", dest)
                    gsub(/"/, "", dest)
                }
                if (crane_line == 0) crane_line = sline[i]
            }
            if (sbuf[i] ~ /^[[:space:]]*if:/ && sbuf[i] ~ /prerelease/) cond = 1
        }
        if (cond)
            printf "%s\t-\tpre_if\t%d\t-\n", job, (sn ? sline[1] : 0)
        if (has_crane) {
            printf "%s\t-\tcranetag\t%d\t%s\n", job, crane_line, (dest == "" ? "-" : dest)
            if (cond)
                printf "%s\t-\tcrane_if\t%d\t%s\n", job, crane_line, (dest == "" ? "-" : dest)
        }
        sn = 0; crane_line = 0
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
            printf "%s\t%s\tneeds\t0\t-\n", job, (needs == "" ? "-" : needs)
        }
        job = ""; needs = ""; sn = 0; crane_line = 0
    }
    /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
    in_jobs && /^[^[:space:]#]/ { flush_job(); in_jobs = 0; next }
    !in_jobs { next }
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { flush_job(); job = $1; sub(/:$/, "", job); next }
    # Comments never carry behaviour, and this file is heavily commented
    # — including prose that names `crane tag` and `prerelease` while
    # explaining this very rule. Counting that as a step would make the
    # check fire on its own documentation.
    /^[[:space:]]*#/ { in_needs = 0; next }
    {
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
        # A new step begins. Everything buffered belongs to the previous one.
        if ($0 ~ /^      - /) flush_step()
        sn++; sbuf[sn] = $0; sline[sn] = FNR
    }
    END { flush_job() }
    ' "$1"
}

declare -A NEEDS=() HAS_PRE_IF=() HAS_RECENCY=()
jobs=()
crane_jobs=()
crane_records=()
crane_if_records=()
seen_jobs=0

while IFS=$'\t' read -r job needs kind line detail; do
    [ -n "${job:-}" ] || continue
    case "$kind" in
        needs)
            seen_jobs=$((seen_jobs + 1))
            jobs+=("$job")
            [ "$needs" = "-" ] && needs=""
            NEEDS["$job"]="$needs"
            ;;
        cranetag)
            crane_jobs+=("$job")
            crane_records+=("$job	$line	$detail")
            ;;
        crane_if)
            crane_if_records+=("$job	$line	$detail")
            ;;
        pre_if)
            HAS_PRE_IF["$job"]=1
            ;;
        recency)
            HAS_RECENCY["$job"]=1
            ;;
    esac
done < <(scan "$FILE")

if [ "$seen_jobs" -eq 0 ]; then
    echo "::error title=No jobs parsed::$(basename "$FILE") yielded no jobs." \
         "This check would otherwise pass having examined nothing." >&2
    exit 2
fi

if [ "${#crane_records[@]}" -eq 0 ]; then
    echo "::error title=No floating-tag promotion found::no 'crane tag' step in" \
         "$(basename "$FILE"). Either the promotion moved to a tool this check" \
         "does not know about, or it was dropped — both need a human." >&2
    exit 2
fi

# reaches JOB TARGET — true if TARGET is JOB or is reachable through
# `needs:`. Transitive counts: a failed gate skips everything downstream
# of it, which is exactly the protection being asserted.
reaches() {
    local start="$1" target="$2"
    local -A seen=()
    local queue=("$start") cur next deps
    while [ "${#queue[@]}" -gt 0 ]; do
        cur="${queue[0]}"
        queue=("${queue[@]:1}")
        [ -n "${seen[$cur]:-}" ] && continue
        seen["$cur"]=1
        [ "$cur" = "$target" ] && return 0
        IFS=',' read -r -a deps <<< "${NEEDS[$cur]:-}"
        for next in "${deps[@]}"; do
            [ -n "$next" ] && queue+=("$next")
        done
    done
    return 1
}

findings=()

# (1) ordering — every retag is behind both verify-install jobs.
for entry in "${crane_records[@]}"; do
    IFS=$'\t' read -r job line dest <<<"$entry"
    for gate in "${REQUIRED_GATES[@]}"; do
        if ! reaches "$job" "$gate"; then
            findings+=("$job	$line	promotes the floating tag without depending on '$gate'")
        fi
    done
done

# (2) an rc must run the promotion, not skip it.
for entry in "${crane_if_records[@]}"; do
    IFS=$'\t' read -r job line dest <<<"$entry"
    findings+=("$job	$line	'crane tag' is conditioned on prerelease — an rc would skip the promote path instead of exercising it")
done

# (3) the destination must be computed, so an rc can be aimed elsewhere.
for entry in "${crane_records[@]}"; do
    IFS=$'\t' read -r job line dest <<<"$entry"
    case "$dest" in
        *'${'*) ;;
        latest|latest-*)
            findings+=("$job	$line	promotes to the literal '$dest' — a pre-release cannot be aimed at a harmless tag")
            ;;
    esac
done

# (4) the promotion job must also prove the rc left `:latest` alone.
for job in $(printf '%s\n' "${crane_jobs[@]}" | sort -u); do
    if [ -z "${HAS_PRE_IF[$job]:-}" ]; then
        findings+=("$job	0	promotes the floating tag but has no prerelease-conditional step — nothing asserts a pre-release left ':latest' untouched")
    fi
done

# (5) the promotion job must also refuse to move the tag backwards.
for job in $(printf '%s\n' "${crane_jobs[@]}" | sort -u); do
    if [ -z "${HAS_RECENCY[$job]:-}" ]; then
        findings+=("$job	0	promotes the floating tag without running assert-newest-release-tag.sh — a dispatch of an older tag would move it backwards, which is the runbook's own recovery step")
    fi
done

if [ "${#findings[@]}" -ne 0 ]; then
    echo "::error title=Floating tag is promoted unsafely::the release workflow" \
         "moves ':latest' in a way #736 exists to prevent:" >&2
    for entry in "${findings[@]}"; do
        IFS=$'\t' read -r job line msg <<<"$entry"
        if [ "$line" = "0" ]; then
            printf '  job %s — %s\n' "$job" "$msg" >&2
        else
            printf '  job %s (line %s) — %s\n' "$job" "$line" "$msg" >&2
        fi
    done
    echo >&2
    echo "':latest' is what an unpinned 'docker plugin install' resolves to." >&2
    echo "It must be moved only after the version tag is signed, attested and" >&2
    echo "proven installable, because this workflow cannot roll back: a rebuild" >&2
    echo "re-tars the rootfs non-reproducibly, so a bad promotion can only be" >&2
    echo "overwritten with a new digest, orphaning the old signature." >&2
    echo >&2
    echo "The shape this expects:" >&2
    echo >&2
    echo "  promote-latest:" >&2
    echo "    needs: [release, release-arm64, verify-install, verify-install-arm64," >&2
    echo "            verify-install-hub, verify-install-hub-arm64]" >&2
    echo "    steps:" >&2
    echo "      - run: bash scripts/assert-newest-release-tag.sh \"\${TAG}\"" >&2
    echo "      - run: crane tag \"\${GHCR_NAME}:\${TAG}\" \"\${LATEST}\"" >&2
    echo "      - name: Assert a pre-release did not move :latest" >&2
    echo "        if: needs.release.outputs.prerelease == 'true'" >&2
    echo "        run: ..." >&2
    echo >&2
    echo "with LATEST resolving to 'latest' on a bare tag and 'latest-rc' on an" >&2
    echo "rc, so the dry-run exercises this job instead of skipping it." >&2
    exit 1
fi

echo "OK: ${#crane_records[@]} floating-tag promotion step(s) in" \
     "$(basename "$FILE"), all behind ${REQUIRED_GATES[*]}, all exercised by an" \
     "rc, none able to move the tag backwards."
exit 0
