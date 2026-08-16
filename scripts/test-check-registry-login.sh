#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-registry-login.sh (#562).
#
# The gate's whole value is that it goes RED on the tree that shipped
# the outage, so most of these cases are red cases. A check nobody has
# watched fail is a check nobody knows the shape of.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-registry-login.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

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
