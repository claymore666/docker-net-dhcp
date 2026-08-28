#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-pool-facts.sh (#879).
#
# THE MARKER IS NEVER SPELLED OUT IN THIS FILE, and that is not style.
# The gate sweeps the whole tracked tree, so a literal marker written here
# would be a site the real gate then tries to check — and every fixture
# below states deliberately WRONG numbers, which would turn the real lane
# red for reasons that have nothing to do with the tree. So $M is
# assembled at run time and the fixtures are written through it. The same
# trick is why the gate's own header writes NAME=VALUE rather than digits.
#
# Fixture numbers are 6 and 3 on purpose: neither is the real pool size
# nor the real per-run job count, so a fixture cannot pass by accidentally
# agreeing with the tree this file lives in.
#
# WHAT THIS DRIVES, in both directions:
#   - the real tree stays green                    (preservation control)
#   - a stale number goes red                      (the finding direction)
#   - a marker bumped without its sentence goes red
#   - every refusal path is asserted BY EXIT CODE, never by message text
#   - the domain being empty is a refusal, not a pass
#   - deleting what the gate guards makes something go red
#   - the watchdog's operator text really does carry the derived numbers,
#     asserted against what the script PRINTS rather than against the
#     source it was read from
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-pool-facts.sh"
WATCHDOG="$HERE/ci-queue-watchdog.sh"
ROOT="$(cd "$HERE/.." && pwd)"
pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# Assembled, never written literally — see the header.
M="pool""-facts:"

TMPS=()
cleanup() { local d; for d in ${TMPS+"${TMPS[@]}"}; do rm -rf "$d"; done; }
trap cleanup EXIT

# newfix builds a self-consistent fixture repository: a facts file, a
# workflow to derive the per-run count from, and two marked prose sites
# that agree with both. Every case below starts from this and breaks
# exactly one thing.
#
# `git add` without `git commit`: the gate enumerates its domain with
# `git ls-files`, which reads the index, so nothing here needs a commit —
# and therefore nothing here needs the developer's git identity or
# `commit.gpgsign`.
newfix() {
    local d
    d=$(mktemp -d)
    TMPS+=("$d")
    mkdir -p "$d/.github/workflows" "$d/docs"
    cat > "$d/.github/ci-pool-facts.env" <<'ENV'
DHCP_CI_POOL_SIZE=6
DHCP_CI_POOL_SIZE_MEASURED=2026-08-28
DHCP_CI_POOL_LABEL=fixture-pool
DHCP_CI_JOBS_PER_RUN_WORKFLOW=.github/workflows/integration.yml
ENV
    cat > "$d/.github/workflows/integration.yml" <<'WF'
name: Integration
on: push
jobs:
  hosted:
    runs-on: ubuntu-latest
    steps:
      - run: echo hosted
  suite:
    runs-on: [self-hosted, fixture-pool]
    strategy:
      fail-fast: false
      matrix:
        include:
          - suite: a
          - suite: b
          - suite: c
    steps:
      - run: echo suite
WF
    {
        printf 'The pool is 6 runners. <!-- %s pool-size=6 -->\n' "$M"
        printf 'A run places 3 jobs on the pool. <!-- %s jobs-per-run=3 -->\n' "$M"
    } > "$d/docs/pool.md"
    git init -q "$d" >/dev/null 2>&1
    git -C "$d" add -A >/dev/null 2>&1
    printf '%s' "$d"
}

# run <dir> [args...] -> prints combined output, returns the gate's exit.
run() {
    local d="$1"
    shift
    POOL_FACTS_ROOT="$d" bash "$GATE" "$@" 2>&1
}

# rc <dir> [args...] -> just the exit code, which is what every refusal
# assertion below keys on. A refusal proved by its message is a refusal
# proved by prose.
rc() {
    run "$@" >/dev/null 2>&1
    printf '%s' "$?"
}

# --- the preservation control ----------------------------------------
# First, because everything after it is worthless if the gate cannot go
# green on the tree it ships in.
if out=$(bash "$GATE" 2>&1); then
    ok "the real tracked tree passes"
else
    no "the real tree does not pass the gate it ships with: $out"
fi
case "$out" in
    *"check-pool-facts: OK"*) ok "the passing outcome names itself as the normal one" ;;
    *) no "a pass printed nothing that says it is a pass: $out" ;;
esac

# --- the fixture control ---------------------------------------------
d=$(newfix)
[ "$(rc "$d")" = 0 ] && ok "a self-consistent fixture passes" \
    || no "the baseline fixture fails, so every mutation below is uninterpretable: $(run "$d")"

# The derivation is a derivation: it counts matrix entries of the
# pool-labelled job and ignores the hosted one.
out=$(run "$d" --facts)
case "$out" in
    *"jobs-per-run=3"*) ok "--facts derives 3 from the fixture matrix, not from any declaration" ;;
    *) no "--facts did not derive the per-run count from the workflow: $out" ;;
esac
case "$out" in
    *"pool-size=6"*) ok "--facts reports the declared pool size" ;;
    *) no "--facts did not report the declared pool size: $out" ;;
esac

# --- the finding direction: a stale number goes red -------------------
d=$(newfix)
sed -i 's/pool-size=6/pool-size=5/; s/The pool is 6 runners/The pool is 5 runners/' "$d/docs/pool.md"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 1 ] && ok "a site stating a stale pool size is a finding (exit 1)" \
    || no "a stale pool size did not produce exit 1, got $(rc "$d")"

d=$(newfix)
# Widen the matrix and leave the prose behind — the decay this exists for.
sed -i 's/          - suite: c/          - suite: c\n          - suite: d/' "$d/.github/workflows/integration.yml"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 1 ] && ok "widening the matrix without touching the prose is a finding (exit 1)" \
    || no "a widened matrix left the stale prose green, got $(rc "$d")"

d=$(newfix)
# The marker bumped, the sentence beside it not. This is the half a
# two-way check would miss.
sed -i "s/A run places 3 jobs/A run places 9 jobs/" "$d/docs/pool.md"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 1 ] && ok "a marker that disagrees with its own sentence is a finding (exit 1)" \
    || no "marker and prose were allowed to disagree, got $(rc "$d")"

d=$(newfix)
# The spelling trap, driven: the digit is replaced by the word.
sed -i 's/The pool is 6 runners/The pool is six runners/' "$d/docs/pool.md"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 1 ] && ok "a site spelling the number as a word is a finding (exit 1)" \
    || no "'six' satisfied a check for 6 — the spelling problem is not closed, got $(rc "$d")"

# --- driving the absence ---------------------------------------------
# Delete what the gate guards and something must go red. A universal
# check is satisfied by emptying its domain, so this is the case that
# says whether it is a check at all.
d=$(newfix)
rm -f "$d/docs/pool.md"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 2 ] && ok "deleting every marked site is a REFUSAL, not a pass (exit 2)" \
    || no "the gate passed with an empty domain, got $(rc "$d")"

d=$(newfix)
sed -i '/jobs-per-run/d' "$d/docs/pool.md"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 2 ] && ok "one fact losing all its sites is a refusal (exit 2)" \
    || no "a fact with zero sites passed, got $(rc "$d")"

d=$(newfix)
rm -f "$d/.github/ci-pool-facts.env"
[ "$(rc "$d")" = 2 ] && ok "deleting the canonical facts file is a refusal (exit 2)" \
    || no "a missing facts file did not refuse, got $(rc "$d")"

d=$(newfix)
rm -f "$d/.github/workflows/integration.yml"
[ "$(rc "$d")" = 2 ] && ok "deleting the workflow the count derives from is a refusal (exit 2)" \
    || no "a missing derivation source did not refuse, got $(rc "$d")"

# --- refusals: a missing or unreadable input --------------------------
d=$(newfix)
sed -i '/DHCP_CI_POOL_SIZE=/d' "$d/.github/ci-pool-facts.env"
[ "$(rc "$d")" = 2 ] && ok "an absent pool size refuses rather than becoming zero (exit 2)" \
    || no "an absent DHCP_CI_POOL_SIZE did not refuse, got $(rc "$d")"

d=$(newfix)
sed -i 's/DHCP_CI_POOL_SIZE=6/DHCP_CI_POOL_SIZE=/' "$d/.github/ci-pool-facts.env"
[ "$(rc "$d")" = 2 ] && ok "an empty pool size refuses (exit 2)" \
    || no "an empty DHCP_CI_POOL_SIZE did not refuse, got $(rc "$d")"

d=$(newfix)
sed -i 's/DHCP_CI_POOL_SIZE=6/DHCP_CI_POOL_SIZE=0/' "$d/.github/ci-pool-facts.env"
[ "$(rc "$d")" = 2 ] && ok "a pool of zero refuses — an error folded into a value has no direction" \
    || no "DHCP_CI_POOL_SIZE=0 was accepted, got $(rc "$d")"

d=$(newfix)
sed -i 's/DHCP_CI_POOL_SIZE=6/DHCP_CI_POOL_SIZE=sixteen/' "$d/.github/ci-pool-facts.env"
[ "$(rc "$d")" = 2 ] && ok "a non-numeric pool size refuses (exit 2)" \
    || no "a non-numeric DHCP_CI_POOL_SIZE was accepted, got $(rc "$d")"

d=$(newfix)
sed -i '/DHCP_CI_POOL_SIZE_MEASURED/d' "$d/.github/ci-pool-facts.env"
[ "$(rc "$d")" = 2 ] && ok "an undated pool size refuses — the number is worth what its date says" \
    || no "an undated DHCP_CI_POOL_SIZE was accepted, got $(rc "$d")"

d=$(newfix)
sed -i '/DHCP_CI_POOL_LABEL/d' "$d/.github/ci-pool-facts.env"
[ "$(rc "$d")" = 2 ] && ok "no pool label means no derivation, and that refuses (exit 2)" \
    || no "a missing DHCP_CI_POOL_LABEL was accepted, got $(rc "$d")"

d=$(newfix)
chmod 000 "$d/.github/ci-pool-facts.env"
got=$(rc "$d")
chmod 644 "$d/.github/ci-pool-facts.env"
if [ "$(id -u)" = 0 ]; then
    ok "SKIPPED-AS-INAPPLICABLE: running as root, mode 000 is still readable (recorded, not silently dropped)"
else
    [ "$got" = 2 ] && ok "an unreadable facts file refuses (exit 2)" \
        || no "an unreadable facts file did not refuse, got $got"
fi

d=$(newfix)
rm -rf "$d/.git"
[ "$(rc "$d")" = 2 ] && ok "a root that is not a git work tree refuses (exit 2)" \
    || no "a non-repository did not refuse, got $(rc "$d")"

d=$(newfix)
git -C "$d" rm -r -q --cached . >/dev/null 2>&1
[ "$(rc "$d")" = 2 ] && ok "an empty git index refuses — a domain of nothing is not a pass" \
    || no "an empty index did not refuse, got $(rc "$d")"

# --- refusals: shapes the derivation does not model -------------------
d=$(newfix)
sed -i 's/fixture-pool/some-other-label/' "$d/.github/ci-pool-facts.env"
[ "$(rc "$d")" = 2 ] && ok "a pool label no job carries refuses — the scan stopped matching" \
    || no "a label matching no job was treated as a count, got $(rc "$d")"

d=$(newfix)
python3 - "$d" <<'PY'
import io, sys
p = sys.argv[1] + "/.github/workflows/integration.yml"
s = io.open(p, encoding="utf-8").read()
s = s.replace("      matrix:\n", "      matrix:\n        exclude:\n          - suite: a\n")
io.open(p, "w", encoding="utf-8").write(s)
PY
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 2 ] && ok "a matrix using exclude: refuses rather than being approximated" \
    || no "matrix exclude: was silently mis-counted, got $(rc "$d")"

d=$(newfix)
python3 - "$d" <<'PY'
import io, sys
p = sys.argv[1] + "/.github/workflows/integration.yml"
s = io.open(p, encoding="utf-8").read()
s = s.replace("""      matrix:
        include:
          - suite: a
          - suite: b
          - suite: c
""", "      matrix: ${{ fromJSON(needs.gate.outputs.matrix) }}\n")
io.open(p, "w", encoding="utf-8").write(s)
PY
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 2 ] && ok "a matrix built from an expression refuses — it is not knowable from the file" \
    || no "an expression matrix produced a confident count, got $(rc "$d")"

d=$(newfix)
printf 'not: [valid: yaml\n' > "$d/.github/workflows/integration.yml"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 2 ] && ok "an unparseable workflow refuses (exit 2)" \
    || no "an unparseable workflow did not refuse, got $(rc "$d")"

d=$(newfix)
printf 'The pool is 6 runners. <!-- %s poolsize=6 -->\n' "$M" >> "$d/docs/pool.md"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 2 ] && ok "a marker naming an unknown fact refuses — a typo must not drop a site" \
    || no "an unknown fact name was ignored, got $(rc "$d")"

# --- the derivation counts matrix dimensions too ----------------------
d=$(newfix)
python3 - "$d" <<'PY'
import io, sys
p = sys.argv[1] + "/.github/workflows/integration.yml"
s = io.open(p, encoding="utf-8").read()
s = s.replace("""        include:
          - suite: a
          - suite: b
          - suite: c
""", """        suite: [a, b, c]
        mode: [x, y]
""")
io.open(p, "w", encoding="utf-8").write(s)
PY
sed -i 's/A run places 3 jobs/A run places 6 jobs/; s/jobs-per-run=3/jobs-per-run=6/' "$d/docs/pool.md"
git -C "$d" add -A >/dev/null 2>&1
[ "$(rc "$d")" = 0 ] && ok "two matrix dimensions expand to their product, not to their sum" \
    || no "a 3x2 matrix was not counted as 6: $(run "$d")"

# --- --facts refuses too, and prints nothing when it does -------------
d=$(newfix)
rm -f "$d/.github/ci-pool-facts.env"
out=$(run "$d" --facts)
[ "$(rc "$d" --facts)" = 2 ] && ok "--facts refuses when the facts cannot be read (exit 2)" \
    || no "--facts did not refuse on a missing facts file"
case "$out" in
    *pool-size=*) no "--facts printed a value on a path where it had none: $out" ;;
    *) ok "--facts prints no number when it cannot derive one" ;;
esac

[ "$(rc "$ROOT" --nonsense)" = 2 ] && ok "an unknown argument refuses (exit 2)" \
    || no "an unknown argument was accepted"

# --- the operator text: assert on what the watchdog PRINTS ------------
#
# The two ci-queue-watchdog.sh advise() messages are why #879 is not
# cosmetic: they are what an operator reads DURING a capacity incident,
# and both misstated their operands. Asserting that the source no longer
# contains a literal would be a claim about the file. This runs the real
# script down its real STARVATION and POOL SHORT paths and reads the
# numbers out of the output.
facts=$(bash "$GATE" --facts 2>/dev/null) || facts=""
real_pool=$(printf '%s\n' "$facts" | sed -n 's/^pool-size=//p')
real_jobs=$(printf '%s\n' "$facts" | sed -n 's/^jobs-per-run=//p')
if [ -n "$real_pool" ] && [ -n "$real_jobs" ]; then
    ok "the gate yields both facts for the watchdog to quote"
else
    no "the gate produced no facts, so the watchdog assertions below cannot be interpreted"
fi

# A curl stub keyed on the URL, the same shape test-ci-queue-watchdog.sh
# uses: one queued job, and a competing run or none, which selects the
# class.
stub_dir=$(mktemp -d); TMPS+=("$stub_dir")
mkdir -p "$stub_dir/bin"
printf '%s\n' '{"workflow_runs":[{"id":997,"run_number":41,"name":"Integration","path":".github/workflows/integration.yml"}]}' > "$stub_dir/busy.json"
printf '%s\n' '{"workflow_runs":[]}' > "$stub_dir/idle.json"
cat > "$stub_dir/bin/curl" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do
    case "$a" in
        */cancel) printf '202'; exit 0 ;;
        *"actions/runs?status=in_progress"*) cat "$RUNS_FIXTURE"; exit 0 ;;
    esac
done
echo '{"jobs":[{"name":"main-1-suite","status":"queued"}]}'
STUB
chmod +x "$stub_dir/bin/curl"

# watchdog_out <runs-fixture> [script-path]
watchdog_out() {
    PATH="$stub_dir/bin:$PATH" RUNS_FIXTURE="$1" GATE_REPO=o/r GH_TOKEN=x \
        WATCHDOG_NO_CANCEL=1 bash "${2:-$WATCHDOG}" 12345 0 1 2>&1
}

out=$(watchdog_out "$stub_dir/busy.json")
case "$out" in
    *"CLASS: STARVATION"*) ok "the starvation path was reached (the assertions below mean something)" ;;
    *) no "could not reach the STARVATION path: $out" ;;
esac
case "$out" in
    *"puts ${real_jobs} jobs on"*) ok "the STARVATION advice states the DERIVED per-run job count" ;;
    *) no "the STARVATION advice does not state the derived job count ${real_jobs}: $out" ;;
esac
case "$out" in
    *"pool has ${real_pool} runners"*) ok "the STARVATION advice states the canonical pool size" ;;
    *) no "the STARVATION advice does not state the canonical pool size ${real_pool}: $out" ;;
esac
# The pre-#879 sentence, driven by its own words. "puts 4 jobs" is
# deliberately NOT one of them: 4 is also the value the derivation
# currently yields, so that assertion could not tell the literal from the
# derived answer and would have been a check with one possible verdict.
case "$out" in
    *"8-runner pool"*|*"Two runs fit"*) no "the pre-#879 fixed arithmetic survives in the STARVATION advice" ;;
    *) ok "the pre-#879 fixed arithmetic is gone from the STARVATION advice" ;;
esac

out=$(watchdog_out "$stub_dir/idle.json")
case "$out" in
    *"CLASS: POOL SHORT"*) ok "the POOL SHORT path was reached" ;;
    *) no "could not reach the POOL SHORT path: $out" ;;
esac
case "$out" in
    *"contracted at ${real_pool} runners"*) ok "the POOL SHORT advice states the canonical contracted size" ;;
    *) no "the POOL SHORT advice does not state the contracted size ${real_pool}: $out" ;;
esac
case "$out" in
    *"contracted eight"*) no "'the contracted eight' survives in the POOL SHORT advice" ;;
    *) ok "'the contracted eight' is gone from the POOL SHORT advice" ;;
esac

# All three arms of the fit arithmetic, driven through the SEAM rather
# than through the tree. The real pool divides exactly today, so the
# remainder arm and the pool-too-small arm are unreachable from the real
# values -- and an arm that never executes is an arm nobody has run. A
# stub `check-pool-facts.sh` beside a copy of the watchdog supplies the
# numbers; it stubs the TRANSPORT, not the verdict.
seam() {  # seam <pool> <jobs> -> the advice the watchdog prints
    local d
    d=$(mktemp -d); TMPS+=("$d")
    cp "$WATCHDOG" "$d/ci-queue-watchdog.sh"
    printf '#!/usr/bin/env bash\n[ "${1:-}" = --facts ] || exit 2\necho pool-size=%s\necho jobs-per-run=%s\n' \
        "$1" "$2" > "$d/check-pool-facts.sh"
    chmod +x "$d/check-pool-facts.sh"
    watchdog_out "$stub_dir/busy.json" "$d/ci-queue-watchdog.sh"
}

out=$(seam 16 4)
case "$out" in
    *"4 concurrent runs fit exactly"*) ok "an exact division is reported as an exact fit" ;;
    *) no "16/4 was not reported as 4 exact: $out" ;;
esac
out=$(seam 16 6)
case "$out" in
    *"fit exactly"*) no "an inexact division was reported as an exact fit: $out" ;;
    *) ok "an inexact division is not reported as an exact fit" ;;
esac
case "$out" in
    *"PARTIAL pickup — 4 of its 6 jobs assigned"*) ok "a remainder is reported as a partial pickup, with both operands" ;;
    *) no "16/6 did not describe the partial pickup: $out" ;;
esac
out=$(seam 3 6)
case "$out" in
    *"only 3 runners"*) ok "a pool smaller than one run says so instead of dividing to zero" ;;
    *) no "a pool of 3 against 6 jobs per run was not called out: $out" ;;
esac

# Drive the ABSENCE on the watchdog's side too: take the gate away and
# the advice must SAY it could not derive, never fall back to a number.
lone=$(mktemp -d); TMPS+=("$lone")
cp "$WATCHDOG" "$lone/ci-queue-watchdog.sh"
out=$(watchdog_out "$stub_dir/idle.json" "$lone/ci-queue-watchdog.sh")
case "$out" in
    *"could not be derived"*) ok "with the gate absent the advice says so instead of guessing" ;;
    *) no "with the gate absent the advice did not report an undetermined pool: $out" ;;
esac
case "$out" in
    *"contracted at "[0-9]*) no "a number was printed on a path where nothing could be derived: $out" ;;
    *) ok "no number is printed when nothing could be derived" ;;
esac

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
