#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for ci/runner-image/register.sh — the standing arm64 runner
# (#632).
#
# The failure this guards is not "registration broke". It is a boot that
# LOOKS successful and still needs a human: a registration written to a
# directory that does not persist, a half-restored identity, a runner
# that starts unregistered. All three produce a green log and an absent
# runner at the next release candidate, which is the state #632 exists
# to end — so every one of them is driven here in both directions.
#
# No GitHub contact: config.sh and run.sh are stubs, and the mount check
# reads a fixture mountinfo instead of /proc.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
LIB="$REPO/ci/runner-image/register.sh"
[ -f "$LIB" ] || { echo "FAIL: $LIB does not exist"; exit 2; }

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

# Build a fresh scratch world and run runner_prepare in a SUBSHELL, so
# each case starts from the library's own defaults rather than from the
# previous case's exports. Prints "ok" or "fail"; the log lands in
# $TMP/log and the stub's argv in $home/config-args.
#
#   run_case <case> <state-has> <env...>
#     state-has: comma list of identity files to pre-place, or "none"
run_case() {
    local name="$1" state_has="$2"; shift 2
    local root="$TMP/$name" home="$TMP/$name/home" state="$TMP/$name/state"
    mkdir -p "$home" "$state"

    # config.sh stub: records argv, then writes the three identity files
    # the way the real one does. CONFIG_WRITES/CONFIG_RC let a case make
    # it fail or write a partial set.
    cat > "$home/config.sh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$(dirname "$0")/config-args"
# The real config.sh refuses an already-configured home, which is what a
# --restart=always retry always hands it. Reproduced here so the caller
# is required to clear the leftovers itself.
if [ -e "$(dirname "$0")/.runner" ]; then
    echo "Cannot configure the runner because it is already configured." >&2
    exit 1
fi
[ "${CONFIG_RC:-0}" = "0" ] || exit "${CONFIG_RC}"
for f in ${CONFIG_WRITES:-.runner .credentials .credentials_rsaparams}; do
    echo "written-by-stub" > "$(dirname "$0")/$f"
done
STUB
    chmod +x "$home/config.sh"

    if [ "${DIRTY_HOME:-0}" = "1" ]; then
        echo "leftover-from-a-failed-attempt" > "$home/.runner"
    fi

    if [ "$state_has" != "none" ]; then
        local f
        for f in ${state_has//,/ }; do echo "pre-existing" > "$state/$f"; done
    fi

    # mountinfo fixture: by default the state dir IS a mount.
    local mi="$root/mountinfo"
    if [ "${FIXTURE_UNMOUNTED:-0}" = "1" ]; then
        : > "$mi"
    else
        echo "1 2 0:3 / $(cd "$state" && pwd -P) rw,relatime shared:1 - ext4 /dev/x rw" > "$mi"
    fi

    (
        set +e
        export RUNNER_HOME="$home" RUNNER_STATE_DIR="$state" RUNNER_MOUNTINFO="$mi"
        export RUNNER_NAME="rpi-arm64-1"
        # shellcheck source=/dev/null
        . "$LIB"
        runner_prepare
    ) >"$root/log" 2>&1 && echo ok || echo fail
}

# --- 1. first boot: token present, state is a mount --------------------
got=$(RUNNER_REGISTRATION_TOKEN=TOKENSECRET RUNNER_LABELS=dhcp-ci-arm64 run_case first none)
check "first boot with a token registers" ok "$got"
args="$TMP/first/home/config-args"
grep -qx -- '--unattended' "$args" && grep -qx -- '--replace' "$args" \
    && echo "PASS: config.sh got --unattended --replace" \
    || { echo "FAIL: config.sh argv missing --unattended/--replace"; fails=1; }
grep -qx -- '--ephemeral' "$args" \
    && { echo "FAIL: --ephemeral passed — the runner would deregister after one job, which is the state #632 removes"; fails=1; } \
    || echo "PASS: not --ephemeral (a standing runner, not a single-use one)"
got=$(grep -c . "$TMP/first/state/.credentials_rsaparams" 2>/dev/null || echo 0)
check "identity persisted to the state dir" 1 "$got"
got=$(grep -c TOKENSECRET "$TMP/first/log" || true)
check "the registration token is never logged" 0 "$got"

# --- 2. every later boot: identity present, NO token -------------------
got=$(RUNNER_LABELS=dhcp-ci-arm64 run_case reboot .runner,.credentials,.credentials_rsaparams)
check "a later boot with no token restores and runs" ok "$got"
got=$([ -f "$TMP/reboot/home/config-args" ] && echo called || echo not-called)
check "config.sh is NOT re-run when an identity exists" not-called "$got"
got=$(cat "$TMP/reboot/home/.credentials" 2>/dev/null)
check "the persisted identity is what lands in the runner home" pre-existing "$got"

# --- 3. no identity, no token: must fail, never start bare -------------
got=$(RUNNER_LABELS=dhcp-ci-arm64 run_case bare none)
check "no identity and no token fails" fail "$got"
grep -q "Refusing to start unregistered" "$TMP/bare/log" \
    && echo "PASS: says why it refused" \
    || { echo "FAIL: refused without saying why"; fails=1; }

# --- 4. a PARTIAL identity is not an identity --------------------------
# The dangerous middle: .runner alone looks like "already registered" to
# any check that tests one file, and cannot authenticate.
got=$(RUNNER_REGISTRATION_TOKEN=T RUNNER_LABELS=dhcp-ci-arm64 run_case partial .runner)
check "a partial identity plus a token re-registers" ok "$got"
grep -q "PARTIAL registration" "$TMP/partial/log" \
    && echo "PASS: the partial state is reported, not silently overwritten" \
    || { echo "FAIL: re-registered over a partial identity without saying so"; fails=1; }

got=$(RUNNER_LABELS=dhcp-ci-arm64 run_case partial_notoken .runner,.credentials)
check "a partial identity with no token fails (does not half-restore)" fail "$got"

# --- 4b. a dirty runner home must not wedge the retry loop -------------
# Found on the real host: a first attempt that registered and then failed
# leaves .runner in the container's writable layer, --restart=always
# reuses that layer, and config.sh refuses every retry afterwards. The
# runner never comes back without a human — the exact state #632 removes,
# reached by the mechanism meant to prevent it.
mkdir -p "$TMP/dirty/home"
got=$(RUNNER_REGISTRATION_TOKEN=T RUNNER_LABELS=dhcp-ci-arm64 \
      DIRTY_HOME=1 run_case dirty none)
check "a re-register against an already-configured home succeeds" ok "$got"

# --- 4c. the key file is matched by pattern, not by name ---------------
# .credentials_rsaparams is runner 2.x with FIPS crypto; the name has
# been spelled otherwise before. A rename must keep working, and its
# ABSENCE must still be fatal.
got=$(CONFIG_WRITES=".runner .credentials .credentials_rsakey" \
      RUNNER_REGISTRATION_TOKEN=T RUNNER_LABELS=dhcp-ci-arm64 run_case renamedkey none)
check "a differently-spelled .credentials_rsa* key still registers" ok "$got"
got=$([ -s "$TMP/renamedkey/state/.credentials_rsakey" ] && echo persisted || echo lost)
check "the renamed key file is persisted" persisted "$got"

got=$(CONFIG_WRITES=".runner .credentials" \
      RUNNER_REGISTRATION_TOKEN=T RUNNER_LABELS=dhcp-ci-arm64 run_case nokey none)
check "no key file at all is still fatal" fail "$got"

# --- 5. the state dir must actually persist ----------------------------
got=$(FIXTURE_UNMOUNTED=1 RUNNER_REGISTRATION_TOKEN=T RUNNER_LABELS=dhcp-ci-arm64 run_case nomount none)
check "an unmounted state dir fails by default" fail "$got"
grep -q "not a mount point" "$TMP/nomount/log" \
    && echo "PASS: names the reason (registration would not survive the container)" \
    || { echo "FAIL: rejected an unmounted state dir without explaining"; fails=1; }

got=$(FIXTURE_UNMOUNTED=1 RUNNER_REQUIRE_PERSISTENT_STATE=0 RUNNER_REGISTRATION_TOKEN=T \
      RUNNER_LABELS=dhcp-ci-arm64 run_case nomount_ok none)
check "an unmounted state dir is allowed when explicitly opted out" ok "$got"
grep -q "WARNING" "$TMP/nomount_ok/log" \
    && echo "PASS: the opt-out still warns" \
    || { echo "FAIL: opting out went silent"; fails=1; }

# --- 6. label rules ----------------------------------------------------
# `dhcp-ci` on a non-x86 runner poaches the amd64 pool's jobs.
got=$(bash -c '. "$1"; runner_labels_ok "self-hosted,dhcp-ci" aarch64' _ "$LIB" >/dev/null 2>&1 && echo ok || echo fail)
check "label dhcp-ci on aarch64 is refused" fail "$got"
got=$(bash -c '. "$1"; runner_labels_ok "self-hosted,dhcp-ci" x86_64' _ "$LIB" >/dev/null 2>&1 && echo ok || echo fail)
check "label dhcp-ci on x86_64 is allowed" ok "$got"
# The substring trap: dhcp-ci-arm64 CONTAINS dhcp-ci and must still pass.
got=$(bash -c '. "$1"; runner_labels_ok "dhcp-ci-arm64" aarch64' _ "$LIB" >/dev/null 2>&1 && echo ok || echo fail)
check "label dhcp-ci-arm64 on aarch64 is allowed (not matched as dhcp-ci)" ok "$got"

# --- 7. config.sh failing must not fall through to run.sh --------------
got=$(CONFIG_RC=1 RUNNER_REGISTRATION_TOKEN=T RUNNER_LABELS=dhcp-ci-arm64 run_case cfgfail none)
check "a failing config.sh fails the boot" fail "$got"

# A config.sh that exits 0 having written an incomplete set is the
# quietest version of the same bug.
got=$(CONFIG_WRITES=".runner .credentials" RUNNER_REGISTRATION_TOKEN=T \
      RUNNER_LABELS=dhcp-ci-arm64 run_case cfgpartial none)
check "config.sh exiting 0 with an incomplete identity fails the boot" fail "$got"

# A copy that REPORTS success and lands nothing is the last direction:
# distinct from an incomplete config.sh above, because the source files
# are all present and cp exits 0. Not hypothetical here — this runner's
# root filesystem is an NFS share, where a write can be acknowledged and
# then fail on the server. Driven by shadowing cp, which is the only way
# to produce it deterministically.
mkdir -p "$TMP/lostcopy/home" "$TMP/lostcopy/state"
for f in .runner .credentials .credentials_rsaparams; do echo x > "$TMP/lostcopy/home/$f"; done
got=$(
    set +e
    export RUNNER_HOME="$TMP/lostcopy/home" RUNNER_STATE_DIR="$TMP/lostcopy/state"
    # shellcheck source=/dev/null
    . "$LIB"
    cp() { return 0; }          # exits 0, writes nothing
    runner_persist_identity >"$TMP/lostcopy/log" 2>&1 && echo ok || echo fail
)
check "a copy that reports success but lands nothing fails the boot" fail "$got"
grep -q "did not land in" "$TMP/lostcopy/log" \
    && echo "PASS: names the state dir the identity failed to reach" \
    || { echo "FAIL: silent about an identity that never landed"; fails=1; }

# --- 8. the real invocation path -------------------------------------
# Testing the library standalone does not prove the image uses it.
ENTRY="$REPO/ci/runner-image/entrypoint.sh"
DOCKERFILE="$REPO/ci/runner-image/Dockerfile"
grep -q '\. /register\.sh' "$ENTRY" \
    && echo "PASS: entrypoint.sh sources /register.sh" \
    || { echo "FAIL: entrypoint.sh does not source /register.sh — this library runs nowhere"; fails=1; }
grep -q 'runner_prepare' "$ENTRY" \
    && echo "PASS: entrypoint.sh calls runner_prepare" \
    || { echo "FAIL: entrypoint.sh never calls runner_prepare"; fails=1; }
grep -q 'register\.sh /register\.sh' "$DOCKERFILE" \
    && echo "PASS: Dockerfile copies register.sh into the image" \
    || { echo "FAIL: Dockerfile does not COPY register.sh — the register mode would fail at runtime"; fails=1; }
# The default mode must keep exiting after one job: the amd64 pool is
# ephemeral by design and a standing runner there would carry state
# between jobs.
grep -q 'run\.sh --jitconfig' "$ENTRY" \
    && echo "PASS: the default JIT mode is unchanged" \
    || { echo "FAIL: the single-use JIT path is gone — the amd64 pool depends on it"; fails=1; }

if [ "$fails" -ne 0 ]; then
    echo "runner register self-test FAILED"
    exit 1
fi
echo "PASS  register.sh handles first boot, reboot, partial state, unpersisted state and label poaching"
