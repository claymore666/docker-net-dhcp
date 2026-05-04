FROM golang:1.25-alpine AS builder

WORKDIR /usr/local/src/docker-net-dhcp
COPY go.* ./
RUN go mod download

COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN mkdir bin/ && go build -o bin/ ./cmd/...


FROM alpine:3.20.3@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

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
