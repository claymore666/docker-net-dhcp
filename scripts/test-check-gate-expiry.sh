#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-gate-expiry.sh (#749) against fixture script
# directories inside a scratch git work tree, then the real tree.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(cd "$(dirname "$0")" && pwd)/check-gate-expiry.sh"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
guarded_tmpdir TMP
git init -q "$TMP"
failures=0
n=0

# gate FILE [header lines...]: a gate whose leading comment block holds the given lines.
gate() {
    local f="$1"; shift
    { echo '#!/usr/bin/env bash'; echo '# Copyright the docker-net-dhcp contributors.'
      echo '#'; echo "# Purpose of ${f##*/} (#1)."
      for l in "$@"; do echo "$l"; done
      echo; echo 'set -uo pipefail'; echo 'exit 0'; } > "$f"
}

# fixture: sets d to a passing directory with two check gates, one named gate and the non-gates beside them.
fixture() {
    n=$((n + 1))
    d="$TMP/case$n"
    mkdir -p "$d"
    gate "$d/check-a.sh" "# Expires-when: the alpha lane is retired by #11 and nothing reads alpha."
    gate "$d/check-b.sh" "# Expires-when: never: the beta invariant is the product contract" "#   that #12 states for every release."
    gate "$d/x-gate.sh" "# Expires-when: the x suite becomes cheap enough to run always (#13)."
    gate "$d/test-check-a.sh"
    gate "$d/test-x-gate.sh"
    gate "$d/gatelib.sh"
    gate "$d/run-gate-selftests.sh"
}

check() {
    local name="$1" want_exit="$2" want_grep="$3"; shift 3
    bash "$GATE" "$@" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && { [ -z "$want_grep" ] || grep -qF -- "$want_grep" "$TMP/out"; }; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want_exit, want text '$want_grep')"; sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

fixture
check "a directory where every gate states its expiry passes; self-tests, gatelib and the runner are not gates" \
    0 "3 gates state when they expire" "$d"

fixture
git -C "$TMP" add "case$n/check-a.sh"
gate "$d/check-new.sh"
check "an untracked new gate without the line is red" 1 "check-new.sh: no '# Expires-when:' line" "$d"

fixture
printf 'check-ignored.sh\n' > "$d/.gitignore"
gate "$d/check-ignored.sh"
check "an ignored file is not a gate" 0 "3 gates" "$d"

fixture
gate "$d/x-gate.sh"
check "a named *-gate.sh without the line is red" 1 "x-gate.sh: no '# Expires-when:'" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when:"
check "an empty line is red" 1 "check-a.sh:5: Expires-when is empty" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when:" "#   the alpha lane is retired by #11 and nothing reads alpha."
check "an empty key line with a continuation carries the text" 0 "3 gates" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: TODO (#749)"
check "TODO is a placeholder" 1 "placeholder" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: never (#11)"
check "a bare never is a placeholder" 1 "placeholder" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: n/a #11"
check "n/a is a placeholder" 1 "placeholder" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: TBD, decide once the alpha lane is gone in #11"
check "a long TBD is still a placeholder" 1 "placeholder" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: when #746 lands"
check "fewer than six words is red" 1 "has 2 words" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: the alpha lane is retired and nothing reads alpha any more."
check "a condition citing no #N is red" 1 "cites no issue or PR" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: the alpha lane is retired by #11" "#   and nothing" "#   reads" "#   alpha" "#   any more."
check "more than four lines is red" 1 "runs 5 lines" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: the alpha lane is retired by #11" "#   and nothing" "#   reads alpha."
check "four lines or fewer passes" 0 "3 gates" "$d"

fixture
gate "$d/x-gate.sh" "# Expires-when: The alpha lane is retired by #11 and   nothing reads alpha."
check "the same text in two gates is boilerplate" 1 "same Expires-when as check-a.sh" "$d"

fixture
gate "$d/check-a.sh"
echo '# Expires-when: the alpha lane is retired by #11 and nothing reads alpha.' >> "$d/check-a.sh"
check "a key below the header does not count" 1 "is below the header" "$d"

fixture
gate "$d/check-a.sh" "# Expires_when: the alpha lane is retired by #11 and nothing reads alpha."
check "a near-miss spelling is named" 1 "near-miss spelling" "$d"

fixture
gate "$d/check-a.sh" "#  Expires-when: the alpha lane is retired by #11 and nothing reads alpha."
check "an indented key is a near miss" 1 "near-miss spelling" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: the alpha lane is retired by #11 and nothing reads alpha." "#" "# Expires-when:"
check "two key lines are red" 1 "2 Expires-when lines" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: the alpha lane is retired by #11 and nothing reads alpha." "#" "# More prose follows the key."
check "prose after the key is not part of it" 0 "3 gates" "$d"

fixture
gate "$d/check-a.sh" "# Expires-when: the alpha lane is retired by #11 and nothing reads alpha." "# Usage: check-a.sh"
check "a one-space comment line ends the key" 0 '| `check-a.sh` | the alpha lane is retired by #11 and nothing reads alpha. |' --list "$d"

# root reads a mode-000 file, so the case needs an unprivileged runner.
if [ "$(id -u)" -ne 0 ]; then
    fixture
    chmod 000 "$d/check-a.sh"
    check "an unreadable gate refuses rather than being skipped" 2 "cannot read" "$d"
    chmod 644 "$d/check-a.sh"
else
    echo "SKIP: unreadable gate (running as root)"
fi

n=$((n + 1)); mkdir -p "$TMP/case$n"; gate "$TMP/case$n/x-gate.sh" "# Expires-when: the x suite becomes cheap enough to run always (#13)."
check "no check-*.sh at all refuses" 2 "a pass here would have read nothing" "$TMP/case$n"

check "a missing directory refuses" 2 "is not a directory" "$TMP/nowhere"
check "two directories refuse" 2 "usage" "$TMP" "$TMP"

fixture
gate "$d/x-gate.sh" "# Expires-when: the x | y suite becomes cheap enough (#13)."
summary="$TMP/summary$n"
GITHUB_STEP_SUMMARY="$summary" check "--list passes and prints every gate" 0 '| `check-b.sh` | never: the beta invariant is the product contract that #12 states for every release. |' --list "$d"
check "--list escapes a pipe in the text" 0 'the x \| y suite' --list "$d"
if [ "$(grep -c '^| `' "$summary" 2>/dev/null)" = 3 ]; then
    echo "PASS: --list writes the three rows to the job summary"
else
    echo "FAIL: --list job summary: $(cat "$summary" 2>&1)"; failures=$((failures + 1))
fi

fixture
gate "$d/check-a.sh"
check "--list still lists a gate without the line, and is red" 1 '| `check-a.sh` | (missing or rejected' --list "$d"

check "the real tree passes" 0 "gates state when they expire" "$REPO/scripts"

if ! grep -qE '^\s+run: bash scripts/check-gate-expiry\.sh$' "$REPO/.github/workflows/test.yaml"; then
    echo "FAIL: gate is not wired into .github/workflows/test.yaml"; failures=$((failures + 1))
else
    echo "PASS: gate is wired into test.yaml"
fi
if ! grep -qE 'run: bash scripts/check-gate-expiry\.sh --list$' "$REPO/.github/workflows/gate-expiry.yml"; then
    echo "FAIL: the scheduled list is not wired into .github/workflows/gate-expiry.yml"; failures=$((failures + 1))
else
    echo "PASS: the scheduled list is wired into gate-expiry.yml"
fi

if [ "$failures" -ne 0 ]; then echo "$failures failure(s)"; exit 1; fi
echo "all passed"
