#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-plugin-set-settings.sh.
#
# The gate derives its population from git, so each case is a throwaway
# repository rather than a file argument. That is the point: a case that
# passed a path in would not exercise the derivation, which is where the
# interesting failures live -- an empty manifest set, an empty invocation
# set, a parser that reads no operands.
#
# THE CASE THIS GATE WAS WRITTEN FOR is `catches the real defect`: the
# two outage settings, deleted from config.json with the watchdog, left
# behind at five install sites. Every job that installs a plugin went red
# at once and the daemon's error named the setting, not the file.
#
# The negative cases carry as much weight. The first draft failed four
# lines that are not invocations at all -- a sibling gate's own
# `SET_RE='docker plugin set'` (an assignment BEFORE the phrase, read as
# an operand because operands were taken from the whole line) and the
# reference's `docker plugin set <plugin> NAME=value` (a syntax form; a
# command whose plugin operand is an angle-bracket placeholder cannot run
# as written, so it cannot name a setting wrongly). Both shapes are
# pinned here, and so is the limit of the second exemption: it is keyed
# on the REF, so a real invocation is judged however metasyntactic its
# setting looks.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-plugin-set-settings.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT

# Builds a repository from `name=content` pairs and runs the gate in it.
# The gate is copied in rather than symlinked so that its own `cd
# $(dirname $0)/..` lands in the fixture.
# A fresh directory per case, from mktemp rather than a counter: the
# first draft incremented a counter inside the function, which runs in a
# command substitution here, so the counter never advanced in the parent
# and every case ran in the first case's directory on top of the previous
# case's files. Two cases failed and the other eleven were passing partly
# on inherited fixtures.
run_case() {
    local d
    d=$(mktemp -d "$ROOT/caseXXXXXX")
    mkdir -p "$d/scripts" "$d/.github/workflows"
    cp "$CHECK" "$d/scripts/"
    local pair
    for pair in "$@"; do
        local f="${pair%%=*}" body="${pair#*=}"
        mkdir -p "$d/$(dirname "$f")"
        printf '%s\n' "$body" > "$d/$f"
    done
    git -C "$d" init -q
    git -C "$d" add -A >/dev/null 2>&1
    ( cd "$d" && bash scripts/check-plugin-set-settings.sh 2>&1 )
}

MANIFEST='{"env":[{"name":"LOG_LEVEL","value":"info","settable":["value"]},{"name":"STATE_DIR","value":"/run","settable":["value"]}]}'
COVER='{"env":[{"name":"LOG_LEVEL","value":"info","settable":["value"]},{"name":"STATE_DIR","value":"/run","settable":["value"]},{"name":"GOCOVERDIR","value":"/cov","settable":["value"]}]}'

# --- the defect this gate exists for -----------------------------------

out=$(run_case "config.json=$MANIFEST" \
  'Makefile=create:
	docker plugin set $(PLUGIN_NAME) LOG_LEVEL=trace OUTAGE_TICK=2s'); rc=$?
[ $rc -eq 1 ] && ok "catches the real defect: an operand no manifest declares" \
              || no "the OUTAGE defect returned $rc (: $out)"
case "$out" in *OUTAGE_TICK*) ok "names the setting the daemon will name" ;;
  *) no "the finding does not name the operand: $out" ;; esac
case "$out" in *Makefile*) ok "names the file the daemon will not name" ;;
  *) no "the finding does not name the file: $out" ;; esac

# The declared operand on the SAME line must not be reported. A gate that
# flagged the whole line would read as noise and get discharged.
case "$out" in *LOG_LEVEL*'names a setting no manifest declares'*)
    no "reported the declared operand beside the undeclared one: $out" ;;
  *) ok "the declared operand on the same line is not reported" ;; esac

# --- the shape that must pass ------------------------------------------

out=$(run_case "config.json=$MANIFEST" \
  'Makefile=create:
	docker plugin set $(PLUGIN_NAME) LOG_LEVEL=trace'); rc=$?
[ $rc -eq 0 ] && ok "a declared operand passes" || no "a declared operand returned $rc (: $out)"

# A cover-only setting is declared by the OTHER manifest. The union is
# the population on purpose; check-manifest-parity.sh owns the question
# of whether a setting may live in only one of them.
out=$(run_case "config.json=$MANIFEST" "config-cover.json=$COVER" \
  'Makefile=cover:
	docker plugin set $(PLUGIN_NAME) GOCOVERDIR=/cov'); rc=$?
[ $rc -eq 0 ] && ok "a setting declared by only the cover manifest passes" \
              || no "the union is not the population (rc=$rc: $out)"

# --- the two false positives the first draft produced -------------------

# A real invocation sits beside the mention, as it does in the tree:
# without one the gate refuses for having judged nothing, which is the
# right verdict on that fixture but not the question this case asks.
out=$(run_case "config.json=$MANIFEST" \
  'Makefile=create:
	docker plugin set lan-dhcp LOG_LEVEL=trace' \
  "scripts/sibling.sh=SET_RE='docker plugin set'"); rc=$?
[ $rc -eq 0 ] && ok "an assignment BEFORE the phrase is not an operand" \
              || no "read a token preceding the command as an operand (rc=$rc: $out)"

out=$(run_case "config.json=$MANIFEST" \
  'Makefile=create:
	docker plugin set $(PLUGIN_NAME) LOG_LEVEL=trace' \
  'docs/reference.md=Change one with `docker plugin set <plugin> NAME=value`.'); rc=$?
[ $rc -eq 0 ] && ok "a <placeholder> ref makes the line a syntax form, not an invocation" \
              || no "judged a command that cannot run as written (rc=$rc: $out)"
case "$out" in *'Declined 1 invocation'*) ok "the declination is announced, not silent" ;;
  *) no "the exemption is silent: $out" ;; esac

# --- the LIMIT of that exemption ---------------------------------------
# Keyed on the ref, not the operand: a real invocation is judged however
# metasyntactic its setting looks. An exemption keyed on `NAME=value`
# would let a real `NAME=` through.

out=$(run_case "config.json=$MANIFEST" \
  'Makefile=create:
	docker plugin set lan-dhcp NAME=value'); rc=$?
[ $rc -eq 1 ] && ok "a REAL invocation with a metasyntactic operand is still judged" \
              || no "the form exemption leaked to a runnable command (rc=$rc: $out)"

# --- gate self-tests are excluded, and the exclusion has a limit ------
# Their fixtures are defects by construction: the first case in THIS file
# writes an undeclared setting on purpose. A gate that fails on the
# commit proving it works gets waived by reflex.

out=$(run_case "config.json=$MANIFEST" \
  'Makefile=create:
	docker plugin set lan-dhcp LOG_LEVEL=trace' \
  'scripts/test-check-thing.sh=fixture="docker plugin set lan-dhcp OUTAGE_TICK=2s"'); rc=$?
[ $rc -eq 0 ] && ok "a gate self-test's fixture defect is not a finding" \
              || no "failed on a self-test fixture (rc=$rc: $out)"
# Two, not one: run_case copies the gate into the fixture, where it is
# tracked and mentions `docker plugin set` in its own comments, so it
# excludes itself there exactly as it does in the real tree.
case "$out" in *'Excluded 2 file'*) ok "the exclusion is counted in the output" ;;
  *) no "the exclusion is silent: $out" ;; esac

# The limit: excluded on the FILE, not on the content. A real install
# site is judged however much it looks like a fixture.
out=$(run_case "config.json=$MANIFEST" \
  'scripts/install-thing.sh=docker plugin set lan-dhcp OUTAGE_TICK=2s'); rc=$?
[ $rc -eq 1 ] && ok "a script that is NOT a self-test is judged" \
              || no "the self-test exclusion leaked to an install site (rc=$rc: $out)"

# And a tree where the exclusion leaves nothing must refuse, not pass.
out=$(run_case "config.json=$MANIFEST" \
  'scripts/test-check-thing.sh=fixture="docker plugin set lan-dhcp OUTAGE_TICK=2s"'); rc=$?
[ $rc -eq 2 ] && ok "a domain emptied by the self-test exclusion is a refusal" \
              || no "the exclusion swallowed the domain silently (rc=$rc: $out)"

# --- the refusals: a gate that reads nothing must not report a pass ----

out=$(run_case "config.json=$MANIFEST"); rc=$?
[ $rc -eq 2 ] && ok "no invocations at all is a refusal, not a pass" \
              || no "passed having read nothing (rc=$rc: $out)"

out=$(run_case 'config.json={"env":[]}' \
  'Makefile=create:
	docker plugin set lan-dhcp LOG_LEVEL=trace'); rc=$?
[ $rc -eq 2 ] && ok "a manifest declaring no settings is a refusal, not a pass" \
              || no "a manifest with no settings did not refuse (rc=$rc: $out)"

# Every invocation an expansion: nothing was judged, so there is no
# verdict to report. This is the emptied-domain failure at the operand
# level rather than the file level.
out=$(run_case "config.json=$MANIFEST" \
  'Makefile=create:
	docker plugin set lan-dhcp $(EXTRA_SETTINGS)'); rc=$?
[ $rc -eq 2 ] && ok "invocations but no literal operand is a refusal" \
              || no "reported a pass having judged no operand (rc=$rc: $out)"

# And the same at the exemption: if declining is ALL the gate ever did,
# the placeholder rule has swallowed the domain.
out=$(run_case "config.json=$MANIFEST" \
  'docs/reference.md=Run `docker plugin set <plugin> NAME=value`.'); rc=$?
[ $rc -eq 2 ] && ok "a domain emptied by the form exemption is a refusal" \
              || no "the exemption swallowed the domain silently (rc=$rc: $out)"

# --- the line continuation ---------------------------------------------
# The defect shipped as a continued line. A parser reading physical lines
# would have seen the operands on a line with no `docker plugin set` on
# it and reported a clean pass over the very thing it was written for.

out=$(run_case "config.json=$MANIFEST" \
  'Makefile=create:
	docker plugin set $(PLUGIN_NAME) LOG_LEVEL=trace \
		OUTAGE_TICK=2s'); rc=$?
[ $rc -eq 1 ] && ok "an operand on a CONTINUED line is judged" \
              || no "a backslash continuation hid the operand (rc=$rc: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
