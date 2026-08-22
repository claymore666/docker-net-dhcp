#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-plugin-set-order.sh.
#
# Fixtures are generated rather than pointing at the repository's own
# documents, so a case keeps meaning after the snippet it was written
# about is rewritten.
#
# The cases that matter most are the NEGATIVE ones — the shapes this
# gate must NOT fail. Its first draft failed test/integration/README.md,
# which states the rule correctly and therefore necessarily names
# `docker plugin set` before it names the disable. A gate that has to be
# described as "except that file" is a waiver wearing a rule's clothes,
# so the rule was narrowed to judge INVOCATIONS rather than mentions,
# and the shapes it now passes are pinned here.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-plugin-set-order.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

run() { bash "$CHECK" "$1" 2>&1; }

# --- fenced blocks: the recipe shape -----------------------------------

cat > "$DIR/ok-block.md" <<'EOF'
# Settings

```sh
docker plugin disable lan-dhcp
docker plugin set lan-dhcp LOG_LEVEL=trace
docker plugin enable lan-dhcp
```
EOF
out=$(run "$DIR/ok-block.md"); rc=$?
[ $rc -eq 0 ] && ok "disable, set, enable passes" || no "the correct order failed (rc=$rc: $out)"

cat > "$DIR/no-disable.md" <<'EOF'
# Settings

```sh
docker plugin set lan-dhcp LOG_LEVEL=trace
docker plugin enable lan-dhcp
```
EOF
out=$(run "$DIR/no-disable.md"); rc=$?
[ $rc -eq 1 ] && ok "a block that sets with no disable before it fails" \
              || no "a missing disable returned $rc (: $out)"
case "$out" in *"refuses the call on an enabled plugin"*) ok "the failure quotes what the daemon does" ;;
  *) no "the failure does not explain itself: $out" ;; esac

cat > "$DIR/backwards.md" <<'EOF'
# Settings

```sh
docker plugin set lan-dhcp LOG_LEVEL=trace
docker plugin disable lan-dhcp
docker plugin enable lan-dhcp
```
EOF
out=$(run "$DIR/backwards.md"); rc=$?
[ $rc -eq 1 ] && ok "set before disable fails even though both commands are present" \
              || no "the backwards order returned $rc (: $out)"

cat > "$DIR/no-enable.md" <<'EOF'
# Settings

```sh
docker plugin disable lan-dhcp
docker plugin set lan-dhcp LOG_LEVEL=trace
```
EOF
out=$(run "$DIR/no-enable.md"); rc=$?
[ $rc -eq 1 ] && ok "a block that never enables again fails — it leaves the plugin down" \
              || no "a missing enable returned $rc (: $out)"

# Two sets in one block: the enable has to come after the LAST one, or
# the second set runs against a plugin that was just re-enabled.
cat > "$DIR/enable-too-early.md" <<'EOF'
# Settings

```sh
docker plugin disable lan-dhcp
docker plugin set lan-dhcp LOG_LEVEL=trace
docker plugin enable lan-dhcp
docker plugin set lan-dhcp METRICS_ADDR=:9100
```
EOF
out=$(run "$DIR/enable-too-early.md"); rc=$?
[ $rc -eq 1 ] && ok "an enable before the last set fails" \
              || no "an early enable returned $rc (: $out)"

# --- prose: an invocation is judged, a mention is not ------------------

cat > "$DIR/prose-backwards.md" <<'EOF'
# Settings

Run `docker plugin set lan-dhcp LOG_LEVEL=trace`, then disable and
enable the plugin for it to take effect.
EOF
out=$(run "$DIR/prose-backwards.md"); rc=$?
[ $rc -eq 1 ] && ok "a paragraph telling the reader to set and THEN disable fails" \
              || no "backwards prose returned $rc (: $out)"

# THE CASE THAT NARROWED THE RULE. This states the constraint
# correctly, and stating it correctly means naming the command before
# naming the disable. The gate must read this as prose, not as an
# instruction with its steps in the wrong order.
cat > "$DIR/rule-stated.md" <<'EOF'
# Settings

`docker plugin set` requires the plugin to be disabled, so changing one
means `docker plugin disable`, `set`, `enable`.
EOF
out=$(run "$DIR/rule-stated.md"); rc=$?
[ $rc -eq 0 ] && ok "a paragraph STATING the rule passes, though it names the command first" \
              || no "the rule stated correctly was read as an instruction (rc=$rc: $out)"

# A bare mention with no disable anywhere is not a recipe either.
cat > "$DIR/mention.md" <<'EOF'
# Settings

Settings are changed with `docker plugin set`; see the reference for
the full list.
EOF
out=$(run "$DIR/mention.md"); rc=$?
[ $rc -eq 0 ] && ok "naming the command without giving a recipe passes" \
              || no "a bare mention was judged (rc=$rc: $out)"

# The prose shorthands the reference actually uses must count as a
# disable, or the gate passes a backwards instruction by not
# recognising its second half.
for phrase in "disable/enable" "disable and enable"; do
    cat > "$DIR/shorthand.md" <<EOF
# Settings

Run \`docker plugin set lan-dhcp LOG_LEVEL=trace\` and then do a
$phrase cycle.
EOF
    out=$(run "$DIR/shorthand.md"); rc=$?
    [ $rc -eq 1 ] && ok "the prose shorthand \"$phrase\" counts as the disable" \
                  || no "\"$phrase\" was not recognised, so backwards prose passed (rc=$rc: $out)"
done

# Correct prose order passes with the same vocabulary — otherwise the
# case above would be satisfied by a gate that fails every paragraph
# containing both words.
cat > "$DIR/prose-ok.md" <<'EOF'
# Settings

Disable the plugin, run `docker plugin set lan-dhcp LOG_LEVEL=trace`,
then enable it again — a disable/enable cycle is required because the
daemon refuses the call on an active plugin.
EOF
out=$(run "$DIR/prose-ok.md"); rc=$?
[ $rc -eq 0 ] && ok "correct prose order passes with the same vocabulary" \
              || no "correct prose was failed (rc=$rc: $out)"

# --- scope -------------------------------------------------------------

cat > "$DIR/clean.md" <<'EOF'
# Nothing to see

This document does not mention the command at all.
EOF
out=$(run "$DIR/clean.md"); rc=$?
[ $rc -eq 0 ] && ok "a file that never names the command passes" \
              || no "an unrelated file was judged (rc=$rc: $out)"

out=$(bash "$CHECK" "$DIR/does-not-exist.md" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a named file that does not exist is skipped, not a crash" \
              || no "a missing file returned $rc (: $out)"

# The repository's own documents must pass, which is the case that
# would have caught the four backwards snippets this gate was written
# for had it existed then.
out=$(cd "$HERE/.." && bash "$CHECK" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "the repository's own documents pass" \
              || no "the repository has a backwards snippet (rc=$rc: $out)"
case "$out" in *"file(s) read"*) ok "the PASS line reports how many files it read" ;;
  *) no "the PASS line does not tally what it read: $out" ;; esac

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
