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

# Pin both the Alpine minor and the apk package versions: busybox supplies
# udhcpc (the entire DHCP exchange), so a silent regression in busybox-extras
# would land in plugin builds without warning. Bump together when refreshing.
RUN mkdir -p /run/docker/plugins /var/lib/net-dhcp && \
    apk add --no-cache \
        busybox-extras=1.36.1-r31 \
        iproute2=6.9.0-r0

COPY --from=builder /usr/local/src/docker-net-dhcp/bin/net-dhcp /usr/sbin/
COPY --from=builder /usr/local/src/docker-net-dhcp/bin/udhcpc-handler /usr/lib/net-dhcp/udhcpc-handler

ENTRYPOINT ["/usr/sbin/net-dhcp"]
