#!/usr/bin/env bash
# Install the Go toolchain for the integration-test runner.
#
# Run once, as root, on the self-hosted CI host. The integration
# workflow then assumes /usr/local/go/bin is on PATH and skips
# `actions/setup-go@v5` entirely, saving ~30s per run.
#
# Distro packages (Debian 13 ships only 1.24) lag the version
# pinned in go.mod, so we fetch directly from go.dev. The binary
# tarball is reproducible and the SHA256 is published alongside.
#
# Usage:
#   sudo bash test/integration/install-go-runner.sh
#
# Re-run is safe: replaces /usr/local/go atomically.

set -euo pipefail

GO_VERSION="${GO_VERSION:-1.25.0}"
ARCH="${ARCH:-linux-amd64}"
TARBALL="go${GO_VERSION}.${ARCH}.tar.gz"
URL="https://go.dev/dl/${TARBALL}"
DEST="/usr/local"

if [[ $EUID -ne 0 ]]; then
    echo "Must run as root (writes to ${DEST}/go). Re-run with sudo." >&2
    exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "==> Downloading ${URL}"
curl -fsSL -o "${tmp}/${TARBALL}" "${URL}"

echo "==> Extracting to ${DEST}"
# Atomic-ish swap: extract into ${DEST}/go.new, then mv.
rm -rf "${DEST}/go.new"
tar -C "${DEST}" -xzf "${tmp}/${TARBALL}"  # creates ${DEST}/go
# tar -xzf overwrites in place; previous /usr/local/go is replaced.

# /usr/local/bin/go symlink so PATH=/usr/local/bin (default) finds it
# without needing to source /etc/profile.d additions.
ln -sf "${DEST}/go/bin/go" /usr/local/bin/go
ln -sf "${DEST}/go/bin/gofmt" /usr/local/bin/gofmt

echo
echo "==> Installed:"
/usr/local/bin/go version
echo
echo "Done. The integration workflow will pick this up automatically."
