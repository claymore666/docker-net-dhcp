#!/usr/bin/env bash
# Guard against drift between the production plugin manifest
# (config.json) and the coverage-instrumented one (config-cover.json).
# The two must agree on every privilege-relevant field — the cover
# plugin exists to run the SAME suite with instrumentation, so a
# privilege mismatch makes coverage runs test a different plugin than
# the one that ships. Learned the hard way: #317's CAP_SYS_PTRACE fix
# landed in config.json only, and the release-PR coverage run failed
# the new non-root test against the unfixed cover manifest.
#
# Cover-specific additions (env like GOCOVERDIR, extra mounts) are
# expected and NOT compared. Usage:
#   scripts/check-manifest-parity.sh [config.json] [config-cover.json]
set -euo pipefail

MAIN="${1:-config.json}"
COVER="${2:-config-cover.json}"

fails=0
for field in '.linux.capabilities' '.network.type' '.pidhost' '.interface.types'; do
    a=$(jq -cS "$field" "$MAIN")
    b=$(jq -cS "$field" "$COVER")
    if [ "$a" != "$b" ]; then
        echo "FAIL  $field differs: $MAIN=$a $COVER=$b"
        fails=1
    else
        echo "ok    $field $a"
    fi
done

if [ "$fails" -ne 0 ]; then
    echo "manifest parity check FAILED — align $COVER with $MAIN"
    exit 1
fi
echo "PASS  plugin manifests agree on privilege-relevant fields"
