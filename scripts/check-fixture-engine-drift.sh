#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Fail when the captured libnetwork request fixtures were recorded on a
# different Docker Engine minor than the daemon this host runs (#644).
#
# WHY THIS EXISTS
#
# pkg/plugin/testdata/requests holds real request bodies, and the unit
# tests replay them instead of hand-building structs. That is only
# better than the hand-built structs it replaced for as long as the
# recording still describes the engine we run against. A fixture nobody
# refreshes is a fossilised assumption that agrees with itself forever —
# the same "asserts our model, not the daemon" problem, except now there
# is a green test sitting next to it, which is worse, because it looks
# like evidence.
#
# So the recording has to be able to go red on its own. This is that.
#
# WHAT COUNTS AS DRIFT
#
# major.minor. libnetwork's remote-driver payloads are stable across
# patch releases and across distro suffixes (26.1.5 vs 26.1.5+dfsg1);
# they are exactly what can move on a minor bump — that is how #218 and
# #125 come to be waiting on an engine release at all. Patch and suffix
# differences pass; a minor difference is a verdict somebody has to make
# deliberately.
#
# WHERE IT RUNS
#
# On a host whose daemon is the one the integration suite uses. That is
# the self-hosted suite job, and only there — a hosted runner's engine,
# or a developer laptop's, is unrelated to these fixtures and comparing
# against it would report drift that means nothing. That is also why
# this is not in scripts/local-lane.sh: it would go red on any machine
# running a newer engine than the integration lane, which is not a
# defect in the fixtures. Its self-test runs everywhere, so the logic is
# still covered by the fast lane; only the verdict needs the runner.
#
# With no daemon answering it reports NOT INSPECTED rather than a pass —
# an absent check is not a green check.
set -euo pipefail

FIXTURE_ROOT="${FIXTURE_ROOT:-pkg/plugin/testdata/requests}"

# Seam for the self-test, and an escape hatch for anyone reproducing a
# verdict without that engine installed.
ENGINE_VERSION="${FIXTURE_ENGINE_VERSION:-}"

fail=0

# 26.1.5+dfsg1 -> 26.1 ; 28.0.0-beta.1 -> 28.0
minor_of() {
    printf '%s' "$1" | sed -n 's/^[^0-9]*\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1.\2/p'
}

engine_of() {
    sed -n 's/.*"engine"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1" | head -1
}

if [ ! -d "$FIXTURE_ROOT" ]; then
    echo "FAIL: $FIXTURE_ROOT does not exist."
    echo "  The fixture suite cannot be checked for drift because there are no"
    echo "  fixtures. Regenerate with \`make capture-fixtures\` (see"
    echo "  docs/internals.md#request-fixtures)."
    exit 2
fi

mapfile -t MANIFESTS < <(find "$FIXTURE_ROOT" -mindepth 2 -maxdepth 2 -name manifest.json | sort)
if [ "${#MANIFESTS[@]}" -eq 0 ]; then
    echo "FAIL: no flow manifests under $FIXTURE_ROOT."
    echo "  Nothing to compare, which must not read as \"no drift\". Regenerate"
    echo "  with \`make capture-fixtures\` (see docs/internals.md#request-fixtures)."
    exit 2
fi

if [ -z "$ENGINE_VERSION" ]; then
    ENGINE_VERSION="$(docker version --format '{{.Server.Version}}' 2>/dev/null || true)"
fi
if [ -z "$ENGINE_VERSION" ]; then
    echo "NOT INSPECTED — no Docker daemon answered, so the engine these fixtures"
    echo "  were recorded on could not be compared with anything. This gate has a"
    echo "  verdict only where the integration suite runs. Set FIXTURE_ENGINE_VERSION"
    echo "  to check a specific engine by hand:"
    for m in "${MANIFESTS[@]}"; do
        printf '    %-52s recorded on %s\n' "$m" "$(engine_of "$m")"
    done
    exit 0
fi

RUNNING_MINOR="$(minor_of "$ENGINE_VERSION")"
if [ -z "$RUNNING_MINOR" ]; then
    echo "FAIL: could not read a major.minor out of the running engine version '$ENGINE_VERSION'."
    exit 2
fi

echo "running engine: $ENGINE_VERSION (minor $RUNNING_MINOR)"

for m in "${MANIFESTS[@]}"; do
    flow="$(basename "$(dirname "$m")")"
    recorded="$(engine_of "$m")"

    if [ -z "$recorded" ]; then
        echo "FAIL: $m has no \"engine\" field."
        echo "  A capture nobody can attribute to an engine cannot be checked for"
        echo "  staleness at all, which is the failure this gate exists to prevent."
        fail=1
        continue
    fi

    recorded_minor="$(minor_of "$recorded")"
    if [ -z "$recorded_minor" ]; then
        echo "FAIL: $m records engine '$recorded', which has no major.minor to compare."
        fail=1
        continue
    fi

    if [ "$recorded_minor" != "$RUNNING_MINOR" ]; then
        echo "FAIL: flow '$flow' was captured on engine $recorded (minor $recorded_minor),"
        echo "  but this host runs $ENGINE_VERSION (minor $RUNNING_MINOR)."
        echo "  The replayed request bodies no longer describe the daemon the suite"
        echo "  talks to, so the fixture tests are asserting against a recording of a"
        echo "  different engine. Re-record:"
        echo "      sudo make capture-fixtures CAPTURE_COMMIT=\$(git rev-parse --short HEAD)"
        echo "  and review the diff — a changed payload on a minor bump is exactly what"
        echo "  #218 and #125 are waiting to see (see docs/internals.md#request-fixtures)."
        echo "  If you are running this outside the integration lane: the comparison"
        echo "  target is that lane's daemon, not this host's, so this is not a verdict"
        echo "  on your checkout."
        fail=1
        continue
    fi

    printf 'ok: %-24s captured on %s\n' "$flow" "$recorded"
done

if [ "$fail" -ne 0 ]; then
    exit 1
fi

echo "PASS: ${#MANIFESTS[@]} flow(s) recorded on engine minor $RUNNING_MINOR"
