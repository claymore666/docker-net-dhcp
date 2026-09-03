# syntax=docker/dockerfile:1
# The syntax directive enables BuildKit's RUN --mount=type=cache below.
# Docker >= 23 (the runner is 26.1.5) defaults to BuildKit; the Makefile
# also exports DOCKER_BUILDKIT=1 so the classic builder can never be
# picked up and choke on the mount flags.

# The tag names the patch release and the digest pins it. Both, because
# the digest is what Docker enforces and the tag is what a reader — or
# scripts/check-go-pins.sh — can compare against the other Go pins in
# this tree. A `1.26-alpine` tag hid go1.26.5 here through v1.5.0 (#525).
FROM golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder

# COVER_FLAGS is empty for the production build and `-cover -coverpkg=./...`
# for the instrumented build used by the coverage workflow. Keeping the
# instrumentation behind a build arg means the production image is byte-
# identical to the unparameterized build — no risk of accidentally shipping
# a cover-instrumented binary.
ARG COVER_FLAGS=

WORKDIR /usr/local/src/docker-net-dhcp
COPY go.* ./
# The replaced module's own go.mod must exist before `go mod download`
# reads the replace directive, so its manifest is copied ahead of the
# source. Its tree carries no dependencies of its own (the sync script
# refuses a library that grew one), so this adds nothing to download.
COPY internal/dhcp-golib/go.mod ./internal/dhcp-golib/go.mod
# Persist the module cache across builds: unchanged go.* means no
# re-download, and the modules survive even when the build layer is
# invalidated by a code change (#255).
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
# The DHCP library travels as a directory of this branch, resolved by
# the `replace` in go.mod (D21). It is a nested module, so `go build
# ./...` above does not walk into it; the compiler reaches it only
# through the import path.
COPY internal/ ./internal/
# The COPY above invalidates this layer on every code change, so go build
# re-runs each PR — but the mounted build cache makes it INCREMENTAL:
# only the packages that actually changed recompile, the rest are reused.
# Go's build cache is keyed on source + flags, so the production and
# -cover builds never reuse each other's objects (no stale-digest hazard).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir bin/ && go build $COVER_FLAGS -o bin/ ./cmd/...


FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# THE IMAGE CONTAINS NO DHCP CLIENT, AND THAT IS AN ASSERTION, NOT A
# SIDE EFFECT. 2.0 performs the whole exchange in the plugin process
# through the in-house library, so there is no dhcpcd, no hook script
# and no helper the plugin shells out to: `grep -rn exec.Command pkg/
# cmd/` matches nothing.
#
# The absence is asserted positively below because the test that used to
# guarantee this list has had its domain emptied. It derived the
# binaries pkg/dhcp execs and compared them against the `test -x`
# operands in both directions; with no exec sites left, the derived set
# is empty and every hand-written operand would be an error while
# asserting nothing at all. An empty universal is satisfied by emptying
# its domain, so the guarantee is restated as something emptying cannot
# satisfy: no DHCP client is installed, and none can be resolved from
# PATH.
#
# libcrypto3/libssl3 are pinned to a patched version of a library the
# plugin does not use — it is Go, and links neither. The image is not
# free of OpenSSL (apk-tools, busybox's ssl_client and OpenSSL's engine
# modules link it), but none of those is on a path the plugin invokes,
# which is why this is hygiene and not an incident. They are pinned
# because the base digest above ships 3.5.7-r0, whose unfixed CVEs the
# scanner reports against every release; carrying known findings makes a
# real one harder to see.
#
# Exact pins rather than `apk upgrade`: a version that has been
# superseded fails the build loudly instead of drifting silently between
# builds of the same Dockerfile.
#
# Clears: when the base digest above moves to an Alpine that already
# ships 3.5.8-r0 or later, these two lines are redundant and should go.
RUN mkdir -p /run/docker/plugins /var/lib/net-dhcp && \
    apk add --no-cache \
        iproute2=7.0.0-r0 \
        libcrypto3=3.5.8-r0 \
        libssl3=3.5.8-r0 && \
    for c in dhcpcd udhcpc udhcpc6 dhclient dhcpcd6; do \
        if command -v "$c" >/dev/null 2>&1 || [ -e "/sbin/$c" ] || [ -e "/usr/sbin/$c" ]; then \
            echo "A DHCP client ($c) is present in the image. 2.0 leases in-process; anything that can also lease is a second, unmanaged client on the same link." >&2; \
            exit 1; \
        fi; \
    done

COPY --from=builder /usr/local/src/docker-net-dhcp/bin/net-dhcp /usr/sbin/

# The plugin binary is the only thing this image runs, so it is the only
# thing asserted. The dhcp-handler assertion that stood here went with
# the handler: there is no hook script, because there is no child
# process to hook.
RUN test -x /usr/sbin/net-dhcp

ENTRYPOINT ["/usr/sbin/net-dhcp"]
