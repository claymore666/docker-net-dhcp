# Ephemeral CI runner image

Self-contained GitHub Actions runner for this repository's
privileged integration workloads: one container = one job, each with
its own nested Docker daemon. Design record: issue #149; the
nested-daemon approach was validated end-to-end before this image
existed (full integration suite green inside DinD — #145 carries the
one harness fix that validation surfaced).

Published as `ghcr.io/claymore666/dhcp-ci-runner` by the
`runner-image` workflow on changes under `ci/runner-image/`.

## Orchestrator contract

```
docker run --rm --privileged \
  -e RUNNER_JIT_CONFIG=<encoded_jit_config> \
  ghcr.io/claymore666/dhcp-ci-runner:latest
```

- **`--privileged` is required**: the nested daemon plus the
  integration suite's netns/mount operations, CAP_NET_ADMIN, and
  UDP/67 binding. The trust boundary is the host this runs on, not
  the container — run it on an isolated machine.
- **`--rm` is required**: `/var/lib/docker` is an anonymous volume
  (the nested daemon needs a real filesystem to get overlay2 instead
  of vfs); `--rm` reaps it. Nothing else should be mounted in.
- **One container, one job.** JIT configs are single-use and the
  runner exits after its job; the container's exit code is the
  runner's. Relaunch with a fresh JIT config for the next job.
- JIT configs come from
  `POST /repos/<owner>/<repo>/actions/runners/generate-jitconfig`
  (GitHub App credential with repo-scoped **Administration: write**).
  Suggested fields: `name` unique per instance, `labels` matching the
  workflows' `runs-on`, `runner_group_id: 1`.
- **No inbound network, no LAN dependencies.** The runner long-polls
  GitHub over outbound 443; the test DHCP traffic stays on virtual
  interfaces inside the container. Outbound allowlist: `github.com`,
  `api.github.com`, `*.actions.githubusercontent.com`,
  `objects.githubusercontent.com`, `ghcr.io`,
  `pkg-containers.githubusercontent.com`, `registry-1.docker.io`,
  `auth.docker.io`, `production.cloudflare.docker.com`,
  `proxy.golang.org`, `sum.golang.org`, `go.dev`, `dl.google.com`.

## Self-test (no GitHub contact)

```
docker run --rm --privileged ghcr.io/claymore666/dhcp-ci-runner:latest selftest
```

Verifies: nested daemon comes up with overlay2, seed images load, and
a SIGTERM'd dockerd is relaunched by the supervisor — the property the
daemon-restart integration test depends on (`harness.RestartDockerDaemon`,
#145). Run it after any change to this directory and on any new host.

## What's baked in, and why

| Piece | Why |
|---|---|
| Docker Engine ≥ 28 (docker-ce) | nested daemon runs the plugin under test; ≥ 28 unblocks engine-gated tests (#125) |
| supervised dockerd (relaunch loop under tini) | daemon-restart recovery test must be able to bounce the daemon without killing the environment (#145) |
| Go toolchain (go.mod's version) | test compilation on the runner, mirrors `install-go-runner.sh` |
| dnsmasq, iproute2, iptables | integration fixtures (test-spawned DHCP server on veth pairs) |
| Go module + compile caches | ephemeral containers start cold; baking turns minutes of per-job downloads/compiles into cache hits |
| seed image tars (golang builder, alpine test image) | `docker load` at start beats pulling ~250 MB per job. The plugin's digest-pinned base still pulls (3.5 MB — `docker load` can't satisfy digest references) |

## Known limits

- The plugin build's `go mod download` inside its builder stage still
  fetches modules from the network per job (the baked cache helps the
  runner-side test compile, not the docker-build stage). Acceptable at
  current module sizes; a host-side GOPROXY cache is the upgrade path
  if it ever isn't.
- Image rebuilds don't track `go.mod` bumps automatically — the
  workflow triggers on `ci/runner-image/**` changes and manual
  dispatch. A stale cache costs seconds, not correctness.
