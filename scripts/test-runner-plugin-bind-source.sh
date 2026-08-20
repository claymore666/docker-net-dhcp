#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for the plugin bind source in ci/runner-image/entrypoint.sh.
#
# The failure this guards is not "the directory was missing". It is a
# runner that reports ONLINE while its nested daemon is dead: dockerd
# restores an already-enabled docker-net-dhcp from the persistent
# /var/lib/docker volume, cannot bind its state directory because the
# recreated container's root filesystem does not carry one, and then
# SIGSEGVs in libnetwork's remote-driver registration instead of
# degrading. The supervisor relaunches it into the same panic forever.
# Observed on the arm64 host the first time its runner container was
# recreated.
#
# Two things have to hold, and only one of them is about the mkdir:
# the directory must be created, and it must be created BEFORE dockerd
# is ever started. Creating it afterwards is indistinguishable from not
# creating it at all, because the daemon has already restored and
# crashed by then. So the ordering is asserted here as its own case.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
EP="$REPO/ci/runner-image/entrypoint.sh"
[ -f "$EP" ] || { echo "FAIL: $EP does not exist"; exit 2; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

# Run the real function out of the real entrypoint against a scratch
# path, rather than restating it here: a copy in the test would keep
# passing after the shipped one was edited.
run_fn() {
    local script="$1" dir="$2"
    local body
    body=$(sed -n '/^ensure_plugin_bind_source()/,/^}/p' "$script")
    [ -n "$body" ] || { echo "no-function"; return; }
    (
        eval "log() { :; }
$body"
        PLUGIN_BIND_SOURCE="$dir" ensure_plugin_bind_source
    ) >/dev/null 2>&1 && echo ok || echo fail
}

# --- 1. it creates the bind source -------------------------------------
target="$TMP/state"
got=$(run_fn "$EP" "$target")
check "creating the bind source succeeds" ok "$got"
[ -d "$target" ] && got=yes || got=no
check "the bind source exists afterwards" yes "$got"

# --- 2. idempotent, and it does not clobber a populated dir -------------
# The real directory carries live lease state on a host that has run the
# plugin before; a create that emptied it would be a data-loss bug that
# an existence check alone would never see.
echo "lease" > "$target/keep"
got=$(run_fn "$EP" "$target")
check "running again on an existing dir succeeds" ok "$got"
[ -f "$target/keep" ] && got=kept || got=lost
check "existing state in the bind source survives" kept "$got"

# --- 3. it defaults to the manifest's path when unset -------------------
# The manifest binds /var/lib/net-dhcp; a default that drifted from it
# would create a directory the plugin never mounts.
grep -q 'PLUGIN_BIND_SOURCE:-/var/lib/net-dhcp' "$EP" && got=yes || got=no
check "defaults to the manifest bind source /var/lib/net-dhcp" yes "$got"

# --- 4. it runs BEFORE dockerd is started ------------------------------
# The case the panic is actually about.
order_ok() {
    local script="$1" call dockerd
    call=$(grep -n '^ensure_plugin_bind_source$' "$script" | head -1 | cut -d: -f1)
    dockerd=$(grep -n '^supervise_dockerd &' "$script" | head -1 | cut -d: -f1)
    [ -n "$call" ] || { echo "not-called"; return; }
    [ -n "$dockerd" ] || { echo "no-dockerd"; return; }
    [ "$call" -lt "$dockerd" ] && echo before || echo after
}
check "the bind source is created before dockerd starts" before "$(order_ok "$EP")"

# --- 5. mutations: each failure mode must be caught --------------------
# A guard that cannot fail is not a guard. Both ways this can regress are
# driven against the same assertions above.
mut="$TMP/mutant.sh"

# 5a. the call is dropped entirely
grep -v '^ensure_plugin_bind_source$' "$EP" > "$mut"
check "mutation: dropping the call is caught by the ordering check" not-called "$(order_ok "$mut")"

# 5b. the call survives but moves after dockerd — the useless-but-present
#     regression that an existence-only test would pass
grep -v '^ensure_plugin_bind_source$' "$EP" \
    | sed 's/^supervise_dockerd &$/supervise_dockerd \&\nensure_plugin_bind_source/' > "$mut"
check "mutation: calling it after dockerd is caught" after "$(order_ok "$mut")"

# 5c. the function itself is removed
sed '/^ensure_plugin_bind_source()/,/^}/d' "$EP" > "$mut"
check "mutation: removing the function is caught" no-function "$(run_fn "$mut" "$TMP/gone")"

if [ "$fails" -ne 0 ]; then
    echo "FAILED"
    exit 1
fi
echo "OK"
