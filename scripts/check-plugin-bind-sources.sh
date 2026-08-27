#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every workflow step that runs `docker plugin create` must first create
# every /var/lib bind source declared by the manifest it is installing.
#
# Why this exists: a bind source that does not exist on the host does not
# degrade the plugin, it kills `docker plugin enable` with an opaque
# "failed to fulfil mount request" (#440, #588, #660). The Makefile's
# create-cover derives the list from the manifest, but the workflows each
# carried their own hardcoded copy of it. When #662 added /var/lib/dh-capture
# to config-cover.json, coverage.yml's copy was still naming /var/lib/net-dhcp
# alone and the lane broke — the fix in one mechanism could not reach the
# other four.
#
# So: the manifest is the source of truth, and this gate holds every copy
# to it. A step may satisfy a source either by naming it literally
# (`mkdir -p /var/lib/net-dhcp`) or by deriving the whole set from the
# manifest with jq. Sockets under /var/run are excluded deliberately —
# mkdir -p over a socket replaces it with a directory.
set -euo pipefail

cd "$(dirname "$0")/.."

MAKEFILE=${MAKEFILE:-Makefile}
WORKFLOW_DIR=${WORKFLOW_DIR:-.github/workflows}

command -v jq >/dev/null || { echo "this check needs jq"; exit 2; }

# Map plugin build dir -> manifest, from the Makefile itself, so a renamed
# manifest cannot leave this gate checking a file nobody installs.
declare -A MANIFEST=()
target=""
while IFS= read -r line; do
    case "$line" in
        [!$'\t'#]*:*) target=${line%%:*} ;;
    esac
    if [[ "$line" =~ ^$'\t'@?cp[[:space:]]+(config[^[:space:]]*\.json)[[:space:]] ]]; then
        [ -n "$target" ] && MANIFEST["$target"]="${BASH_REMATCH[1]}"
    fi
done < "$MAKEFILE"

if [ ${#MANIFEST[@]} -eq 0 ]; then
    echo "FAIL: found no 'cp config*.json' rule in $MAKEFILE — the plugin dir"
    echo "      to manifest mapping this gate depends on is gone or moved."
    exit 1
fi

rc=0
checked=0

# BOTH EXTENSIONS, because GitHub Actions honours both and this directory
# already contains one of each. A `*.yml`-only scan does not fail — it
# reports a clean pass over a corpus it silently made smaller, which is
# the shape #832 was filed for. `check-lane-hygiene.sh` and
# `check-dispatch-reachable.sh` read the directory this way; this one was
# the odd gate out. The self-test plants an install in a `.yaml` file, so
# narrowing this glob again goes red instead of quiet.
shopt -s nullglob
WF_FILES=("$WORKFLOW_DIR"/*.yml "$WORKFLOW_DIR"/*.yaml)
shopt -u nullglob

for wf in "${WF_FILES[@]}"; do
    # Walk the file, remembering where the current step began, so the window
    # we search for mkdir lines is exactly the step doing the create.
    step_start=1
    lineno=0
    while IFS= read -r line; do
        lineno=$((lineno + 1))
        case "$line" in
            *"- name:"*) step_start=$lineno ;;
        esac
        [[ "$line" == *"docker plugin create"* ]] || continue
        # Prose about the command is not the command. Skip comment lines,
        # or every paragraph explaining #440 becomes a phantom install.
        [[ "$(printf '%s' "$line" | sed 's/^[[:space:]]*//')" == "#"* ]] && continue
        # `docker plugin create <ref> <dir>` — the build dir is the last field.
        dir=$(printf '%s\n' "$line" | sed -e 's/[[:space:]]*$//' -e 's/.*[[:space:]]//')
        manifest=${MANIFEST[$dir]:-}
        if [ -z "$manifest" ]; then
            echo "FAIL: $wf:$lineno installs plugin dir '$dir', which $MAKEFILE"
            echo "      never populates with a manifest. Either the dir is wrong"
            echo "      or the Makefile rule that builds it was renamed."
            rc=1
            continue
        fi
        [ -f "$manifest" ] || { echo "FAIL: $wf:$lineno -> missing $manifest"; rc=1; continue; }

        window=$(sed -n "${step_start},${lineno}p" "$wf")
        # A jq expression reading this step's manifest covers the whole set
        # at once, and keeps covering it when a source is added. The jq and
        # the path routinely sit on different lines of the same recipe, so
        # look for both anywhere in the step, not on one line.
        # No `grep -q` as a pipeline consumer here: it exits early, SIGPIPEs
        # the producer, and pipefail then reports failure on success.
        if printf '%s\n' "$window" | grep -F 'jq' >/dev/null \
           && printf '%s\n' "$window" | grep -E -e "$manifest" -e "$dir/config\\.json" >/dev/null; then
            checked=$((checked + 1))
            continue
        fi
        missing=()
        while IFS= read -r src; do
            [ -n "$src" ] || continue
            # LITERAL comparison, not a regex. The path used to be
            # interpolated into an ERE, where '.' is a metacharacter — so
            # /var/lib/net-dhcp.d matched /var/lib/net-dhcpXd, and any
            # dotted bind source could be reported as created by a line
            # that creates something else. A false pass here is expensive
            # and silent: the missing source SIGSEGVs dockerd while the
            # runner still reports online.
            #
            # Cut each mkdir invocation down to its own argument list
            # (everything after `mkdir`, up to the next command
            # separator) and compare whole tokens with grep -Fx. Flags
            # like -p simply never equal a path.
            printf '%s\n' "$window" \
                | sed -n 's/.*mkdir//p' \
                | sed 's/[;&|].*//' \
                | tr ' \t' '\n\n' \
                | grep -Fx -- "$src" >/dev/null \
                || missing+=("$src")
        done < <(jq -r '.mounts[]? | select(.type=="bind") | .source | select(startswith("/var/lib/"))' "$manifest")

        if [ ${#missing[@]} -gt 0 ]; then
            echo "FAIL: $wf:$lineno installs $dir (from $manifest) but the step"
            echo "      never creates: ${missing[*]}"
            echo "      docker plugin enable will die on 'failed to fulfil mount"
            echo "      request'. Derive the list from the manifest instead of"
            echo "      naming sources by hand."
            rc=1
        fi
        checked=$((checked + 1))
    done < "$wf"
done

if [ "$checked" -eq 0 ]; then
    echo "FAIL: found no 'docker plugin create' in $WORKFLOW_DIR — this gate"
    echo "      would report clean over a repo it never inspected."
    exit 1
fi

[ "$rc" -eq 0 ] && echo "OK: $checked plugin install(s) create every /var/lib bind source their manifest declares"
exit "$rc"
