PLUGIN_NAME ?= ghcr.io/claymore666/docker-net-dhcp
PLUGIN_TAG ?= golang

SOURCES = $(shell find pkg/ cmd/ -name '*.go')
BINARY = bin/net-dhcp

PLUGIN_COVER_TAG ?= golang-cover

# Outage-watchdog cadence for locally built / test plugins (#278). The
# shipped defaults are 30s/25s; the failure suite pays them on top of a
# fixture lease it cannot shorten below dnsmasq's 2-minute floor, so a
# tighter cadence buys back most of that wait. The grace stays well
# above a healthy client's acquisition time — drop it near zero and
# ordinary start-up registers as an outage.
#
# These are set on locally created plugins only. Released images carry
# config.json's defaults, and nothing here reaches them.
TEST_OUTAGE_TICK ?= 2s
TEST_OUTAGE_GRACE ?= 10s

.PHONY: all debug build create enable disable pdebug push clean check integration-test \
        integration-test-failure integration-test-shard integration-local integration-cleanup \
        build-cover plugin-cover create-cover enable-cover disable-cover capture-fixtures

# These targets' prerequisites are a SEQUENCE, not a set: `enable` needs
# `create` to have finished, and integration-local's cleanup must precede
# the install it cleans up for. Under `make -jN` make is free to run
# prerequisites concurrently, which would race the plugin install against
# the suite that uses it. Scoped .NOTPARALLEL (GNU Make >= 4.4) serialises
# only these, leaving -j useful everywhere else.
.NOTPARALLEL: all pdebug integration-local

all: create enable

bin/%: $(SOURCES)
	go build -o $@ ./cmd/$(shell basename $@)

debug: $(BINARY)
	sudo $< -log debug

build: $(SOURCES)
	DOCKER_BUILDKIT=1 docker build -t $(PLUGIN_NAME):rootfs .

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
	docker plugin set $(PLUGIN_NAME):$(PLUGIN_TAG) LOG_LEVEL=trace \
	    OUTAGE_TICK=$(TEST_OUTAGE_TICK) OUTAGE_GRACE=$(TEST_OUTAGE_GRACE)

# STATE_DIR is bind-mounted from the host (#440) and the daemon does not
# create a missing bind source, so enabling without it fails with an OCI
# mount error. It lives here and not in `create` (#517): packaging needs
# no host directory, and putting it in `create` meant the release build —
# which packages and pushes but never enables — tried to create a
# root-owned directory as an unprivileged runner, and v1.5.0-rc1 died on
# exactly that. Unprivileged first, sudo only if that fails, so a root
# shell without sudo installed still works.
enable: plugin
	[ -d /var/lib/net-dhcp ] || mkdir -p /var/lib/net-dhcp 2>/dev/null || sudo mkdir -p /var/lib/net-dhcp
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
# plugin namespace, which is bind-mounted from the host's /var/lib/dh-cover.
# `create-cover` creates every /var/lib bind source its manifest declares,
# derived from config-cover.json rather than listed here: a bind source
# whose creation is prose is a bind source that eventually is not created,
# and dockerd does not degrade when one is missing -- it fails the mount,
# and an already-enabled plugin takes the daemon down with it (#588, #660).
# The /var/lib filter is deliberate: /var/run/docker.sock is a bind source
# too, and `mkdir -p` over a socket path would replace it with a directory.
#
# This path is for the integration coverage workflow only — production
# installs continue to use `make create enable` / the unparameterized
# image. The two tags coexist on the same host without conflicting.
build-cover: $(SOURCES)
	DOCKER_BUILDKIT=1 docker build --build-arg COVER_FLAGS="-cover -coverpkg=./..." -t $(PLUGIN_NAME):rootfs-cover .

plugin-cover/rootfs: build-cover
	mkdir -p plugin-cover/rootfs
	docker create --name tmp-cover $(PLUGIN_NAME):rootfs-cover
	docker export tmp-cover | tar xC plugin-cover/rootfs
	docker rm -vf tmp-cover

plugin-cover: plugin-cover/rootfs config-cover.json
	cp config-cover.json $@/config.json

create-cover: plugin-cover
	@command -v jq >/dev/null || { echo "create-cover needs jq"; exit 1; }
	@jq -r '.mounts[]? | select(.type=="bind") | .source | select(startswith("/var/lib/"))' \
	    config-cover.json | xargs -r mkdir -p
	docker plugin rm -f $(PLUGIN_NAME):$(PLUGIN_COVER_TAG) || true
	docker plugin create $(PLUGIN_NAME):$(PLUGIN_COVER_TAG) $<
	docker plugin set $(PLUGIN_NAME):$(PLUGIN_COVER_TAG) LOG_LEVEL=trace \
	    OUTAGE_TICK=$(TEST_OUTAGE_TICK) OUTAGE_GRACE=$(TEST_OUTAGE_GRACE)

enable-cover:
	docker plugin enable $(PLUGIN_NAME):$(PLUGIN_COVER_TAG)
disable-cover:
	docker plugin disable $(PLUGIN_NAME):$(PLUGIN_COVER_TAG)

# There is deliberately NO multiarch/manifest-list target. `docker
# plugin install` cannot resolve a manifest list at all — measured on
# #507 — so the only shipping shape is one single-arch build per
# architecture, tagged with the arch in the tag name (vX.Y.Z-arm64).
# release.yml runs this same `push` target once per architecture on a
# native host of that architecture; the build follows the host.

clean:
	-rm -rf plugin/
	-rm -rf plugin-cover/
	-rm bin/*

# Both suite targets tee their output to a timestamped file under
# $(ITEST_LOG_DIR) (#378). A local run is ~20 minutes and its output
# previously existed only in terminal scrollback, so the evidence for
# a run was gone the moment the window scrolled or the session ended —
# which is exactly when you want it. CI tees separately in
# integration.yml; this makes the local path recoverable without
# depending on the workflow.
#
# ITEST_STAMP is expanded once at parse time (`:=`, not `=`), so both
# suites in one `make integration-test integration-test-failure`
# invocation land under the same stamp and are trivially correlated.
ITEST_LOG_DIR ?= logs
ITEST_STAMP := $(shell date +%Y%m%d-%H%M%S)
ITEST_MAIN_LOG = $(ITEST_LOG_DIR)/integration-main-$(ITEST_STAMP).log
ITEST_FAILURE_LOG = $(ITEST_LOG_DIR)/integration-failure-$(ITEST_STAMP).log

# The `bash -o pipefail` wrapper below is LOAD-BEARING, for exactly the
# reason `shell: bash` is load-bearing on integration.yml's tee step.
# make runs recipes under /bin/sh — dash on Debian, which has no
# pipefail — so a bare `go test ... | tee` reports *tee's* exit 0 and
# turns a failing suite green. That is the precise mechanism that hid
# a broken feature behind a green gate for a month (#297). The paths
# are echoed BEFORE the run, not after, because make stops at the
# failing recipe line and a trailing echo would never print on the one
# outcome where you need the log. scripts/test-makefile-tee.sh pins
# both properties.

# The local entry point: rebuild, reinstall, then run both suites.
#
# `integration-test` and `integration-test-failure` deliberately do NOT
# depend on a rebuild. CI calls them in sequence between its own build
# and teardown steps, so a rebuild dependency there would reinstall the
# plugin BETWEEN the two suites — recycling the plugin mid-run and
# resetting the health floor's observation window with it.
#
# Locally there is no such build step, so `make integration-test` alone
# tests whatever plugin happens to be installed. That is not a
# hypothetical: validating #374, a stale installed build made two tests
# fail for reasons unrelated to the branch AND made the health floor
# report `clean` for counters that build could not publish at all —
# wrong in both directions from one cause. Rebuilding reproduced CI
# exactly.
#
# The fast CI lane, locally (#636). No privileges, no host mutation, no
# network — the counterpart to integration-local, which covers the
# privileged lane.
#
# It is a thin call on purpose. The lane's contents live in
# scripts/local-lane.sh so that scripts/check-local-lane.sh can reconcile
# them against test.yaml; listing the gates here instead would rebuild the
# #542 hole, where a gate added to CI silently never runs locally and the
# target still exits 0.
#
# A step whose tool is missing is skipped LOUDLY and named in the summary.
# STRICT=1 makes a skip a failure — use it anywhere a green exit is read
# as coverage rather than by a human who can see the summary.
check:
	@bash scripts/local-lane.sh

# Orphan cleanup runs FIRST, mirroring the CI job's own first step.
# Without it a single container left behind by an earlier aborted run
# fails the next local run with a name conflict, days later, in a test
# that has nothing to do with whatever is being changed — and it looks
# exactly like a regression. That cost a diagnosis on #449: a container
# created the previous afternoon, never started, failed
# TestLifecycleMacvlan_GoldenPath on a branch that does not touch it.
#
# CI never sees this because its runners are ephemeral and it cleans
# anyway; local runs are the only place the state accumulates, which is
# precisely why the local target is the one that needs the step.
#
# Use this target locally; use the two suite targets directly only when
# you have just built and installed the plugin yourself.
integration-local: integration-cleanup create enable integration-test integration-test-failure

# Live integration tests. Need privileges (CAP_NET_ADMIN, mount/netns
# ops, bind UDP/67) and the plugin already enabled at PLUGIN_NAME:golang.
# Locally: `sudo make integration-test`. CI: runner is root, target
# detects and skips the sudo wrapper.
integration-test:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "integration-test must run as root. Re-run with sudo."; \
		exit 1; \
	fi
	@mkdir -p $(ITEST_LOG_DIR)
	@echo "==> test output: $(ITEST_MAIN_LOG)"
	@id=$$(docker plugin inspect $(PLUGIN_NAME):$(PLUGIN_TAG) --format '{{.Id}}' 2>/dev/null); \
	 if [ -n "$$id" ]; then \
		echo "==> plugin log:  /var/lib/docker/plugins/$$id/rootfs/var/log/net-dhcp.log"; \
	 fi
	@bash -o pipefail -c 'go test -v -tags integration -count=1 -timeout 20m -skip "TestFailure_" ./test/integration/... 2>&1 | tee $(ITEST_MAIN_LOG)'
# (20m, not #146's 15m: the suite measured 558s on runner-class
# hardware before the v1.0.0 additions; 20m keeps the same headroom
# ratio with them.)

# Failure-injection suite (#128): crosses real DHCP timing boundaries
# (lease expiry, NAK at T1) against per-test ephemeral DHCP servers —
# ~9 serial minutes of mostly deliberate waiting, so it runs as its
# own step instead of inflating the main suite's feedback loop.
#
# 20m, not 15m (#278): each test waits for the persistent client's own
# bind BEFORE killing the server, so the outage it injects has to be
# detected the slow way — a bound 2m lease lapsing, plus the watchdog
# grace and up to one tick. TEST_OUTAGE_* above trims the plugin-side
# part of that; the 2m lease floor is dnsmasq's and stays (#356). The
# ceiling is kept sized for the shipped 30s/25s cadence so the suite
# still passes against a default-configured plugin.
# One shard of the main suite (#381). SHARD is 1-based, OF is the total.
#
# The partition comes from scripts/integration-shard.sh, which balances
# by measured duration and — the property that actually matters —
# guarantees every main-suite test lands in exactly one shard.
# scripts/test-integration-shard.sh asserts that, because a test in no
# shard is silently never run and the gate goes green having tested
# less.
integration-test-shard:
	@if [ -z "$(SHARD)" ] || [ -z "$(OF)" ]; then \
		echo "usage: make integration-test-shard SHARD=<1-based> OF=<total>"; \
		exit 2; \
	fi
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "integration-test-shard must run as root. Re-run with sudo."; \
		exit 1; \
	fi
	@mkdir -p $(ITEST_LOG_DIR)
	@sel=$$(bash scripts/integration-shard.sh $(SHARD) $(OF)) || exit 1; \
	 echo "==> shard $(SHARD)/$(OF): $$(echo "$$sel" | tr '|' '\n' | wc -l) test(s)"; \
	 echo "==> test output: $(ITEST_LOG_DIR)/main-shard$(SHARD).log"; \
	 bash -o pipefail -c "go test -v -tags integration -count=1 -timeout 20m \
	     -run '$$sel' ./test/integration/ 2>&1 | tee $(ITEST_LOG_DIR)/main-shard$(SHARD).log"
	# The harness package, unfiltered, in EVERY shard.
	#
	# Three of its test files carry the integration build tag, and today
	# they run only because the unsharded main target globs
	# ./test/integration/... with a -skip. A -run regex naming suite
	# tests matches none of them, so sharding without this line would
	# drop an entire package — including the guards that stop a
	# hand-rolled counter read (#405) and a bare HostConfig literal
	# (#367) creeping back. Silently, with the gate still green.
	#
	# Run in every shard rather than one: it needs no fixture and takes
	# milliseconds, and "which shard owns it" is one more thing to get
	# wrong later.
	@go test -tags integration -count=1 ./test/integration/harness/

integration-test-failure:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "integration-test-failure must run as root. Re-run with sudo."; \
		exit 1; \
	fi
	@mkdir -p $(ITEST_LOG_DIR)
	@echo "==> test output: $(ITEST_FAILURE_LOG)"
	@id=$$(docker plugin inspect $(PLUGIN_NAME):$(PLUGIN_TAG) --format '{{.Id}}' 2>/dev/null); \
	 if [ -n "$$id" ]; then \
		echo "==> plugin log:  /var/lib/docker/plugins/$$id/rootfs/var/log/net-dhcp.log"; \
	 fi
	@bash -o pipefail -c 'go test -v -tags integration -count=1 -timeout 20m -run "TestFailure_" ./test/integration/... 2>&1 | tee $(ITEST_FAILURE_LOG)'

# Manual orphan cleanup for when an integration test panics mid-setup
# and leaves dh-itest-* interfaces / containers / networks behind.
integration-cleanup:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "integration-cleanup must run as root. Re-run with sudo."; \
		exit 1; \
	fi
	bash test/integration/cleanup-orphans.sh

# Regenerate the captured libnetwork request fixtures (#644).
#
# WHY THIS TARGET EXISTS AT ALL
#
# pkg/plugin/testdata/requests holds the raw request bodies the daemon
# actually sends. The unit tests replay them instead of hand-building
# request structs, which is the difference between asserting against
# what libnetwork sends and asserting against our model of it. #298 is
# the worked example of that gap: stable_lease was designed against an
# assumed CreateEndpoint payload and was reverted from v1.3.0 once the
# endpoint identity turned out to be unresolvable in the real flows.
#
# A fixture nobody can regenerate decays into an assumption that agrees
# with itself forever, so regeneration is one command and the manifest
# it writes records which engine produced the capture.
#
# HOW IT WORKS
#
# The capture knob lives in config-cover.json only (alongside
# GOCOVERDIR), so this drives the :golang-cover plugin and points the
# harness at it via INTEGRATION_PLUGIN_REF. The shipped manifest, and
# therefore every production install, is untouched.
#
# One flow at a time, each into a cleared directory: the capture is a
# flat sequence of files, so two flows in one run would interleave into
# a transcript that never happened.
CAPTURE_HOST_DIR ?= /var/lib/dh-capture
FIXTURE_DIR      ?= pkg/plugin/testdata/requests
COVER_PLUGIN_REF  = $(PLUGIN_NAME):$(PLUGIN_COVER_TAG)

# Overridable because the recipe runs as root against a checkout the
# invoking user owns: git refuses that as dubious ownership and would
# leave `commit` empty, producing a fixture nobody can attribute.
# Pass it from the unprivileged shell instead:
#   sudo make capture-fixtures CAPTURE_COMMIT=$(git rev-parse --short HEAD)
CAPTURE_COMMIT ?= $(shell git rev-parse --short HEAD)

# One flow per line, three parallel variables rather than a single
# colon-packed list. The list form was word-split by the shell: the
# descriptions contain spaces, so `for spec in $(CAPTURE_FLOWS)` turned
# every word of a description into its own bogus flow. Expanding at make
# time with $(foreach)/$(call) keeps the quoting intact.
CAPTURE_FLOWS ?= macvlan-run bridge-run macvlan-restart

CAPTURE_TEST_macvlan-run     = TestLifecycleMacvlan_GoldenPath
CAPTURE_DESC_macvlan-run     = docker run on a macvlan DHCP network, to exit
CAPTURE_TEST_bridge-run      = TestLifecycleBridge_GoldenPath
CAPTURE_DESC_bridge-run      = docker run on a bridge DHCP network, to exit
CAPTURE_TEST_macvlan-restart = TestTombstoneRestart_PreservesMACAndIP
CAPTURE_DESC_macvlan-restart = docker restart on a macvlan DHCP network

# $(1) is the flow name. Runs inside the recipe's single shell, so `rc`
# accumulates across flows and one failure does not abandon the rest.
define capture_one_flow
	echo "==> capturing flow '$(1)' from $(CAPTURE_TEST_$(1))"; \
	rm -rf $(CAPTURE_HOST_DIR)/*; \
	if ! INTEGRATION_PLUGIN_REF=$(COVER_PLUGIN_REF) \
	     go test -tags integration -count=1 -timeout 20m \
	     -run '^$(CAPTURE_TEST_$(1))$$' ./test/integration/; then \
		echo "!!  $(CAPTURE_TEST_$(1)) failed; flow '$(1)' NOT refreshed"; rc=1; \
	else \
		n=$$(find $(CAPTURE_HOST_DIR) -maxdepth 1 -name '*.json' | wc -l); \
		if [ "$$n" -eq 0 ]; then \
			echo "!!  $(CAPTURE_TEST_$(1)) passed but captured NOTHING — the plugin is probably"; \
			echo "!!  not the cover build, or REQUEST_CAPTURE_DIR is unset on it."; \
			echo "!!  Flow '$(1)' NOT refreshed."; \
			rc=1; \
		else \
			rm -rf $(FIXTURE_DIR)/$(1); mkdir -p $(FIXTURE_DIR)/$(1); \
			cp $(CAPTURE_HOST_DIR)/*.json $(FIXTURE_DIR)/$(1)/; \
			jq -n --arg e "$$engine" --arg c "$$stamp" --arg g "$$commit" \
			      --arg f '$(CAPTURE_DESC_$(1))' \
			   '{engine:$$e, captured:$$c, commit:$$g, flow:$$f}' \
			   > $(FIXTURE_DIR)/$(1)/manifest.json; \
			echo "==> $(1): $$n call(s)"; \
		fi; \
	fi;
endef

# The mkdir comes BEFORE create/enable, not after: config-cover.json
# bind-mounts CAPTURE_HOST_DIR into the plugin, and a bind source that
# does not yet exist fails `docker plugin enable` outright — the same
# lazily-created-bind-source trap as #588. create-cover mkdirs the state
# dir for exactly this reason; this is its capture twin.
# The fixtures are written by root and land in the working tree, so
# without the chown below every regeneration leaves a checkout its own
# owner cannot rebase, `git clean`, or check out over — git refuses to
# replace a file it has no permission to unlink. SUDO_UID/SUDO_GID name
# the invoking user; when the target is run as real root they are unset
# and root-owned is the correct answer.
# The teardown calls `docker plugin disable` directly rather than
# $(MAKE) disable-cover. GNU Make executes any recipe line containing the
# recursion marker even under -n; the standalone $(MAKE) lines above are
# fine, because -n propagates through MAKEFLAGS and the sub-make only
# prints. But the whole flow loop is ONE recipe line, so a marker inside
# it made `make -n capture-fixtures` run every integration test for real.
capture-fixtures:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "capture-fixtures must run as root (it drives the integration suite). Re-run with sudo."; \
		exit 1; \
	fi
	@command -v jq >/dev/null || { echo "capture-fixtures needs jq"; exit 1; }
	@mkdir -p $(CAPTURE_HOST_DIR) $(FIXTURE_DIR)
	$(MAKE) create-cover
	docker plugin set $(COVER_PLUGIN_REF) REQUEST_CAPTURE_DIR=/capture
	$(MAKE) enable-cover
	@engine=$$(docker version --format '{{.Server.Version}}'); \
	 commit='$(CAPTURE_COMMIT)'; \
	 if [ -z "$$commit" ]; then \
		echo "CAPTURE_COMMIT is empty (git could not read this checkout as root)."; \
		echo "Re-run as: sudo make capture-fixtures CAPTURE_COMMIT=\$$(git rev-parse --short HEAD)"; \
		exit 1; \
	 fi; \
	 stamp=$$(date -u +%Y-%m-%d); \
	 rc=0; \
	 $(foreach f,$(CAPTURE_FLOWS),$(call capture_one_flow,$(f))) \
	 if [ -n "$${SUDO_UID:-}" ]; then \
		chown -R "$$SUDO_UID:$${SUDO_GID:-$$SUDO_UID}" $(FIXTURE_DIR); \
	 fi; \
	 docker plugin disable $(COVER_PLUGIN_REF) || true; \
	 exit $$rc
