#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Four per-workflow invariants that only one lane was missing (#742).
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
#   B. THE FAILURE SUITE RUNS ANYWAY. A step invoking
#      `integration-test-failure` must carry `if: always()`, or a red
#      main suite silently skips failure injection — reporting on half
#      the lane at precisely the moment something is already wrong.
#      This is the pre-#375 semantics integration.yml documents as the
#      bug `fail-fast: false` fixed there.
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
# looking for a concurrency rule will open. Stated here because a gate
# covering three of four findings reads like an oversight otherwise.
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

# Comments stripped. Everything below reads this, never the raw file.
strip() { grep -vE '^[[:space:]]*#' "$1"; }

# Split a workflow into step blocks on stdout, one block per record,
# separated by a sentinel line. A step begins at a `- name:`, `- uses:`
# or `- run:` line and ENDS at the first non-blank line indented no
# further than its own dash.
#
# The dedent rule is load-bearing, not tidiness. Without it a block ran
# on to the end of the file, absorbing the next job's `strategy.matrix`
# — so integration.yml, whose failure suite is a separate matrix JOB
# covered by `fail-fast: false` and correctly has no step-level `if:`,
# was reported as broken by check B. A block that never ends makes every
# per-step question a whole-file question.
steps_of() {
    strip "$1" | awk '
        /^[[:space:]]*-[[:space:]]+(name|uses|run):/ {
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

    # --- A. teardown for a lane that installs a plugin -----------------
    if printf '%s\n' "$body" | grep -F 'docker plugin create' >/dev/null; then
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
        if ! steps_of "$f" | awk -v RS='\x01' '
                { n++ }
                /docker plugin create/ { if (!created) created = n }
                /if:[[:space:]]*always\(\)/ && /docker plugin rm/ {
                    if (created && n > created) found = 1
                }
                END { exit !found }
             '; then
            note "$rel runs 'docker plugin create' with no 'if: always()' teardown step after it."
            echo "      On an ephemeral runner that costs nothing; on a STANDING one the" >&2
            echo "      plugin stays enabled between runs and /var/lib/net-dhcp — bind-" >&2
            echo "      mounted precisely so it survives 'plugin rm' (#440) — accumulates." >&2
            echo "      Copy the block from integration.yml." >&2
        fi
    fi

    # --- B. the failure suite runs even after a red main suite ---------
    # One awk over the block stream, not a shell loop: a step block spans
    # many lines, and `read` would hand back one line at a time — which
    # is a check that asks whether ONE LINE contains both the invocation
    # and the `if:`, i.e. a check that reports every lane as broken. It
    # did, on the first run, including two this branch had just fixed.
    # `make integration-test-failure`, not the bare target name: the
    # matrix `target: integration-test-failure` in integration.yml is a
    # DECLARATION, run from a `make ${{ matrix.target }}` step in a
    # separate job where `fail-fast: false` is what keeps the suites
    # independent. A step-level `if:` there would be wrong, not missing.
    if steps_of "$f" | awk -v RS='\x01' '
            /make[[:space:]]+integration-test-failure/ && !/if:[[:space:]]*always\(\)/ { bad = 1 }
            END { exit !bad }
         '; then
        note "$rel invokes 'integration-test-failure' in a step without 'if: always()'."
        echo "      A red main suite then skips failure injection entirely, so the lane" >&2
        echo "      reports on half of itself exactly when something is already wrong." >&2
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

echo "PASS  ${#WF_FILES[@]} workflow(s): teardown, failure-suite if:always, body-gate 'edited'"
