#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# A /proc/<pid> path must never be built as a string outside the one
# function that revalidates the PID first (#688, and the netns sibling).
#
# WHY THIS EXISTS
#
# The plugin runs in the HOST PID namespace. Every PID it gets from
# Docker is stale the moment it arrives: the container can exit, and the
# kernel can hand that number to an unrelated host task. #688 closed
# that for resolv.conf with openContainerProc, which reads the target's
# cgroup, confirms it names the expected container, and returns a
# DIRECTORY FD -- procfs invalidates a /proc/<pid> dentry when the task
# exits, so every openat below that fd either reaches the same task or
# fails with ESRCH.
#
# openContainerProc's own comment states the rule this script enforces:
#
#     Re-deriving the path as a string afterwards would reopen the
#     window the check just closed.
#
# It was reopened anyway, in the same release, three files away:
# dhcp_manager.go built "/proc/%v/ns/net" from the same Docker-supplied
# PID and handed it to netns.GetFromPath -- twice, independently, so the
# two resolutions could even land in different namespaces. The sink
# there is not one file: it is a netlink handle carrying every address,
# MTU and route the manager applies, with CAP_NET_ADMIN, plus a root
# dhcpcd spawned into the namespace.
#
# The fix reached one call site. Nothing stopped the next one. That is
# what this script is for: the comment explains the rule to whoever
# reads that function, and the gate applies it to everyone who does not.
#
# WHAT IT CHECKS
#
# No Go file under pkg/ or cmd/ may compose a "/proc/<something>" path
# through a format string or concatenation, except:
#
#   - openContainerProc itself, which is where the check lives.
#   - /proc/self/... paths, which name the calling process. There is no
#     recycled-PID hazard in asking about yourself.
#
# WHAT IT CANNOT DO
#
# It sees string construction, not intent: a PID smuggled through a
# variable and joined with filepath.Join would pass. It is a tripwire on
# the shape the mistake has actually taken twice, not a proof.
#
# Usage: check-proc-path-discipline.sh [<tree>]
# Exit: 0 clean, 1 violation, 2 cannot check.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2
cd "${1:-.}" || exit 2

[ -d pkg ] || { echo "check-proc-path-discipline: no pkg/ directory here" >&2; exit 2; }

# The one function allowed to build the path, and the file it lives in.
ALLOWED_FUNC='openContainerProc'
ALLOWED_FILE='pkg/plugin/resolvconf.go'

[ -f "$ALLOWED_FILE" ] || {
    echo "check-proc-path-discipline: $ALLOWED_FILE does not exist -- has $ALLOWED_FUNC moved?" >&2
    exit 2
}
# Anchored and with the paren: "func openContainerProcMoved(" must not
# satisfy a check that the guard is still here. (The meta-test caught
# exactly that, with a bare substring match.)
grep -qE "^func $ALLOWED_FUNC\\(" "$ALLOWED_FILE" || {
    echo "check-proc-path-discipline: $ALLOWED_FUNC is not in $ALLOWED_FILE any more." >&2
    echo "  This gate's whole exemption is 'the function that revalidates the PID'." >&2
    echo "  Point ALLOWED_FILE at wherever it lives now rather than deleting the check." >&2
    exit 2
}

# Every /proc path built from something that is not a literal. Test
# files are included on purpose: a test that reaches a live /proc path
# by PID is doing the unsafe thing to prove something, and should say so
# with an explicit allow comment.
hits=$(grep -rnE '"/proc/(%[a-z]|" *\+)' --include='*.go' pkg cmd 2>/dev/null || true)

fail=0
while IFS= read -r line; do
    [ -n "$line" ] || continue
    file=${line%%:*}
    rest=${line#*:}
    lineno=${rest%%:*}
    code=${line#*:*:}

    # /proc/self is always fine: no other task can be at "self".
    case "$code" in
        *'"/proc/self'*) continue ;;
    esac

    # A test's failure MESSAGE is not a path. t.Fatalf("/proc/%d/... is
    # empty") opens nothing, and demanding an allow comment on every one
    # of those teaches the next reader to wave this gate through.
    case "$code" in
        *t.Fatalf*|*t.Errorf*|*t.Logf*|*t.Skipf*) continue ;;
    esac

    # The revalidating function itself, and the one line inside it.
    if [ "$file" = "$ALLOWED_FILE" ]; then
        # Confirm the hit really is inside openContainerProc rather than
        # anywhere else in the same file: take the last func declaration
        # at or above this line.
        owner=$(head -n "$lineno" "$file" | grep -E '^func ' | tail -1)
        case "$owner" in
            *"$ALLOWED_FUNC"*) continue ;;
        esac
    fi

    # An explicit, deliberate exemption anywhere in the comment block
    # directly above. The marker sits at the top of a paragraph that
    # says WHY, so looking only at the immediately preceding line would
    # push the reason away from the marker.
    prev=$((lineno - 1))
    # grep without -q on purpose: -q exits at the first match, the
    # SIGPIPE kills the producer, and under pipefail the pipeline then
    # reports failure ON SUCCESS. Redirecting reads to EOF instead.
    if [ "$prev" -ge 1 ] && head -n "$prev" "$file" | tac | awk '/^[[:space:]]*\/\//{print;next}{exit}' \
        | grep 'proc-path-discipline: allow' >/dev/null; then
        continue
    fi

    if [ "$fail" -eq 0 ]; then
        echo "FAIL  a /proc/<pid> path is built as a string outside $ALLOWED_FUNC:" >&2
    fi
    fail=1
    echo "  $file:$lineno: $(echo "$code" | sed 's/^[[:space:]]*//')" >&2
done <<< "$hits"

if [ "$fail" -ne 0 ]; then
    cat >&2 <<'MSG'

  The plugin shares the host PID namespace, so a PID from Docker names
  an arbitrary task by the time it is used. Open the process once with
  openContainerProc(pid, ctrID) -- which confirms the cgroup names the
  container and returns a directory fd procfs pins to that task -- then
  reach everything else with unix.Openat below that fd.

  Pass the DESCRIPTOR onward, never a path to be resolved again: two
  resolutions of the same string can disagree, which is exactly how the
  sandbox netns open was reopened after #688 closed it for resolv.conf.

  If a line genuinely must build the path (a test proving the hazard),
  put "proc-path-discipline: allow" in a comment on the line above and
  say why.
MSG
    exit 1
fi

echo "PASS  no /proc/<pid> paths are built outside $ALLOWED_FUNC"
