#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Go version-pin consistency gate (#525).
#
# There is no single place in this tree that says which Go we build
# with, and before this gate there were four answers that disagreed:
# go.mod said 1.26.4, six workflow steps said '1.26', the CI runner
# image baked 1.26.4, and the builder image shipped go1.26.5 to users.
# The last one is the one that mattered, and it was the least visible.
#
# What this gate enforces:
#
#   1. Every TOOLCHAIN pin names an exact patch release (X.Y.Z). A
#      range like '1.26' is rejected on its shape, not its value:
#      actions/setup-go satisfies a range from the hosted image's tool
#      cache without consulting upstream, so a range does not name a
#      version at all — it names whatever GitHub baked in that week.
#      That is how go1.26.5 got into a release nobody chose it for.
#   2. All toolchain pins agree with each other.
#   3. go.mod's `go` directive shares their minor and is not ahead of
#      them. The directive is a MINIMUM, not a build pin, so it is
#      allowed to trail within the minor (the self-hosted pool bakes
#      its own Go, and raising the directive past the pool's toolchain
#      breaks every integration build). Trailing a whole minor is not
#      allowed: that is rot, not slack.
#
# What it deliberately does NOT do: check whether a newer Go exists
# upstream. That needs network, and govulncheck already tells us —
# loudly, by failing — the moment a patch we are missing matters.
#
# `go-version-file:` is an accepted shape wherever it points at go.mod:
# it resolves to the directive, which this gate has already checked.
# Pointing it anywhere else would create a second source of truth and
# is rejected.
#
# Usage: check-go-pins.sh [<repo-root>]   (defaults to the repo root)
set -u

root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$root" || { echo "cannot enter $root" >&2; exit 2; }

fails=0
fail() { echo "FAIL  $*" >&2; fails=$((fails + 1)); }

is_exact() { case "$1" in [0-9]*.[0-9]*.[0-9]*) return 0 ;; *) return 1 ;; esac; }
minor_of() { printf '%s\n' "${1%.*}"; }

# Collected as "<version>\t<where>" so a disagreement can name its
# sources instead of just reporting that one exists.
pins=""
add_pin() { pins="${pins}${1}	${2}
"; }

# --- 1. Workflow setup-go steps ---------------------------------------
# Every `go-version:`/`go-version-file:` key under .github/workflows,
# found by scanning the directory rather than a list of known files —
# a workflow added later must not be able to introduce a pin this gate
# never looks at.
shopt -s nullglob
for wf in .github/workflows/*.yml .github/workflows/*.yaml; do
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        key="${line%%:*}"; key="${key##*[[:space:]]}"
        val="${line#*:}"
        # Strip whitespace, quotes and any trailing comment.
        val="$(printf '%s' "$val" | sed -E "s/#.*$//; s/^[[:space:]]*//; s/[[:space:]]*$//; s/^['\"]//; s/['\"]$//")"
        case "$key" in
            go-version)
                if is_exact "$val"; then
                    add_pin "$val" "${wf} (go-version)"
                else
                    fail "${wf}: go-version '${val}' is not an exact X.Y.Z pin."
                    echo "      A range or alias lets the hosted tool cache choose the patch" >&2
                    echo "      release we build and scan with. Name the patch." >&2
                fi
                ;;
            go-version-file)
                [ "$val" = "go.mod" ] || \
                    fail "${wf}: go-version-file points at '${val}', not go.mod — a second source of truth."
                ;;
        esac
    done <<< "$(grep -hE '^[[:space:]]*go-version(-file)?:' "$wf" 2>/dev/null)"
done
shopt -u nullglob

# --- 2. The CI runner image's baked toolchain -------------------------
if [ -f ci/runner-image/Dockerfile ]; then
    v="$(sed -nE 's/^ARG[[:space:]]+GO_VERSION=([^[:space:]]+).*/\1/p' ci/runner-image/Dockerfile | head -1)"
    if [ -z "$v" ]; then
        fail "ci/runner-image/Dockerfile: no 'ARG GO_VERSION=' found — the baked toolchain is invisible to this gate."
    elif is_exact "$v"; then
        add_pin "$v" "ci/runner-image/Dockerfile (ARG GO_VERSION)"
    else
        fail "ci/runner-image/Dockerfile: GO_VERSION '${v}' is not an exact X.Y.Z pin."
    fi
fi

# --- 3. The builder image that produces what users run ----------------
# Matched on the tag, which is why the tag has to carry the patch
# release alongside the digest. A `golang:1.26-alpine@sha256:...`
# reference is digest-pinned and therefore perfectly reproducible, and
# still tells nobody which Go it is — that is the shape that shipped
# go1.26.5 through v1.5.0.
if [ -f Dockerfile ]; then
    ref="$(grep -oE '^FROM[[:space:]]+golang:[^[:space:]]+' Dockerfile | head -1)"
    if [ -z "$ref" ]; then
        fail "Dockerfile: no 'FROM golang:...' builder stage found."
    else
        tag="${ref#*golang:}"; tag="${tag%%@*}"
        v="${tag%-alpine}"
        if is_exact "$v"; then
            add_pin "$v" "Dockerfile (builder image tag)"
        else
            fail "Dockerfile: builder tag 'golang:${tag}' does not name a patch release."
            echo "      Use golang:X.Y.Z-alpine@sha256:... — the digest pins it, the tag says what it is." >&2
        fi
        case "$ref" in
            *@sha256:*) ;;
            *) fail "Dockerfile: builder image is not digest-pinned." ;;
        esac
    fi
fi

# --- 4. The bare-metal runner install helper --------------------------
if [ -f test/integration/install-go-runner.sh ]; then
    v="$(sed -nE 's/^GO_VERSION="\$\{GO_VERSION:-([^}]+)\}".*/\1/p' test/integration/install-go-runner.sh | head -1)"
    if [ -z "$v" ]; then
        fail "test/integration/install-go-runner.sh: no default GO_VERSION found."
    elif is_exact "$v"; then
        add_pin "$v" "test/integration/install-go-runner.sh (default)"
    else
        fail "test/integration/install-go-runner.sh: default '${v}' is not an exact X.Y.Z pin."
    fi
fi

# --- 5. All toolchain pins must agree ---------------------------------
versions="$(printf '%s' "$pins" | grep -v '^$' | cut -f1 | sort -u)"
count="$(printf '%s\n' "$versions" | grep -c . || true)"

if [ "$count" -eq 0 ]; then
    fail "no Go toolchain pins found at all — this gate is looking in the wrong place."
elif [ "$count" -gt 1 ]; then
    fail "Go toolchain pins disagree:"
    printf '%s' "$pins" | grep -v '^$' | sort | sed 's/^/      /' >&2
    echo "      Every pin must name the same patch release. If a bump is" >&2
    echo "      in progress, finish it — a half-applied bump is what this" >&2
    echo "      gate exists to catch." >&2
fi

# --- 6. go.mod's directive: same minor, not ahead ---------------------
if [ -f go.mod ]; then
    gomod="$(sed -nE 's/^go[[:space:]]+([0-9][^[:space:]]*).*/\1/p' go.mod | head -1)"
    if [ -z "$gomod" ]; then
        fail "go.mod: no 'go' directive found."
    elif [ "$count" -eq 1 ]; then
        toolchain="$versions"
        if [ "$(minor_of "$gomod")" != "$(minor_of "$toolchain")" ]; then
            fail "go.mod says 'go ${gomod}' but the toolchain is ${toolchain} — different minor versions."
            echo "      The directive may trail within a minor (the self-hosted pool" >&2
            echo "      bakes its own Go); trailing a whole minor is rot." >&2
        elif [ "$gomod" != "$toolchain" ]; then
            # Trailing within the minor is allowed; leading is not — it
            # would make the pool's baked Go too old to build the module.
            newest="$(printf '%s\n%s\n' "$gomod" "$toolchain" | sort -V | tail -1)"
            [ "$newest" = "$toolchain" ] || \
                fail "go.mod requires go ${gomod}, newer than the ${toolchain} toolchain every pin names — nothing can build this."
        fi
    fi
fi

if [ "$fails" -ne 0 ]; then
    echo >&2
    echo "${fails} Go pin problem(s). See scripts/check-go-pins.sh for what each pin is for." >&2
    exit 1
fi

echo "OK  Go pins agree: $(printf '%s' "$versions") (go.mod directive: ${gomod:-none})"
