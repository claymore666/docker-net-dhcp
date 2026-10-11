#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Run one lane's integration suites as ONE step: the main make target, then
# the failure target whatever main returned, each tee'd under pipefail. Exit
# status is main's if non-zero, else the failure suite's. Called by
# .github/actions/run-suites from every lane (#733), whose next step
# summarises the logs.
#
# Environment (the action's inputs):
#   SUITES_MAIN             make arguments for the first suite (required)
#   SUITES_FAILURE          make arguments for the failure suite (may be empty)
#   SUITES_LOG              log name; logs land in /tmp/itest-<log>-{main,failure}.log
#   SUITES_MAIN_TIMEOUT     ITEST_TIMEOUT for the first suite (empty: Makefile default)
#   SUITES_FAILURE_TIMEOUT  ITEST_TIMEOUT for the failure suite (empty: Makefile default)
#   INTEGRATION_PLUGIN_REF  the plugin the suites drive (required)
# Exit: 0 every suite passed, else the first non-zero suite status; 2 refused.
set -uo pipefail

refuse() {
    echo "::error title=integration-suites.sh refused::$*" >&2
    exit 2
}

[ -n "${SUITES_MAIN:-}" ] || refuse "SUITES_MAIN is empty, so this step would run no suite"
case "${SUITES_LOG:-}" in
    ''|*/*|*[!A-Za-z0-9._-]*) refuse "SUITES_LOG '${SUITES_LOG:-}' is not a plain log name" ;;
esac
[ -n "${INTEGRATION_PLUGIN_REF:-}" ] || refuse "INTEGRATION_PLUGIN_REF is empty, so the harness would pick its default plugin"
# An exported ITEST_TIMEOUT, even empty, overrides the Makefile's `?=` for
# both suites; the per-suite inputs are the one way in.
[ -z "${ITEST_TIMEOUT+set}" ] || refuse "ITEST_TIMEOUT is set in the environment; use the main-timeout / failure-timeout inputs"

# The suites need root. The hosted runner is unprivileged with passwordless
# sudo, whose secure_path drops the runner's PATH (setup-go's toolchain) and
# whose env_reset drops everything else, so each variable the suites read is
# handed over by name, and only when set.
# A standing runner (arm64) keeps /tmp between runs; a log left from an
# earlier run must not be summarised as this one's.
rm -f "/tmp/itest-${SUITES_LOG}-main.log" "/tmp/itest-${SUITES_LOG}-failure.log"

prefix=()
if [ "$(id -u)" -ne 0 ]; then
    prefix=(sudo)
fi

run_suite() {
    local name="$1" args="$2" timeout="$3" log="/tmp/itest-${SUITES_LOG}-$1.log"
    local -a argv assign=("PATH=${PATH}" "INTEGRATION_PLUGIN_REF=${INTEGRATION_PLUGIN_REF}")
    local v
    read -ra argv <<< "$args"
    for v in ITEST_HEAP_BOUND_MB NET_DHCP_EXPECT_COMMIT; do
        [ -n "${!v:-}" ] && assign+=("${v}=${!v}")
    done
    [ -n "$timeout" ] && assign+=("ITEST_TIMEOUT=${timeout}")
    echo "::group::${name} suite: make ${argv[*]}"
    ${prefix[@]+"${prefix[@]}"} env "${assign[@]}" make "${argv[@]}" 2>&1 | tee "$log"
    local rc=$?
    echo "::endgroup::"
    echo "${name} suite exit status ${rc}"
    return "$rc"
}

run_suite main "$SUITES_MAIN" "${SUITES_MAIN_TIMEOUT:-}"
rc_main=$?
rc_failure=0
if [ -n "${SUITES_FAILURE:-}" ]; then
    run_suite failure "$SUITES_FAILURE" "${SUITES_FAILURE_TIMEOUT:-}"
    rc_failure=$?
fi

[ "$rc_main" -ne 0 ] && exit "$rc_main"
exit "$rc_failure"
