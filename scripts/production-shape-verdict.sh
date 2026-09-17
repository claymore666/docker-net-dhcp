#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Does a cell log say the production shape worked on the production
# engine (#1014)?
#
# WHY THIS IS NOT `grep result=pass`. The release lane runs one engine
# cell before it publishes, and the question it has to answer is not
# "did the script exit 0" but "is there a verdict, is it about the row
# we asked for, and does it say pass". Three of those go wrong quietly:
#
#   - A cell can die before it prints a row at all. The engine matrix
#     treats a missing row as red for that reason
#     (.github/workflows/engine-matrix.yml records a no-verdict row);
#     here the same absence must refuse the release rather than sail
#     through a grep that found nothing and was never asked to care.
#   - `unavailable` is not a pass and it is not a failure either: it
#     says the rig never got far enough to ask about the plugin. At a
#     release that is the absence of evidence at the one moment
#     evidence is needed, so it refuses. Nothing about it says the
#     plugin is broken, and the message says so.
#   - A log from a different row satisfies "result=pass" perfectly. The
#     expected tag is an argument so that a stale or mismatched log is a
#     refusal instead of a pass about another engine.
#
# Usage: production-shape-verdict.sh <cell-log> <expected-row-tag>
# Exit:  0 the production shape passed on that row
#        1 it did not, or the log does not say
#        2 cannot check
set -uo pipefail

LOG="${1:-}"
WANT_TAG="${2:-}"

if [ -z "$LOG" ] || [ -z "$WANT_TAG" ]; then
    echo "usage: $0 <cell-log> <expected-row-tag>" >&2
    exit 2
fi
if [ ! -r "$LOG" ]; then
    echo "FAIL  $LOG is not readable. No recorded row for this engine reached" >&2
    echo "  this gate, which is an absence of evidence and never a pass." >&2
    exit 2
fi

mapfile -t lines < <(grep '^ENGINE_MATRIX_ROW ' "$LOG")

if [ "${#lines[@]}" -eq 0 ]; then
    echo "FAIL  $LOG carries no ENGINE_MATRIX_ROW line." >&2
    echo "  The cell produced no verdict, which is not the same as a pass." >&2
    exit 1
fi
if [ "${#lines[@]}" -gt 1 ]; then
    echo "FAIL  $LOG carries ${#lines[@]} ENGINE_MATRIX_ROW lines." >&2
    echo "  One cell, one verdict: more than one leaves it open which run this is." >&2
    exit 1
fi

line="${lines[0]}"
tag=""; result=""; step=""
for field in $line; do
    case "$field" in
        tag=*)    tag="${field#tag=}" ;;
        result=*) result="${field#result=}" ;;
        step=*)   step="${field#step=}" ;;
    esac
done

if [ "$tag" != "$WANT_TAG" ]; then
    echo "FAIL  the verdict is about engine row '$tag', not the production row '$WANT_TAG'." >&2
    exit 1
fi

case "$result" in
    pass)
        echo "PASS  the production shape ran on engine row $tag."
        exit 0
        ;;
    unavailable)
        echo "FAIL  engine row $tag was not measured at all (stopped at step '$step')." >&2
        echo "  Nothing here says the plugin is broken and nothing here says it works." >&2
        echo "  A release needs the second one." >&2
        exit 1
        ;;
    fail)
        echo "FAIL  the production shape failed on engine row $tag at step '$step'." >&2
        exit 1
        ;;
    *)
        echo "FAIL  engine row $tag reached no verdict (result='$result')." >&2
        exit 1
        ;;
esac
