# Code Review Report — docker-net-dhcp (full pass)

**Project**: docker-net-dhcp (claymore666 fork at v0.5.0, commit `68e8759`)
**Language**: Go 1.25 (+ Python 3 build scripts)
**Date**: 2026-05-03
**Scope**: Entire codebase. Previous review (2026-05-02) was scoped to the v0.5.0 changes; this pass covers everything: entry points, util package, udhcpc client/handler, libnetwork API surface, build & CI, scripts, Dockerfile, config.json.
**Files reviewed**: 23 Go files + 3 Python scripts + Dockerfile + Makefile + CI workflow + plugin manifest
**Lines of code**: 4,484 Go (incl. tests) + 244 Python

## Executive Summary

This is the second pass; the first one (2026-05-02) generated 27 GitHub issues (#5–#31) covering the v0.5.0 deltas. **Those findings are not duplicated here.** This pass found **5 new critical-or-warning findings** in the parts the first pass skipped — most importantly a nil-pointer panic in the udhcpc handler that the first review missed because it was scoped to v0.5.0 work.

**Build & static analysis**: clean. `go build`, `go vet`, `staticcheck`, `gofmt`, and the existing `-race` test suite all pass; CI runs the same set on every push to `main` and `dev`.

**Biggest new finding (N-1)**: `cmd/udhcpc-handler/main.go:27-32` — when `net.ParseCIDR` fails on a malformed `ipv6` env var, the code logs the error and **continues**, then dereferences a nil `netV6` and panics. A handler panic means the corresponding DHCP event never reaches the persistent client; the lease silently stops getting renewed. This is exactly the kind of bug that hides in always-on infrastructure for months until the one weird LAN packet that triggers it.

**Pattern bug (N-2)**: All four `Await*` helpers in `pkg/util/` (`AwaitContainerInspect`, `AwaitNetNS`, `AwaitLinkByIndex`, `AwaitCondition`) use the same goroutine-leak-on-cancel pattern. On context timeout, the inner goroutine continues polling and eventually blocks forever trying to send on an unbuffered channel that no one will read. Not the largest leak class in absolute terms (these helpers run only at endpoint setup), but it's a four-times-repeated bug that is fixed once.

**Verdict**: still production-ready (it's running on docker-ai right now and behaving). One-day cleanup pass on the new findings + the v0.5.0 issues already filed and you have a v0.5.1 worth tagging.

## Tooling Results

| Tool | Version | Findings | Notes |
|------|---------|----------|-------|
| `go build ./...` | go 1.25 | 0 | Clean |
| `go vet ./...` | go 1.25 | 0 | Clean |
| `staticcheck ./...` | latest (honnef.co/go/tools) | 0 | Clean. Already in CI. |
| `gofmt -l .` | go 1.25 | 0 unformatted | Clean. Already in CI. |
| `go test ./... -race` | go 1.25 (CGO) | 0 fail | Coverage 18.2% — see I-5 from prior review (#26). |
| `govulncheck ./...` | 1.3.0 | 2 informational | Both daemon-side moby vulns; not reachable from our client-only usage. Filed as #31. |

**Build**: pass. **Tests**: all pass under `-race`.

## Findings

> Findings here are **new** to this pass. The 27 issues filed yesterday (#5–#31) are still open; the metrics table at the end consolidates both passes.

### 🔴 Critical

#### N-1: `udhcpc-handler` nil-pointer panic on malformed `ipv6` env var
**File**: `cmd/udhcpc-handler/main.go:27-32`

```go
_, netV6, err := net.ParseCIDR(v6 + "/128")
if err != nil {
    log.WithError(err).Warn("Failed to parse IPv6 address")
}

event.Data.IP = netV6.String()  // netV6 is nil if err != nil → panic
```

When `net.ParseCIDR` fails — e.g., udhcpc6 emits `ipv6=fe80::1/64` (already CIDR-form, then we append `/128`), or emits a corrupt value, or env is empty after a glibc/musl quirk — the error is logged but execution proceeds and `netV6.String()` dereferences a nil pointer. The handler crashes with `runtime error: invalid memory address`, the udhcpc child's script invocation exits non-zero, and the corresponding `bound`/`renew` event is never delivered to the persistent client. The lease silently ages out; the container loses its IP at lease-expiry boundary with no log line tying it back to the cause.

Compounding factor: this is one of the few pieces of code that runs **outside** the main plugin process (it's invoked by udhcpc as `-s /usr/lib/net-dhcp/udhcpc-handler`), so even structured logging/metrics from the plugin won't show the failure.

**Fix**:
```go
case "bound", "renew":
    if v6, ok := os.LookupEnv("ipv6"); ok {
        _, netV6, err := net.ParseCIDR(v6 + "/128")
        if err != nil {
            log.WithError(err).WithField("ipv6", v6).Error("Failed to parse IPv6 address; skipping event")
            return  // exit non-zero so udhcpc logs it; do not emit garbage event
        }
        event.Data.IP = netV6.String()
    } else {
        // ...
    }
```
And consider a defensive `if v6 == ""` short-circuit before the ParseCIDR.

### 🟡 Warning

#### N-2: `Await*` helpers in `pkg/util/` all leak goroutines on context cancellation
**Files**: `pkg/util/general.go:8-33` (`AwaitCondition`), `pkg/util/docker.go:16-42` (`AwaitContainerInspect`), `pkg/util/netlink.go:12-38` (`AwaitNetNS`), `pkg/util/netlink.go:40-66` (`AwaitLinkByIndex`)

All four helpers spawn an inner goroutine that polls until success, then writes to an **unbuffered** channel. The outer `select` returns on `ctx.Done()` and the function exits — but the inner goroutine is still polling. When the next poll succeeds, it tries `chan <- value` on a channel no one is reading; it blocks forever. The polling continues consuming syscalls (`netns.GetFromPath`, `LinkByIndex`, `ContainerInspect`) every `interval` until the process dies.

Real-world consequence: each context-cancelled `CreateEndpoint` (slow Docker, slow netns sync, container-startup race) leaks one goroutine + one fd-allocating syscall loop. Bounded by `ulimit -n` and `GOMAXPROCS`; on a host with many container churns + transient docker-daemon stalls, it's a slow drift toward fd exhaustion.

**Fix** (applied to `AwaitCondition`; same pattern for the others):
```go
func AwaitCondition(ctx context.Context, cond func() (bool, error), interval time.Duration) error {
    errChan := make(chan error, 1)  // buffered
    go func() {
        for {
            select {
            case <-ctx.Done():
                return  // stop polling when caller gives up
            default:
            }
            ok, err := cond()
            if err != nil {
                select {
                case errChan <- err:
                default:
                }
                return
            }
            if ok {
                select {
                case errChan <- nil:
                default:
                }
                return
            }
            select {
            case <-ctx.Done():
                return
            case <-time.After(interval):
            }
        }
    }()
    select {
    case err := <-errChan:
        return err
    case <-ctx.Done():
        return ctx.Err()
    }
}
```
Apply the same pattern (buffered chan + ctx-aware sleep + ctx-aware send) to all four. Worth extracting into a single `Poll(ctx, fn, interval)` and replacing the three near-duplicates.

#### N-3: Log file is opened once and never reopened on logrotate
**File**: `cmd/net-dhcp/main.go:36-44`

```go
if *logFile != "" {
    f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
    // ...
    log.StandardLogger().Out = f
}
```

Plugin runs forever. If logrotate moves `/var/log/net-dhcp.log` aside (or uses `copytruncate`), our fd still points at the moved/truncated file. Logs vanish after rotation.

**Fix**: handle SIGHUP to reopen, or use `lumberjack.Logger` (or equivalent) to rotate internally:
```go
sigs := make(chan os.Signal, 2)
signal.Notify(sigs, unix.SIGINT, unix.SIGTERM, unix.SIGHUP)
// ...
go func() {
    for sig := range sigs {
        if sig == unix.SIGHUP {
            // reopen logFile
        } else {
            // shutdown path
        }
    }
}()
```

#### N-4: `AWAIT_TIMEOUT` default differs between binary and plugin manifest
**File**: `cmd/net-dhcp/main.go:46` (defaults to `5*time.Second`) vs `config.json:21-25` (defaults to `"10s"`)

Running the binary directly (e.g. `make debug`) silently uses 5s; the plugin runtime injects 10s. Functional difference is small but the divergence is invisible to anyone debugging the binary versus the plugin. One source of truth is the plugin manifest; the Go default should match.

**Fix**:
```go
awaitTimeout := 10 * time.Second  // match config.json default
```

#### N-5: `.dockerignore` is missing `.git/` and `.github/` — sends 8MB+ of git history to the daemon on every build
**File**: `.dockerignore` (only excludes `bin/`, `plugin/`, `multiarch/`)

```bash
$ du -sh .git/
8.1M    .git/
```

Every `make build` / `docker build .` ships the entire `.git/` directory to the daemon as build context. Slow on cold cache, and it bypasses the explicit `COPY` allowlist principle (since the COPY only copies what it asks for, this is "merely" wasted bandwidth — but on a CI runner with rate-limited registries, it adds up).

**Fix**:
```
.git/
.github/
*.md
LICENSE.md
docs/
scripts/
test_env.sh
```
(Keep README.md only if you `COPY` it in the runtime stage; otherwise exclude it too.)

### 🔵 Info

#### N-6: HTTP handler boilerplate repeated 6 times
**File**: `pkg/plugin/endpoints.go` — `apiCreateNetwork`, `apiDeleteNetwork`, `apiCreateEndpoint`, `apiEndpointOperInfo`, `apiDeleteEndpoint`, `apiJoin`, `apiLeave` all follow `parse → call → respond/err`. Pure boilerplate. Could be a generic helper:
```go
func handle[Req, Res any](w http.ResponseWriter, r *http.Request, impl func(context.Context, Req) (Res, error)) {
    var req Req
    if err := util.ParseJSONBody(&req, w, r); err != nil { return }
    res, err := impl(r.Context(), req)
    if err != nil { util.JSONErrResponse(w, err, 0); return }
    util.JSONResponse(w, res, http.StatusOK)
}
```
But — this churns the diff for cosmetic gain only. Worth doing only if these handlers grow tracing/metrics/auth hooks later. Otherwise leave it.

#### N-7: `log.Fatal` skips deferred log-file close
**File**: `cmd/net-dhcp/main.go:32, 39, 50, 56, 65, 72`. Every `log.Fatal` calls `os.Exit(1)`, which skips the deferred `f.Close()` on the log file at line 41. Some final lines may be lost in the buffered logger. Minor; OS reclaims the fd. Worth replacing with a cleanup helper:
```go
fatalCleanup := func(format string, args ...any) {
    if f != nil { _ = f.Close() }
    log.Fatalf(format, args...)
}
```

#### N-8: `ParseJSONBody` is a footgun: it writes HTTP responses *and* returns errors
**File**: `pkg/util/json.go:50-58`. The current callers (`endpoints.go`) check the error and return without writing again, but the API name reads as "parse" while the function also takes over response writing. A future caller writing the obvious-looking `if err := ParseJSONBody(...); err != nil { JSONErrResponse(w, err, ...); return }` would double-write headers. Worth either renaming to `ParseJSONOrErrorResponse` or splitting into a pure parse + a handler helper.

#### N-9: `ErrToStatus` lumps non-validation errors into 500
**File**: `pkg/util/errors.go:41-52`. Errors like `ErrNoLease` (DHCP server didn't reply — upstream issue), `ErrNoContainer` (Docker state inconsistency — service unavailable), and `ErrNoSandbox` (Docker race window — retryable) all map to 500. For the libnetwork integration this doesn't matter (dockerd treats all 5xx the same), but a cleaner mapping (502 / 503 / 409) would help if the plugin ever exposes its API to other consumers.

#### N-10: `udhcpc6` env-var format is assumed without version pinning
**File**: `cmd/udhcpc-handler/main.go:27`. We assume `os.Getenv("ipv6")` is a bare address (no mask). The fix `+ "/128"` works for that. busybox `udhcpc6` does emit it that way as of current versions, but a future busybox change to emit CIDR form would silently produce parse errors → falls into N-1's nil-deref. Worth a comment pinning the assumption to the busybox version, plus a `strings.SplitN(v6, "/", 2)` defensive split.

#### N-11: Alpine package versions in Dockerfile are not pinned
**File**: `Dockerfile:14`. `apk add --no-cache busybox-extras iproute2` installs whatever versions the Alpine repos serve at build time. A regression in busybox's udhcpc/udhcpc6 (where the entire DHCP exchange happens) would silently land on the next build. For a plugin whose correctness depends on busybox behavior, version-pin or pin the Alpine minor version (already partially flagged as I-1 in #22 — this is the package-level extension).

**Fix**: pin Alpine minor + record installed versions:
```dockerfile
FROM alpine:3.20.3
RUN apk add --no-cache busybox-extras=1.36.1-r29 iproute2=6.9.0-r0
```
(Adjust to whatever current versions are; verify on each release.)

#### N-12: Two stale references in upstream-inherited Makefile
**File**: `Makefile:1`. `PLUGIN_NAME = ghcr.io/devplayer0/...` is the upstream registry; everything in this fork is published to `ghcr.io/claymore666/...` (overridden via env every time). The default points at a registry the fork can't push to. Worth flipping the default:
```make
PLUGIN_NAME ?= ghcr.io/claymore666/docker-net-dhcp
```
(Use `?=` so an env override still wins.)

## Metrics Summary

This consolidates **both** review passes (yesterday + today). Yesterday's 27 issues are filed (#5–#31); today adds 12 more.

| Category | Issues | Critical | Warning | Info |
|----------|--------|----------|---------|------|
| Error Handling & Robustness | 6 | 4 (C-1, C-2, C-3, **N-1**) | 2 (W-2, W-9) | 0 |
| Performance & Efficiency | 1 | 0 | 1 (W-12) | 0 |
| Dead Code & Unused | 0 | 0 | 0 | 0 |
| Code Style & Idioms | 6 | 0 | 0 | 6 (I-4, I-6, I-8, **N-6, N-8, N-12**) |
| Concurrency | 6 | 1 (C-2) | 5 (W-1, W-5, W-6, W-10, **N-2**) | 0 |
| Testing | 2 | 0 | 1 (W-11) | 1 (I-5) |
| Dependencies | 1 | 0 | 0 | 1 (I-10) |
| Configuration & Environment | 2 | 0 | 1 (**N-4**) | 1 (**N-9**) |
| Logging & Observability | 4 | 1 (C-4) | 1 (**N-3**) | 2 (I-3, **N-7**) |
| Security | 2 | 0 | 0 | 2 (I-1, I-2) |
| Architecture | 1 | 1 (C-5) | 0 | 0 |
| Project Hygiene | 4 | 0 | 2 (**N-5, N-11**) | 2 (I-7, I-9, **N-10**) |
| **Total** | **39** | **7** | **13** | **15** |

**Of which new this pass**: 1 critical + 4 warning + 7 info = 12.

## What's Done Well

1. **CI is right-sized.** `go build`, `go vet`, `gofmt -l`, `staticcheck`, and `go test -race` on every push, with caching. No flaky-flag, no skip lists, no theatre. The same set runs locally with one command. This is the "make it once, make it right" tone the project asks for.
2. **Error sentinels are well-organized.** `pkg/util/errors.go` declares a clean set of typed sentinel errors with `errors.Is`-friendly mapping to HTTP status. Easy to extend, easy to test, easy to grep for. The mapping in `ErrToStatus` is centralized rather than scattered.
3. **The udhcpc client wrapper is small and focused.** `pkg/udhcpc/client.go` does one thing: spawn busybox udhcpc with the right flags and read events. The DHCP-option crafting (option 12 hostname, option 50 requested-IP, option 61 client-id) is well-commented and references RFCs. It's the kind of code that's hard to write correctly and easy to read once written.
4. **`/Plugin.Health` exists and is at the right scope.** Most plugins ship without any liveness/state probe. Adding one at the same socket as the libnetwork RPC is the lowest-friction way to make the plugin operable. The four counters (uptime, active endpoints, pending hints) are exactly the information a sysadmin needs.

## Top Recommendations

Ordered by value-to-effort, **across both review passes**:

1. **Fix N-1 (udhcpc-handler nil-deref).** Lone-line bug with crash potential. ~5-line fix. Highest priority.
2. **Fix W-10 (`Plugin.Close` doesn't stop persistent managers).** ~15 lines. Restores the lease-release-on-shutdown invariant. Highest impact.
3. **Fix N-2 (extract a single `Poll` helper, replace 4 copies).** Eliminates four goroutine-leak sites at once. ~40 lines net (more removed than added).
4. **Fix C-5 (add hostname to tombstone match key).** ~30 lines incl. a test. Closes the cross-container identity-swap during sequential `compose restart`.
5. **Fix C-1/C-2/W-9 (buffer the udhcpc channels).** Three one-line edits (`make(chan ..., N)`) that eliminate three goroutine/process leak classes. Trivially safe.
6. **Fix C-3 (`shortID` helper, ~7 sites).** Mechanical, removes a panic class for a degraded-Docker-response edge case.

Estimated total for all six: ~half a day. v0.5.1 worth tagging after.
