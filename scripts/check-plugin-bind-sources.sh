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
# manifest with jq fed to `xargs mkdir`. Sockets under /var/run are excluded deliberately:
# mkdir -p over a socket replaces it with a directory.
set -euo pipefail

cd "$(dirname "$0")/.."

MAKEFILE=${MAKEFILE:-Makefile}
WORKFLOW_DIR=${WORKFLOW_DIR:-.github/workflows}

command -v jq >/dev/null || { echo "this check needs jq"; exit 2; }
# shellcheck source=scripts/workflow-shell-lines.sh
. scripts/workflow-shell-lines.sh

# The commands a step runs, one per line, `sudo` and its flags dropped:
# the gate reads what executes, not what a line names (#883).
step_commands() {
    local c w i
    while IFS= read -r c; do
        read -r -a w <<< "$c"
        i=0
        if [ "${w[0]:-}" = sudo ]; then
            i=1
            while [[ "${w[i]:-}" == -* ]]; do i=$((i + 1)); done
        fi
        if [ "$i" -lt "${#w[@]}" ]; then printf '%s\n' "${w[*]:i}"; fi
    done < <(workflow_shell_lines --raw - | shell_simple_commands)
}

# What one simple command does with `docker plugin create`: "install
# <dir>" when it runs it (bare or behind timeout/env/nice/command/exec),
# "none" when echo, printf, : or true only name it, "unread" otherwise.
# An unread wrapper is a failure, never a skip: a skipped install is the
# #832 shape, a green count over a smaller corpus (#883).
create_of() {
    local -a w
    read -r -a w <<< "$1"
    local k=0 n=${#w[@]}
    while [ "$k" -lt "$n" ] && [ "${w[*]:k:3}" != "docker plugin create" ]; do
        k=$((k + 1))
    done
    [ "$k" -lt "$n" ] || { echo absent; return; }
    local i=0
    while [ "$i" -lt "$k" ]; do
        case "${w[i]}" in
            echo|printf|:|true) echo none; return ;;
            timeout) i=$((i + 1))
                while [[ "${w[i]:-}" == -* ]]; do i=$((i + 1)); done
                i=$((i + 1)) ;;
            env) i=$((i + 1))
                while [[ "${w[i]:-}" == -* || "${w[i]:-}" == *=* ]]; do i=$((i + 1)); done ;;
            nice|command|exec) i=$((i + 1))
                while [[ "${w[i]:-}" == -* ]]; do i=$((i + 1)); done ;;
            *) echo unread; return ;;
        esac
    done
    if [ "$i" -eq "$k" ] && [ $((n - k)) -ge 5 ]; then
        echo "install ${w[n-1]}"
    else
        echo unread
    fi
}

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
        # Prose, an echo or a comment naming the command is not an
        # install, or every paragraph explaining #440 becomes one.
        cmds=$(sed -n "${step_start},${lineno}p" "$wf" | step_commands)
        # The commands this line adds to the step, and the build dir from
        # the create's own words (`docker plugin create <ref> <dir>`).
        before=0
        if [ "$lineno" -gt "$step_start" ]; then
            before=$(sed -n "${step_start},$((lineno - 1))p" "$wf" | step_commands | grep -c . || true)
        fi
        dir=""
        while IFS= read -r c; do
            verdict=$(create_of "$c")
            case "$verdict" in
                install\ *) dir=${verdict#install } ;;
                unread)
                    echo "FAIL: $wf:$lineno runs '$c', which this gate cannot"
                    echo "      read as a plugin install or as a mention."
                    rc=1 ;;
            esac
        done < <(printf '%s\n' "$cmds" | tail -n "+$((before + 1))")
        [ -n "$dir" ] || continue
        manifest=${MANIFEST[$dir]:-}
        if [ -z "$manifest" ]; then
            echo "FAIL: $wf:$lineno installs plugin dir '$dir', which $MAKEFILE"
            echo "      never populates with a manifest. Either the dir is wrong"
            echo "      or the Makefile rule that builds it was renamed."
            rc=1
            continue
        fi
        [ -f "$manifest" ] || { echo "FAIL: $wf:$lineno -> missing $manifest"; rc=1; continue; }

        # A `jq` reading this step's manifest, fed to `xargs mkdir`, covers
        # the whole set and keeps covering it when a source is added. Both
        # must run: a jq named in an echo or a comment creates nothing.
        reads=0; feeds=0; made=""
        while read -r -a w; do
            case "${w[0]:-}" in
                jq)
                    for a in "${w[@]:1}"; do
                        if [ "$a" = "$manifest" ] || [ "$a" = "$dir/config.json" ]; then
                            reads=1
                        fi
                    done ;;
                xargs)
                    for a in "${w[@]:1}"; do
                        [[ "$a" == -* ]] && continue
                        if [ "$a" = mkdir ]; then feeds=1; fi
                        break
                    done ;;
                mkdir) made+=$(printf '%s\n' "${w[@]:1}")$'\n' ;;
            esac
        done <<< "$cmds"
        if [ "$reads" -eq 1 ] && [ "$feeds" -eq 1 ]; then
            checked=$((checked + 1))
            continue
        fi
        missing=()
        while IFS= read -r src; do
            [ -n "$src" ] || continue
            # LITERAL comparison of whole mkdir arguments, not a regex: as
            # an ERE /var/lib/net-dhcp.d matched /var/lib/net-dhcpXd (#710).
            # A missing source SIGSEGVs dockerd while the runner reports online.
            printf '%s\n' "$made" | grep -Fx -- "$src" >/dev/null \
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
