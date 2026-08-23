#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Assert that no image-publishing job waits on another one (#796).
#
# WHAT #796 CHANGED AND WHY IT NEEDS A GATE. `release-arm64` used to
# carry `needs: release`, so the arm64 build did not start until the
# entire amd64 job had finished, and was SKIPPED outright when amd64
# failed. Its own digest gate therefore never ran, and the first rc of
# a new version could only ever hand back the amd64 block — costing a
# whole extra rc round, every release, each one a separate PR into
# `main` carrying `coverage` on a tree whose Go code had not changed.
#
# The fix was to hoist tag resolution into a `resolve` job and point
# both builds at that instead of at each other. Nothing crosses the
# edge that was removed: the two builds share no artifact, no digest
# and no manifest, only the tag that triggered the run.
#
# THE FAILURE MODE THIS EXISTS FOR IS SILENCE. Re-adding `needs:
# release` breaks nothing. Every job still runs, every check still goes
# green, the release still ships — it just serialises again and the
# arm64 digest block goes back to needing its own rc. The only observer
# would be the next release, noticed by whoever is holding the rc at
# the time, weeks later, under pressure. That is the exact shape this
# project keeps getting bitten by, so it is checked instead of
# remembered.
#
# KEYED ON THE PROPERTY, NOT THE NAMES. A publishing job is one whose
# steps run `make ... push` — that is what makes a job a per-arch build
# rather than what it happens to be called. A future `release-riscv64`
# is covered the day it is added, without this file being edited, and
# renaming `release-arm64` does not silently empty the rule.
#
# WHAT IT DOES NOT CLAIM. It reads the workflow text. It cannot know
# whether the runners exist, whether the jobs really start together, or
# whether a push succeeds. It answers one question — "does any
# publishing job wait, directly or transitively, on another" — which is
# the question that was answered wrong.
#
# Usage: check-build-job-independence.sh [workflow-file]
# Exit:  0 no publishing job depends on another
#        1 one does — the serialised shape is back
#        2 the check cannot render a verdict (unreadable file, or fewer
#          than two publishing jobs, which would make the rule vacuous)
set -euo pipefail

WF="${1:-.github/workflows/release.yml}"

if [ ! -f "$WF" ] || [ ! -r "$WF" ]; then
    echo "::error title=Nothing to inspect::$WF is not a readable file." \
         "The rule would otherwise pass having examined nothing." >&2
    exit 2
fi

# Parse jobs, their `needs:`, and whether they publish. Emitted as
# `job<TAB>publishes<TAB>needs,needs,...` for the checker below.
parsed="$(awk '
    function flush() {
        if (job != "") printf "%s\t%s\t%s\n", job, pub, needs
    }
    /^jobs:[ \t]*$/ { injobs = 1; next }
    !injobs { next }
    # a new job header at exactly two spaces
    /^  [A-Za-z0-9_-]+:[ \t]*$/ {
        flush()
        job = $1; sub(/:$/, "", job); gsub(/^[ \t]+/, "", job)
        pub = 0; needs = ""
        next
    }
    /^    needs:[ \t]*/ {
        line = $0
        sub(/^    needs:[ \t]*/, "", line)
        gsub(/[][ \t]/, "", line)
        needs = line
        next
    }
    # `make ... push` anywhere in the job body, comments excluded
    {
        line = $0
        if (line ~ /^[ \t]*#/) next
        if (line ~ /make[ \t].*[ \t]push([ \t]|$)/) pub = 1
    }
    END { flush() }
' "$WF")"

publishers="$(printf '%s\n' "$parsed" | awk -F'\t' '$2 == 1 { print $1 }')"
npub="$(printf '%s' "$publishers" | grep -c . || true)"

# NON-VACUITY. "No publishing job depends on another" is satisfied for
# free by a file with one publishing job, or none. A rule that passes
# by having an empty domain is the failure this tree has hit before, so
# it refuses a verdict instead.
if [ "$npub" -lt 2 ]; then
    echo "::error title=Nothing to compare::$WF has $npub job(s) running" \
         "\`make ... push\`; the rule needs at least two to mean anything." \
         "Either the per-arch builds were removed or the step that" \
         "identifies them was rewritten -- re-derive this check." >&2
    exit 2
fi

# Transitive closure of `needs:`, then the question.
verdict="$(printf '%s\n' "$parsed" | awk -F'\t' -v pubs="$publishers" '
    BEGIN { n = split(pubs, p, "\n"); for (i = 1; i <= n; i++) if (p[i] != "") ispub[p[i]] = 1 }
    { needs[$1] = $3; seen[$1] = 1 }
    function reaches(from, target,   i, m, parts, r) {
        if (from in visiting) return 0
        visiting[from] = 1
        m = split(needs[from], parts, ",")
        for (i = 1; i <= m; i++) {
            if (parts[i] == "") continue
            if (parts[i] == target) { delete visiting[from]; return 1 }
            if (reaches(parts[i], target)) { delete visiting[from]; return 1 }
        }
        delete visiting[from]
        return 0
    }
    END {
        for (a in ispub) for (b in ispub) {
            if (a == b) continue
            delete visiting
            if (reaches(a, b)) printf "%s\t%s\n", a, b
        }
    }
')"

if [ -n "$verdict" ]; then
    echo "::error title=Publishing jobs are serialised::a job that publishes" \
         "images waits on another one, so the second architecture cannot" \
         "start until the first finishes and is skipped when it fails (#796)." >&2
    printf '%s\n' "$verdict" | while IFS=$'\t' read -r a b; do
        [ -n "$a" ] || continue
        echo "  $a reaches $b through needs:" >&2
    done
    echo >&2
    echo "  Both builds derive everything they need from the tag that" >&2
    echo "  triggered the run. Depend on the job that resolves it, not on" >&2
    echo "  each other. The contract that must stay is downstream:" >&2
    echo "  promote-latest and github-release name BOTH builds, so no" >&2
    echo "  floating tag moves unless both succeeded." >&2
    exit 1
fi

echo "OK: $npub publishing job(s) in $WF, none waiting on another:" \
     "$(printf '%s' "$publishers" | tr '\n' ' ')"
