#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Plugin.mu and the tombstone store's lock must never be held together
# (#643).
#
# WHY THIS EXISTS
#
# The rule was written down as a comment on the mutex declaration for
# most of this project's life:
#
#     // Held only across that small operation; never combined with mu
#     // so the two locks can't deadlock against each other.
#
# That is a real invariant with a real consequence — combining them in
# opposite orders on two paths deadlocks the plugin, and a deadlocked
# network driver takes every `docker run` on the host with it. But a
# comment is checked only by whoever happens to read it, and it sits on
# the DECLARATION, not on the call sites where the mistake is actually
# made. Somebody adding a tombstone lookup inside an existing
# `p.mu.Lock()` section would never see it.
#
# Moving the lock into tombstoneStore did not fix that: Go's unexported
# identifiers are package-visible, so the field is reachable from every
# file in the package exactly as before. The type buys readability; this
# script is what buys enforcement.
#
# WHAT IT CHECKS
#
# No function that takes Plugin.mu may call a tombstone entry point.
#
# The rule is deliberately coarser than the invariant: it forbids the
# call anywhere in a function that locks p.mu, rather than only between
# Lock and Unlock. That is because `defer p.mu.Unlock()` — which is how
# nearly every one of these is written — holds the lock to the end of the
# function, so "after the Unlock" is not a thing a reader can rely on.
# Being coarse costs nothing here and removes the judgement call.
#
# WHAT IT CANNOT DO
#
# It is textual, not a call graph. A function that locks p.mu and calls a
# helper that calls a tombstone entry point is NOT caught. Said out loud
# because "lock discipline is gated" invites the belief that it is gated
# completely; this closes the direct case, which is the one that has
# actually shown up.
#
# Usage: check-lock-discipline.sh [<dir>]
# Exit: 0 clean, 1 violation, 2 cannot check.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2
DIR="${1:-pkg/plugin}"
[ -d "$DIR" ] || { echo "check-lock-discipline: $DIR is not a directory" >&2; exit 2; }

# The awk binary is indirected so the self-test can prove what this
# gate does when its own engine dies. See the exit-status check below.
AWK="${AWK:-awk}"

mapfile -t files < <(find "$DIR" -maxdepth 1 -name '*.go' ! -name '*_test.go' | sort)
if [ "${#files[@]}" -eq 0 ]; then
    echo "::error title=No Go files inspected::check-lock-discipline found no production Go files in ${DIR}." >&2
    echo "This gate would otherwise pass having read nothing." >&2
    exit 2
fi

# Scan function by function. Production Go here is gofmt'd, so a
# top-level func starts at column 0 and its closing brace is a bare "}".
#
# The tombstone entry points — the store's own methods and the Plugin
# wrappers around them — are written as an awk regex LITERAL, not passed
# in with -v. POSIX leaves undefined escape sequences in a -v assignment
# undefined: some awks keep `\.`, others strip the backslash. Stripped,
# this pattern's `\(` becomes an unbalanced `(` and the regex no longer
# compiles, so awk dies and the gate reports the repository clean. That
# is exactly what happened — it passed on the author's machine and let a
# planted violation through on CI, in the direction that does not fail
# loudly. A regex literal is read by the awk parser and never goes
# through -v processing.
violations=$($AWK '
    /^func / { infunc = 1; body = ""; fname = $0; next }
    infunc && /^}/ {
        if (body ~ /\.mu\.Lock\(\)/ &&
            body ~ /p\.tombstones\.(add|consume)\(|p\.(addTombstone|consumeTombstone)\(/) {
            printf "%s:%d\t%s\n", FILENAME, start, fname
        }
        infunc = 0; next
    }
    infunc {
        if (body == "") start = FNR
        body = body "\n" $0
    }
' "${files[@]}")
awk_status=$?

# A gate whose engine failed has not found nothing — it has found out
# nothing. Without this, a broken regex, an unreadable file or a missing
# awk all render as an empty $violations and a PASS.
if [ "$awk_status" -ne 0 ]; then
    echo "::error title=Lock-discipline scan did not run::${AWK} exited ${awk_status}; no file was judged." >&2
    echo "This gate would otherwise pass having inspected nothing." >&2
    exit 2
fi

# The store itself locks its own mu and does not touch Plugin — assert
# that structurally, because the whole separation rests on it.
# Comments are stripped first: this file's own documentation discusses
# `p.tombstoneMu` and `p.tombstones.mu` by name, and a gate that counted
# prose would flag the explanation of the rule it enforces.
# `grep -q` is deliberately not used: it exits at the first match and
# SIGPIPEs the producing sed, which under `set -o pipefail` reports
# failure on a *successful* find. Redirect instead — grep reads to EOF,
# so the exit status is the real one.
if [ -f "$DIR/tombstone_store.go" ] \
   && sed 's,//.*,,' "$DIR/tombstone_store.go" | grep -E '\bp\.' >/dev/null; then
    echo "FAIL  tombstone_store.go references a Plugin receiver." >&2
    echo "  The store must not reach back into Plugin: if it can take p.mu," >&2
    echo "  the two locks are one lock order away from deadlocking." >&2
    exit 1
fi

if [ -n "$violations" ]; then
    echo "FAIL  a function holding Plugin.mu calls into the tombstone store:" >&2
    printf '  %s\n' "$violations" >&2
    echo >&2
    echo "  Plugin.mu and the tombstone lock must never be held together." >&2
    echo "  Taking them in opposite orders on two paths deadlocks the plugin," >&2
    echo "  and a deadlocked network driver takes every docker run with it." >&2
    echo "  Read the tombstone value out before locking, or drop p.mu first." >&2
    exit 1
fi

echo "PASS  no function holds Plugin.mu across a tombstone store call ($(printf '%s\n' "${files[@]}" | wc -l) file(s))"
