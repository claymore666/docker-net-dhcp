#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-dockerfile-pins.sh (#633).
#
# Fixtures are written to a temp tree and fed in through PIN_GATE_FILES,
# so the cases never depend on what the repo's own Dockerfiles happen to
# contain today.
#
# The cases that keep the rest honest are the ones where the gate must
# NOT fire: a `scratch` base has no digest to name, and a later stage
# referring to an earlier one by alias cannot carry a digest either.
# Without those, a gate that rejected everything unconditionally would
# still pass the positive cases. The mirror of that is the empty-input
# case: inspecting nothing must be an error, not a pass.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-dockerfile-pins.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# run_with <file-content>
run_with() {
    local dir; dir=$(mktemp -d)
    printf '%s\n' "$1" > "$dir/Dockerfile"
    PIN_GATE_FILES="$dir/Dockerfile" bash "$CHECK" "$dir" >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

D='sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258'

# --- pinned ------------------------------------------------------------

out=$(run_with "FROM debian:trixie-slim@$D"); rc=$?
[ $rc -eq 0 ] && ok "a digest-pinned FROM passes" || no "a digest-pinned FROM passes (rc=$rc: $out)"

# The flag must not be mistaken for the image reference.
out=$(run_with "FROM --platform=\$BUILDPLATFORM debian:trixie-slim@$D AS seeder"); rc=$?
[ $rc -eq 0 ] && ok "a --platform flag is skipped, not read as the image" \
               || no "a --platform flag is skipped, not read as the image (rc=$rc: $out)"

# --- unpinned ----------------------------------------------------------

out=$(run_with "FROM debian:trixie-slim"); rc=$?
[ $rc -eq 1 ] && ok "a tag-only FROM fails" || no "a tag-only FROM fails (rc=$rc: $out)"
case "$out" in
  *"not pinned by digest"*) ok "the failure says what is wrong" ;;
  *) no "the failure says what is wrong (got: $out)" ;;
esac

# The v1.7.0 miss in its original form: one pinned file, one that is not.
out=$(run_with "FROM golang:1.26.6-alpine@$D AS builder
RUN true
FROM alpine:3.24.1"); rc=$?
[ $rc -eq 1 ] && ok "one unpinned stage among pinned ones fails" \
               || no "one unpinned stage among pinned ones fails (rc=$rc: $out)"

# --- must NOT fire ------------------------------------------------------

out=$(run_with "FROM scratch"); rc=$?
[ $rc -eq 0 ] && ok "scratch is exempt" || no "scratch is exempt (rc=$rc: $out)"

# A stage alias defined earlier in the same file carries no digest and
# needs none — it is already pinned where it was declared.
out=$(run_with "FROM debian:trixie-slim@$D AS builder
RUN true
FROM builder AS final"); rc=$?
[ $rc -eq 0 ] && ok "a reference to an earlier stage is exempt" \
               || no "a reference to an earlier stage is exempt (rc=$rc: $out)"

# ...but only within its own file. A stage name is not a global licence
# to skip a digest, which is what makes the exemption safe.
dir=$(mktemp -d)
printf 'FROM debian:trixie-slim@%s AS builder\n' "$D" > "$dir/a.Dockerfile"
printf 'FROM builder\n' > "$dir/b.Dockerfile"
out=$(PIN_GATE_FILES="$dir/a.Dockerfile
$dir/b.Dockerfile" bash "$CHECK" "$dir" 2>&1); rc=$?
rm -rf "$dir"
[ $rc -eq 1 ] && ok "a stage alias does not carry across files" \
               || no "a stage alias does not carry across files (rc=$rc: $out)"

# --- inspecting nothing is not a pass ----------------------------------

dir=$(mktemp -d)
out=$(bash "$CHECK" "$dir" 2>&1); rc=$?
rm -rf "$dir"
[ $rc -eq 2 ] && ok "a tree with no Dockerfiles exits 2, not 0" \
               || no "a tree with no Dockerfiles exits 2, not 0 (rc=$rc: $out)"

out=$(bash "$CHECK" /nonexistent-path-for-the-gate-test 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing root exits 2" || no "a missing root exits 2 (rc=$rc: $out)"

# --- the repository itself ---------------------------------------------

out=$(bash "$CHECK" "$HERE/.." 2>&1); rc=$?
[ $rc -eq 0 ] && ok "the repository's own Dockerfiles are all pinned" \
               || no "the repository's own Dockerfiles are all pinned (rc=$rc: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
