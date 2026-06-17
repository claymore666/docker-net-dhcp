# syntax=docker/dockerfile:1
# The syntax directive enables BuildKit's RUN --mount=type=cache below.
# Docker >= 23 (the runner is 26.1.5) defaults to BuildKit; the Makefile
# also exports DOCKER_BUILDKIT=1 so the classic builder can never be
# picked up and choke on the mount flags.

FROM golang:1.26-alpine@sha256:f1ddd9fe14fffc091dd98cb4bfa999f32c5fc77d2f2305ea9f0e2595c5437c14 AS builder

# COVER_FLAGS is empty for the production build and `-cover -coverpkg=./...`
# for the instrumented build used by the coverage workflow. Keeping the
# instrumentation behind a build arg means the production image is byte-
# identical to the unparameterized build — no risk of accidentally shipping
# a cover-instrumented binary.
ARG COVER_FLAGS=

WORKDIR /usr/local/src/docker-net-dhcp
COPY go.* ./
# Persist the module cache across builds: unchanged go.* means no
# re-download, and the modules survive even when the build layer is
# invalidated by a code change (#255).
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
# The COPY above invalidates this layer on every code change, so go build
# re-runs each PR — but the mounted build cache makes it INCREMENTAL:
# only the packages that actually changed recompile, the rest are reused.
# Go's build cache is keyed on source + flags, so the production and
# -cover builds never reuse each other's objects (no stale-digest hazard).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir bin/ && go build $COVER_FLAGS -o bin/ ./cmd/...


FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Pin both the Alpine minor and the apk package versions: dhcpcd performs
# the entire DHCP/DHCPv6 exchange (#152 — it replaced busybox udhcpc/udhcpc6
# so the DHCPv6 IAID can be pinned and the one-shot + persistent clients
# share one identity association). A silent regression here would land in
# plugin builds without warning, so pin and bump deliberately. dhcpcd 10.x
# is required (the per-interface model used here is removed in dhcpcd 11).
# `sh`, `mount`, and `unshare` (per-client mount-namespace isolation of
# dhcpcd's state dir) come from the base Alpine busybox.
RUN mkdir -p /run/docker/plugins /var/lib/net-dhcp && \
    apk add --no-cache \
        dhcpcd=10.3.2-r0 \
        iproute2=7.0.0-r0

COPY --from=builder /usr/local/src/docker-net-dhcp/bin/net-dhcp /usr/sbin/
COPY --from=builder /usr/local/src/docker-net-dhcp/bin/dhcp-handler /usr/lib/net-dhcp/dhcp-handler

ENTRYPOINT ["/usr/sbin/net-dhcp"]
