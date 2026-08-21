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
# Pinned to the BUILD platform on purpose: skopeo only fetches, so emulating it
# under qemu would cost minutes and buy nothing. --override-arch is what makes
# it fetch the TARGET architecture's images regardless of where it runs.
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258 AS seeder
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends skopeo ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN mkdir /seed \
    && skopeo copy --override-os linux --override-arch "${TARGETARCH}" \
         docker://docker.io/library/golang:1.26-alpine \
         docker-archive:/seed/golang-builder.tar:golang:1.26-alpine \
    && skopeo copy --override-os linux --override-arch "${TARGETARCH}" \
         docker://docker.io/library/alpine:3.20 \
         docker-archive:/seed/alpine-testimage.tar:alpine:3.20

# ---- runner image
FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

ARG GO_VERSION=1.26.6
# Resolved by the build workflow from actions/runner's latest release;
# no default on purpose so a stale pin can't hide in this file.
ARG RUNNER_VERSION
# Set by BuildKit. The two downloads below are the only arch-dependent steps,
# and they disagree on spelling: Go says linux-amd64/linux-arm64, the runner
# agent says x64/arm64. Both are derived here rather than duplicated.
ARG TARGETARCH

# Base tooling + Docker Engine from download.docker.com. The reason is
# the source, not a version threshold: Debian 13 ships docker.io 26.x,
# and we want the lane driving a current engine rather than the distro's
# frozen one. No test in test/integration/ conditions on an engine or API
# version, so treat ">= 28" as what current docker-ce happens to give us,
# not as a floor anything asserts.
#
# Note this is NOT what gates #125: `interface_name` waits on the
# upstream remote-driver change (moby/moby#52866), not on an engine
# version, so no engine floor here can unblock it. The suite decides at
# runtime with the `engineAppliesIfname` capability probe (#673).
#
# TWO DHCP servers, deliberately, for the duration of the #356
# migration. dnsmasq is what the fixture uses today; kea is what it is
# moving to, because dnsmasq's minimum lease is two minutes and every
# failure-suite wait is built on that floor rather than on anything the
# protocol or the plugin requires. Kea takes a 20s lease, which is worth
# ~7 minutes off the failure suite.
#
# Both are binary-only — there is no init system in here to start
# either; the fixtures spawn them directly on veth pairs. dnsmasq comes
# out once the fixture no longer references it, not before.
#
# chmod 0755 /etc/kea:
# the package ships it 0750 _kea:_kea, and Debian's AppArmor profile for
# /usr/sbin/kea-dhcp4 denies dac_override/dac_read_search. kea runs as root
# here, so root cannot traverse a 0750 directory it does not own and every
# config read fails with a misleading "Unable to open file". The profile is
# loaded from the HOST and attaches on exec even under --privileged, so this
# bites on any host with kea installed natively and on none without it --
# which is why CI never saw it.
#
# iputils-ping:
# the failure-injection suite's L2-reachability check shells out to
# `ping` on the runner host (failure_test.go, TestFailure_LeaseExpiry);
# the bare-metal runner had it, this slim base does not (#158).
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl git make jq procps tini xz-utils \
        iproute2 iptables dnsmasq kea-dhcp4-server kea-dhcp6-server iputils-ping \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: trixie\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
         > /etc/apt/sources.list.d/docker.sources \
    && apt-get update && apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin \
    && chmod 0755 /etc/kea \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain, same procedure as test/integration/install-go-runner.sh.
RUN case "${TARGETARCH}" in \
      amd64) go_arch=linux-amd64 ;; \
      arm64) go_arch=linux-arm64 ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${go_arch}.tar.gz" | tar -xz -C /usr/local \
    && ln -sf /usr/local/go/bin/go /usr/local/bin/go \
    && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# Actions runner agent. Jobs run as root by requirement (CAP_NET_ADMIN,
# netns/mount ops, UDP/67) — RUNNER_ALLOW_RUNASROOT is set in the
# entrypoint.
RUN test -n "${RUNNER_VERSION}" || { echo "RUNNER_VERSION build-arg is required" >&2; exit 1; } \
    && case "${TARGETARCH}" in \
         amd64) runner_arch=x64 ;; \
         arm64) runner_arch=arm64 ;; \
         *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && mkdir -p /opt/runner \
    && curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${runner_arch}-${RUNNER_VERSION}.tar.gz" \
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
COPY ci/runner-image/register.sh /register.sh
RUN chmod 755 /entrypoint.sh && chmod 644 /register.sh

# The nested daemon's data root must be a real filesystem — overlay2 on
# top of the container's own overlayfs silently degrades to vfs. An
# anonymous volume per container; the orchestrator's --rm reaps it.
VOLUME /var/lib/docker

ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
