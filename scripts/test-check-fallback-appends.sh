#!/usr/bin/env bash
# Self-test for check-fallback-appends.sh.
#
# THE TWO CASES THAT MATTER ARE THE POLARITY ONES, and they do not test the
# gate at all -- they test the CLAIM the gate is built on. The gate flags a
# construct; the reason it is worth flagging is that the same construct fails
# open on one side of a conditional and closed on the other, which is why "the
# other sites were fine" is not evidence about this one. Cases 1 and 2 build
# both shapes and drive them, so if that stops being true the gate's whole
# rationale goes red rather than quietly becoming folklore.
#
# The rest drive the gate: the oracle's two halves (>/dev/null clears,
# 2>/dev/null does NOT), the marker, the comment-block lookback, both
# non-vacuity refusals, and mutants for each half of the oracle.

set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-fallback-appends.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok   $1"; }
bad() { fail=$((fail+1)); echo "  FAIL $1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$2' want '$3')"; fi; }

# A directory holding exactly the scripts a case needs.
mk() { rm -rf "$TMP/d"; mkdir -p "$TMP/d"; }
put() { printf '%s\n' "$2" > "$TMP/d/$1.sh"; }
run() { ( cd "$TMP" && bash "$GATE" d >"$TMP/out" 2>"$TMP/err" ); echo "$?"; }

echo "1..N check-fallback-appends"

# ---------------------------------------------------------------- polarity
# 1 THE FAIL-OPEN SHAPE. `|| FAIL` swallows the erroring comparison, so a
#   suite whose stub was never invoked reports PASS. This is the live defect
#   the campaign started from, reduced to eight lines.
cat > "$TMP/open.sh" <<'EOF'
: > "$1/calls"
# fallback-fixture: a deliberate specimen of the defect under test.
calls=$(grep -c . "$1/calls" 2>/dev/null || echo 0)
if [ "$calls" -ne 3 ]; then echo FAIL; else echo PASS; fi
EOF
mkdir -p "$TMP/p"
got=$(bash "$TMP/open.sh" "$TMP/p" 2>/dev/null)
chk "|| polarity: a 0-call case reports PASS with the count unchecked" "$got" "PASS"

# 2 THE FAIL-CLOSED SHAPE. Identical substitution, conditional inverted, and
#   now the same crash lands on the other branch. Same bug, opposite verdict.
cat > "$TMP/closed.sh" <<'EOF'
: > "$1/calls"
# fallback-fixture: a deliberate specimen of the defect under test.
calls=$(grep -c . "$1/calls" 2>/dev/null || echo 0)
if [ "$calls" -eq 3 ]; then echo PASS; else echo FAIL; fi
EOF
got=$(bash "$TMP/closed.sh" "$TMP/p" 2>/dev/null)
chk "&& polarity: the identical defect reports FAIL instead" "$got" "FAIL"
if [ "$(bash "$TMP/open.sh" "$TMP/p" 2>/dev/null)" = "$(bash "$TMP/closed.sh" "$TMP/p" 2>/dev/null)" ]; then
    bad "the two polarities agreed -- the gate's rationale no longer holds"
else
    ok "the two polarities disagree, which is the reason this gate exists"
fi

# ------------------------------------------------------------- the oracle
# 3 the risky shape is flagged
# fallback-fixture: a deliberate specimen of the defect under test.
mk; put risky 'x=$(grep -c . "$f" 2>/dev/null || echo 0)'
chk "unredirected stdout is flagged" "$(run)" "1"
grep -q 'risky.sh:1' "$TMP/err" && ok "and it names the file and line" || bad "no file:line in the report"

# 4 THE TRAP: 2>/dev/null must NOT clear a site. Every broken instance found
#   carries it, which is exactly why they all look careful.
# fallback-fixture: a deliberate specimen of the defect under test.
mk; put trap2 'x=$(some_cmd 2>/dev/null || echo 0)'  # fallback-fixture: a deliberate specimen; the string above is the input.
chk "2>/dev/null does NOT clear a site" "$(run)" "1"

# 5 the real cure: stdout redirected
mk; put cured 'x=$(some_cmd >/dev/null 2>&1 || echo 0)'
chk ">/dev/null clears it" "$(run)" "0"
mk; put cured1 'x=$(some_cmd 1>/dev/null || echo 0)'
chk "1>/dev/null clears it" "$(run)" "0"

# 6 a pure-status left side prints nothing by construction
mk; put teststmt 'x=$([ -f "$f" ] && echo yes || echo no)'
chk "a [ … ] left side clears it" "$(run)" "0"

# 7 the split fix -- the shape the recovered commit used, and the one the
#   error message steers people toward. It must read as clean.
mk; put split 'x=$(grep -c . "$f" 2>/dev/null); x=${x:-0}'
chk "the bare-substitution + \${x:-0} fix reads clean" "$(run)" "0"

# ------------------------------------------------------------- the marker
# 8 a written claim clears a site; silence does not (case 3 is the control)
# fallback-fixture: a deliberate specimen of the defect under test.
mk; put marked '# fallback-safe: it cannot print.
x=$(some_cmd 2>/dev/null || echo 0)'  # fallback-fixture: a deliberate specimen; the string above is the input.
chk "a fallback-safe: marker above clears the site" "$(run)" "0"
mk; put trailing 'x=$(some_cmd 2>/dev/null || echo 0)  # fallback-safe: cannot print.'
chk "a trailing marker clears the site" "$(run)" "0"

# 9 THE COMMENT-BLOCK LOOKBACK. A marker with its reasoning under it is the
#   normal shape, and keying on the immediately-preceding line silently voided
#   two real markers the first time this gate ran against the tree.
# fallback-fixture: a deliberate specimen of the defect under test.
mk; put block '# fallback-safe: cat on a missing path writes nothing.
# So the fallback replaces the value rather than appending to it.
x=$(cat "$f" 2>/dev/null || echo 0)'  # fallback-fixture: a deliberate specimen; the string above is the input.
chk "a marker two lines up still clears it" "$(run)" "0"

# 10 and the block must not leak past a code line onto the next site
# fallback-fixture: a deliberate specimen of the defect under test.
mk; put leak '# fallback-safe: this one really is.
x=$(some_cmd >/dev/null || echo 0)
y=$(other_cmd 2>/dev/null || echo 0)'  # fallback-fixture: a deliberate specimen; the string above is the input.
chk "the marker does not carry over to the next site" "$(run)" "1"

# 10b THE SECOND MARKER. A deliberate specimen -- this file is full of them --
#     must not have to claim it is SAFE to keep the gate quiet, and the escape
#     must not be silent: the pass line carries the count, so a fixture marker
#     used to wave real code through is visible rather than invisible.
mk; put spec 'x=$(some_cmd 2>/dev/null || echo 0)  # fallback-fixture: on purpose.'
chk "a fallback-fixture: marker clears the site" "$(run)" "0"
grep -q '1 declared deliberate fixtures' "$TMP/out" \
    && ok "and the pass line reports it as a fixture, not as fallback-safe" \
    || bad "the fixture escape is not counted in the pass line: $(cat "$TMP/out")"
grep -q '0 declared fallback-safe' "$TMP/out" \
    && ok "the two escapes are counted separately" \
    || bad "a fixture was tallied as fallback-safe"

# ---------------------------------------------------------- non-vacuity
# 11 no scripts at all: refuse, do not report the strongest possible pass
mk
chk "an empty directory is exit 2, not a pass" "$(run)" "2"

# 12 scripts with no substitutions at all: the matcher has probably moved
mk; put plain 'echo hello
exit 0'
chk "zero substitutions anywhere is exit 2" "$(run)" "2"

# 13 a missing directory is exit 2
rc=$( cd "$TMP" && bash "$GATE" nosuchdir >/dev/null 2>&1; echo $? )
chk "a missing directory is exit 2" "$rc" "2"

# ------------------------------------------------------------- mutants
# Two halves of the oracle, two mutants. A composite deletion would yield the
# union of their killers and name neither correctly.
mut() { sed "$1" "$GATE" > "$TMP/mut.sh"; }
mrun() { ( cd "$TMP" && bash "$TMP/mut.sh" d >/dev/null 2>&1 ); echo "$?"; }

# 14 delete the stdout-redirect half: case 5 must go red.
mut 's|^      if (lhs ~ /(\^\|\[\^0-9&2\])>\[\[:space:\]\]\*\\/dev\\/null/) safe=1|      # MUTANT|'
if grep -q '# MUTANT' "$TMP/mut.sh"; then
    ok "mutant A built (stdout-redirect detection removed)"
    mk; put cured 'x=$(some_cmd >/dev/null 2>&1 || echo 0)'
    r=$(mrun)
    if [ "$r" = "1" ]; then ok "without it, >/dev/null no longer clears (rc=1) -- case 5 is live"
    else bad "mutant A still passed case 5 (rc=$r): case 5 is not measuring that branch"; fi
else
    bad "could not build mutant A; case 5 is unverified"
fi

# 15 delete the marker check: case 8 must go red.
mut 's|if (prev ~ /fallback-safe:/ .*|# MUTANT|'
if grep -q '# MUTANT' "$TMP/mut.sh" && ! grep -q 'marked++' "$TMP/mut.sh"; then
    ok "mutant B built and really lacks the marker check"
    # fallback-fixture: a deliberate specimen of the defect under test.
mk; put marked '# fallback-safe: it cannot print.
x=$(some_cmd 2>/dev/null || echo 0)'  # fallback-fixture: a deliberate specimen; the string above is the input.
    r=$(mrun)
    if [ "$r" = "1" ]; then ok "without it, a marked site is flagged (rc=1) -- case 8 is live"
    else bad "mutant B still cleared the marked site (rc=$r)"; fi
else
    bad "could not build mutant B; case 8 is unverified"
fi

# 16 THE CONTROL FOR BOTH MUTANTS: the unmutated gate on the same fixtures.
#    Without it a mutant that simply crashes scores as a kill.
mk; put cured 'x=$(some_cmd >/dev/null 2>&1 || echo 0)'
chk "control: the real gate still passes the fixtures the mutants failed" "$(run)" "0"

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
