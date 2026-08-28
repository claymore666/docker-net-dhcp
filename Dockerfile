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
# `sh`, `mount`, `mkdir`, `unshare` (per-client mount-namespace
# isolation of dhcpcd's state dir), `echo` (the per-step failure
# marker mountPrep writes to stderr, #780) and `grep` (the
# Router-Advertisement guard reads each sysctl back after writing it,
# #875) come from the base Alpine busybox. Every one of them is
# asserted below; the count that used to stand in this sentence is
# deliberately gone, because it named three while the list beside it
# named four, and a count goes false without anything editing it.
#
# The `test -x` is not belt-and-braces. pkg/dhcp names dhcpcd by the
# ABSOLUTE path /sbin/dhcpcd (#707) — a bare name would be resolved out
# of PATH by the shell that execs it — so an Alpine bump that relocates
# the binary would turn every lease into a "not found" at runtime,
# discovered by a container, not by a build.
#
# THIS LIST IS NOT MAINTAINED BY HAND, and it is not allowed to be.
# TestDockerfileGuaranteesEveryAbsoluteBinary derives the binaries
# pkg/dhcp actually runs — from its exec sites, its argv literals and
# the command words inside mountPrep's `sh -c` body — and compares that
# set against the operands below, in both directions. Adding a binary to
# the package without adding it here fails that test; asserting one here
# that nothing runs fails it too.
#
# The list grew from three to five when the derivation replaced the
# hand-written one, which is the reason it is derived now. mount and
# mkdir were missed by the original audit, then sh and unshare were
# missed by the audit that added mount and mkdir — three passes, each
# looking at the words the previous one had happened to be looking at.
# Every one of those would break the per-client mount namespace, and
# until the same change that derived this list they would have broken it
# SILENTLY: every call in mountPrep carried 2>/dev/null. Their stderr
# now reaches the plugin log.
#
# echo arrived by that derivation rather than by an audit, which is the
# point: #780 made mountPrep report each failed step by echoing a marker
# to stderr, and the test named the new command word before it could
# ship. An unasserted /bin/echo would have been the worst version of
# this bug — the marker that exists to make a silent failure loud would
# itself have failed silently, and the counter reading zero would have
# been indistinguishable from a namespace that prepared cleanly.
#
# Separate `test -x` per path, not `test -x a b c`. The one-line form is
# not a shorthand for them: busybox sh answers it
# `sh: /bin/mount: unknown operand`, rc=2, whatever the files are.
# Measured on this exact digest, all arguments present and executable
# still exits 2 — it checks nothing and fails the build.
RUN mkdir -p /run/docker/plugins /var/lib/net-dhcp && \
    apk add --no-cache \
        dhcpcd=10.3.2-r0 \
        iproute2=7.0.0-r0 && \
    test -x /sbin/dhcpcd && test -x /bin/mount && test -x /bin/mkdir && \
    test -x /bin/sh && test -x /usr/bin/unshare && test -x /bin/echo && \
    test -x /bin/grep

COPY --from=builder /usr/local/src/docker-net-dhcp/bin/net-dhcp /usr/sbin/
COPY --from=builder /usr/local/src/docker-net-dhcp/bin/dhcp-handler /usr/lib/net-dhcp/dhcp-handler

# The handler is a binary this image runs, so it is asserted like the
# rest — separately, because it arrives by COPY and cannot be checked
# before the layer that creates it.
#
# It is dhcpcd's hook script (`-c <handler>`, pkg/dhcp.DefaultHandler),
# executed on every lease event, and it is the one path where the
# destination above and the constant naming it are two copies of one
# fact. If they drift, dhcpcd reports the missing script per event and
# the plugin sees no lease — a run-time failure from a build-time typo,
# which is precisely what the assertions above exist to prevent.
RUN test -x /usr/lib/net-dhcp/dhcp-handler

ENTRYPOINT ["/usr/sbin/net-dhcp"]
