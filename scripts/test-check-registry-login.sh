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

# expect_err <label> <want-rc> <needle> <dir> [env-assignment...]
#
# The exit code alone cannot tell a guard from a crash, from a
# different guard, or from the same guard reporting the wrong thing --
# two guards in the sibling gate for #839 were surviving deletion for
# exactly that reason. So the refusal cases below assert the MESSAGE as
# well, and `clean` asserts that no `::error` was printed at all.
expect_err() {
    local label="$1" want="$2" needle="$3" dir="$4"; shift 4
    local out rc why=''
    out=$(env "$@" bash "$GATE" "$dir" 2>&1); rc=$?
    [ "$rc" = "$want" ] || why="exit $rc, want $want"
    if [ "$needle" = clean ]; then
        printf '%s\n' "$out" | grep -F '::error' >/dev/null \
            && why="${why:+$why; }printed an ::error and should not have"
    elif ! printf '%s\n' "$out" | grep -F -- "$needle" >/dev/null; then
        why="${why:+$why; }no output matching \"$needle\""
    fi
    if [ -z "$why" ]; then ok "$label"; else no "$label ($why)"; printf '      %s\n' "$out"; fi
}

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

# --------------------------------- a file that could not be READ (2)
#
# `examined` counted loop visits and `scan_file()` discarded awk's exit
# status, so a workflow nobody could open was reported as a workflow
# with nothing wrong in it. Measured 2026-08-28 against the gate as it
# stood: with a REAL #562 violation planted -- a job on the pool
# running `docker build` and no login step -- exit 1 while readable,
# and exit 0 at mode 000, printing
# `OK — examined 2 workflow file(s)` and counting the file it could
# not read.
#
# Each refusal below is paired with the readable control, because a
# gate that refused everything would satisfy the refusal cases without
# ever having been fixed.
mkdir -p "$TMP/unreadable"
cat > "$TMP/unreadable/ok.yml" <<'EOF'
name: X
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - name: Log in to Docker Hub
        uses: docker/login-action@aaaa
      - run: docker build .
EOF
cat > "$TMP/unreadable/zz-bad.yml" <<'EOF'
name: X
jobs:
  suite:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - run: docker build .
EOF
expect_err "the planted violation is red while the file is readable" 1 \
    "no login step" "$TMP/unreadable"
chmod 000 "$TMP/unreadable/zz-bad.yml"
expect_err "an unreadable workflow refuses instead of losing its finding" 2 \
    "is not a readable regular file" "$TMP/unreadable"
chmod 644 "$TMP/unreadable/zz-bad.yml"
expect_err "the same corpus readable again is still red, not refused" 1 \
    "no login step" "$TMP/unreadable"

# A DIRECTORY named *.yml is the shape the two awks disagree about:
# mawk cannot open it and exits 2, gawk skips it with a warning and
# exits 0. Deciding readability in the shell is what makes the verdict
# the same on both.
rm -f "$TMP/unreadable/zz-bad.yml"
mkdir -p "$TMP/unreadable/zz-bad.yml"
expect_err "a directory named *.yml refuses on either awk" 2 \
    "is not a readable regular file" "$TMP/unreadable"
rmdir "$TMP/unreadable/zz-bad.yml"

# THE OTHER HALF OF THE REFUSAL, which no workflow fixture can drive.
# With `-f` and `-r` asked first, awk is never handed a file it cannot
# open, so "awk failed while still printing" is a branch with no case
# unless the awk itself is a seam. The stub prints a plausible,
# well-formed finding line and then exits non-zero: only the status can
# tell the gate that something went wrong.
AWK_STUB="$TMP/awk-stub"
cat > "$AWK_STUB" <<'STUB'
#!/bin/sh
printf 'x.yml	suite	no login step	7
'
exit 2
STUB
chmod +x "$AWK_STUB"
expect_err "an awk that prints a finding and then fails is a refusal" 2 \
    "so its jobs were not judged" "$TMP/unreadable" "REGISTRY_LOGIN_AWK=$AWK_STUB"

# Preservation for the seam itself: the same stub exiting 0 must be
# BELIEVED, or the case above would pass because the seam is broken
# rather than because the status is read.
cat > "$AWK_STUB" <<'STUB'
#!/bin/sh
printf 'x.yml	suite	no login step	7
'
STUB
chmod +x "$AWK_STUB"
expect_err "the same finding line with status 0 is believed, not refused" 1 \
    "no login step" "$TMP/unreadable" "REGISTRY_LOGIN_AWK=$AWK_STUB"

# The third preservation control, and the one that says the status
# check did not simply start refusing: an awk that reads every file,
# finds nothing and exits 0 is a PASS, printing no `::error` at all.
cat > "$AWK_STUB" <<'STUB'
#!/bin/sh
exit 0
STUB
chmod +x "$AWK_STUB"
expect_err "an awk that reads every file and finds nothing is still a pass" 0 \
    clean "$TMP/unreadable" "REGISTRY_LOGIN_AWK=$AWK_STUB"

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
