FROM golang:1.26-alpine@sha256:7a3e50096189ad57c9f9f865e7e4aa8585ed1585248513dc5cda498e2f41812c AS builder

# COVER_FLAGS is empty for the production build and `-cover -coverpkg=./...`
# for the instrumented build used by the coverage workflow. Keeping the
# instrumentation behind a build arg means the production image is byte-
# identical to the unparameterized build — no risk of accidentally shipping
# a cover-instrumented binary.
ARG COVER_FLAGS=

WORKDIR /usr/local/src/docker-net-dhcp
COPY go.* ./
RUN go mod download

COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN mkdir bin/ && go build $COVER_FLAGS -o bin/ ./cmd/...


FROM alpine:3.24.0@sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

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
COPY --from=builder /usr/local/src/docker-net-dhcp/bin/udhcpc-handler /usr/lib/net-dhcp/udhcpc-handler

ENTRYPOINT ["/usr/sbin/net-dhcp"]
