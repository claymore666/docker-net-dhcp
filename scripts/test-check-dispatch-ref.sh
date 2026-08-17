#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-dispatch-ref.sh (#593).
#
# Two halves, matching the gate's two modes.
#
# The RUNTIME half pins the allowlist. `refs/pull/N/head` is the exploit
# that was actually available, so it gets a case; the rest of the red
# set is there because a deny list only knows the routes somebody
# already thought of, and this issue exists because a route nobody
# thought of was taken.
#
# The STATIC half is the one that earns its keep. #593 was not "nobody
# wrote the rule down" — the rule WAS written down, in integration.yml's
# SECURITY block, correctly. It was that a second consumer of the same
# input appeared and the prose had no way to notice. So the cases that
# matter here are the ones where a guard exists and is in the wrong
# place: covering the first consumer and not the second, and sitting
# below the checkout it claims to guard. Both are green to a reader
# skimming for the presence of a guard, and both are red here.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-dispatch-ref.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

ok() {
    echo "  ok   — $1"
    pass=$((pass + 1))
}
no() {
    echo "  FAIL — $1"
    fail=$((fail + 1))
}

# validate <ref> -> prints exit code
validate() {
    bash "$GATE" --validate "$1" >/dev/null 2>&1
    echo $?
}

# run <dir> -> prints exit code
run() {
    bash "$GATE" "$1" >/dev/null 2>&1
    echo $?
}

fixture() {
    local dir="$TMP/$1"
    mkdir -p "$dir"
    cat >"$dir/wf.yml"
    echo "$dir"
}

echo "check-dispatch-ref.sh self-test"

# ------------------------------------------------------- runtime: green

for good in "" dev main v1.6.0 v1.7.0-rc1 refs/heads/dev refs/tags/v1.6.0 \
    feature/218-stable-mac 8558ff2b465e7534419a85ab499edc4cc1f7ae83 8558ff2; do
    [ "$(validate "$good")" = "0" ] &&
        ok "accepts '${good:-<empty>}'" ||
        no "'${good:-<empty>}' is a legitimate branch/tag/SHA and must be accepted"
done

# ---------------------------------------------------------- runtime: red

# THE case. A fork's PR head is fetchable from the base repository, and
# a dispatch run carries the repository secrets.
[ "$(validate refs/pull/593/head)" = "1" ] &&
    ok "rejects refs/pull/593/head" ||
    no "refs/pull/593/head is the #593 exploit and must be rejected"

[ "$(validate refs/pull/593/merge)" = "1" ] &&
    ok "rejects refs/pull/593/merge" ||
    no "the merge ref is as unreviewed as the head ref"

# The bare spelling somebody tries after the qualified one is refused.
[ "$(validate pull/593/head)" = "1" ] &&
    ok "rejects the bare pull/593/head" ||
    no "pull/593/head must be rejected too"

[ "$(validate refs/remotes/pull/1/head)" = "1" ] &&
    ok "rejects refs/remotes/pull/1/head" ||
    no "the remotes/ spelling must be rejected too"

# Not a PR head, and still not something this input is for. This is the
# allowlist doing the work a deny list could not.
[ "$(validate refs/notes/commits)" = "1" ] &&
    ok "rejects a qualified ref that is neither heads/ nor tags/" ||
    no "only refs/heads/* and refs/tags/* are fully-qualified refs we run"

# A ref beginning with a dash reaches git as an option, not a ref.
[ "$(validate --upload-pack=id)" = "1" ] &&
    ok "rejects a ref that would reach git as an option" ||
    no "a leading '-' is argument injection, not a ref"

for bad in 'dev;id' 'dev$(id)' 'dev with spaces' 'a..b'; do
    [ "$(validate "$bad")" = "1" ] &&
        ok "rejects '$bad'" ||
        no "'$bad' is not a well-formed ref and must be rejected"
done

# Usage errors are exit 2, distinct from a rejected ref. A caller that
# passes no argument at all must not look like a caller that passed a
# safe one.
bash "$GATE" --validate >/dev/null 2>&1
[ "$?" = "2" ] &&
    ok "--validate with no argument is a usage error, not a pass" ||
    no "--validate with no argument must exit 2, not 0"

bash "$GATE" --nonsense >/dev/null 2>&1
[ "$?" = "2" ] &&
    ok "an unknown option is a usage error" ||
    no "an unknown option must exit 2"

# ----------------------------------------------------------- static: red

# The shape that shipped. This is integration.yml's `suite` job before
# the fix: a dispatched ref checked out with nothing having refused a
# PR head first.
d=$(fixture unguarded <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
      - run: make integration-test
EOF
)
[ "$(run "$d")" = "1" ] &&
    ok "an ungated checkout of inputs.ref is red" ||
    no "the pre-#593 shape must be red — this is the whole point of the gate"

# THE case #593 asks for by name. The guard is present and wired into
# the first consumer; a second job consumes the same input and is not
# covered. A reader greps for the guard, finds it, and moves on.
d=$(fixture guarded-first-consumer-only <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - run: bash scripts/check-dispatch-ref.sh --validate "${{ inputs.ref }}"
  suite:
    needs: gate
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
  extra:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
EOF
)
[ "$(run "$d")" = "1" ] &&
    ok "a job outside the guard's needs graph is red even though the guard exists" ||
    no "a guard on the first consumer only is exactly the #593 failure and must be red"

# Ordering. A guard below the checkout it guards is decoration: the
# untrusted tree is already on disk by the time it speaks.
d=$(fixture guard-after-checkout <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  suite:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
      - run: bash scripts/check-dispatch-ref.sh --validate "${{ inputs.ref }}"
EOF
)
[ "$(run "$d")" = "1" ] &&
    ok "a --validate call BELOW the checkout is red" ||
    no "a guard that runs after the checkout guards nothing and must be red"

# The guard must run from a job the consumer actually depends on. A
# validating job that merely exists proves nothing about ordering:
# without `needs`, the two run concurrently.
d=$(fixture guard-not-needed <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  guard:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - run: bash scripts/check-dispatch-ref.sh --validate "${{ inputs.ref }}"
  suite:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
EOF
)
[ "$(run "$d")" = "1" ] &&
    ok "a guard job the consumer does not need is red" ||
    no "without needs: the guard and the consumer run concurrently — must be red"

# The indirect spellings. pages.yml reached the same place through a
# step output rather than through inputs. directly, which is why the
# gate resolves one hop rather than grepping for `inputs.`.
d=$(fixture indirect-step-output <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      tag:
        type: string
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - name: Resolve
        id: ref
        env:
          INPUT_TAG: ${{ inputs.tag }}
        run: echo "ref=${INPUT_TAG:-dev}" >> "$GITHUB_OUTPUT"
      - uses: actions/checkout@v7
        with:
          ref: ${{ steps.ref.outputs.ref }}
EOF
)
[ "$(run "$d")" = "1" ] &&
    ok "an input laundered through a step output is still caught" ||
    no "the pages.yml shape must be caught — inputs. need not appear in the ref itself"

d=$(fixture indirect-job-output <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      tag:
        type: string
jobs:
  resolve:
    runs-on: ubuntu-latest
    outputs:
      tag: ${{ steps.t.outputs.tag }}
    steps:
      - id: t
        run: echo "tag=${{ inputs.tag }}" >> "$GITHUB_OUTPUT"
  publish:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ needs.resolve.outputs.tag }}
EOF
)
[ "$(run "$d")" = "1" ] &&
    ok "an input laundered through a job output is still caught" ||
    no "the release.yml shape must be caught — needs.<job>.outputs carries the input too"

# --------------------------------------------------------- static: green

# The fixed shape: one guard, in the job every consumer needs.
d=$(fixture gated-transitively <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - run: bash scripts/check-dispatch-ref.sh --validate "${{ inputs.ref }}"
  suite:
    needs: gate
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
EOF
)
[ "$(run "$d")" = "0" ] &&
    ok "a consumer that needs the guard job is green" ||
    no "the shipped fix's shape must be green"

# Two hops. The graph is walked, not just the direct parents.
d=$(fixture gated-two-hops <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/check-dispatch-ref.sh --validate "${{ inputs.ref }}"
  middle:
    needs: gate
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  suite:
    needs: [middle]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
EOF
)
[ "$(run "$d")" = "0" ] &&
    ok "the needs graph is walked transitively, not one level" ||
    no "a guard two hops up still covers the consumer"

# The block spelling of needs:, which is what release.yml's neighbours
# use. A parser that only understood the scalar form would report a
# correctly-guarded job as red and get switched off.
d=$(fixture needs-block-form <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/check-dispatch-ref.sh --validate "${{ inputs.ref }}"
  suite:
    needs:
      - gate
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
EOF
)
[ "$(run "$d")" = "0" ] &&
    ok "needs: in block form is understood" ||
    no "all three spellings of needs: must parse or the gate reports false reds"

# A checkout with no ref at all — the trusted case the fix moved
# integration.yml's gate and watchdog jobs to.
d=$(fixture no-ref <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - run: bash scripts/integration-run-gate.sh dispatch
EOF
)
[ "$(run "$d")" = "0" ] &&
    ok "a checkout with no ref: needs no guard" ||
    no "a trusted checkout is the fix, not a finding"

# A literal ref cannot carry an input, and pinning that keeps the gate
# from firing on ordinary workflows.
d=$(fixture literal-ref <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  docs:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: gh-pages
EOF
)
[ "$(run "$d")" = "0" ] &&
    ok "a literal ref: is not a finding" ||
    no "a literal ref carries no input and must not be reported"

# github.* is not a dispatch input. This workflow uses no
# pull_request_target, and reporting github.ref would make the gate
# fire on almost every workflow in the repo — noise that gets a check
# switched off rather than fixed.
d=$(fixture github-context-ref <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ github.sha }}
EOF
)
[ "$(run "$d")" = "0" ] &&
    ok "a github.* context ref is not a dispatch input" ||
    no "github.sha is not attacker-supplied and must not be reported"

# The gate reads its own file, which is dense with commentary quoting
# the very patterns it looks for. A check that counts its own
# explanation as behaviour reports the opposite of the truth.
d=$(fixture comments-only <<'EOF'
name: X
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      # Never do this:
      #   - uses: actions/checkout@v7
      #     with:
      #       ref: ${{ inputs.ref }}
      - uses: actions/checkout@v7
      - run: echo hi
EOF
)
[ "$(run "$d")" = "0" ] &&
    ok "a checkout described in a comment is not a checkout" ||
    no "commented-out YAML must not be read as behaviour"

# ------------------------------------------------------- static: cannot run

# The empty-glob guard, same reasoning as run-gate-selftests.sh: a
# discovered list that matches nothing is worse than a hand-maintained
# one, because it reports success having examined nothing.
d="$TMP/empty"
mkdir -p "$d"
[ "$(run "$d")" = "2" ] &&
    ok "a directory with no workflows is exit 2, not a pass" ||
    no "examining nothing must not report success"

[ "$(run "$TMP/definitely-not-here")" = "2" ] &&
    ok "a missing directory is exit 2" ||
    no "a missing directory must not report success"

# --------------------------------------------------------------- verdict

echo
echo "check-dispatch-ref.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
