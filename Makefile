PLUGIN_NAME ?= ghcr.io/claymore666/docker-net-dhcp
PLUGIN_TAG ?= golang
PLATFORMS ?= linux/amd64,linux/arm64

SOURCES = $(shell find pkg/ cmd/ -name '*.go')
BINARY = bin/net-dhcp

PLUGIN_COVER_TAG ?= golang-cover

.PHONY: all debug build create enable disable pdebug push clean integration-test integration-cleanup \
        build-cover plugin-cover create-cover enable-cover disable-cover

all: create enable

bin/%: $(SOURCES)
	go build -o $@ ./cmd/$(shell basename $@)

debug: $(BINARY)
	sudo $< -log debug

build: $(SOURCES)
	docker build -t $(PLUGIN_NAME):rootfs .

plugin/rootfs: build
	mkdir -p plugin/rootfs
	docker create --name tmp $(PLUGIN_NAME):rootfs
	docker export tmp | tar xC plugin/rootfs
	docker rm -vf tmp

plugin: plugin/rootfs config.json
	cp config.json $@/

create: plugin
	docker plugin rm -f $(PLUGIN_NAME):$(PLUGIN_TAG) || true
	docker plugin create $(PLUGIN_NAME):$(PLUGIN_TAG) $<
	docker plugin set $(PLUGIN_NAME):$(PLUGIN_TAG) LOG_LEVEL=trace

enable: plugin
	docker plugin enable $(PLUGIN_NAME):$(PLUGIN_TAG)
disable:
	docker plugin disable $(PLUGIN_NAME):$(PLUGIN_TAG)

pdebug: create enable
	sudo sh -c 'tail -f /var/lib/docker/plugins/*/rootfs/var/log/net-dhcp.log'

push: create
	docker plugin push $(PLUGIN_NAME):$(PLUGIN_TAG)

# Coverage-instrumented build path. Produces a parallel plugin tagged
# :golang-cover with `go build -cover` instrumentation. On graceful
# shutdown the runtime flushes counter files into /coverage inside the
# plugin namespace, which is bind-mounted from the host's /var/lib/dh-cover
# (must exist and be writable; create it once with `mkdir -p
# /var/lib/dh-cover` before the first `make create-cover`).
#
# This path is for the integration coverage workflow only — production
# installs continue to use `make create enable` / the unparameterized
# image. The two tags coexist on the same host without conflicting.
build-cover: $(SOURCES)
	docker build --build-arg COVER_FLAGS="-cover -coverpkg=./..." -t $(PLUGIN_NAME):rootfs-cover .

plugin-cover/rootfs: build-cover
	mkdir -p plugin-cover/rootfs
	docker create --name tmp-cover $(PLUGIN_NAME):rootfs-cover
	docker export tmp-cover | tar xC plugin-cover/rootfs
	docker rm -vf tmp-cover

plugin-cover: plugin-cover/rootfs config-cover.json
	cp config-cover.json $@/config.json

create-cover: plugin-cover
	docker plugin rm -f $(PLUGIN_NAME):$(PLUGIN_COVER_TAG) || true
	docker plugin create $(PLUGIN_NAME):$(PLUGIN_COVER_TAG) $<
	docker plugin set $(PLUGIN_NAME):$(PLUGIN_COVER_TAG) LOG_LEVEL=trace

enable-cover:
	docker plugin enable $(PLUGIN_NAME):$(PLUGIN_COVER_TAG)
disable-cover:
	docker plugin disable $(PLUGIN_NAME):$(PLUGIN_COVER_TAG)

multiarch: $(SOURCES)
	docker buildx build --platform=$(PLATFORMS) -o type=local,dest=$@ .

push-multiarch: multiarch config.json
	scripts/push_multiarch_plugin.py -p $(PLATFORMS) config.json multiarch $(PLUGIN_NAME):$(PLUGIN_TAG)

clean:
	-rm -rf multiarch/
	-rm -rf plugin/
	-rm -rf plugin-cover/
	-rm bin/*

# Live integration tests. Need privileges (CAP_NET_ADMIN, mount/netns
# ops, bind UDP/67) and the plugin already enabled at PLUGIN_NAME:golang.
# Locally: `sudo make integration-test`. CI: runner is root, target
# detects and skips the sudo wrapper.
integration-test:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "integration-test must run as root. Re-run with sudo."; \
		exit 1; \
	fi
	go test -v -tags integration -count=1 -timeout 10m ./test/integration/...

# Manual orphan cleanup for when an integration test panics mid-setup
# and leaves dh-itest-* interfaces / containers / networks behind.
integration-cleanup:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "integration-cleanup must run as root. Re-run with sudo."; \
		exit 1; \
	fi
	bash test/integration/cleanup-orphans.sh
