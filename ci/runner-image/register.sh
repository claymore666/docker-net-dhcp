#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Persistent-registration mode for the CI runner container (#632).
#
# WHY THIS EXISTS
#
# The default mode is single-use: an orchestrator mints a JIT config per
# job and the container exits when the job does. That works for the
# amd64 pool, which has an orchestrator. The arm64 host has none — so
# every release candidate needed a human to mint a config and launch a
# container by hand, which is the whole of #632.
#
# A standing registration removes that. The runner configures itself
# ONCE against a short-lived registration token, keeps its own
# credentials on persistent storage, and reconnects on every boot. From
# then on the only human action is powering the machine on.
#
# WHAT IS DELIBERATELY NOT DONE
#
# The obvious alternative is to store a credential that can CREATE
# runners (a PAT, or an App key with Administration: write) on the host
# and have it mint a JIT config at boot. That is rejected: this host's
# root filesystem is a network share served by another machine, so the
# credential would sit on storage with a wider trust boundary than the
# secret itself. A registration token expires within the hour and is
# consumed by first boot; what persists afterwards is the runner's OWN
# credential, which can act as that one runner and nothing else.
#
# THE FAILURE DIRECTION THAT MATTERS
#
# Every check below is written against the SAME failure: a boot that
# looks successful but has to be repeated by hand. Registering into a
# directory that does not persist, half-restoring an identity, or
# quietly starting unregistered all produce a green log and a runner
# that is gone after a reboot — the exact state #632 exists to end. So
# each of those is fatal here, loudly, naming the file.
#
# Sourced by entrypoint.sh, and sourced directly by
# scripts/test-runner-register.sh, which drives every branch with stub
# binaries. Nothing here contacts GitHub on its own.

RUNNER_HOME="${RUNNER_HOME:-/opt/runner}"
RUNNER_STATE_DIR="${RUNNER_STATE_DIR:-/opt/runner-state}"
RUNNER_URL="${RUNNER_URL:-https://github.com/claymore666/docker-net-dhcp}"
RUNNER_NAME="${RUNNER_NAME:-$(hostname)}"
RUNNER_LABELS="${RUNNER_LABELS:-dhcp-ci-arm64}"

# The files config.sh writes that together ARE the runner's identity.
# .credentials_rsakey is the private key; without it the other two
# cannot authenticate, which is why a partial set is treated as no
# identity at all rather than as something to repair.
RUNNER_IDENTITY_FILES=(.runner .credentials .credentials_rsakey)

reglog() { echo "[register] $*" >&2; }

# Prints the identity files missing from a directory, one per line.
# Returns 0 only when all of them are present and non-empty.
runner_identity_missing() {
    local dir="$1" f missing=()
    for f in "${RUNNER_IDENTITY_FILES[@]}"; do
        [ -s "$dir/$f" ] || missing+=("$f")
    done
    [ "${#missing[@]}" -eq 0 ] || printf '%s\n' "${missing[@]}"
    [ "${#missing[@]}" -eq 0 ]
}

runner_state_writable() {
    local dir="$1"
    mkdir -p "$dir" 2>/dev/null || { reglog "FATAL: cannot create state dir $dir"; return 1; }
    if ! touch "$dir/.write-probe" 2>/dev/null; then
        reglog "FATAL: state dir $dir is not writable"
        return 1
    fi
    rm -f "$dir/.write-probe"
    return 0
}

# Persistence cannot be proved from inside the container, so prove the
# closest thing that fails in the same direction: that the directory is
# not part of the image's own filesystem, i.e. something is mounted
# there. An unmounted state dir registers fine, runs fine, and is empty
# on the next boot.
#
# mountinfo is read directly rather than shelling out to mountpoint(1):
# it needs no package in the image, and comparing device numbers against
# the parent — the other common idiom — reports "not a mount" for a bind
# mount off the same filesystem, which is exactly how a docker -v mount
# can look. Field 5 is the mount point.
RUNNER_MOUNTINFO="${RUNNER_MOUNTINFO:-/proc/self/mountinfo}"
runner_state_is_mount() {
    local dir="$1" resolved
    resolved=$(cd "$dir" 2>/dev/null && pwd -P) || return 1
    [ -r "$RUNNER_MOUNTINFO" ] || return 1
    awk -v d="$resolved" '$5 == d { found = 1 } END { exit !found }' "$RUNNER_MOUNTINFO"
}

# `dhcp-ci` is the amd64 pool's label, and the amd64 workflows ask for
# it WITHOUT an arch label — so a non-x86 runner carrying it would poach
# their jobs and run the suite on the wrong architecture, silently and
# only some of the time. actionlint.yaml documents the rule in prose;
# this makes it fail closed at registration instead of depending on
# whoever types the launch command.
runner_labels_ok() {
    local labels="$1" arch="${2:-$(uname -m)}"
    case ",$labels," in
        *,dhcp-ci,*)
            if [ "$arch" != "x86_64" ]; then
                reglog "FATAL: label 'dhcp-ci' requested on arch '$arch'."
                reglog "That label belongs to the amd64 pool, whose workflows use"
                reglog "runs-on: [self-hosted, dhcp-ci] with no arch label — this"
                reglog "runner would poach their jobs. Use dhcp-ci-arm64."
                return 1
            fi
            ;;
    esac
    return 0
}

# Copy the identity into persistent storage and verify it arrived. A
# persist that silently did nothing is the same green-log-empty-next-
# boot failure as an unmounted state dir.
runner_persist_identity() {
    local f missing
    for f in "${RUNNER_IDENTITY_FILES[@]}"; do
        [ -s "$RUNNER_HOME/$f" ] || { reglog "FATAL: config.sh left no $f in $RUNNER_HOME"; return 1; }
        cp -f "$RUNNER_HOME/$f" "$RUNNER_STATE_DIR/$f" || { reglog "FATAL: could not persist $f"; return 1; }
    done
    chmod 600 "$RUNNER_STATE_DIR"/.credentials* 2>/dev/null || true
    if ! missing=$(runner_identity_missing "$RUNNER_STATE_DIR"); then
        reglog "FATAL: identity did not land in $RUNNER_STATE_DIR (missing: $(tr "\n" " " <<<"$missing"))"
        return 1
    fi
    reglog "identity persisted to $RUNNER_STATE_DIR"
    return 0
}

runner_restore_identity() {
    local f
    for f in "${RUNNER_IDENTITY_FILES[@]}"; do
        cp -f "$RUNNER_STATE_DIR/$f" "$RUNNER_HOME/$f" || { reglog "FATAL: could not restore $f"; return 1; }
    done
    chmod 600 "$RUNNER_HOME"/.credentials* 2>/dev/null || true
    reglog "restored an existing registration from $RUNNER_STATE_DIR (name=$RUNNER_NAME)"
    return 0
}

# The token is passed by env and never echoed: it is short-lived, but it
# is still a credential and this log goes to the host's journal.
runner_configure() {
    local token="$1"
    runner_labels_ok "$RUNNER_LABELS" || return 1
    reglog "registering '$RUNNER_NAME' at $RUNNER_URL with labels [$RUNNER_LABELS]"
    ( cd "$RUNNER_HOME" && RUNNER_ALLOW_RUNASROOT=1 ./config.sh \
        --unattended --replace \
        --url "$RUNNER_URL" \
        --token "$token" \
        --name "$RUNNER_NAME" \
        --labels "$RUNNER_LABELS" \
        --work _work ) || { reglog "FATAL: config.sh failed"; return 1; }
    runner_persist_identity
}

# Bring the container to the point where run.sh can be exec'd: restore a
# registration, or create one. Returns non-zero rather than exiting, so
# the self-test can drive every branch in one process.
runner_prepare() {
    local require_mount="${RUNNER_REQUIRE_PERSISTENT_STATE:-1}" missing present

    runner_state_writable "$RUNNER_STATE_DIR" || return 1

    if ! runner_state_is_mount "$RUNNER_STATE_DIR"; then
        if [ "$require_mount" = "1" ]; then
            reglog "FATAL: $RUNNER_STATE_DIR is not a mount point."
            reglog "The registration would live in the container's own filesystem, so it"
            reglog "would survive exactly until this container is replaced — and the next"
            reglog "boot would need a human again, which is the whole point of #632."
            reglog "Mount a volume there, or set RUNNER_REQUIRE_PERSISTENT_STATE=0 if you"
            reglog "genuinely meant it to be throwaway."
            return 1
        fi
        reglog "WARNING: $RUNNER_STATE_DIR is not a mount — this registration will NOT survive the container"
    fi

    if missing=$(runner_identity_missing "$RUNNER_STATE_DIR"); then
        runner_restore_identity || return 1
        if [ -n "${RUNNER_REGISTRATION_TOKEN:-}" ]; then
            reglog "note: a registration token was supplied but an identity already exists; ignoring the token"
        fi
        return 0
    fi

    # A partial identity is not repairable — the three files are written
    # together by one config.sh run — so it is reported as such and
    # re-registered from scratch, never patched up.
    present=$(( ${#RUNNER_IDENTITY_FILES[@]} - $(printf '%s\n' "$missing" | grep -c .) ))
    if [ "$present" -gt 0 ]; then
        reglog "WARNING: $RUNNER_STATE_DIR holds a PARTIAL registration (missing: $(tr "\n" " " <<<"$missing"))"
        reglog "Re-registering from scratch; a partial identity cannot authenticate."
    fi

    if [ -z "${RUNNER_REGISTRATION_TOKEN:-}" ]; then
        reglog "FATAL: no registration in $RUNNER_STATE_DIR and no RUNNER_REGISTRATION_TOKEN given."
        reglog "First boot needs a token from:"
        reglog "  gh api -X POST repos/<owner>/<repo>/actions/runners/registration-token --jq .token"
        reglog "Refusing to start unregistered — a runner that connects to nothing looks"
        reglog "alive to the host and is invisible to the workflow."
        return 1
    fi

    runner_configure "$RUNNER_REGISTRATION_TOKEN"
}
