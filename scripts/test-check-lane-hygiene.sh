#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-lane-hygiene.sh (#742).
#
# The two invariants this gate carries share a property that makes
# them hard to test any other way: each failure is an ABSENCE. A lane
# without a teardown leaves state on a machine nothing inspects; a
# workflow without `edited` in its types simply never runs. So the cases
# below check that the gate reports the absence — and, in every section,
# that the corresponding presence still reads as clean, because a gate
# that fires on both is one nobody can satisfy.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(cd "$(dirname "$0")" && pwd)/check-lane-hygiene.sh"
pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# verdict <dir> -> the exit code
verdict() { bash "$GATE" "$1" >/dev/null 2>&1; echo $?; }

# --- A. teardown for a lane that installs a plugin ----------------------

mk_install() {
    # $1 dir, $2 "teardown"|"none"
    local d="$1"
    {
        printf 'name: lane\non:\n  workflow_dispatch:\njobs:\n  suite:\n'
        printf '    runs-on: ubuntu-latest\n    steps:\n'
        printf '      - name: Build + install\n        run: |\n'
        printf '          docker plugin rm -f "$REF" 2>/dev/null || true\n'
        printf '          docker plugin create "$REF" plugin\n'
        printf '          docker plugin enable "$REF"\n'
        if [ "$2" = teardown ]; then
            printf '      - name: Tear down integration plugin\n        if: always()\n        run: |\n'
            printf '          docker plugin rm -f "$REF" 2>/dev/null || true\n'
        fi
    } > "$d/lane.yml"
}

guarded_tmpdir d; mk_install "$d" none
[ "$(verdict "$d")" = 1 ] \
    && ok "a lane that installs a plugin with no teardown is reported" \
    || no "a lane with no teardown should exit 1"
rm -rf "$d"

guarded_tmpdir d; mk_install "$d" teardown
[ "$(verdict "$d")" = 0 ] \
    && ok "the same lane with an if: always() teardown is clean" \
    || no "a lane with a teardown should be clean"
rm -rf "$d"

# The pre-install `docker plugin rm -f` inside the BUILD step is not a
# teardown — it runs before the plugin this job installs exists. That is
# exactly what integration-arm64.yml had, and the reason a naive
# whole-file grep for `plugin rm` would have called it clean.
guarded_tmpdir d
{
    printf 'name: lane\non:\n  workflow_dispatch:\njobs:\n  suite:\n'
    printf '    runs-on: ubuntu-latest\n    steps:\n'
    printf '      - name: Build + install\n        if: always()\n        run: |\n'
    printf '          docker plugin rm -f "$REF" 2>/dev/null || true\n'
    printf '          docker plugin create "$REF" plugin\n'
} > "$d/lane.yml"
[ "$(verdict "$d")" = 1 ] \
    && ok "a pre-install 'plugin rm' inside the build step is not a teardown" \
    || no "a pre-install rm was accepted as a teardown"
rm -rf "$d"

# --- a mention guards and runs nothing (#883) --------------------------
# Each decoy passed while the gate matched `if: always()` and the command
# as text anywhere in a step. The control is the same step with the
# decoy text gone: red on any version of this gate, so the decoy was
# the whole pass.
mk_step() {
    # $1 dir, $2.. the lines of one step after the install step
    local d="$1"; shift
    {
        printf 'name: lane\non:\n  workflow_dispatch:\njobs:\n  suite:\n'
        printf '    runs-on: ubuntu-latest\n    steps:\n'
        printf '      - name: Build + install\n        run: |\n'
        printf '          docker plugin create "$REF" plugin\n'
        printf '%s\n' "$@"
    } > "$d/lane.yml"
}
decoy() {
    # $1 want, $2 label, $3.. step lines
    local want="$1" label="$2"; shift 2
    guarded_tmpdir d; mk_step "$d" "$@"
    [ "$(verdict "$d")" = "$want" ] && ok "$label" || no "$label (want $want)"
    rm -rf "$d"
}
decoy 1 "A: a teardown whose plugin rm is an echo argument is not a teardown" \
    '      - name: Tear down' '        if: always()' \
    '        run: echo "docker plugin rm -f $REF runs elsewhere"'
decoy 1 "A control: the same step with the echo gone" \
    '      - name: Tear down' '        if: always()' '        run: "true"'
decoy 1 "A: if: always() written in run: is not the step's if" \
    '      - name: Tear down' '        run: |' '          echo "if: always()"' \
    '          docker plugin rm -f "$REF" || true'
decoy 1 "A: if: always() in a step name is not the step's if" \
    '      - name: "Tear down, if: always()"' '        run: docker plugin rm -f "$REF" || true'
decoy 1 "A: an if: always() line inside a run block is not the step's if" \
    '      - name: Tear down' '        run: |' "          cat <<'Y'" '          if: always()' \
    '          Y' '          docker plugin rm -f "$REF" || true'
decoy 1 "A control: the teardown with no if: at all" \
    '      - name: Tear down' '        run: docker plugin rm -f "$REF" || true'

decoy 0 "A: a teardown whose first key is if: always() is a teardown" \
    '      - if: always()' '        name: Tear down' '        run: docker plugin rm -f "$REF" || true'

# --- C. `edited` where a gate reads the PR body -------------------------

mk_body_gate() {
    # $1 dir, $2 types-line-or-empty
    local d="$1"
    {
        printf 'name: t\non:\n  pull_request:\n'
        [ -n "$2" ] && printf '    types: %s\n' "$2"
        printf 'jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n'
        printf '      - name: Weakening\n        run: bash scripts/check-test-weakening.sh "$R" "$B"\n'
    } > "$d/t.yaml"
}

guarded_tmpdir d; mk_body_gate "$d" ""
[ "$(verdict "$d")" = 1 ] \
    && ok "a body-reading gate with the default pull_request types is reported" \
    || no "default types with a body-reading gate should exit 1"
rm -rf "$d"

# The default set spelled out explicitly is still the default set. This
# is the case that separates "checks for edited" from "checks that types
# is present at all" — and the latter would have called the tree clean.
guarded_tmpdir d; mk_body_gate "$d" "[opened, synchronize, reopened]"
[ "$(verdict "$d")" = 1 ] \
    && ok "types listed without 'edited' is still reported" \
    || no "an explicit types list missing 'edited' should exit 1"
rm -rf "$d"

guarded_tmpdir d; mk_body_gate "$d" "[opened, synchronize, reopened, edited]"
[ "$(verdict "$d")" = 0 ] \
    && ok "types including 'edited' is clean" \
    || no "types including 'edited' should be clean"
rm -rf "$d"

# A workflow that runs no body-reading gate is out of scope: demanding
# `edited` everywhere would fire runs on every typo in every PR body.
guarded_tmpdir d
{
    printf 'name: t\non:\n  pull_request:\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n'
    printf '      - name: Unit\n        run: go test ./...\n'
} > "$d/t.yaml"
[ "$(verdict "$d")" = 0 ] \
    && ok "a workflow with no body-reading gate does not need 'edited'" \
    || no "a workflow with no body-reading gate was required to have 'edited'"
rm -rf "$d"

# --- prose cannot trip or satisfy any of it -----------------------------
# These workflows document their own invariants at length, quoting the
# very strings this gate matches on.
guarded_tmpdir d
{
    printf 'name: lane\non:\n  workflow_dispatch:\n'
    printf '# this lane runs docker plugin create and tears it down with if: always()\n'
    printf '# and its failure suite runs make integration-test-failure\n'
    printf 'jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n'
    printf '      - name: Unit\n        run: go test ./...\n'
} > "$d/lane.yml"
[ "$(verdict "$d")" = 0 ] \
    && ok "comments naming the matched strings neither trip nor satisfy the gate" \
    || no "a comment tripped the gate"
rm -rf "$d"

# --- A through a local composite action (#746) --------------------------
# The lanes install and tear down through ./.github/actions/*, so the
# create and the rm sit in an action.yml, not in the workflow. Each case
# below that expects 1 or 2 passed as 0 before the gate followed them:
# the workflow text no longer named `docker plugin create` at all.

mk_composite_lane() {
    # $1 root, $2 teardown step: "always" | "plain" | "none" | "quoted"
    local d="$1"
    mkdir -p "$d/.github/workflows" "$d/.github/actions/install" "$d/.github/actions/teardown"
    printf 'name: i\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: |\n        docker plugin rm -f "$REF" || true\n        docker plugin create "$REF" plugin\n' \
        > "$d/.github/actions/install/action.yml"
    printf 'name: t\nruns:\n  using: composite\n  steps:\n    - shell: bash\n      run: |\n        docker plugin disable "$REF" || true\n        docker plugin rm -f "$REF" || true\n' \
        > "$d/.github/actions/teardown/action.yml"
    {
        printf 'name: lane\non:\n  workflow_dispatch:\njobs:\n  suite:\n'
        printf '    runs-on: ubuntu-latest\n    steps:\n'
        printf '      - uses: actions/checkout@0000000000000000000000000000000000000000\n'
        printf '      - name: Enable\n        uses: ./.github/actions/install\n'
        case "$2" in
            always) printf '      - name: Tear down\n        if: always()\n        uses: ./.github/actions/teardown\n' ;;
            plain)  printf '      - name: Tear down\n        uses: ./.github/actions/teardown\n' ;;
            quoted) printf '      - name: Tear down\n        if: always()\n        uses: "./.github/actions/teardown/"\n' ;;
        esac
    } > "$d/.github/workflows/lane.yml"
}

guarded_tmpdir d; mk_composite_lane "$d" none
[ "$(verdict "$d/.github/workflows")" = 1 ] \
    && ok "a plugin installed through a composite with no teardown is reported" \
    || no "a composite install with no teardown should exit 1"
rm -rf "$d"

guarded_tmpdir d; mk_composite_lane "$d" always
[ "$(verdict "$d/.github/workflows")" = 0 ] \
    && ok "a composite install with an if: always() composite teardown is clean" \
    || no "a composite install and if: always() composite teardown should be clean"
rm -rf "$d"

guarded_tmpdir d; mk_composite_lane "$d" quoted
[ "$(verdict "$d/.github/workflows")" = 0 ] \
    && ok "a quoted ./path/ with a trailing slash resolves to the same action" \
    || no "a quoted, slash-terminated local action should resolve"
rm -rf "$d"

guarded_tmpdir d; mk_composite_lane "$d" plain
[ "$(verdict "$d/.github/workflows")" = 1 ] \
    && ok "a composite teardown without if: always() is not a teardown" \
    || no "a composite teardown without if: always() should exit 1"
rm -rf "$d"

guarded_tmpdir d; mk_composite_lane "$d" always
mv "$d/.github/actions/teardown/action.yml" "$d/.github/actions/teardown/action.yaml"
[ "$(verdict "$d/.github/workflows")" = 0 ] \
    && ok "a composite spelled action.yaml is read like action.yml" \
    || no "an action.yaml teardown should count as the teardown"
rm -rf "$d"

guarded_tmpdir d; mk_composite_lane "$d" always
rm -rf "$d/.github/actions/teardown"
[ "$(verdict "$d/.github/workflows")" = 2 ] \
    && ok "a local action with no action.yml is rc2, not a step with no commands" \
    || no "a missing local action should exit 2"
rm -rf "$d"

# A composite that calls another local action hides its commands from this
# gate, so it is refused in either spelling of the step (#746).
for spelling in 'on a dash line:    - uses: ./.github/actions/install' 'on its own line:    - name: nested\n      uses: ./.github/actions/install' 'quoted:    - uses: "./.github/actions/install/"'; do
    guarded_tmpdir d; mk_composite_lane "$d" none
    mkdir "$d/.github/actions/lane-install"
    printf 'name: n\nruns:\n  using: composite\n  steps:\n%b\n' "${spelling#*:}" \
        > "$d/.github/actions/lane-install/action.yml"
    sed -i 's|uses: ./.github/actions/install$|uses: ./.github/actions/lane-install|' "$d/.github/workflows/lane.yml"
    [ "$(verdict "$d/.github/workflows")" = 2 ] \
        && ok "a composite calling a local action is rc2, not a step with no commands: ${spelling%%:*}" \
        || no "a nested local action call should exit 2"
    rm -rf "$d"
done

guarded_tmpdir d; mk_composite_lane "$d" always
mkdir "$d/.github/actions/lane-note"
printf 'name: n\nruns:\n  using: composite\n  steps:\n    # uses: ./.github/actions/install\n    - shell: bash\n      run: echo hi\n' \
    > "$d/.github/actions/lane-note/action.yml"
sed -i 's|^      - name: Enable$|      - uses: ./.github/actions/lane-note\n      - name: Enable|' "$d/.github/workflows/lane.yml"
[ "$(verdict "$d/.github/workflows")" = 0 ] \
    && ok "a comment naming a local action in a composite is not a call" \
    || no "a commented uses: in a composite should not be refused"
rm -rf "$d"

guarded_tmpdir d; mk_composite_lane "$d" always
mkdir "$d/.github/actions/lane-remote"
printf 'name: n\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@0000000000000000000000000000000000000000\n' \
    > "$d/.github/actions/lane-remote/action.yml"
sed -i 's|^      - name: Enable$|      - uses: ./.github/actions/lane-remote\n      - name: Enable|' "$d/.github/workflows/lane.yml"
[ "$(verdict "$d/.github/workflows")" = 0 ] \
    && ok "a composite using a remote action is not refused" \
    || no "a remote action inside a composite should not be refused"
rm -rf "$d"

# The real lanes, with one teardown removed: the arm64 runner is the
# standing one, so its teardown is the one that matters (#742).
guarded_tmpdir d
mkdir -p "$d/.github"
cp -r "$(dirname "$GATE")/../.github/workflows" "$(dirname "$GATE")/../.github/actions" "$d/.github/"
awk '/^      - name: Tear down integration plugin$/ { skip = 1; next }
     skip && /^[[:space:]]*$/ { skip = 0 }
     !skip' "$(dirname "$GATE")/../.github/workflows/integration-arm64.yml" \
    > "$d/.github/workflows/integration-arm64.yml"
[ "$(verdict "$d/.github/workflows")" = 1 ] \
    && ok "integration-arm64.yml with its teardown removed is reported" \
    || no "the real arm64 lane without its teardown should exit 1"
rm -rf "$d"

# --- inspecting nothing is not a pass -----------------------------------
guarded_tmpdir d
[ "$(verdict "$d")" = 2 ] \
    && ok "an empty workflow directory is rc2, not a pass" \
    || no "an empty directory should exit 2"
rm -rf "$d"

[ "$(verdict /nonexistent-workflow-dir)" = 2 ] \
    && ok "a missing workflow directory is rc2" \
    || no "a missing workflow directory should exit 2"

# A workflow whose indentation this gate cannot parse must REFUSE. The
# alternative is the failure mode every gate here was audited for in
# #743: a clean pass rendered over an input set that came out empty.
guarded_tmpdir d
printf 'name: x\non:\n  workflow_dispatch:\njobs:\n  a:\n    steps:\n      "not a step"\n' > "$d/x.yml"
[ "$(verdict "$d")" = 2 ] \
    && ok "a 'steps:' block this gate cannot parse is rc2, not a clean pass" \
    || no "an unparseable steps block should exit 2"
rm -rf "$d"

# --- the real repository ------------------------------------------------
real=$( cd "$(dirname "$GATE")/.." && bash "$GATE" >/dev/null 2>&1; echo $? )
[ "$real" = 0 ] \
    && ok "this repository passes its own lane-hygiene gate" \
    || no "this repository fails its own lane-hygiene gate (exit $real)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
