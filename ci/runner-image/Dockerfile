# Ephemeral CI runner image — one container, one job, own nested
# Docker daemon. See README.md in this directory for the orchestrator
# contract and issue #149 for the design record.
#
# Build context is the REPO ROOT (caches are baked from go.mod and the
# source tree):
#   docker build -f ci/runner-image/Dockerfile \
#     --build-arg RUNNER_VERSION=<x.y.z> -t dhcp-ci-runner .

# ---- seed stage: fetch images the jobs would otherwise pull on every
# run. skopeo needs no docker daemon, so this works in a plain build.
# The plugin's digest-pinned base (alpine:3.20.3@sha256:...) is NOT
# seeded: `docker load` can't satisfy digest references, and at 3.5 MB
# the per-job pull is noise. The golang builder is the one that hurts.
FROM debian:trixie-slim@sha256:4e401d95de7083948053197a9c3913343cd06b706bf15eb6a0c3ccd26f436a0e AS seeder
RUN apt-get update && apt-get install -y --no-install-recommends skopeo ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN mkdir /seed \
    && skopeo copy docker://docker.io/library/golang:1.25-alpine \
         docker-archive:/seed/golang-builder.tar:golang:1.25-alpine \
    && skopeo copy docker://docker.io/library/alpine:3.20 \
         docker-archive:/seed/alpine-testimage.tar:alpine:3.20

# ---- runner image
FROM debian:trixie-slim@sha256:4e401d95de7083948053197a9c3913343cd06b706bf15eb6a0c3ccd26f436a0e

ARG GO_VERSION=1.25.9
# Resolved by the build workflow from actions/runner's latest release;
# no default on purpose so a stale pin can't hide in this file.
ARG RUNNER_VERSION

# Base tooling + Docker Engine from download.docker.com (Debian 13's
# docker.io is 26.x; the nested daemon must be >= 28 — engine-gated
# tests, #125). dnsmasq is the test fixture's DHCP server: binary
# only, there is no init system in here to start it. iputils-ping:
# the failure-injection suite's L2-reachability check shells out to
# `ping` on the runner host (failure_test.go, TestFailure_LeaseExpiry);
# the bare-metal runner had it, this slim base does not (#158).
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl git make jq procps tini xz-utils \
        iproute2 iptables dnsmasq iputils-ping \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: trixie\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
         > /etc/apt/sources.list.d/docker.sources \
    && apt-get update && apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain, same procedure as test/integration/install-go-runner.sh.
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -xz -C /usr/local \
    && ln -sf /usr/local/go/bin/go /usr/local/bin/go \
    && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# Actions runner agent. Jobs run as root by requirement (CAP_NET_ADMIN,
# netns/mount ops, UDP/67) — RUNNER_ALLOW_RUNASROOT is set in the
# entrypoint.
RUN test -n "${RUNNER_VERSION}" || { echo "RUNNER_VERSION build-arg is required" >&2; exit 1; } \
    && mkdir -p /opt/runner \
    && curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" \
       | tar -xz -C /opt/runner \
    && /opt/runner/bin/installdependencies.sh \
    && rm -rf /var/lib/apt/lists/*

# Baked Go caches: module cache from go.mod/go.sum, then a warm compile
# cache from building the tree (integration tag pulls in the docker SDK
# test deps). PR source will differ, but stdlib and dependency compile
# units still hit. Source is dropped afterwards; only caches stay.
COPY go.mod go.sum /tmp/cache-src/
RUN cd /tmp/cache-src && go mod download
COPY pkg/ /tmp/cache-src/pkg/
COPY cmd/ /tmp/cache-src/cmd/
COPY test/ /tmp/cache-src/test/
RUN cd /tmp/cache-src && go build -tags integration ./... && go vet -tags integration ./... || true; \
    rm -rf /tmp/cache-src

COPY --from=seeder /seed /seed
COPY ci/runner-image/entrypoint.sh /entrypoint.sh
RUN chmod 755 /entrypoint.sh

# The nested daemon's data root must be a real filesystem — overlay2 on
# top of the container's own overlayfs silently degrades to vfs. An
# anonymous volume per container; the orchestrator's --rm reaps it.
VOLUME /var/lib/docker

ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
