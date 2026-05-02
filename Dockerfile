FROM golang:1.25-alpine AS builder

WORKDIR /usr/local/src/docker-net-dhcp
COPY go.* ./
RUN go mod download

COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN mkdir bin/ && go build -o bin/ ./cmd/...


FROM alpine:3.20

RUN mkdir -p /run/docker/plugins && apk add --no-cache busybox-extras iproute2

COPY --from=builder /usr/local/src/docker-net-dhcp/bin/net-dhcp /usr/sbin/
COPY --from=builder /usr/local/src/docker-net-dhcp/bin/udhcpc-handler /usr/lib/net-dhcp/udhcpc-handler

ENTRYPOINT ["/usr/sbin/net-dhcp"]
