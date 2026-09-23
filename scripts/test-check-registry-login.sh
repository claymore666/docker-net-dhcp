#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-registry-login.sh (#562).
#
# The gate's whole value is that it goes RED on the tree that shipped
# the outage, so most of these cases are red cases. A check nobody has
# watched fail is a check nobody knows the shape of.

set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-registry-login.sh"
guarded_tmpdir TMP

pass=0
fail=0

ok()  { echo "  ok   — $1"; pass=$((pass + 1)); }
no()  { echo "  FAIL — $1"; fail=$((fail + 1)); }

# run <dir> -> prints exit code, swallows output
run() { bash "$GATE" "$1" >/dev/null 2>&1; echo $?; }

fixture() {
    local dir="$TMP/$1"
    mkdir -p "$dir"
    cat > "$dir/wf.yml"
    echo "$dir"
}

echo "check-registry-login.sh self-test"

# ---------------------------------------------------------------- red

d=$(fixture pool-build-no-login <<'EOF'
name: X
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - uses: actions/checkout@v5
      - name: Build
        run: make plugin
EOF
)
[ "$(run "$d")" = "1" ] && ok "a pool job that builds without logging in is red" \
    || no "a pool job that builds without logging in must be red"

# The case that motivated the ordering rule. A login placed after the
# build authenticates nothing: the pull has already happened, and
# already 429'd.
d=$(fixture login-after-build <<'EOF'
name: X
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - name: Build
        run: make plugin
      - uses: docker/login-action@abc
EOF
)
[ "$(run "$d")" = "1" ] && ok "a login AFTER the build is red" \
    || no "a login after the build authenticates nothing and must be red"

# `docker build` directly, rather than through Make.
d=$(fixture raw-docker-build <<'EOF'
name: X
jobs:
  scan:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - run: docker build -t x:scan .
EOF
)
[ "$(run "$d")" = "1" ] && ok "a raw docker build on the pool is red" \
    || no "docker build must count as a build, not only the Make targets"

# `make create` packages the plugin, which runs a docker build — the
# release path's own step. It must not be invisible for want of the
# word "build".
d=$(fixture make-create <<'EOF'
name: X
jobs:
  package:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - run: make create
EOF
)
[ "$(run "$d")" = "1" ] && ok "make create counts as a build" \
    || no "make create runs docker build and must count as one"

# One clean job must not launder a dirty one in the same file.
d=$(fixture one-of-two-jobs-dirty <<'EOF'
name: X
jobs:
  good:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - uses: docker/login-action@abc
      - run: make plugin
  bad:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - run: make plugin
EOF
)
[ "$(run "$d")" = "1" ] && ok "a clean job does not launder a dirty one beside it" \
    || no "each job is judged on its own steps"

# -------------------------------------------------------------- green

d=$(fixture pool-build-with-login <<'EOF'
name: X
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    env:
      HAS_HUB_CREDS: ${{ secrets.DOCKERHUB_USERNAME != '' }}
    steps:
      - uses: actions/checkout@v5
      - name: Log in to Docker Hub
        if: env.HAS_HUB_CREDS == 'true'
        uses: docker/login-action@abc
      - name: Build
        run: make plugin
EOF
)
[ "$(run "$d")" = "0" ] && ok "a pool job that logs in before building is green" \
    || no "the fixed shape must be green"

# A hosted runner pulls from an address this project neither controls
# nor shares with itself, so it is out of scope. Widening to hosted
# jobs would demand a credential on every fork PR's hosted job — which
# is precisely the thing that must never be mandatory.
d=$(fixture hosted-build-no-login <<'EOF'
name: X
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - run: docker build -t x:scan .
EOF
)
[ "$(run "$d")" = "0" ] && ok "a hosted job that builds without logging in is out of scope" \
    || no "only the shared pool is in scope"

# A pool job that never builds has nothing to authenticate for.
d=$(fixture pool-no-build <<'EOF'
name: X
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - run: go test ./...
EOF
)
[ "$(run "$d")" = "0" ] && ok "a pool job that builds nothing needs no login" \
    || no "a job with no build must not be required to log in"

# A raw `docker login` is authentication too. The gate asserts the
# property, not one spelling of it.
d=$(fixture raw-docker-login <<'EOF'
name: X
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - run: docker login -u u --password-stdin
      - run: make plugin
EOF
)
[ "$(run "$d")" = "0" ] && ok "a raw docker login counts as authentication" \
    || no "docker login must satisfy the check as well as the action"

# This gate's own file is dense with prose naming `make plugin` and
# `docker build`. If comments counted, the check would fire on its own
# explanation — and the fix would be to delete the explanation.
d=$(fixture commented-build <<'EOF'
name: X
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      # This job used to run `make plugin` and `docker build` here.
      # See the note about docker/login-action.
      - run: go test ./...
EOF
)
[ "$(run "$d")" = "0" ] && ok "a build named only in a comment is not a build" \
    || no "comments carry no behaviour and must not trip the check"

# ------------------------------------ a mention logs nothing in (#883)
# Each decoy authenticated while any job line naming the login counted.
# Its control is the same job with the decoy line gone, red on any
# version of this gate, so the decoy alone was the pass.
decoy() {
    # $1 want, $2 label, $3 fixture name, stdin the steps after checkout
    local want="$1" label="$2" d
    d=$(fixture "$3" < <(printf 'name: X\njobs:\n  suite:\n    runs-on: [self-hosted, dhcp-ci]\n    steps:\n      - uses: actions/checkout@v5\n'; cat))
    [ "$(run "$d")" = "$want" ] && ok "$label" || no "$label (want $want)"
}
decoy 1 "an echoed docker login is not a login" echo-login <<'EOF'
      - name: Log in
        run: echo "docker login runs elsewhere"
      - run: make plugin
EOF
decoy 1 "control: the same job without the echo" echo-login-del <<'EOF'
      - name: Log in
        run: "true"
      - run: make plugin
EOF
decoy 1 "a step named after the login action is not a login" name-login <<'EOF'
      - name: docker/login-action@abc runs in the runner image
        run: "true"
      - run: make plugin
EOF
decoy 1 "the login action as a with: value is not a login" with-login <<'EOF'
      - uses: actions/cache@v4
        with:
          key: docker/login-action@abc
      - run: make plugin
EOF
decoy 1 "docker login in an env: value is not a login" env-login <<'EOF'
      - env:
          HOW: docker login -u u
        run: "true"
      - run: make plugin
EOF
decoy 1 "a uses: line inside a run: block is not a login" run-uses-login <<'EOF'
      - run: |
          cat <<'Y'
          uses: docker/login-action@abc
          Y
      - run: make plugin
EOF
decoy 1 "a mention before the build does not move a later login forward" order-login <<'EOF'
      - name: docker login comes after the build here
        run: "true"
      - run: make plugin
      - uses: docker/login-action@abc
EOF
decoy 1 "control: the later login without the mention" order-login-del <<'EOF'
      - run: make plugin
      - uses: docker/login-action@abc
EOF
decoy 0 "a docker login fed by a pipe counts" pipe-login <<'EOF'
      - run: echo "$T" | docker login -u u --password-stdin
      - run: make plugin
EOF
decoy 0 "the login action on the dash line counts" dash-login <<'EOF'
      - uses: docker/login-action@abc
      - run: make plugin
EOF

decoy 0 "a second login after the build does not undo the first" second-login <<'EOF'
      - uses: docker/login-action@abc
      - run: make plugin
      - uses: docker/login-action@abc
EOF
decoy 1 "a login after the build in the same step is after it" same-step-login <<'EOF'
      - run: |
          make plugin
          docker login -u u --password-stdin
EOF
decoy 1 "a step name naming docker login does not date a later login in its shell" same-step-named <<'EOF'
      - name: Build, then docker login
        run: |
          make plugin
          docker login -u u --password-stdin
EOF
decoy 1 "an echo before the build does not date a later login in the same step" same-step-echo <<'EOF'
      - run: |
          echo "docker login follows"
          make plugin
          docker login -u u --password-stdin
EOF
decoy 0 "control: a login before the build in the same step, echo first" same-step-before <<'EOF'
      - run: |
          echo "docker login follows"
          docker login -u u --password-stdin
          make plugin
EOF
decoy 1 "a login inside a string spanning lines does not date the real one" same-step-string <<'EOF'
      - run: |
          echo "note:
          docker login later"
          make plugin
          docker login -u u --password-stdin
EOF
decoy 0 "a one-line run: login is dated at its own line" run-line-login <<'EOF'
      - run: docker login -u u --password-stdin
        env:
          NEXT: make plugin
          NOTE: docker login
EOF
decoy 1 "a login split over lines is dated at its last mention, not an earlier echo" split-login <<'EOF'
      - run: |
          echo "docker login follows"
          make plugin
          docker login -u u \
            --password-stdin
EOF
# ------------------------------------------------- could-not-run (2)

[ "$(run "$TMP/does-not-exist")" = "2" ] && ok "a missing directory exits 2, not 0" \
    || no "a missing directory must not read as a pass"

mkdir -p "$TMP/empty"
[ "$(run "$TMP/empty")" = "2" ] && ok "a directory with no workflows exits 2, not 0" \
    || no "examining nothing must not report success"

# ------------------------------------------------ the real workflows

# The tree itself, so the gate cannot drift from what it guards. This
# is what would have caught #562 before it cost a diagnostic round trip.
if [ -d "$HERE/../.github/workflows" ]; then
    [ "$(run "$HERE/../.github/workflows")" = "0" ] \
        && ok "this repository's own workflows pass" \
        || no "this repository's own workflows FAIL the check — see the gate's output"
fi

echo
echo "check-registry-login.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
