#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# A DHCP manager registration must never have its result discarded
# (#480).
#
# WHY THIS EXISTS
#
# registerDHCPManager returns the manager it displaced, and its doc
# comment has always said what the caller owes that manager:
#
#     Silently dropping it from the map would leak its running dhcpcd —
#     unstoppable forever, and colliding with the new client on the same
#     interface — so the caller must Stop it.
#
# Join obeys it. The recovery path did not: it called the function as a
# bare statement and threw the return value away. A Join that landed in
# recovery's check-then-register window had its live manager evicted
# from the registry while its dhcpcd kept running — untracked,
# unstoppable, and bidding for the same lease as the client recovery
# then started. The rule was stated on the declaration; the mistake was
# made at a call site three hundred lines away, where nobody reading the
# call would see it.
#
# Go will not help here. An unused return value is legal, so the
# compiler, go vet and staticcheck all pass the bare call. Nothing in
# the toolchain distinguishes "this function's result is advisory" from
# "discarding this result leaks a process".
#
# WHAT IT CHECKS
#
#   1. no call to registerDHCPManager or registerDHCPManagerIfAbsent
#      appears as a bare statement, or under an explicit `_ =`
#   2. registerDHCPManagerIfAbsent holds the lock across its whole body,
#      so the check and the write cannot be split back into two steps
#   3. at least one call site was seen, so a rename cannot turn this
#      into a gate that passes over an empty search
#   4. a manager bound from registerDHCPManager has Stop() called on it
#      in the same function -- the obligation itself, not the proxy (#682)
#
# (4) exists because (1)-(3) check that the result is not DISCARDED,
# which is a proxy. A caller can bind the displaced manager, satisfy all
# three, and stop nothing. Measured by deleting the stop from Join while
# leaving displaced_stops incrementing: the whole pkg/plugin unit suite
# passed, all 53 local-lane checks passed, this gate included. The
# counter an operator reads kept moving and the client kept running.
#
# Both helpers are in scope. IfAbsent returns whether it won rather than
# what it displaced, and a caller that ignores that proceeds as though
# it owns an endpoint somebody else is managing — a different bug with
# the same root, and the same one-character fix.
#
# (2) is here because no test can hold it. A concurrency test against a
# deliberately two-step implementation was written and PASSED, three
# runs out of three with 64 racers: they all queue on the same mutex, so
# the losers' reads happen after the winner's write and the window never
# opens. The property is structural, so it is checked structurally.
#
# Usage: check-manager-registration.sh [<dir>]
# Exit:  0 clean, 1 violation, 2 cannot check.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2
DIR="${1:-pkg/plugin}"
[ -d "$DIR" ] || { echo "check-manager-registration: $DIR is not a directory" >&2; exit 2; }

mapfile -t files < <(find "$DIR" -maxdepth 1 -name '*.go' ! -name '*_test.go' | sort)
if [ "${#files[@]}" -eq 0 ]; then
    echo "::error title=No Go files inspected::check-manager-registration found no production Go files in ${DIR}." >&2
    echo "This gate would otherwise pass having read nothing." >&2
    exit 2
fi

# Comments are stripped before matching: this gate's rule gets explained
# in prose next to the code it governs, and a scan that counted prose
# would flag the explanation of the rule it enforces.
#
# Per file rather than over the concatenation, so a violation is
# reported as file:line an editor can jump to. A line number into a
# merged stream names nothing.
AWK="${AWK:-awk}"
# Separately overridable so the self-test can kill THIS scan alone. Sharing
# one knob with the atomicity scan below would make this exit unobservable:
# the other scan dies on the same binary and reports first.
AWKD="${AWKD:-$AWK}"
calls=0
violations=""
unstopped=""
defs=0
sites=0
for f in "${files[@]}"; do
    stripped=$(sed 's,//.*,,' "$f" 2>/dev/null)
    if [ $? -ne 0 ]; then
        echo "::error title=Registration scan did not run::sed failed on ${f}; that file was not judged." >&2
        echo "This gate would otherwise pass having skipped it." >&2
        exit 2
    fi
    n=$(printf '%s\n' "$stripped" | grep -cE 'registerDHCPManager(IfAbsent)?\(')
    calls=$((calls + n))
    hits=$(printf '%s\n' "$stripped" |
        grep -nE '^[[:space:]]*(_[[:space:]]*=[[:space:]]*)?[A-Za-z_][A-Za-z0-9_]*\.registerDHCPManager(IfAbsent)?\(')
    [ -n "$hits" ] && violations="${violations}$(printf '%s\n' "$hits" | sed "s,^,${f}:,")
"
    # RULE 4: what the caller OWES the manager it displaced (#682).
    #
    # Rules 1-3 check that the result is not DISCARDED. That is a proxy
    # for the obligation, and a proxy is not the property: a caller can
    # bind the displaced manager, satisfy every rule above, and still
    # never stop it. Measured on the shipped tree by deleting the stop
    # from Join while leaving displaced_stops incrementing -- the whole
    # pkg/plugin unit suite passed, and so did all 53 local-lane checks
    # including this gate. Nothing in the repository saw it.
    #
    # So the binding is followed to its use: a variable bound from
    # registerDHCPManager must have Stop() called on it somewhere in the
    # same function. Whether that is direct, deferred or inside a
    # goroutine is the caller's business; that it happens is not.
    #
    # Keyed on the BINDING rather than on one statement shape, so the
    # `x := ...` form and the `if x := ...; x != nil` form are both seen.
    # A rename of Stop reports rather than passes: the gate finds a
    # binding with no matching call and says so.
    unstopped_f=$(printf '%s\n' "$stripped" | $AWKD -v fname="$f" '
        function flush(   v) {
            for (v in bound)
                if (!(v in stopped))
                    printf "%s:%d\t%s\n", fname, bound[v], v
            delete bound; delete stopped
        }
        /^func / { flush() }
        {
            if (match($0, /[A-Za-z_][A-Za-z0-9_]*[ \t]*:=[ \t]*[A-Za-z_][A-Za-z0-9_]*\.registerDHCPManager\(/)) {
                s = substr($0, RSTART, RLENGTH)
                match(s, /^[A-Za-z_][A-Za-z0-9_]*/)
                bound[substr(s, RSTART, RLENGTH)] = FNR
            }
            if (match($0, /[A-Za-z_][A-Za-z0-9_]*\.Stop\(/)) {
                s = substr($0, RSTART, RLENGTH)
                match(s, /^[A-Za-z_][A-Za-z0-9_]*/)
                stopped[substr(s, RSTART, RLENGTH)] = 1
            }
        }
        END { flush() }
    ')
    awk_status=$?
    if [ "$awk_status" -ne 0 ]; then
        echo "::error title=Displacement scan did not run::${AWKD} exited ${awk_status} on ${f}; that file's displaced managers were not judged." >&2
        echo "This gate would otherwise pass having inspected nothing." >&2
        exit 2
    fi
    [ -n "$unstopped_f" ] && unstopped="${unstopped}${unstopped_f}
"
    # The domain of rule 4, counted so it cannot be emptied in silence.
    # A tree that DEFINES registerDHCPManager and calls it nowhere has
    # either lost the displacement path or renamed around this gate;
    # either way rule 4 has become a universal over nothing.
    defs=$((defs + $(printf '%s\n' "$stripped" | grep -cE '^func \([^)]*\) registerDHCPManager\(')))
    sites=$((sites + $(printf '%s\n' "$stripped" | grep -cE '\.registerDHCPManager\(')))
done

# Seen at all? A rename that this gate does not follow must not read as
# compliance. Counted with grep -c above, never `grep -q`: -q exits at
# the first match and SIGPIPEs the producer, which under pipefail turns
# a successful find into a failure.
if [ "$calls" -eq 0 ]; then
    echo "::error title=No registrations found::check-manager-registration matched no call to registerDHCPManager in ${DIR}." >&2
    echo "Either the helper was renamed and this gate was not, or it is judging the wrong tree." >&2
    exit 2
fi

# A binding call reads `x := p.register...`, `if ok := p.register...`,
# `if !p.register...`. A discarding one starts the statement with the
# call itself, or launders it through the blank identifier.
# The atomicity of the compare-and-set. One Lock, an unlock that is
# deferred, and nothing in between that hands the mutex back: any other
# shape is a read and a later write with a window between them, which is
# precisely the bug (1) exists to prevent, reintroduced inside the
# helper meant to prevent it.
#
# Scanned with awk over the function body, and the pattern is an awk
# regex LITERAL rather than a -v assignment: POSIX leaves escapes in -v
# undefined, some awks strip the backslash, and a pattern that then
# fails to compile kills awk and reports the tree clean. The exit status
# is checked below for the same reason.
atomicity=$($AWK '
    /^func \(p \*Plugin\) registerDHCPManagerIfAbsent\(/ { infunc = 1; locks = 0; bare = 0; next }
    infunc && /^}/ {
        if (locks != 1 || bare != 0)
            printf "%s\tlock()=%d bare-unlock()=%d\n", FILENAME, locks, bare
        infunc = 0; next
    }
    infunc {
        if ($0 ~ /p\.mu\.Lock\(\)/) locks++
        if ($0 ~ /p\.mu\.Unlock\(\)/ && $0 !~ /defer/) bare++
    }
' "${files[@]}")
awk_status=$?
if [ "$awk_status" -ne 0 ]; then
    echo "::error title=Atomicity scan did not run::${AWK} exited ${awk_status}; the compare-and-set was not judged." >&2
    echo "This gate would otherwise pass having inspected nothing." >&2
    exit 2
fi
if [ -n "$atomicity" ]; then
    echo "FAIL  registerDHCPManagerIfAbsent does not hold the lock across its body:" >&2
    printf '%s\n' "$atomicity" | sed 's/^/  /' >&2
    echo "  A second Lock, or an Unlock that is not deferred, splits the check" >&2
    echo "  from the write. A Join landing in that window keeps its manager in" >&2
    echo "  the map only until this overwrite lands on top of it." >&2
    exit 1
fi

if [ "$defs" -gt 0 ] && [ "$sites" -eq 0 ]; then
    echo "::error title=No displacement site::registerDHCPManager is defined in ${DIR} but called nowhere." >&2
    echo "Rule 4 below would be a universal over an empty set: either the displacement" >&2
    echo "path was removed, or a call site was renamed around this gate." >&2
    exit 2
fi

if [ -n "$unstopped" ]; then
    echo "FAIL  a displaced manager is bound and never stopped:" >&2
    printf '%s' "$unstopped" | sed '/^$/d;s/^/  /' >&2
    echo "  The variable named above holds the manager this registration displaced." >&2
    echo "  Binding it satisfies the discard rule and stops nothing: its dhcpcd keeps" >&2
    echo "  running, untracked, bidding for the same lease as the client that just" >&2
    echo "  replaced it. Call Stop() on it -- directly, deferred or on a goroutine." >&2
    exit 1
fi

if [ -n "$violations" ]; then
    echo "FAIL  a manager registration discards its result:" >&2
    printf '%s' "$violations" | sed '/^$/d;s/^/  /' >&2
    echo "  registerDHCPManager returns the manager it displaced; dropping it leaks" >&2
    echo "  a running dhcpcd that nothing can stop and that competes for the same" >&2
    echo "  lease. Stop it the way Join does, or register through" >&2
    echo "  registerDHCPManagerIfAbsent and yield when it reports false." >&2
    exit 1
fi

echo "check-manager-registration: OK (${calls} call site(s), none discarding; ${sites} displacement site(s), all stopped)"
