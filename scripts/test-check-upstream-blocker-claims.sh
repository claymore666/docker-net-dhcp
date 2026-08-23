#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-upstream-blocker-claims.sh (#673).
#
# Every case is a synthesized tree, never the repository's own files, so
# the cases keep their meaning after the upstream PRs merge and both
# blocked issues close.
#
# Two cases carry the weight:
#
#   * the TRUE phrasings must pass. "Compose `interface_name`, engine
#     28+" is correct — it is about the Compose key, not about the
#     issue — and so is "the runner image needs Engine >= 28". A gate
#     that fired on those would be deleted within a week, and rightly.
#   * a THIRD blocked issue, added to the roadmap, must be picked up
#     with no edit here. The issue list is derived; a gate hardcoded to
#     #125 would satisfy every other case in this file.
#
# The fixture repo is only `git init`-ed and written into — nothing is
# committed, so it inherits no identity and cannot block on a signing
# key (see check-selftest-fixtures.sh).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-upstream-blocker-claims.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

WS="$DIR/ws"
mkdir -p "$WS/docs" "$WS/ci"
git -C "$WS" init -q

# The roadmap the issue list is derived from. Two blocked issues, each
# with its upstream PR, mirroring the real table's shape.
write_roadmap() {
    cat > "$WS/docs/roadmap.md" <<'EOF'
# roadmap

## Blocked upstream, not unplanned

| Here | Needs | Upstream |
| --- | --- | --- |
| [#125] — Compose `interface_name` | the remote driver to honour a plugin-returned `DstName` | [moby/moby#52865] (issue), [moby/moby#52866] (PR) |
| [#218] — deterministic MAC | the endpoint name at `CreateEndpoint` | [moby/moby#52870] (issue), [moby/moby#52871] (PR) |

## What this project will deliberately not do

Nothing here.
EOF
}

# run — prints the gate's output, returns its exit code.
run() { bash "$CHECK" --root "$WS" 2>&1; }

# case <want-exit> <label> [<needle>]
case_is() {
    local want="$1" label="$2" needle="${3:-}" out rc
    out=$(run); rc=$?
    if [ "$rc" -ne "$want" ]; then
        no "$label — exit $rc, want $want"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
        return
    fi
    if [ -n "$needle" ] && ! printf '%s' "$out" | grep -F -- "$needle" >/dev/null; then
        no "$label — output missing '$needle'"
        printf '%s\n' "$out" | sed 's/^/      /' >&2
        return
    fi
    ok "$label"
}

reset_tree() {
    rm -f "$WS"/*.md "$WS"/ci/* "$WS"/docs/notes.md 2>/dev/null
    write_roadmap
}

# --- the true phrasings must all pass ----------------------------------
reset_tree
cat > "$WS/docs/notes.md" <<'EOF'
# reference

| `com.docker.network.endpoint.ifname` | Request a specific interface name inside the container (Compose `interface_name`, engine 28+; or this key under `driver_opts`, any engine). |

Multi-network containers work, but interface naming order is
engine-determined until the pass-through ships — see issue #125.
EOF
cat > "$WS/ci/README.md" <<'EOF'
# runner image

| Piece | Why |
|---|---|
| Docker Engine >= 28 (docker-ce) | the distro package is 26.x and the nested daemon must be current |
EOF
case_is 0 "a version about the Compose key, and an issue reference elsewhere, pass" "PASS"

reset_tree
cat > "$WS/docs/notes.md" <<'EOF'
The `interface_name` pass-through ([moby/moby#52866]) has been approved
and is milestoned for engine **29.8.0**; it unblocks [#125] once an
engine carrying it is released.
EOF
case_is 0 "the version that carries the upstream fix, named beside it, passes" "PASS"

reset_tree
cat > "$WS/docs/notes.md" <<'EOF'
- **`interface_name` support, plugin side (#125)** — the endpoint option
  (Compose `services.*.networks.*.interface_name`, engine 28+) is
  validated and honored. **Engine limitation:** moby's remote-driver
  layer currently discards the returned name, so no engine applies it
  for plugin drivers yet.
EOF
case_is 0 "a release note that names the version and the real blocker passes" "PASS"

reset_tree
cat > "$WS/docs/notes.md" <<'EOF'
The capability probe decides at runtime; there is no version threshold
to hit, and #125 stays open until the pass-through lands.
EOF
case_is 0 "an issue reference with no version at all passes" "PASS"

# --- the shipped claim ---------------------------------------------------
reset_tree
cat > "$WS/ci/README.md" <<'EOF'
# runner image

| Piece | Why |
|---|---|
| Docker Engine >= 28 (docker-ce) | nested daemon runs the plugin under test; >= 28 unblocks engine-gated tests (#125) |
| supervised dockerd | the restart test must be able to bounce the daemon |
EOF
case_is 1 "the shipped table row fails" "ci/README.md"
out=$(run)
case "$out" in *"#125"*) ok "the failure names the issue" ;;
  *) no "the failure does not name the issue: $out" ;; esac
case "$out" in *moby/moby#52866*) ok "the failure names the upstream PR the roadmap declares" ;;
  *) no "the failure does not name the upstream PR: $out" ;; esac

# The Dockerfile shape: the version and the issue are on DIFFERENT lines
# of one comment block. A line-scoped check reads them apart and passes,
# which is why the chunk is a run of non-blank lines.
reset_tree
cat > "$WS/ci/Dockerfile" <<'EOF'
FROM debian:13

# Base tooling + Docker Engine from download.docker.com (Debian 13's
# docker.io is 26.x; the nested daemon must be >= 28 — engine-gated
# tests, #125).
RUN true
EOF
case_is 1 "a claim split across two lines of one comment block fails" "ci/Dockerfile"

# --- adjacent rows must not contaminate each other -----------------------
# The version and the issue in two different rows of the same table is
# not a claim. If this ever fails, the chunker has stopped splitting
# table rows and every case above is passing by accident.
reset_tree
cat > "$WS/ci/README.md" <<'EOF'
| Piece | Why |
|---|---|
| Docker Engine >= 28 (docker-ce) | the distro package is too old |
| interface_name tests | capability-probe gated, pending upstream (#125) |
EOF
case_is 0 "a version and an issue in separate table rows do not combine" "PASS"

# --- growth: the issue list is derived, not written here -----------------
reset_tree
python3 - "$WS/docs/roadmap.md" <<'PY'
import sys
p = sys.argv[1]
s = open(p, encoding="utf-8").read()
s = s.replace(
    "| [#218] — deterministic MAC",
    "| [#999] — a future blocked feature | something upstream | [moby/moby#60000] (PR) |\n"
    "| [#218] — deterministic MAC")
open(p, "w", encoding="utf-8").write(s)
PY
cat > "$WS/ci/README.md" <<'EOF'
| Piece | Why |
|---|---|
| Docker Engine >= 30 | >= 30 unblocks the engine-gated work in #999 |
EOF
case_is 1 "a newly blocked issue is picked up with no edit to the gate" "#999"
out=$(run)
case "$out" in *moby/moby#60000*) ok "the new issue's upstream PR is read from the roadmap" ;;
  *) no "the new issue's upstream PR was not read: $out" ;; esac

# ...and it stops being checked when the roadmap says it is unblocked.
write_roadmap
case_is 0 "an issue no longer listed as blocked is no longer judged" "PASS"

# --- the index blind spot ------------------------------------------------
# A file that has never been `git add`ed is exactly the file being
# written right now. An index-scoped gate reads none of it and reports
# clean on the run that was supposed to check it.
reset_tree
cat > "$WS/ci/README.md" <<'EOF'
| Docker Engine >= 28 | >= 28 unblocks the engine-gated tests (#125) |
EOF
git -C "$WS" ls-files --error-unmatch ci/README.md >/dev/null 2>&1 \
    && no "the fixture is tracked; this case no longer tests the untracked path"
case_is 1 "an untracked file is scanned" "ci/README.md"

# ...and a tracked one still is.
git -C "$WS" add -A >/dev/null 2>&1
case_is 1 "a tracked file is scanned" "ci/README.md"
git -C "$WS" rm -r --cached . -q >/dev/null 2>&1

# An ignored file is not part of the tree and must not be judged.
reset_tree
printf 'build/\n' > "$WS/.gitignore"
mkdir -p "$WS/build"
cat > "$WS/build/generated.md" <<'EOF'
| Docker Engine >= 28 | >= 28 unblocks the engine-gated tests (#125) |
EOF
case_is 0 "an ignored artifact is not judged" "PASS"
rm -rf "$WS/build" "$WS/.gitignore"

# --- cannot see: every one of these must be loud -------------------------
reset_tree
rm -f "$WS/docs/roadmap.md"
case_is 2 "a missing roadmap exits 2, not clean" "Roadmap missing"

reset_tree
printf '# roadmap\n\nNothing blocked here.\n' > "$WS/docs/roadmap.md"
case_is 2 "a roadmap with no blocked-upstream section exits 2" "No blocked-upstream section"

reset_tree
cat > "$WS/docs/roadmap.md" <<'EOF'
# roadmap

## Blocked upstream, not unplanned

The table moved somewhere else.

## Next
EOF
case_is 2 "a blocked-upstream section with no issue rows exits 2" "No blocked issues declared"

reset_tree
out=$(bash "$CHECK" --root "$DIR/does-not-exist" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing root exits 2" || no "a missing root returned $rc (: $out)"

# --- the repository itself -----------------------------------------------
out=$(bash "$CHECK" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "the repository ties no engine version to a blocked issue" \
              || no "the repository carries such a claim (rc=$rc): $out"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
