#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-go-pins.sh.
#
# Each case builds a throwaway tree with the five files the gate reads,
# mutates exactly one of them, and asserts the verdict. Building the
# tree instead of pointing at the repository matters: a test that ran
# against the real checkout would pass for as long as the checkout
# happens to be consistent, and would stop testing anything the moment
# someone fixed the tree — which is the vacuous-pass shape that let
# #517 reach a tag.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-go-pins.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
n=0

# A tree where every pin agrees on 1.26.6 and go.mod trails within the
# minor — deliberately the real repository's shape, so the fixture and
# the thing it stands for cannot drift apart silently.
make_tree() { # make_tree <dir>
    local d="$1"
    mkdir -p "$d/.github/workflows" "$d/ci/runner-image" "$d/test/integration"
    printf 'module example.com/x\n\ngo 1.26.4\n' > "$d/go.mod"
    printf 'FROM golang:1.26.6-alpine@sha256:abc AS builder\n' > "$d/Dockerfile"
    printf 'ARG GO_VERSION=1.26.6\n' > "$d/ci/runner-image/Dockerfile"
    printf 'GO_VERSION="${GO_VERSION:-1.26.6}"\n' > "$d/test/integration/install-go-runner.sh"
    cat > "$d/.github/workflows/test.yaml" <<'EOF'
jobs:
  a:
    steps:
      - uses: actions/setup-go@v7
        with:
          go-version: '1.26.6'
EOF
}

run_case() { # run_case <name> <want_exit> <mutator>
    n=$((n + 1))
    local name="$1" want="$2" mutate="$3" d="$TMP/case$n" got
    make_tree "$d"
    "$mutate" "$d"
    bash "$CHECK" "$d" > "$TMP/out" 2>&1
    got=$?
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name — wanted exit $want, got $got"
        sed 's/^/      /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

noop() { :; }

# The baseline. If this ever fails, every negative case below is
# meaningless — they would be passing for the wrong reason.
run_case "consistent tree passes" 0 noop

# The bug that started #525: a minor-version range. setup-go resolves it
# from the hosted tool cache, so it names no particular release.
floating() { sed -i "s/'1.26.6'/'1.26'/" "$1/.github/workflows/test.yaml"; }
run_case "floating go-version range fails" 1 floating

alias_ver() { sed -i "s/'1.26.6'/stable/" "$1/.github/workflows/test.yaml"; }
run_case "go-version alias 'stable' fails" 1 alias_ver

# A workflow added after this gate was written must still be read. The
# gate scans the directory; a whitelist of filenames would miss this.
new_wf() {
    cat > "$1/.github/workflows/later.yml" <<'EOF'
jobs:
  b:
    steps:
      - uses: actions/setup-go@v7
        with:
          go-version: '1.26'
EOF
}
run_case "a later workflow's bad pin is caught" 1 new_wf

# go-version-file is a legitimate shape — it resolves to the directive,
# which the gate checks separately.
gvf_gomod() {
    sed -i "s/go-version: '1.26.6'/go-version-file: go.mod/" "$1/.github/workflows/test.yaml"
}
run_case "go-version-file: go.mod passes" 0 gvf_gomod

gvf_other() {
    sed -i "s/go-version: '1.26.6'/go-version-file: .go-version/" "$1/.github/workflows/test.yaml"
}
run_case "go-version-file pointing elsewhere fails" 1 gvf_other

# The builder image is what users actually run. A digest-pinned tag that
# does not name the release is exactly what shipped go1.26.5.
tagless() {
    printf 'FROM golang:1.26-alpine@sha256:abc AS builder\n' > "$1/Dockerfile"
}
run_case "builder tag without a patch release fails" 1 tagless

undigested() {
    printf 'FROM golang:1.26.6-alpine AS builder\n' > "$1/Dockerfile"
}
run_case "builder image without a digest fails" 1 undigested

# A half-applied bump — the state this gate exists to catch.
half_bump() { printf 'ARG GO_VERSION=1.26.4\n' > "$1/ci/runner-image/Dockerfile"; }
run_case "disagreeing toolchain pins fail" 1 half_bump

stale_helper() {
    printf 'GO_VERSION="${GO_VERSION:-1.25.0}"\n' > "$1/test/integration/install-go-runner.sh"
}
run_case "stale install-helper default fails" 1 stale_helper

# go.mod trails within the minor by design: the self-hosted pool bakes
# its own toolchain, and raising the directive past it breaks every
# integration build. Trailing a whole minor is rot, not slack.
same_minor() { printf 'module example.com/x\n\ngo 1.26.0\n' > "$1/go.mod"; }
run_case "go.mod trailing within the minor passes" 0 same_minor

old_minor() { printf 'module example.com/x\n\ngo 1.25.9\n' > "$1/go.mod"; }
run_case "go.mod a whole minor behind fails" 1 old_minor

# Requiring more than any toolchain provides: nothing can build it.
ahead() { printf 'module example.com/x\n\ngo 1.26.9\n' > "$1/go.mod"; }
run_case "go.mod ahead of the toolchain fails" 1 ahead

# A pin the gate cannot see is worse than one it disagrees with.
no_arg() { printf '# nothing here\n' > "$1/ci/runner-image/Dockerfile"; }
run_case "missing ARG GO_VERSION fails" 1 no_arg

no_from() { printf '# no builder stage\n' > "$1/Dockerfile"; }
run_case "missing builder FROM fails" 1 no_from

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures case(s) failed"
    exit 1
fi
echo "all $n cases passed"
