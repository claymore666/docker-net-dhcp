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

# The STATE_DIR mount is compared even though other mounts are not
# (#440). It is what makes tombstones, per-network options and the
# audit ledger survive `docker plugin rm` — the documented upgrade path
# — so a cover manifest without it runs the suite against a plugin with
# DIFFERENT persistence behaviour from the one that ships. That is the
# same shape as #317, where a capability landed in config.json only and
# the release-PR coverage run failed against the unfixed cover manifest.
#
# Also checked against the manifest's own STATE_DIR default: if the two
# drift, the mount covers a path the plugin does not use and durability
# is silently lost while the mount is still there to reassure a reader.
for f in "$MAIN" "$COVER"; do
    want=$(jq -r '.env[] | select(.name=="STATE_DIR") | .value' "$f")
    if [ -z "$want" ] || [ "$want" = "null" ]; then
        echo "FAIL  $f declares no STATE_DIR default"
        fails=1
        continue
    fi
    got=$(jq -r --arg d "$want" '[.mounts[]? | select(.destination == $d)] | length' "$f")
    if [ "$got" != "1" ]; then
        echo "FAIL  $f has no bind mount at STATE_DIR ($want) — state will not survive plugin rm (#440)"
        fails=1
    else
        echo "ok    $f mounts STATE_DIR $want"
    fi
done

if [ "$fails" -ne 0 ]; then
    echo "manifest parity check FAILED — align $COVER with $MAIN"
    exit 1
fi
echo "PASS  plugin manifests agree on privilege-relevant fields"
