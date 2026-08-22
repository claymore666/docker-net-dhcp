#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-release-notes-symbols.sh.
#
# ATTRIBUTION, NOT JUST REDNESS. Every failing case asserts that the
# message NAMES the offending symbol. A gate that exits 1 for an
# unrelated reason -- a missing file, a broken corpus, a shell error --
# looks identical to a gate that caught the defect, and a suite that
# only checks the exit code scores both as working. That distinction is
# the whole reason this check exists: the paragraph it was written for
# was a true statement about the wrong object.
#
# The corpus seam is SYMBOL_SOURCES, so the cases run against a handful
# of fixture files rather than the repository, and the expected answers
# are readable in the fixture itself.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-release-notes-symbols.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

WORK=$(mktemp -d) || exit 2
trap 'rm -rf "$WORK"' EXIT

cat > "$WORK/src.go" <<'GO'
package fixture

// noteDNSPropagationPIDMismatch is named in a COMMENT here and nowhere
// else in code -- the case that made the check strip comments.
type manager struct {
	lastIP   int
	lastIPv6 int
}

func (m *manager) setLastIP(v int) {
	m.lastIP = v
	// A string containing a slash-slash must not open a comment, and a
	// comment containing a quote must not open a string:
	_ = "https://example.invalid/a\"b"
	_ = `a raw string with backtickish // content`
}
GO

# run <notes-file> -- sets OUT and RC in THIS shell.
#
# Deliberately not `out=$(run ...)`: a command substitution is a
# subshell, so an RC assigned inside it never reaches the caller, and
# under `set -u` the first read of RC aborts the suite. The first
# version of this file did that and died on line 63 rather than
# reporting a result.
run() {
    OUT=$(SYMBOL_SOURCES="$WORK/src.go" SYMBOL_WAIVERS="$WORK/waivers.txt" \
          bash "$GATE" "$1" 2>&1)
    RC=$?
}

: > "$WORK/waivers.txt"

# --- a symbol that resolves ------------------------------------------
cat > "$WORK/ok.md" <<'MD'
The manager keeps `lastIPv6` and updates it in `setLastIP`.
MD
run "$WORK/ok.md"
[ "$RC" = 0 ] && ok "a symbol the tree defines passes" \
              || no "expected exit 0 for a resolvable symbol, got $RC: $OUT"

# --- THE ONE THAT MATTERS: an invented symbol -------------------------
# Exit 1 AND the message names the symbol. Redness alone is not a pass:
# a gate that died on a missing corpus would also exit non-zero.
cat > "$WORK/bad.md" <<'MD'
The manager keeps `lastIPv6`, and `notInvented` does the work.
MD
run "$WORK/bad.md"
if [ "$RC" != 1 ]; then
    no "an invented symbol did not exit 1 (got $RC): $OUT"
elif ! printf '%s' "$OUT" | grep -q 'notInvented'; then
    no "exit 1, but the message does not name notInvented -- redness without attribution: $OUT"
else
    ok "an invented symbol exits 1 and the message names it"
fi

# --- comments do not resolve a symbol --------------------------------
# noteDNSPropagationPIDMismatch appears in src.go, in a comment only.
# A grep-based resolver passes this; that is the defect being closed.
cat > "$WORK/comment.md" <<'MD'
Both call sites were extracted to `noteDNSPropagationPIDMismatch`.
MD
run "$WORK/comment.md"
if [ "$RC" != 1 ]; then
    no "a symbol present only in a COMMENT was accepted (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep -q 'noteDNSPropagationPIDMismatch'; then
    no "flagged, but did not name the comment-only symbol: $OUT"
else
    ok "a symbol present only in a comment does not resolve"
fi

# --- a string literal does not resolve a symbol either ---------------
cat > "$WORK/str.md" <<'MD'
The URL helper is `exampleInvalid`.
MD
run "$WORK/str.md"
[ "$RC" = 1 ] && ok "a token that only occurs inside a string literal does not resolve" \
              || no "expected exit 1 for a string-only token, got $RC: $OUT"

# --- the stripper must not EAT code ----------------------------------
# If // stripping ran before string stripping, "https://..." would leave
# an unterminated literal and swallow setLastIP. This is the case that
# caught a real defect: the first corpus builder slurped every file into
# one perl string and lost half the tree.
cat > "$WORK/after.md" <<'MD'
The setter is `setLastIP` and the field is `lastIP`.
MD
run "$WORK/after.md"
[ "$RC" = 0 ] && ok "code following a URL-bearing string is still in the corpus" \
              || no "the stripper ate code after a // inside a string: $OUT"

# --- a waiver silences it, and is reported ---------------------------
printf '# a reason, as the format requires\nnotInvented\n' > "$WORK/waivers.txt"
run "$WORK/bad.md"
if [ "$RC" != 0 ]; then
    no "a waived symbol still failed (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep -q 'Waived'; then
    no "the waiver was honoured silently -- a waiver nobody can see is a hole: $OUT"
else
    ok "a waived symbol passes and the waiver is printed"
fi
: > "$WORK/waivers.txt"

# --- the resolved SHA appears on the pass line AND on the error ------
# A verdict with no tree is the defect this gate exists to prevent,
# applied to the gate itself.
run "$WORK/ok.md"
printf '%s' "$OUT" | grep -qE 'at [0-9a-f]{7,}|at unknown-tree' \
    && ok "the pass line names the tree it resolved against" \
    || no "a green verdict with no SHA is unfalsifiable: $OUT"

run "$WORK/bad.md"
printf '%s' "$OUT" | grep -qE 'at [0-9a-f]{7,}|at unknown-tree' \
    && ok "the failure names the tree it resolved against" \
    || no "a failing verdict with no SHA: $OUT"

# --- an empty corpus must NOT pass -----------------------------------
# The empty-glob trap: nothing to contradict a symbol is not the same as
# every symbol being fine.
empty=$(SYMBOL_SOURCES="$WORK/no-such-dir/*.go" SYMBOL_WAIVERS="$WORK/waivers.txt" \
      bash "$GATE" "$WORK/bad.md" 2>&1)
rc=$?
[ "$rc" = 2 ] && ok "an empty corpus exits 2, it does not pass everything" \
              || no "an empty corpus returned $rc instead of 2: $empty"

# --- a missing notes file --------------------------------------------
absent=$(SYMBOL_SOURCES="$WORK/src.go" bash "$GATE" "$WORK/absent.md" 2>&1)
rc=$?
[ "$rc" = 2 ] && ok "a missing notes file exits 2" \
              || no "a missing notes file returned $rc: $absent"

# --- prose is not a symbol -------------------------------------------
# The candidate filter needs an inner lower->upper transition, or every
# backticked word in the file becomes a candidate.
cat > "$WORK/prose.md" <<'MD'
Set `bridge` and `renew` and `dhcp_timeouts_v4` in the options.
MD
run "$WORK/prose.md"
if [ "$RC" != 0 ]; then
    no "ordinary backticked prose was treated as symbols: $OUT"
elif ! printf '%s' "$OUT" | grep -q '0 candidate'; then
    no "expected zero candidates from prose, got: $OUT"
else
    ok "backticked prose without a camelCase hump is not a candidate"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
