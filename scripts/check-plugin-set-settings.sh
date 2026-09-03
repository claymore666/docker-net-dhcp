#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every `docker plugin set NAME=...` operand must be a setting some
# manifest actually declares.
#
# WHY THIS EXISTS. The daemon refuses the call outright:
#
#   Error response from daemon: setting "OUTAGE_TICK" not found in the
#   plugin configuration
#
# 2.0 deleted the outage watchdog and its two settings from config.json.
# The deletion reached the manifest and the Go code; it did not reach the
# five places that INSTALL the plugin — the Makefile's `create` and
# `create-cover`, and four workflows. Every job that installs a plugin
# went red at once: package, scan, integration, all five main-* shards
# and failure-suite. The integration suite never ran.
#
# A sibling gate, check-plugin-set-order.sh, already reads these same
# invocations — it checks that a `set` is preceded by a `disable`. So
# the invocations were being parsed all along and nothing asked the more
# basic question: does this setting exist? That is the gap.
#
# THE POPULATION IS DERIVED, from the tracked manifests and from the
# tracked files that invoke `docker plugin set`. Neither is a list kept
# here, so a new manifest or a sixth install site is judged the day it
# lands. Both are asserted non-empty below: a gate over no manifests
# would accept every operand, and a gate over no invocations would pass
# having read nothing — the failure mode this repository keeps meeting.
#
# THE UNION, NOT PER-MANIFEST. config-cover.json declares GOCOVERDIR and
# REQUEST_CAPTURE_DIR, which config.json does not; a coverage recipe
# setting those is correct. Pairing each invocation with the manifest it
# was created from would mean parsing the recipe's control flow. That is
# left undone deliberately: check-manifest-parity.sh already refuses a
# setting present in one manifest and missing from the other for any
# reason but a declared cover-only one, so the union cannot quietly
# widen. This gate answers "does it exist anywhere", which is exactly
# the question the daemon's error asks.
set -u

cd "$(dirname "$0")/.." || exit 2

MANIFESTS="${PLUGIN_SET_MANIFESTS:-}"
if [ -z "$MANIFESTS" ]; then
    MANIFESTS=$(git ls-files 'config*.json') || exit 2
fi
if [ -z "$MANIFESTS" ]; then
    echo "::error title=No manifests::found no config*.json to read settings from, so every" \
         "operand would be accepted. This is a refusal, not a pass." >&2
    exit 2
fi

declared=""
for m in $MANIFESTS; do
    names=$(jq -r '.env[]?.name // empty' "$m" 2>/dev/null) || {
        echo "::error title=Unreadable manifest::$m is not valid JSON." >&2
        exit 2
    }
    declared="$declared
$names"
done
declared=$(printf '%s\n' "$declared" | grep -v '^$' | sort -u)
NL='
'
declared_padded="$NL$declared$NL"
if [ -z "$declared" ]; then
    echo "::error title=No settings declared::the manifest(s) $MANIFESTS declare no env" \
         "settings at all, so this check would accept anything. Refusing." >&2
    exit 2
fi

# Tracked files only: an untracked scratch script is not what CI runs.
# `-I` keeps grep from trying to read the binaries git also tracks.
#
# Two exclusions, and both are about what the file IS, not what it says.
# This gate excludes itself, which needs no argument. It also excludes
# the gate self-tests, `scripts/test-check-*.sh`: their fixtures are
# DEFECTS BY CONSTRUCTION -- this gate's own self-test cannot have a
# case named "catches the real defect" without writing an undeclared
# setting into a fixture, so judging that file would make the gate fail
# on the commit that proves it works. That is the cry-wolf shape
# check-test-weakening.sh met at #828 over the same category. The
# category is not invented here: check-selftest-fixtures.sh exists
# because gate self-tests are structurally unlike the tree they test.
#
# None of this exempts an INSTALL site. A self-test builds fixture files
# in a scratch directory; it never installs a plugin, so nothing it
# contains can reach a daemon. The count is reported below so the
# exclusion cannot quietly grow, and the refusals cover the case where
# it leaves nothing to judge.
files=$(git grep -lI -e 'docker plugin set' -- . 2>/dev/null \
    | grep -v -e '^scripts/check-plugin-set-settings\.sh$' -e '^scripts/test-check-.*\.sh$')
excluded=$(git grep -lI -e 'docker plugin set' -- . 2>/dev/null \
    | grep -c -e '^scripts/check-plugin-set-settings\.sh$' -e '^scripts/test-check-.*\.sh$')
if [ -z "$files" ]; then
    echo "::error title=No invocations found::no tracked file invokes 'docker plugin set'." \
         "This gate would pass having read nothing. Refusing." >&2
    exit 2
fi

findings=0
invocations=0
operands=0
forms=0
for f in $files; do
    # One invocation may continue over a backslash, so the operands are
    # taken from the joined logical line rather than the physical one.
    while IFS= read -r line; do
        invocations=$((invocations + 1))
        # Operands are what FOLLOWS the command. Taking them from the
        # whole line reads assignments that merely sit near the phrase --
        # check-plugin-set-order.sh's own `SET_RE='docker plugin set'`
        # was reported as an undeclared setting named SET_RE.
        rest="${line#*docker plugin set}"
        # shellcheck disable=SC2086
        set -- $rest
        [ "$#" -ge 1 ] || continue
        ref="$1"
        shift
        # A SYNTAX FORM, not an invocation. `docker plugin set <plugin>
        # NAME=value` in the reference names its plugin with an
        # angle-bracket placeholder, which is this documentation's
        # convention for "put your value here" and which no shell will
        # run: `<` redirects. A line that cannot execute cannot name a
        # setting wrongly, and its operand is a placeholder too. The
        # test for this is the REF, not the operand spelling, so a real
        # invocation with a metasyntactic-looking setting is still
        # judged. Declining is counted and reported below, and the gate
        # refuses if declining is all it ever did.
        case "$ref" in
            *'<'*|*'>'*) forms=$((forms + 1)); continue ;;
        esac
        for tok in "$@"; do
            case "$tok" in
                *=*) ;;
                *) continue ;;
            esac
            name="${tok%%=*}"
            # A shell or make expansion is not a literal setting name;
            # this gate cannot resolve it and says nothing about it.
            case "$name" in
                *'$'*|*'{'*|*'"'*|*"'"*|'') continue ;;
            esac
            case "$name" in
                [A-Z_]*) ;;
                *) continue ;;
            esac
            operands=$((operands + 1))
            # An exact-line match without a pipeline: a piped `grep -q`
            # exits early and kills printf with SIGPIPE, so the pipeline
            # reports failure on success under pipefail.
            if [ "$declared_padded" = "${declared_padded#*"$NL$name$NL"}" ]; then
                echo "FAIL  $f: 'docker plugin set $ref $name=' names a setting no manifest declares."
                echo "      The daemon refuses this outright:"
                echo "        Error response from daemon: setting \"$name\" not found in the plugin configuration"
                echo "      Declared: $(printf '%s' "$declared" | tr '\n' ' ')"
                findings=$((findings + 1))
            fi
        done
    done <<EOF
$(sed -e ':a' -e '/\\$/{N;s/\\\n//;ba' -e '}' "$f" | grep -F 'docker plugin set')
EOF
done

if [ "$operands" -eq 0 ]; then
    echo "::error title=No operands inspected::found $invocations invocation(s) of 'docker" \
         "plugin set' across $(printf '%s' "$files" | wc -w) file(s), declined $forms as" \
         "syntax forms, and found not one literal NAME= operand in the rest. Every one cannot" \
         "be an expansion or a placeholder; either the parser is not reading these lines or" \
         "the form exemption has swallowed the domain. Refusing rather than reporting a pass." >&2
    exit 2
fi

if [ "$findings" -ne 0 ]; then
    echo
    echo "::error title=Undeclared plugin setting::$findings 'docker plugin set' operand(s)" \
         "name a setting no manifest declares. Every install site fails at once when this is" \
         "wrong, and the error names the setting rather than the file that set it." >&2
    exit 1
fi

echo "plugin-set settings: $operands literal operand(s) in $invocations invocation(s) across" \
     "$(printf '%s' "$files" | wc -w) file(s); all declared by $(printf '%s' "$MANIFESTS" | wc -w)" \
     "manifest(s). Declined $forms invocation(s) whose plugin operand is a <placeholder>:" \
     "those lines are syntax forms and cannot run as written. Excluded $excluded file(s) as" \
     "this gate or a gate self-test."
