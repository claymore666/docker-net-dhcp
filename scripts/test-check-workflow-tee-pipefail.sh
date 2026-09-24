#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-workflow-tee-pipefail.sh (#1015), and the proof that
# the engine-matrix floor step goes red on a failed reconcile: the shipped
# step's run line and shell are run as the runner runs them, over a planted
# row that fails.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
GATE="$HERE/check-workflow-tee-pipefail.sh"
MATRIX="$ROOT/.github/workflows/engine-matrix.yml"

tmp=""
pass=0
fail=0

# check <label> <want-exit> <want-substring> <output> <got-exit>
check() {
    local label="$1" want="$2" needle="$3" out="$4" got="$5"
    if [ "$got" -eq "$want" ] && printf '%s' "$out" | grep -F -- "$needle" >/dev/null; then
        echo "ok    $label"
        pass=$((pass + 1))
    else
        echo "FAIL  $label: exit $got (want $want), output '$out'"
        fail=$((fail + 1))
    fi
}

guarded_tmpdir tmp

# plant <label> <want-exit> <want-substring> <steps...>: one workflow file
# whose single job carries the given step text.
plant() {
    local label="$1" want="$2" needle="$3" dir out rc
    shift 3
    dir="$tmp/$label"
    mkdir -p "$dir"
    {
        printf 'on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n'
        printf '%s\n' "$@"
    } > "$dir/w.yml"
    out="$(WORKFLOW_DIR="$dir" bash "$GATE" 2>&1)"
    rc=$?
    check "$label" "$want" "$needle" "$out" "$rc"
}

plant inline-tee-implicit-shell 1 "w.yml:7: pipes into tee" \
    '      - name: a' \
    '        run: make | tee out.log'
plant shell-bash-before-run 0 "OK" \
    '      - name: a' \
    '        shell: bash' \
    '        run: make | tee out.log'
plant shell-bash-after-run 0 "OK" \
    '      - name: a' \
    '        run: make | tee out.log' \
    '        shell: bash'
plant set-pipefail-above-tee 0 "OK" \
    '      - name: a' \
    '        run: |' \
    '          set -euo pipefail' \
    '          make | tee out.log'
plant set-pipefail-below-tee 1 "pipes into tee" \
    '      - name: a' \
    '        run: |' \
    '          make | tee out.log' \
    '          set -o pipefail'
plant sudo-tee-in-block 1 "w.yml:8: pipes into tee" \
    '      - name: a' \
    '        run: |' \
    '          printf x | sudo tee /etc/x > /dev/null'
plant tee-in-comment 0 "OK" \
    '      - name: a' \
    '        run: |' \
    '          # make | tee out.log' \
    '          make'
plant shell-sh 1 "shell: sh" \
    '      - name: a' \
    '        shell: sh' \
    '        run: make | tee out.log'
plant shell-names-pipefail 0 "OK" \
    '      - name: a' \
    '        shell: bash -eo pipefail {0}' \
    '        run: make | tee out.log'
plant or-is-not-a-pipe 0 "OK" \
    '      - name: a' \
    '        run: make || tee out.log'
plant shell-of-next-step-does-not-count 1 "w.yml:7: pipes into tee" \
    '      - name: a' \
    '        run: make | tee out.log' \
    '      - name: b' \
    '        shell: bash' \
    '        run: make'

mkdir -p "$tmp/empty"
out="$(WORKFLOW_DIR="$tmp/empty" bash "$GATE" 2>&1)"
check "no-workflows-cannot-check" 2 "holds no workflow files" "$out" "$?"

out="$(bash "$GATE" 2>&1)"
check "shipped-workflows" 0 "OK" "$out" "$?"

# The floor job proof. Read the shipped step, not a copy of it.
step="$(awk '
    /^  [a-z]/ { job = $1 }
    job == "floor:" && /- name: Reconcile the measured floor/ { on = 1; next }
    on && /^      - / { on = 0 }
    on { print }
' "$MATRIX")"
floor_job="$(awk '/^  [a-z]/ { job = $1 } job == "floor:"' "$MATRIX")"
run_line="$(printf '%s\n' "$step" | sed -n 's/^        run: //p')"
shell="$(printf '%s\n' "$step" | sed -n 's/^        shell: //p')"
if [ -z "$run_line" ]; then
    echo "FAIL  the floor step's run line is not in $MATRIX"
    fail=$((fail + 1))
else
    if printf '%s\n' "$floor_job" | grep 'continue-on-error' >/dev/null; then
        echo "FAIL  the floor job carries continue-on-error, so a red step cannot turn it red"
        fail=$((fail + 1))
    else
        echo "ok    floor-job-has-no-continue-on-error"
        pass=$((pass + 1))
    fi

    work="$tmp/floor"
    mkdir -p "$work/downloaded"
    ln -s "$ROOT/scripts" "$work/scripts"
    ln -s "$ROOT/pkg" "$work/pkg"
    ln -s "$ROOT/.github" "$work/.github"
    # The rows of run 35946749955 (2026-09-24), whose floor job was green:
    # every row fail or unavailable, so no row passed.
    while read -r tag engine api result step; do
        mkdir -p "$work/downloaded/engine-row-$tag/rows"
        printf 'ENGINE_MATRIX_ROW tag=%s engine=%s api=%s result=%s step=%s planted\n' \
            "$tag" "$engine" "$api" "$result" "$step" \
            > "$work/downloaded/engine-row-$tag/rows/${tag//./-}.row"
    done <<'ROWS'
19.03 19.03.15 1.40 unavailable engine-control
20.10 20.10.24 1.41 fail option-conflict_check
23 unknown unknown unavailable engine-start
24 24.0.9 1.43 fail option-conflict_check
25 25.0.5 1.44 fail option-conflict_check
26 26.1.4 1.45 fail option-conflict_check
27 27.5.1 1.47 fail option-conflict_check
28 28.5.2 1.51 fail option-conflict_check
29 29.8.1 1.56 fail option-conflict_check
ROWS
    printf '%s\n' "$run_line" > "$work/step.sh"

    # As the runner runs it: `shell: bash` is `bash --noprofile --norc
    # -eo pipefail {0}`, no shell key is `bash -e {0}`.
    if [ "$shell" = bash ]; then
        runner=(bash --noprofile --norc -eo pipefail)
    else
        runner=(bash -e)
    fi
    out="$(cd "$work" && GITHUB_STEP_SUMMARY="$work/summary" "${runner[@]}" step.sh 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep 'no row passed' >/dev/null; then
        echo "ok    floor-step-red-over-the-rows-of-run-35946749955 (exit $rc, shell '${shell:-implicit}')"
        pass=$((pass + 1))
    else
        echo "FAIL  floor step exit $rc over a failed row (shell '${shell:-implicit}'): $out"
        fail=$((fail + 1))
    fi

    # The same line under the implicit shell went green: the defect.
    out="$(cd "$work" && GITHUB_STEP_SUMMARY="$work/summary" bash -e step.sh 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep 'no row passed' >/dev/null; then
        echo "ok    floor-step-without-shell-bash-was-green (the #1015 defect)"
        pass=$((pass + 1))
    else
        echo "FAIL  the implicit shell no longer hides the failure (exit $rc); the planted row may not be failing"
        fail=$((fail + 1))
    fi
fi

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
