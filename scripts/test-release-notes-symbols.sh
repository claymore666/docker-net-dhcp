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
elif ! printf '%s' "$OUT" | grep 'notInvented' >/dev/null; then
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
elif ! printf '%s' "$OUT" | grep 'noteDNSPropagationPIDMismatch' >/dev/null; then
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
# ALSO THE POSITIVE CONTROL for the stale-waiver check below: every
# waiver here is live, and this case must stay GREEN. Without it, a
# stale-waiver check that fired on everything would look identical to
# one that works -- one assertion alone is satisfied by a gate that
# never looks.
printf '# HISTORICAL - renamed by abc1234, the sentence records the old name\nnotInvented\n' > "$WORK/waivers.txt"
run "$WORK/bad.md"
if [ "$RC" != 0 ]; then
    no "a waived symbol still failed (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep 'Waived' >/dev/null; then
    no "the waiver was honoured silently -- a waiver nobody can see is a hole: $OUT"
else
    ok "a waived symbol passes and the waiver is printed"
fi
: > "$WORK/waivers.txt"

# --- the resolved SHA appears on the pass line AND on the error ------
# A verdict with no tree is the defect this gate exists to prevent,
# applied to the gate itself.
run "$WORK/ok.md"
printf '%s' "$OUT" | grep -E 'at [0-9a-f]{7,}|at unknown-tree' >/dev/null \
    && ok "the pass line names the tree it resolved against" \
    || no "a green verdict with no SHA is unfalsifiable: $OUT"

run "$WORK/bad.md"
printf '%s' "$OUT" | grep -E 'at [0-9a-f]{7,}|at unknown-tree' >/dev/null \
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
elif ! printf '%s' "$OUT" | grep '0 candidate' >/dev/null; then
    no "expected zero candidates from prose, got: $OUT"
else
    ok "backticked prose without a camelCase hump is not a candidate"
fi

# =====================================================================
# THE WAIVER FILE'S OWN THREE RULES.
#
# All three were stated in that file's header and enforced by nothing.
# The gate they belong to exists because prose decays, and its rules
# were prose that nothing read -- so these cases are the gate applied to
# itself, and each one is paired with the control that stops it passing
# for the wrong reason.
# =====================================================================

# --- rule 1: an entry needs a category, and a bare token has none -----
printf 'notInvented\n' > "$WORK/waivers.txt"
run "$WORK/bad.md"
if [ "$RC" != 1 ]; then
    no "a bare token was accepted as a waiver (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep 'bare token' >/dev/null; then
    no "a bare token failed for some other reason -- redness without attribution: $OUT"
else
    ok "rule 1: a bare token is not a waiver"
fi

# A reason that is prose but names no category is the same hole with
# something written in it, which is the shape a suppression actually
# takes: nobody writes nothing, they write "the gate complained".
printf '# the gate complained about this one\nnotInvented\n' > "$WORK/waivers.txt"
run "$WORK/bad.md"
[ "$RC" = 1 ] && ok "rule 1: a reason with no category is not a waiver" \
              || no "a categoryless reason was accepted (exit $RC): $OUT"

# CONTROL for rule 1, and it is the case the first implementation got
# wrong. A bare `#` in the real waiver file separates entries AND breaks
# paragraphs INSIDE one reason, so a rule keyed on "the comment run
# directly above the token" called the file's most carefully written
# entry a bare token. The category line marks the entry; distance does
# not.
printf '# HISTORICAL - renamed by abc1234\n#\n#   A second paragraph, about something else entirely.\nnotInvented\n' \
    > "$WORK/waivers.txt"
run "$WORK/bad.md"
if [ "$RC" != 0 ]; then
    no "a reason with a second paragraph was read as a bare token (exit $RC): $OUT"
else
    ok "rule 1 control: a category line still owns its entry across a blank comment"
fi

# --- rule 2: a waiver that cannot fire -------------------------------
# `lastIPv6` RESOLVES in the fixture, so this waiver is unreachable. The
# old gate consulted the map only after resolution failed, which is why
# the one entry that can never fire was the one nothing could see.
printf '# HISTORICAL - this one is stale\nlastIPv6\n' > "$WORK/waivers.txt"
run "$WORK/ok.md"
if [ "$RC" != 1 ]; then
    no "a waiver for a RESOLVING symbol was accepted (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep 'lastIPv6' >/dev/null; then
    no "stale waiver flagged without naming the symbol: $OUT"
else
    ok "rule 2: a waiver for a symbol that resolves is stale and fails"
fi

# The quieter half: the sentence the waiver covered was deleted, so
# nothing resolves it, nothing fails, and it sits there forever.
printf '# HISTORICAL - covers a sentence that no longer exists\nvanishedSymbol\n' > "$WORK/waivers.txt"
run "$WORK/ok.md"
if [ "$RC" != 1 ]; then
    no "a waiver for a symbol the notes never mention was accepted (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep 'vanishedSymbol' >/dev/null; then
    no "stale-absent waiver flagged without naming the symbol: $OUT"
else
    ok "rule 2: a waiver the notes no longer mention is stale and fails"
fi

# --- rule 3: the section being written cannot be waived ---------------
# The notes are newest-first, so the first `## vX.Y.Z` is the section
# under construction. A frozen section describes the tree as it was and
# may legitimately name a symbol that is gone; the current one describes
# the tree as it is, so a waiver there silences a false claim in the
# only place this gate was written to catch one.
cat > "$WORK/sections.md" <<'MD'
# Release notes

## v9.9.0

The new helper is `notInvented` and it does the work.

## v9.8.0

Back then the helper was `alsoInvented`.
MD
printf '# HISTORICAL - both, deliberately\nnotInvented\nalsoInvented\n' > "$WORK/waivers.txt"
run "$WORK/sections.md"
if [ "$RC" != 1 ]; then
    no "a symbol in the CURRENT section was waived (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep 'notInvented' >/dev/null; then
    no "the current-section refusal did not name the symbol: $OUT"
elif printf '%s' "$OUT" | grep 'current release section' >/dev/null; then
    ok "rule 3: a waiver does not reach the section being written"
else
    no "failed, but not for the current-section reason: $OUT"
fi

# CONTROL for rule 3. Same waiver file, same symbols, and the ONLY
# difference is which section names them. Without this the rule is
# satisfied by a gate that refuses every waiver.
cat > "$WORK/frozen.md" <<'MD'
# Release notes

## v9.9.0

Nothing here names a symbol.

## v9.8.0

Back then the helpers were `notInvented` and `alsoInvented`.
MD
run "$WORK/frozen.md"
if [ "$RC" != 0 ]; then
    no "waivers in a FROZEN section were refused (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep 'HISTORICAL' >/dev/null; then
    no "the waiver was honoured without printing its category: $OUT"
else
    ok "rule 3 control: a frozen section may still be waived"
fi
: > "$WORK/waivers.txt"

# --- rule 3, defeated by one extra dot -------------------------------
# THE SAME FACT DERIVED TWICE, TWO WAYS, TWO ANSWERS -- in a gate whose
# whole subject is claims that do not survive re-derivation.
#
#   candidate extraction   sed 's/^.*\.//'                 ANY chain
#   sym_lines              "`([A-Za-z_][A-Za-z0-9_]*\.)?"  exactly ONE
#
# So a candidate could exist that sym_lines could not locate.
# in_current_section then answered "no" for a symbol that IS in the
# current section, and the waiver applied: a fiction in the section
# being written, silenced, by one extra dot.
#
# Not reachable on the release notes as they stand -- all 111 current
# candidates are locatable and the seven multi-dot spans produce no
# candidates at all. It is kept as a case because latent is not fixed,
# and `m.plugin.spawnOrphanRelease` is the shape these notes reach for
# constantly.
cat > "$WORK/twodot.md" <<'MD'
# Release notes

## v9.9.0

The current section names `plugin.Plugin.notInvented` and nothing else.

## v9.8.0

Back then it was `alsoInvented`.
MD
printf '# HISTORICAL - deliberately, to drive the bypass\nnotInvented\nalsoInvented\n' \
    > "$WORK/waivers.txt"
run "$WORK/twodot.md"
if [ "$RC" != 1 ]; then
    no "a two-dot receiver let a waiver reach the CURRENT section (exit $RC): $OUT"
elif ! printf '%s' "$OUT" | grep 'current release section' >/dev/null; then
    no "the two-dot case failed for some other reason: $OUT"
else
    ok "rule 3: a multi-component receiver does not bypass the current section"
fi

# The same cause with the quieter symptom. sym_lines finding nothing
# meant the fallback line number, so the reader was pointed at line 1 of
# a 3000-line file -- a red that names the wrong place, which is the
# same currency as a red that names the wrong remedy.
: > "$WORK/waivers.txt"
run "$WORK/twodot.md"
if [ "$RC" != 1 ]; then
    no "an unwaived two-dot fiction was accepted (exit $RC): $OUT"
elif printf '%s' "$OUT" | grep 'line=5' >/dev/null; then
    ok "a multi-component receiver is reported at its own line, not line 1"
else
    # Every line= in the output, not the first: the other candidate in
    # this fixture legitimately reports line=9, so showing one number
    # would point a reader at the wrong symbol -- the same fault the
    # case is about.
    no "two-component receiver: no line=5 among $(printf '%s' "$OUT" | grep -o 'line=[0-9]*' | tr '\n' ' '): $OUT"
fi
: > "$WORK/waivers.txt"

# --- the shape the widened class must PRESERVE ------------------------
# The fix above widened the receiver class from [A-Za-z0-9_] to
# [A-Za-z0-9_.]. That is only correct if the SINGLE-component receiver
# still resolves -- the common shape in these notes, and the one with no
# case until now. Breaking it is the quiet failure: sym_lines finds
# nothing, so the fallback line number is reported AND in_current_section
# answers "no", which is the same bypass the two-dot case exists for, in
# its more frequent form.
cat > "$WORK/onedot.md" <<'MD'
# Release notes

## v9.9.0

The current section names `plugin.notInvented` and nothing else.
MD
: > "$WORK/waivers.txt"
run "$WORK/onedot.md"
if [ "$RC" != 1 ]; then
    no "an unwaived one-dot fiction was accepted (exit $RC): $OUT"
elif printf '%s' "$OUT" | grep 'line=5' >/dev/null; then
    ok "a single-component receiver still resolves to its own line"
else
    no "single-component receiver: no line=5 among $(printf '%s' "$OUT" | grep -o 'line=[0-9]*' | tr '\n' ' '): $OUT"
fi
: > "$WORK/waivers.txt"

# --- the third shape the comment at :221-223 names ---------------------
# That comment enumerates what sym_lines accepts: a bare name, a trailing
# (), and a receiver prefix. An enumeration sitting three lines above the
# regex is a checklist nobody runs -- until this PR there was a fixture
# for the bare name only, and the two shapes with no case were the two
# that could be dropped with the whole suite green.
#
# One fixture per named shape, and each has to die to its own mutant:
# deleting `(\(\))?` from the re must redden exactly this case.
cat > "$WORK/parens.md" <<'MD'
# Release notes

## v9.9.0

The current section names `notInvented()` and nothing else.
MD
: > "$WORK/waivers.txt"
run "$WORK/parens.md"
if [ "$RC" != 1 ]; then
    no "an unwaived fiction written with () was accepted (exit $RC): $OUT"
elif printf '%s' "$OUT" | grep 'line=5' >/dev/null; then
    ok "a trailing () still resolves to its own line"
else
    no "trailing (): no line=5 among $(printf '%s' "$OUT" | grep -o 'line=[0-9]*' | tr '\n' ' '): $OUT"
fi
: > "$WORK/waivers.txt"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
