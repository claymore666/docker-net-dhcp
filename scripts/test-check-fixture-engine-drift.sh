#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-fixture-engine-drift.sh (#644).
#
# The gate exists so a fixture recorded on one engine cannot keep
# reporting green against another. That claim is only worth anything if
# the gate is shown to go red on drift AND to stay green on the
# differences that are not drift — a patch bump, a distro suffix. Both
# directions are exercised here.
set -uo pipefail

GATE="$(cd "$(dirname "$0")" && pwd)/check-fixture-engine-drift.sh"

pass=0
fail=0

# make_fixtures <dir> <flow>=<engine> [<flow>=<engine> ...]
make_fixtures() {
    local root="$1"; shift
    local spec flow engine
    for spec in "$@"; do
        flow="${spec%%=*}"
        engine="${spec#*=}"
        mkdir -p "$root/$flow"
        if [ "$engine" = "NOFIELD" ]; then
            printf '{\n  "captured": "2026-08-19",\n  "commit": "abc1234",\n  "flow": "x"\n}\n' \
                > "$root/$flow/manifest.json"
        else
            printf '{\n  "engine": "%s",\n  "captured": "2026-08-19",\n  "commit": "abc1234",\n  "flow": "x"\n}\n' \
                "$engine" > "$root/$flow/manifest.json"
        fi
        printf '{"NetworkID":"n","EndpointID":"e"}\n' \
            > "$root/$flow/0001-NetworkDriver.CreateEndpoint.json"
    done
}

# no_docker_path <tmpdir> — a PATH where `docker` exists but always
# fails, standing in for a host with no daemon answering.
no_docker_path() {
    local shim="$1/shim"
    mkdir -p "$shim"
    printf '#!/bin/sh\nexit 1\n' > "$shim/docker"
    chmod +x "$shim/docker"
    printf '%s:%s' "$shim" "$PATH"
}

# check <name> <want_exit> <want_substring> <running_engine|-> <flow=engine>...
check() {
    local name="$1" want_exit="$2" want_sub="$3" running="$4"; shift 4
    local tmp out got
    tmp="$(mktemp -d)"
    make_fixtures "$tmp/requests" "$@"

    if [ "$running" = "-" ]; then
        # No daemon and no override. Shadow `docker` with a stub that
        # fails rather than emptying PATH: the gate needs sed, find and
        # basename to reach the branch under test at all.
        out="$(FIXTURE_ROOT="$tmp/requests" FIXTURE_ENGINE_VERSION="" \
               PATH="$(no_docker_path "$tmp")" bash "$GATE" 2>&1)"
    else
        out="$(FIXTURE_ROOT="$tmp/requests" FIXTURE_ENGINE_VERSION="$running" \
               bash "$GATE" 2>&1)"
    fi
    got=$?

    rm -rf "$tmp"

    if [ "$got" -ne "$want_exit" ]; then
        echo "FAIL  $name: exit $got, want $want_exit"
        echo "$out" | sed 's/^/        /'
        fail=$((fail + 1))
        return
    fi
    if [ -n "$want_sub" ] && ! printf '%s' "$out" | grep -F "$want_sub" >/dev/null; then
        echo "FAIL  $name: output missing '$want_sub'"
        echo "$out" | sed 's/^/        /'
        fail=$((fail + 1))
        return
    fi
    echo "ok    $name"
    pass=$((pass + 1))
}

# --- the gate must stay green on what is not drift -------------------

check "same version passes" 0 "PASS" \
    "26.1.5+dfsg1" "macvlan-run=26.1.5+dfsg1"

check "patch bump is not drift" 0 "PASS" \
    "26.1.9" "macvlan-run=26.1.5"

check "distro suffix is not drift" 0 "PASS" \
    "26.1.5" "macvlan-run=26.1.5+dfsg1"

check "several flows on the same minor pass" 0 "PASS: 3 flow" \
    "26.1.5" "macvlan-run=26.1.5" "bridge-run=26.1.4" "macvlan-restart=26.1.5+dfsg1"

# --- the gate must go red on drift -----------------------------------

check "minor bump is drift" 1 "was captured on engine 26.0.1" \
    "26.1.5" "macvlan-run=26.0.1"

check "major bump is drift" 1 "minor 28.5" \
    "28.5.2" "macvlan-run=26.1.5"

check "one drifted flow among healthy ones fails" 1 "flow 'bridge-run'" \
    "26.1.5" "macvlan-run=26.1.5" "bridge-run=25.0.3" "macvlan-restart=26.1.5"

check "the failure names the regeneration command" 1 "make capture-fixtures" \
    "28.0.0" "macvlan-run=26.1.5"

# --- a manifest that cannot be checked is a failure, not a pass ------

check "manifest without an engine field fails" 1 'has no "engine" field' \
    "26.1.5" "macvlan-run=NOFIELD"

check "unparseable recorded version fails" 1 "no major.minor to compare" \
    "26.1.5" "macvlan-run=unknown"

check "unparseable running version fails" 2 "could not read a major.minor" \
    "not-a-version" "macvlan-run=26.1.5"

# --- absence must never read as a pass -------------------------------

# An empty fixture tree is the shape a botched regeneration leaves
# behind. "Nothing to compare" must not be reported as "no drift".
tmp="$(mktemp -d)"
mkdir -p "$tmp/requests"
out="$(FIXTURE_ROOT="$tmp/requests" FIXTURE_ENGINE_VERSION="26.1.5" bash "$GATE" 2>&1)"; got=$?
rm -rf "$tmp"
if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -F "no flow manifests" >/dev/null; then
    echo "ok    empty fixture tree fails rather than passing"
    pass=$((pass + 1))
else
    echo "FAIL  empty fixture tree: exit $got"
    echo "$out" | sed 's/^/        /'
    fail=$((fail + 1))
fi

tmp="$(mktemp -d)"
out="$(FIXTURE_ROOT="$tmp/gone" FIXTURE_ENGINE_VERSION="26.1.5" bash "$GATE" 2>&1)"; got=$?
rm -rf "$tmp"
if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -F "does not exist" >/dev/null; then
    echo "ok    missing fixture tree fails rather than passing"
    pass=$((pass + 1))
else
    echo "FAIL  missing fixture tree: exit $got"
    echo "$out" | sed 's/^/        /'
    fail=$((fail + 1))
fi

# No daemon and no override: the gate has no verdict and must say so
# out loud, because a silent exit 0 is indistinguishable from a pass.
check "no daemon reports NOT INSPECTED, not a pass" 0 "NOT INSPECTED" \
    "-" "macvlan-run=26.1.5"

tmp="$(mktemp -d)"
make_fixtures "$tmp/requests" "macvlan-run=26.1.5"
out="$(FIXTURE_ROOT="$tmp/requests" FIXTURE_ENGINE_VERSION="" \
       PATH="$(no_docker_path "$tmp")" bash "$GATE" 2>&1)"
rm -rf "$tmp"
if printf '%s' "$out" | grep -F "PASS" >/dev/null; then
    echo "FAIL  no-daemon run printed PASS; NOT INSPECTED must not look like a verdict"
    fail=$((fail + 1))
else
    echo "ok    no-daemon run does not print PASS"
    pass=$((pass + 1))
fi

echo
echo "check-fixture-engine-drift self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
