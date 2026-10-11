#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Per-workflow invariants that only one lane was missing (#742).
#
# Expires-when: a plugin-installing lane gets its teardown, and a body-reading
#   gate its `edited` trigger, from a shared definition it cannot omit (#742, #733).
#
# HALF OF #742 WAS THE SAME FINDING FOUR TIMES: a workflow missing a
# line every sibling already had. None of the four could go red on its
# own, because none of them is a thing that RUNS — an `if:` that is
# absent skips a step silently, a teardown that is absent leaves state
# on a machine nobody inspects, an event type that is absent means no
# run fires at all, and a missing concurrency group is visible only as
# runners you did not know were live. Absence, in every case, rather
# than failure.
#
# So they are asserted here, as text, at the only moment they are
# visible: the edit.
#
#   A. TEARDOWN. A workflow that runs `docker plugin create` must also
#      have an `if: always()` step that removes it. Every lane but one
#      did. The exception was integration-arm64.yml — the ONLY lane on a
#      standing runner, where it is the only lane it actually matters
#      for: everywhere else the machine is discarded after one job.
#
#   B. (retired by #733) The failure suite running after a red main
#      suite is now scripts/integration-suites.sh's own exit logic, which
#      every lane calls through .github/actions/run-suites and which
#      scripts/test-integration-suites.sh pins.
#
#   C. `edited` FOR BODY-READING GATES. check-test-weakening.sh,
#      check-no-ai-attribution.sh, check-issue-ref.sh and
#      check-coverage-floor.sh all read the PR BODY. `pull_request:` with no `types:` defaults to
#      opened/synchronize/reopened — `edited` is NOT in it. So a waiver
#      trailer could be added to pass the gate and then edited out: no
#      re-run fires, the green check stands, and the merged PR carries
#      no waiver. A waiver that can be withdrawn after it is honoured is
#      not a waiver.
#
# The fourth — test.yaml having no concurrency group at all — is
# asserted in check-concurrency-parity.sh instead, which already
# declares which lanes are in that group and is the file a reader
# looking for a concurrency rule will open.
#
# WHY TEXT AND NOT A YAML PARSE. PyYAML is not a dependency of this
# repository and adding one for a gate is a supply-chain cost out of
# proportion to the job. The parse below is line-oriented and depends on
# this tree's consistent step indentation, so it REFUSES rather than
# passing when it finds a workflow with `steps:` and no steps it can
# see — a gate that silently parses nothing is the failure mode every
# other gate here was just audited for (#743).
#
# Comments are stripped before every check, so the prose above — which
# quotes `if: always()` and `docker plugin create` — cannot satisfy or
# trip it.
#
# Usage: check-lane-hygiene.sh [workflow-dir]
# Exit:  0 clean, 1 an invariant is broken, 2 cannot check.
set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

WF_DIR="${1:-.github/workflows}"

[ -d "$WF_DIR" ] || {
    echo "::error title=Nothing to inspect::no workflow directory '$WF_DIR'." >&2
    exit 2
}

shopt -s nullglob
WF_FILES=("$WF_DIR"/*.yml "$WF_DIR"/*.yaml)
shopt -u nullglob

if [ "${#WF_FILES[@]}" -eq 0 ]; then
    echo "::error title=Nothing to inspect::no *.yml or *.yaml files in $WF_DIR." \
         "This gate would otherwise report a clean pass having read nothing." >&2
    exit 2
fi

fail=0
note() { echo "FAIL  $*" >&2; fail=1; }

# The gates that read the PR body. Named rather than discovered: the
# property is "this workflow runs something that reads the body", and a
# pattern broad enough to discover that would also match the many gates
# that read only a commit range.
BODY_GATES='check-test-weakening\.sh|check-no-ai-attribution\.sh|check-issue-ref\.sh|check-coverage-floor\.sh'

# shellcheck source=scripts/workflow-shell-lines.sh
. "$(cd "$(dirname "$0")" && pwd)/workflow-shell-lines.sh"

# A step's `uses: ./x` runs x/action.yml's commands inside that step, and
# GitHub resolves ./x against the checkout, two levels above the
# workflow directory. The lanes install and tear down through two such
# composites (#746), so their commands count as the step's own; one that
# cannot be read is a refusal, never a step with nothing in it. That
# includes one that calls another local action: its commands would count
# as none, so a nested call is refused rather than followed.
LOCAL_USES='^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*["'"'"']?\./'
ROOT="$(cd "$WF_DIR/../.." && pwd)"
action_file() {
    local a
    for a in "$ROOT/$1/action.yml" "$ROOT/$1/action.yaml"; do
        [ -f "$a" ] && { printf '%s\n' "$a"; return 0; }
    done
    return 1
}

# Comments stripped. Everything below reads this, never the raw file.
strip() { grep -vE '^[[:space:]]*#' "$1"; }

# Split a workflow into step blocks on stdout, one block per record,
# separated by a sentinel line. A step begins at any `- key:` line, so
# one opening on `- if:` or `- id:` is still read (#883), and ENDS at the
# first non-blank line indented no further than its own dash.
#
# The dedent rule is load-bearing, not tidiness. Without it a block ran
# on to the end of the file, absorbing the next job's steps, so one job's
# `if: always()` answered for another's. A block that never ends makes
# every per-step question a whole-file question.
steps_of() {
    strip "$1" | awk '
        /^[[:space:]]*-[[:space:]]+[A-Za-z_-]+:/ {
            if (inblk) print "\x01"
            dash = index($0, "-") - 1
            inblk = 1
            print
            next
        }
        inblk && /^[[:space:]]*$/ { print; next }
        inblk {
            lead = match($0, /[^ ]/) - 1
            if (lead <= dash) { print "\x01"; inblk = 0; next }
            print
            next
        }
        END { if (inblk) print "\x01" }
    '
}

# One row per step: <n> <if: always() key> <create> <rm>
# <local action path, or ->.
# The if: counts only as the step's own key, and each command only as a
# command of its run: shell; in an echo, a name: or a run: string they
# guard and run nothing (#883).
step_facts() {
    local n ifa cand raw c cr td
    steps_of "$1" | awk -v RS='\x01' '
        {
            k = split($0, L, "\n"); dash = -1; ifa = 0; raw = ""; u = "-"
            for (i = 1; i <= k; i++) {
                if (dash < 0 && L[i] ~ /^[[:space:]]*-[[:space:]]/) {
                    dash = index(L[i], "-") - 1
                    pre = sprintf("%" dash "s", "")
                }
                if (dash < 0) continue
                if (L[i] ~ ("^" pre "(-|[ ]) if:[[:space:]]*always\\(\\)")) ifa = 1
                if (L[i] ~ ("^" pre "(-|[ ]) uses:[[:space:]]*[\"\047]?\\./")) {
                    u = L[i]; sub(/^[^:]*:[[:space:]]*[\"\047]?\.\//, "", u)
                    sub(/[\"\047]?[[:space:]]*$/, "", u)
                }
                l = L[i]; gsub(/\t/, " ", l); raw = raw (raw == "" ? "" : "\037") l
            }
            n++
            if (dash < 0) next
            cand = index($0, "docker plugin") ? 1 : 0
            printf "%d\t%d\t%d\t%s\t%s\n", n, ifa, cand, (cand ? raw : "-"), u
        }' |
    while IFS=$'\t' read -r n ifa cand raw use; do
        cr=0; td=0
        if [ "$cand" = 1 ] || [ "$use" != - ]; then
            while IFS= read -r c; do
                case "$c" in
                    "docker plugin create"|"docker plugin create "*) cr=1 ;;
                    "docker plugin rm"|"docker plugin rm "*) td=1 ;;
                esac
            done < <(
                if [ "$cand" = 1 ]; then
                    printf '%s\n' "${raw//$'\037'/$'\n'}" | workflow_shell_lines --raw - | shell_simple_commands
                fi
                if [ "$use" != - ]; then
                    workflow_shell_lines --raw "$(action_file "$use")" | shell_simple_commands
                fi)
        fi
        printf '%s\t%s\t%s\t%s\t%s\n' "$n" "$ifa" "$cr" "$td" "$use"
    done
}

for f in "${WF_FILES[@]}"; do
    rel="${f#./}"
    body="$(strip "$f")"

    # --- the parse has to have worked ---------------------------------
    if printf '%s\n' "$body" | grep -E '^[[:space:]]*steps:' >/dev/null; then
        if ! steps_of "$f" | grep -E '^[[:space:]]*-[[:space:]]+(name|uses|run):' >/dev/null; then
            echo "::error title=Nothing to inspect::$rel declares 'steps:' but no step" \
                 "matched this gate's parse. The workflow's indentation is not what this" \
                 "gate assumes, so its verdict here would be a clean pass over nothing." >&2
            exit 2
        fi
    fi

    while IFS= read -r use; do
        af=$(action_file "$use") || {
            echo "::error title=Nothing to inspect::$rel uses ./$use, and $ROOT/$use" \
                 "holds no action.yml. Its commands would count as none." >&2
            exit 2
        }
        if grep -E "$LOCAL_USES" "$af" >/dev/null; then
            echo "::error title=Nothing to inspect::$rel uses ./$use, which itself" \
                 "calls a local action. Its commands would count as none." >&2
            exit 2
        fi
    done < <(step_facts "$f" | cut -f5 | grep -v '^-$')

    # --- A. teardown for a lane that installs a plugin -----------------
    if printf '%s\n' "$body" | grep -F 'docker plugin create' >/dev/null ||
       step_facts "$f" | awk -F '\t' '$3 == 1 { c = 1 } END { exit !c }'; then
        # A TEARDOWN COMES AFTER THE INSTALL, and the ordering is what
        # makes this checkable at all. Every lane here opens its build
        # step with a `docker plugin rm -f` — a PRE-install cleanup of
        # whatever the previous run left. So "somewhere in this file
        # there is an `if: always()` and a `plugin rm`" is satisfied by
        # a build step that happens to carry an `if:`, which is the
        # shape the self-test pins. Requiring a LATER block is the
        # difference between the two, and it is the actual property:
        # integration-arm64.yml had the pre-install rm at :151 and
        # nothing after the suite.
        if ! step_facts "$f" | awk -F '\t' '
                $3 == 1 { if (!created) created = $1 }
                $2 == 1 && $4 == 1 { if (created && $1 > created) found = 1 }
                END { exit !found }
             '; then
            note "$rel runs 'docker plugin create' with no 'if: always()' teardown step after it."
            echo "      On an ephemeral runner that costs nothing; on a STANDING one the" >&2
            echo "      plugin stays enabled between runs and /var/lib/net-dhcp — bind-" >&2
            echo "      mounted precisely so it survives 'plugin rm' (#440) — accumulates." >&2
            echo "      Copy the block from integration.yml." >&2
        fi
    fi

    # --- C. `edited` where a gate reads the PR body --------------------
    if printf '%s\n' "$body" | grep -E "$BODY_GATES" >/dev/null; then
        if printf '%s\n' "$body" | grep -E '^[[:space:]]*pull_request:' >/dev/null; then
            if ! printf '%s\n' "$body" | grep -E '^[[:space:]]*types:.*\bedited\b' >/dev/null; then
                note "$rel runs a gate that reads the PR BODY but does not list 'edited'."
                echo "      pull_request defaults to opened/synchronize/reopened, so editing" >&2
                echo "      the body fires no run: a waiver trailer can be added to pass the" >&2
                echo "      gate and removed afterwards with the green check still standing." >&2
                echo "      Add: types: [opened, synchronize, reopened, edited]" >&2
            fi
        fi
    fi

done

if [ "$fail" -ne 0 ]; then
    echo >&2
    echo "::error title=Lane hygiene::a workflow is missing an invariant its siblings" \
         "carry. None of these can go red on its own — that is why they are asserted here." >&2
    exit 1
fi

echo "PASS  ${#WF_FILES[@]} workflow(s): teardown, body-gate 'edited'"
