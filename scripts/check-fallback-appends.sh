#!/usr/bin/env bash
# A `|| echo` fallback inside $( ) APPENDS when the left side already printed.
#
# THE DEFECT. `calls=$(grep -c . "$f" 2>/dev/null || echo 0)` looks careful and
# is wrong. On an empty file `grep -c .` prints `0` AND exits 1, so the `||`
# fires and `echo 0` prints too. The variable holds TWO lines. The next `[ … ]`
# comparison then dies with "integer expression expected".
#
# THE HALF THAT MAKES IT HARD TO SEE, and the reason this gate exists rather
# than a note: THE IDENTICAL BROKEN CONSTRUCT FAILS OPEN OR CLOSED PURELY ON
# THE POLARITY OF THE CONDITIONAL IT FEEDS. `|| FAIL` swallows the erroring
# comparison; `&& PASS` catches it. An erroring `[` exits 2, `if` reads
# non-zero as false, and which branch that skips depends on which side the
# test sits on:
#
#   if [ got -ne want ] || [ calls -ne wantcalls ]; then FAIL   -> FAILS OPEN
#   if [ got -eq want ] && [ calls -eq wantcalls ]; then PASS   -> fails closed
#
# Measured 2026-08-27 across every branch then in flight: three sites, one of
# each polarity plus one already merged. Disabling the stub's recording line in
# `test-check-fork-execution-policy.sh` -- the exact state its own guard exists
# to catch, whose message reads "the stub was never invoked, so this case
# tested nothing" -- left ALL NINE CASES PASSING, exit 0, with nine
# integer-expression errors on a stream nobody reads. The same mutation against
# `test-check-attestation-parity.sh` failed 7 of 23.
#
# So both lazy readings are unsafe: "same code, same risk" is wrong, and so is
# "the others were fine, so this one probably is". Read the polarity.
#
# This class was already written down when it was first found, three instances
# in one day. It came back because the remedy was a paragraph. A postmortem
# ends in an executable check.
#
# AND THE STRONGER ARGUMENT IS NOT THE DEFECT, IT IS THE FIX. On 2026-08-27
# this construct was found three times over, and the FIRST finder had already
# fixed it -- correctly, at both sites, with a comment explaining the two-line
# variable, the erroring `[`, and the `||` chain reading a crash as a passing
# assertion. Better than either later write-up. It cost the other two a full
# review cycle each anyway, because that commit was never pushed.
#
# Two halves of that, with their sourcing marked, because this file elsewhere
# tells the reader to measure rather than reason. MEASURED here: the commit
# existed, sat one ahead of the pushed head, and its two fixes are the ones
# quoted above -- read out of the worktree and the recovery ref. REPORTED to
# me by the session coordinating the fleet, not verified by me: that three
# separate sessions arrived at it independently, and that the author was
# still live in the same shared checkout throughout. Note that "three
# sessions" is a claim the git record cannot settle either way -- every
# commit in this repository carries one identity -- so do not repeat it as
# though a log proves it.
#
# The part that needs no attribution: the fix reached nobody. There is
# no mechanism by which unpushed work reaches a reviewer, a check, or a `gh`
# query, so the only thing that propagates a repair is a gate that goes red in
# a tree somebody else has.
#
# That is not one incident, and the population is measured rather than felt.
# Counted here on 2026-08-27 over the shared checkout's 61 worktrees, 44 of
# them on a named branch: three branches sat AHEAD of their pushed head, and
# fourteen had never been pushed at all -- no remote ref, therefore no PR, no
# CI and no reviewer. (`git worktree list --porcelain`, then `git rev-list
# --count origin/$b..$b` per branch; re-run it, do not trust the number.) The
# commit recovered above was not special. It was the one that happened to
# surface, because somebody looked in a worktree before writing to a branch.
#
# WHAT IT MATCHES, and why not the spelling. Grepping for `grep -c` would
# reproduce this gate's own silence the day somebody writes `wc -l … || echo 0`.
# The class is: a command substitution whose `||` fallback EMITS, where the
# left-hand side's stdout can still reach the substitution. So the question
# asked of every site is "can the left side print into this?" --
#
#   >/dev/null, 1>/dev/null, &>/dev/null   it cannot. Safe.
#   `[ … ]` or `test …`                    pure status, prints nothing. Safe.
#   2>/dev/null                            ONLY stderr is gone. STDOUT STILL
#                                          ARRIVES -- and this is why every
#                                          broken site looks careful.
#
# Measured on the tree this shipped with: 32 sites carry a `|| echo` fallback
# and this question takes it to 9, with no allowlist. Refining the ORACLE
# rather than filtering the output is what makes the number small enough to
# act on.
#
# THE MARKER, and why a surviving site is not simply "fine". Six of the nine
# were `$(printf … | bash "$GATE" classify && echo docs-only || echo not)`:
# unredirected stdout, correct today only because `classify` happens to print
# nothing. Safe by coincidence, not by construction. So a site is cleared by a
# WRITTEN claim, not by silence:
#
#   # fallback-safe: <why the left side cannot print into this>
#
# on the line above, or trailing on the line itself. That converts a
# coincidence into something the next reader can falsify.
#
# THE SECOND MARKER, AND WHY IT IS NOT THE FIRST ONE. This gate's own
# self-test BUILDS the defect on purpose, several times. Those sites are not
# safe and marking them `fallback-safe:` would be writing something false
# into the tree to keep a check quiet -- the exact move the marker exists to
# prevent. So a deliberate specimen says `# fallback-fixture: <why>`, it is
# counted separately, and the count is printed on every pass. An escape
# hatch nobody can see is how the last one of these got away.
#
# KNOWN BLIND SPOT. The matcher is line-oriented and does not understand
# heredocs or line continuations: a substitution written inside a quoted
# heredoc is read as ordinary source, and one split across two lines is not
# seen at all. The first direction over-reports and a marker settles it; the
# second UNDER-reports and nothing here will tell you. Stated because a gate
# whose limits are undocumented gets read as a guarantee.
#
# NON-VACUITY. A gate that stops finding files reports the strongest possible
# pass. No scripts found -> exit 2. No `$( … )` substitutions seen anywhere ->
# exit 2, because the matcher has probably moved rather than the tree.
#
# Usage: check-fallback-appends.sh [dir ...]      (default: scripts/)
# Exit:  0 clean   1 unmarked risky site(s)   2 cannot judge

set -uo pipefail

DIRS=("$@")
[ ${#DIRS[@]} -eq 0 ] && DIRS=("scripts")

files=()
for d in "${DIRS[@]}"; do
    [ -d "$d" ] || { echo "check-fallback-appends: not a directory: $d" >&2; exit 2; }
    while IFS= read -r f; do files+=("$f"); done < <(find "$d" -maxdepth 1 -type f -name '*.sh' | sort)
done

if [ ${#files[@]} -eq 0 ]; then
    echo "::error title=Fallback-append check cannot be judged::no *.sh found under ${DIRS[*]}." \
         "There is nothing to judge and 'none of nothing is broken' is not an answer." >&2
    exit 2
fi

report=$(LC_ALL=C awk '
function trim(s){ sub(/^[[:space:]]+/,"",s); sub(/[[:space:]]+$/,"",s); return s }
FNR==1 { prev="" }
{
  raw=$0; t=trim(raw)
  # The lookback is the whole CONTIGUOUS comment block, not the single
  # line above. A marker with a two-line reason above it is the normal
  # shape here, and keying on line-above alone silently voided two real
  # markers when this gate was first run against the tree.
  if (t ~ /^#/) { prev = prev " " t; next }

  # Count every substitution so the non-vacuity guard measures the matcher.
  s0=raw; while (match(s0, /\$\(/)) { subs++; s0=substr(s0, RSTART+2) }

  s=raw
  while (match(s, /\$\([^()]*\|\|[^()]*\)/)) {
      expr=substr(s, RSTART, RLENGTH)
      s=substr(s, RSTART+RLENGTH)
      if (expr !~ /\|\|[[:space:]]*(echo|printf)[[:space:]]/) continue

      p=index(expr,"||"); lhs=substr(expr,3,p-3)

      safe=0
      if (lhs ~ /(^|[^0-9&2])>[[:space:]]*\/dev\/null/) safe=1
      if (lhs ~ /1>[[:space:]]*\/dev\/null/)            safe=1
      if (lhs ~ /&>[[:space:]]*\/dev\/null/)            safe=1
      l=trim(lhs)
      if (l ~ /^\[[[:space:]]/ || l ~ /^test[[:space:]]/) safe=1
      if (safe) continue

      # A written claim clears it; silence does not.
      if (prev ~ /fallback-safe:/ || raw ~ /fallback-safe:/) { marked++; continue }

      # A deliberate SPECIMEN is not a safe site and must not borrow the
      # safe marker: a self-test that builds the defect on purpose would
      # otherwise have to assert something false about it. Separate word,
      # counted separately, printed in the pass line -- so using it to
      # wave real code through shows up in the tally.
      if (prev ~ /fallback-fixture:/ || raw ~ /fallback-fixture:/) { fixtures++; continue }

      printf "%s:%d: %s\n", FILENAME, FNR, trim(expr)
      hits++
  }
  prev=""
}
END { printf "@@ %d %d %d %d\n", subs+0, hits+0, marked+0, fixtures+0 }
' "${files[@]}")

tally=$(printf '%s\n' "$report" | grep '^@@ ' | tail -1)
sites=$(printf '%s\n' "$report" | grep -v '^@@ ')
read -r _ n_subs n_hits n_marked n_fixtures <<<"$tally"

if [ "${n_subs:-0}" -eq 0 ]; then
    echo "::error title=Fallback-append check cannot be judged::read ${#files[@]} script(s) and found not one" \
         "\$( … ) substitution. Either these are not shell scripts or the matcher has moved;" \
         "either way nothing was actually checked." >&2
    exit 2
fi

if [ "${n_hits:-0}" -gt 0 ]; then
    echo "::error title=A || fallback can append to a non-empty result::${n_hits} site(s) where the" \
         "left-hand side's stdout still reaches the substitution, so the fallback ADDS a line" \
         "instead of replacing one. Redirect the left side's stdout, or drop the fallback, or" \
         "state why it cannot print with a '# fallback-safe: <reason>' comment. Note 2>/dev/null" \
         "is NOT enough -- it silences stderr and lets stdout through, which is why every" \
         "instance of this looks careful." >&2
    printf '%s\n' "$sites" >&2
    exit 1
fi

echo "fallback appends: ${n_subs} substitution(s) across ${#files[@]} script(s); no unmarked risky site(s), ${n_marked} declared fallback-safe, ${n_fixtures} declared deliberate fixtures"
exit 0
