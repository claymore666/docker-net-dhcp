#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Preflight for the *human* half of a release (docs/release-runbook.md).
#
# The workflow installs everything it needs. This covers the commands the
# maintainer types, and those have no gate behind them: a missing tool
# surfaces as a step that gets skipped, not as a check that goes red.
# That has now cost two releases — v1.3.5 shipped unable to run step 10's
# verification at all, and v1.5.0 was tagged before anyone noticed cosign
# was absent, so the signature went unverified locally until afterwards.
#
# Run it before step 1. Exit 0 means every step in the runbook can
# actually be executed on this box.
set -u

# The cosign major the release is verifiable with. Single source of truth:
# scripts/check-cosign-docs.sh reads this line and asserts every page that
# prints a cosign command names the same major, so a bump here cannot leave
# the user-facing docs behind (#522).
COSIGN_MAJOR=3

fail=0
ok()   { printf 'PASS  %s\n' "$1"; }
bad()  { printf 'FAIL  %s\n' "$1"; fail=1; }
note() { printf 'NOTE  %s\n' "$1"; }

# gh — used by nearly every step: PRs, milestones, run status, release view.
if command -v gh >/dev/null 2>&1; then
    ok "gh present"
else
    bad "gh missing — needed for PRs, milestones, run status and 'gh release view'"
fi

# cosign — step 10 re-verifies checksums.txt the way a consumer would.
# Major 3 specifically: the release emits a Sigstore bundle, which is the
# v3 default, and v2's --output-signature/--output-certificate pair was
# removed in favour of it.
if command -v cosign >/dev/null 2>&1; then
    ver="$(cosign version 2>/dev/null \
           | sed -n 's/.*GitVersion:[[:space:]]*v\{0,1\}\([0-9][0-9.]*\).*/\1/p' \
           | head -1)"
    case "$ver" in
        "$COSIGN_MAJOR".*) ok "cosign v$ver" ;;
        "")  bad "cosign present but its version could not be read — expected a 'GitVersion:' line" ;;
        *)   bad "cosign v$ver — step 10 needs v$COSIGN_MAJOR; older majors are untested against checksums.txt.sigstore.json" ;;
    esac
else
    bad "cosign missing — go install github.com/sigstore/cosign/v$COSIGN_MAJOR/cmd/cosign@latest"
fi

# A signing key, because step 9 tags with -s and a release tag must show
# Verified. Checked here rather than discovered at the tag.
if [ -n "$(git config --get user.signingkey 2>/dev/null || true)" ]; then
    ok "git user.signingkey configured"
else
    bad "no git user.signingkey — step 9 tags with -s and would fail or produce an unsigned tag"
fi

# Optional: only for comparing :latest and :vX.Y.Z digests by hand.
if command -v crane >/dev/null 2>&1; then
    ok "crane present (optional)"
else
    note "crane absent (optional) — go install github.com/google/go-containerregistry/cmd/crane@latest"
fi

echo
if [ "$fail" -ne 0 ]; then
    echo "Release tooling incomplete: fix the FAIL lines before starting the runbook."
    exit 1
fi
echo "Release tooling complete — every runbook step is executable on this box."
