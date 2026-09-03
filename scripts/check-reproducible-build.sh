#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Reproducible-build comparison (#456). `build_reproducible` is an
# OpenSSF Best Practices gold MUST, and the interesting half of it is
# not the property — the build already had it — but keeping it. A
# toolchain bump, an unpinned base image, or a stray timestamp would
# take it away silently: nothing else in CI would go red.
#
# This script is only the comparison. The two builds are the caller's
# job (.github/workflows/reproducible-build.yml runs them on separate
# cold BuildKit builders), which keeps the part that can be reasoned
# about offline separate from the part that needs Docker and five
# minutes.
#
# Usage: check-reproducible-build.sh <dir-a> <dir-b>
#   Each directory is the exported builder stage. Every regular file
#   under a `bin/` directory is compared by SHA-256 against its
#   counterpart at the same relative path.
#
# Exit: 0 identical, 1 a difference, 2 cannot compare.
#
# Note on what "cannot compare" covers, because it is the failure that
# would otherwise look like success: an empty side, or a file present
# in one build and not the other. A comparison over zero files is
# trivially equal, and reporting that as a reproducible build is how
# this gate would go green after someone renames the output directory.
set -u

A="${1:-}"
B="${2:-}"

if [ -z "$A" ] || [ -z "$B" ]; then
    echo "usage: $0 <dir-a> <dir-b>" >&2
    exit 2
fi

for d in "$A" "$B"; do
    if [ ! -d "$d" ]; then
        echo "FAIL  not a directory: $d" >&2
        exit 2
    fi
done

command -v sha256sum >/dev/null 2>&1 || {
    echo "FAIL  sha256sum is required" >&2
    exit 2
}

# Relative paths of every binary, so the two sides can be matched up
# regardless of where the caller exported them.
list() {
    (cd "$1" && find . -type f -path '*/bin/*' -printf '%P\n' | sort)
}

list_a=$(list "$A")
list_b=$(list "$B")

if [ -z "$list_a" ] || [ -z "$list_b" ]; then
    echo "FAIL  no binaries found under a bin/ directory:" >&2
    echo "  $A: $(printf '%s' "$list_a" | grep -c . || true) file(s)" >&2
    echo "  $B: $(printf '%s' "$list_b" | grep -c . || true) file(s)" >&2
    echo "" >&2
    echo "Nothing to compare is not the same as nothing differing." >&2
    exit 2
fi

# The export is the whole builder filesystem, so the sweep above also
# picks up the Go toolchain's own bin/. Comparing those is a bonus — the
# base image is digest-pinned, so they are identical by construction —
# but it means a non-empty result is NOT evidence that our binaries were
# compared. Without this check, moving or renaming the build output
# would leave the gate green over ten unrelated toolchain files.
#
# The list was `net-dhcp dhcp-handler` until 2.0. dhcp-handler was the
# hook binary dhcpcd exec'd on every state change; the beta leases
# in-process and the library reports state changes on a channel, so
# there is no process to exec and cmd/dhcp-handler is deleted. One name
# is enough for what this guard is for — it is the sweep's own
# vacuity it closes, not a count — but ONE name is also the weakest the
# guard has ever been, so if a second binary is ever added to cmd/ it
# belongs here on the same commit.
# shellcheck disable=SC2043  # one element today, and a LIST on purpose:
# the loop is the shape that stays correct when cmd/ gains a second
# binary, and unrolling it is how the second one gets left out.
for want in net-dhcp; do
    if ! printf '%s\n' "$list_a" | grep -E "(^|/)${want}\$" >/dev/null; then
        echo "FAIL  '$want' is not among the binaries found in $A." >&2
        echo "      The build output moved, or the build produced nothing." >&2
        echo "      Found:" >&2
        printf '%s\n' "$list_a" | sed 's/^/        /' >&2
        exit 2
    fi
done

if [ "$list_a" != "$list_b" ]; then
    echo "FAIL  the two builds produced different sets of binaries:" >&2
    diff <(printf '%s\n' "$list_a") <(printf '%s\n' "$list_b") >&2 || true
    exit 2
fi

differences=0
count=0
while IFS= read -r rel; do
    [ -n "$rel" ] || continue
    count=$((count + 1))
    sum_a=$(sha256sum "$A/$rel" | cut -d' ' -f1)
    sum_b=$(sha256sum "$B/$rel" | cut -d' ' -f1)
    if [ "$sum_a" = "$sum_b" ]; then
        printf '  %s  %s\n' "$sum_a" "$rel"
    else
        printf 'DIFFERS %s\n           build A: %s\n           build B: %s\n' \
            "$rel" "$sum_a" "$sum_b" >&2
        differences=$((differences + 1))
    fi
done <<EOF
$list_a
EOF

if [ "$differences" -ne 0 ]; then
    echo "" >&2
    echo "FAIL  $differences of $count binaries differ between two builds of the" >&2
    echo "      same source. The build is no longer reproducible." >&2
    echo "" >&2
    echo "Usual causes, in the order worth checking: a base image or apk" >&2
    echo "package that stopped being pinned, a build flag that embeds a" >&2
    echo "path or timestamp, or a dependency that generates code at build" >&2
    echo "time. docs/verifying-releases.md lists what the determinism" >&2
    echo "currently rests on." >&2
    exit 1
fi

echo "Reproducible — $count binaries identical across two independent builds,"
echo "including net-dhcp. The count is above one because the exported builder"
echo "stage carries the pinned Go toolchain's own bin/ as well."
