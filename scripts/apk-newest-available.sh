#!/bin/sh
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Runs INSIDE the alpine image (fed to `sh -s` on stdin by
# check-apk-pins.sh): for each `name=version` argument print
# `name<TAB>newest-version-a-repository-offers<TAB>cmp`, where cmp is
# `apk version -t PINNED NEWEST` (`<`, `=`, `>`).
#
# `apk policy` lists the INSTALLED version first (apk-tools 2.14 and 3.0.8
# alike), then the repository versions, ascending. Taking its first line
# read the base image's installed package as "available", so a pin equal
# to the installed version could never look behind (#1398). Only blocks
# with a source other than lib/apk/db/installed count as offered, and the
# newest is picked with apk's own ordering.

apk update -q >/dev/null
for p in "$@"; do
    name="${p%%=*}"
    avail=""
    for v in $(apk policy "$name" 2>/dev/null | awk '
        /^  [^ ]/ { v = $1; sub(/:$/, "", v) }
        /^    / && !/lib\/apk\/db\/installed/ { print v }
    ' | sort -u); do
        if [ -z "$avail" ] || [ "$(apk version -t "$avail" "$v")" = "<" ]; then avail=$v; fi
    done
    cmp=""
    [ -z "$avail" ] || cmp=$(apk version -t "${p#*=}" "$avail")
    printf '%s\t%s\t%s\n' "$name" "$avail" "$cmp"
done
