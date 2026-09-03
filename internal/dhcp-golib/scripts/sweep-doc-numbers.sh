#!/usr/bin/env bash
# Enumerate every bare number in the project's prose, and refuse the ones an
# instrument owns.
#
# B13 (round 9) swept README.md and docs/*.md for numbers that an instrument
# recomputes, and deleted thirteen of them: a number that is not written cannot
# drift, which is the only termination argument that does not rest on somebody
# re-reading. Round 10 found two defects in the METHOD rather than the result,
# and this file answers both.
#
#   1. The sweep was a command in a handover document. Nothing re-ran it, so
#      round 9 wrote a fresh derived number into docs/gates.md on the same day
#      it removed thirteen. "Closed by removal" is a claim about the future and
#      needs something that goes red.
#   2. Its date filter was `grep -vE '2026-..-..'`, which drops the whole LINE.
#      A number sharing a line with a date was invisible to it — and a figure
#      beside its date is exactly how this project is asked to write a
#      measurement. The domain excluded the thing the sweep was sweeping for.
#      Here the date, RFC and section TOKENS are blanked and the line is kept.
#
# Usage:
#   sweep-doc-numbers.sh            enumerate the population, one line each
#   sweep-doc-numbers.sh --check    exit 1 if a removed number came back
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FILES=(README.md docs/*.md)

# A line carries a bare number if, once the tokens that legitimately contain
# digits are blanked, a run of digits survives with a non-identifier character
# before it. Ordinals at the head of a numbered list are not counts.
population() {
	local f
	for f in "${FILES[@]}"; do
		[ -f "$f" ] || continue
		awk -v F="$f" '
		{
			s = $0
			gsub(/20[0-9][0-9]-[0-9][0-9]-[0-9][0-9]/, "", s)
			gsub(/RFC ?[0-9]+/, "", s)
			gsub(/§[0-9]+(\.[0-9]+)*/, "", s)
			# ROUND 13. A round number is an identifier of a review round, the
			# way a date is an identifier of a day: no instrument recomputes
			# it, and it cannot go stale. Blanked as a TOKEN, not by dropping
			# the line — the round-10 lesson holds, and a derived number
			# sharing a line with "round 11" stays visible.
			gsub(/[Rr]ound [0-9]+/, "", s)
			if (s ~ /^ *[0-9]+\. /) next
			if (s ~ /[^A-Za-z0-9_.-][0-9]{1,6}( |$|[),.;:\/])/) printf "%s:%d: %s\n", F, NR, $0
		}' "$f"
	done
}

# The thirteen removals of round 9, as patterns. Each one is a number an
# instrument recomputes on every run; writing it in prose is what the sweep
# removed, so writing it again must go red rather than be noticed.
#
# BOUND, stated rather than claimed away: this refuses the SHAPES that came
# back, not every derived number that could ever be written. A new instrument's
# number is not covered until somebody adds its shape here. The POPULATION
# CEILING below is what covers the rest, at a coarser grain.
REINTRODUCED=(
	'[0-9]+ ?/ ?102'
	'[0-9]+ of (the )?102'
	'all 102'
	'the 8[0-9] identifiers'
	'8[0-9] could be removed'
	'[0-9]+ declared, [0-9]+ listed'
	'reports [0-9]+ scenario\(s\); its source defines'
	'all (15|25|77)( |,|\.|$)'
	'encoding/hex [0-9]+/[0-9]+'
	# ROUND 13, N13. The four shapes the round-12 reviewer's probe line
	# carried — the oracle's scenario count, the verdict table's row count,
	# the declared-test count and the package count. All four are printed by
	# verify.sh on every run; all four were free to go stale in prose.
	'oracle runs [0-9]+'
	'verdict table has [0-9]+'
	'declares [0-9]+ test'
	'across [0-9]+ package'
	'[0-9]+ (verdict )?rows?[ ,.;]'
	'table (of|has) [0-9]+ row'
)

# The POPULATION CEILING, which is the general half of N13.
#
# The pattern list above keeps the found numbers gone; it cannot see a shape
# nobody has thought of. MEASURED 2026-08-30 by review: a single added line
# carrying FOUR live instrument-owned numbers passed, and the only thing that
# moved was the population count — a number nothing compared to anything.
#
# So compare it. The count is now held to a ceiling in verify.manifest.sh, and
# going over it prints the WHOLE enumeration rather than the count, because the
# operator's next question is which line is new.
#
# BOUND: this is a size, not a membership. Deleting one bare number and adding
# another is invisible to it, exactly as MIN_DECLARED_TESTS's band is blind to
# a swap. Raising the ceiling is a deliberate one-line edit in a file the Go
# pin also reads, which is the property round 9 asked for.
# shellcheck source=../verify.manifest.sh
. "$ROOT/verify.manifest.sh"

case "${1:-}" in
--check)
	hits=""
	for pat in "${REINTRODUCED[@]}"; do
		while IFS= read -r line; do
			hits="$hits
  $line    <-- matches /$pat/"
		done < <(grep -rEn "$pat" "${FILES[@]}" 2>/dev/null || true)
	done
	if [ -n "$hits" ]; then
		printf 'DOC NUMBERS: a number an instrument owns is back in prose:%s\n' "$hits" >&2
		printf 'Delete the number and name the instrument that prints it.\n' >&2
		exit 1
	fi
	n="$(population | grep -c . || true)"
	if [ "$n" -gt "$DOC_NUMBER_CEILING" ]; then
		printf 'DOC NUMBERS: %d prose line(s) carry a bare number, over the ceiling of %d in verify.manifest.sh.\n' "$n" "$DOC_NUMBER_CEILING" >&2
		printf 'A derived number no pattern here recognises is the shape this ceiling exists to catch. The whole population:\n' >&2
		population >&2
		printf 'Delete the number and name the instrument that prints it, or raise DOC_NUMBER_CEILING deliberately.\n' >&2
		exit 1
	fi
	printf 'doc-numbers: %d prose line(s) carry a bare number (ceiling %d); none is a shape round 9 removed\n' "$n" "$DOC_NUMBER_CEILING"
	;;
"")
	population
	printf -- '--- %d line(s)\n' "$(population | grep -c . || true)"
	;;
*)
	printf 'usage: %s [--check]\n' "$0" >&2
	exit 2
	;;
esac
