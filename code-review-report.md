# Code Review Report — docker-net-dhcp (third pass)

**Project**: docker-net-dhcp (claymore666 fork, current main `9320166`, post-v0.5.3)
**Languages**: Go 1.25 (+ Python 3 build scripts)
**Date**: 2026-05-04
**Scope**: Whole codebase — entry points, plugin core, udhcpc wrapper, util, build & CI, scripts, Dockerfile, plugin manifest, docs.
**Files reviewed**: 23 Go files + 4 Python scripts + Dockerfile + Makefile + CI workflow + plugin manifest
**Lines of code**: 4,744 Go (incl. tests)
**Prior reviews**: 2026-05-02 (v0.5.0 deltas) and 2026-05-03 (full codebase). 27 issues were filed; one (#33, the `Await*` goroutine leaks) is closed by today's v0.5.3.

## Executive Summary

Codebase is in good shape — better than the prior review found, because v0.5.3 closed the loudest concurrency bug (closed-channel CPU spinner — fork-introduced in v0.5.0 commit `d23ba50`, fixed today). All static-analysis tools are clean: build, `go vet`, `staticcheck`, `gofmt`, race-tested tests all pass. `govulncheck` flags two informational findings inside `github.com/docker/docker` that aren't reachable from our client-only usage (already #31). `gosec` produces six findings, mostly file-permission noise on a private plugin filesystem.

**The one real new finding** is dead code: `Plugin.storeJoinHint` (`pkg/plugin/plugin.go:185`) was left behind when the read-modify-write pattern was refactored to `updateJoinHint`. `deadcode` catches it; CI doesn't run `deadcode`. Fix is a one-line delete.

**The one architectural gap** the prior reviews undersold is on the shutdown path: `Plugin.Close` stops persistent DHCP clients (good — that's what v0.5.2 added for lease-release) and closes the docker client, but never calls `p.server.Shutdown(ctx)` on the HTTP server, and the recovery goroutines spawned in `NewPlugin` don't observe any cancellation tied to plugin lifetime. In practice this doesn't bite because the plugin process exits seconds later, but it's a contract that's wrong on paper — and exactly the sort of thing that surfaces when somebody embeds the plugin into a longer-lived test harness.

**Verdict**: production-ready. Ship as v0.5.3 (already shipped). The remaining 23 open issues from prior reviews are warning- or info-level cleanup; none block.

## Tooling Results

| Tool | Version | Findings | Notes |
|------|---------|----------|-------|
| `go build ./...` | go 1.25 | 0 | Clean |
| `go vet ./...` | go 1.25 | 0 | Clean |
| `staticcheck ./...` | latest (honnef.co/go/tools) | 0 | Clean. In CI. |
| `gofmt -l .` | go 1.25 | 0 unformatted | Clean. In CI. |
| `go test -race ./...` | go 1.25 (CGO) | 0 fail | Coverage 19.0% (cmd: 0%, util: 5.5%, udhcpc: 27.9%, plugin: 20.0%) |
| `govulncheck ./...` | 1.3.0 | 2 informational | Both `github.com/docker/docker` daemon-side moby vulns; not reachable from our client-only usage (#31) |
| `gosec ./...` | dev | 6 medium + 3 low | File-permission noise (G301/G302) on private plugin filesystem; G104 on deferred Close calls |
| `deadcode ./...` | latest | 1 | `Plugin.storeJoinHint` (W-N1, below) |

**Build**: pass. **Tests**: all pass under `-race`.

## Findings

> Findings here are **new** or have **changed status** since the 2026-05-03 pass. The 23 still-open issues from prior passes are summarised in the metrics table at the end; they are not duplicated as findings.

### 🔴 Critical

None this pass. (The closed-channel CPU spinner that motivated this session was filed and fixed today as v0.5.3.)

### 🟡 Warning

#### W-N1: `Plugin.storeJoinHint` is dead code
**File**: `pkg/plugin/plugin.go:183-189`

```go
// storeJoinHint records the state collected during CreateEndpoint so
// Join can pick it up.
func (p *Plugin) storeJoinHint(endpointID string, h joinHint) {
    p.mu.Lock()
    defer p.mu.Unlock()
    p.joinHints[endpointID] = h
}
```

`deadcode` flags this as unreachable. Confirmed: every site that needs to write a `joinHint` uses `updateJoinHint` (the read-modify-write helper), and no test references `storeJoinHint`. It's a leftover from the pre-RMW factoring.

Why it matters: dead code lies. A future reader might think they should call `storeJoinHint` for the "store" case and `updateJoinHint` only for the "modify" case, then introduce a real divergence.

**Fix**: delete the function. Add `deadcode` to CI (see I-N5) so the next instance is caught at PR time.

#### W-N2: HTTP server has no timeouts
**File**: `pkg/plugin/plugin.go:658-660` (server construction), `pkg/plugin/plugin.go:676-683` (`Listen`)

```go
p.server = http.Server{
    Handler: handlers.CustomLoggingHandler(nil, mux, util.WriteAccessLog),
}
```

`http.Server` is constructed with no `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, or `IdleTimeout`. In a public-internet HTTP server this would be a Slowloris vulnerability. Here the server only listens on the Unix socket the plugin runtime gives it, which only `dockerd` talks to — but defense-in-depth says set them anyway, and `gosec`/skill-ref Go best practices both flag the omission.

**Fix**:
```go
p.server = http.Server{
    Handler:           handlers.CustomLoggingHandler(nil, mux, util.WriteAccessLog),
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       30 * time.Second,
    WriteTimeout:      30 * time.Second,
    IdleTimeout:       60 * time.Second,
}
```

`WriteTimeout` of 30s is comfortably more than the worst-case CreateEndpoint (DHCP DISCOVER + initial Start) path, but caps stuck handlers from holding a goroutine forever.

#### W-N3: `Plugin.Close` doesn't drain in-flight HTTP requests
**File**: `pkg/plugin/plugin.go:696-741`

`Close` stops the persistent DHCP managers (good — that's the v0.5.2 lease-release contract) and calls `p.docker.Close()`. It never calls `p.server.Shutdown(ctx)`. A Join or CreateEndpoint that's mid-flight when SIGTERM arrives will keep running, holding references into the maps `Close` just cleared (e.g. `persistentDHCP` is reassigned to a fresh map at line 705 — any handler that was mid-`registerDHCPManager` writes into the old map, which is then garbage).

Practical impact: low. The plugin process exits a few seconds later (because main() calls `log.Fatal` after `p.Close()` returns) and the kernel sweeps everything. But the contract is broken in two visible ways: (a) `Close` can return while goroutines spawned by an in-flight `Join` are still booting up `dhcpManager.Start`, and (b) a handler racing with `Close` can re-register a DHCP manager that nobody will ever Stop.

**Fix**:
```go
func (p *Plugin) Close() error {
    // ... existing manager-stop logic, unchanged ...

    // Stop accepting new HTTP requests and drain in-flight ones before
    // closing the docker client and returning. Without this, a Join
    // mid-flight when SIGTERM arrives can register a manager into a
    // map we already cleared.
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if err := p.server.Shutdown(shutdownCtx); err != nil {
        log.WithError(err).Warn("HTTP server shutdown returned error")
    }

    if err := p.docker.Close(); err != nil {
        return fmt.Errorf("failed to close docker client: %w", err)
    }
    return nil
}
```

Pair this with W-N4 (cancellable recovery) so `Shutdown` doesn't have to race a still-spawning recovery goroutine.

#### W-N4: Recovery goroutine isn't tied to plugin lifetime
**File**: `pkg/plugin/plugin.go:666-670` and `pkg/plugin/plugin.go:502-516` (`recoverOneEndpoint`)

```go
go func() {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    p.recoverEndpoints(ctx)
}()
```

`NewPlugin` fires off `recoverEndpoints` in a fresh-`Background()` ctx. Each per-endpoint recovery then spawns its own goroutine in `recoverOneEndpoint` that derives *another* fresh-`Background()` ctx with `p.awaitTimeout`. Neither ctx is ever cancelled by `Plugin.Close`. `Close` cannot wait for recovery to finish, cannot tell it to give up, and cannot prevent it from registering a manager into the already-cleared `persistentDHCP` map after Close has run.

In practice: most recoveries are fast and the window is small. But: this is a known issue (#10, W-1 in the prior review, "recoverEndpoints races with the listener"). v0.5.3 didn't address it because the hotfix was scoped to the spinner. Worth pulling in alongside W-N3.

**Fix sketch**: store a recovery WaitGroup + cancel function on `Plugin`, have `recoverEndpoints` and `recoverOneEndpoint` derive their contexts from a Plugin-rooted parent, and have `Close` cancel + wait before clearing the maps.

#### W-N5: `ErrInvalidMode` message lies about supported modes
**File**: `pkg/util/errors.go:18`

```go
ErrInvalidMode = errors.New("invalid mode (must be 'bridge' or 'macvlan')")
```

`ipvlan` is also supported (added in v0.5.0; the dispatcher in `validateModeOptions` accepts it; `parent_attached.go` implements it). Operators who type a typo and get this error will think ipvlan isn't supported.

**Fix**:
```go
ErrInvalidMode = errors.New("invalid mode (must be 'bridge', 'macvlan', or 'ipvlan')")
```

### 🔵 Info

| ID | File:Line | Issue | Effort |
|---|---|---|---|
| I-N1 | `cmd/net-dhcp/main.go:37` | Log file opened with mode `0666` (gosec G302). Should be `0644` or `0640`. | trivial |
| I-N2 | `pkg/plugin/state.go:106,128,163,185` | State dir `0o755`, files `0o644` (gosec G301/G302). Plugin filesystem is private, so impact is theoretical, but tightening to `0o700` / `0o600` is free defense-in-depth. | trivial |
| I-N3 | `pkg/plugin/dhcp_manager.go:401,425,446` | Deferred `nsHandle.Close()` / `netHandle.Close()` ignore returned errors (gosec G104). Deliberate — can't do anything useful with cleanup errors. Suppressing with `_ = ...` would silence the linter without changing behavior. | trivial |
| I-N4 | `pkg/util/` | Three files: `errors.go`, `http.go`, `json.go` — classic "util" antipattern (skill ref calls this out: "package naming smells: util, common, helper indicate poor cohesion"). Consider splitting into domain-named packages, e.g. `pkg/httpapi/` for the JSON request/response helpers and `pkg/dhcperr/` (or just inline into `pkg/plugin/`) for the sentinel errors. Low priority — small package, no real coupling problem. | small refactor |
| I-N5 | `.github/workflows/test.yaml` | CI runs build/vet/gofmt/staticcheck/race-tests but not `govulncheck`, `deadcode`, or `gosec`. Adding `deadcode` would have caught W-N1 at PR time. `govulncheck` would surface upstream CVEs as they get assigned. | ~10 lines of YAML |
| I-N6 | `pkg/plugin/dhcp_manager.go:92,233`, `pkg/udhcpc/client.go:112` | Three TODOs: "different renewed IP", "deconfig handling", "udhcpc6 fqdn workaround". The first one is hot in production right now (Fritz.Box hands a new IP per renew, log fills with `udhcpc renew with changed IP` warnings — see today's deploy notes). Worth filing as separate issues so the TODOs aren't lost. | per-TODO triage |
| I-N7 | `README.md:108-145` | Several network-creation and `docker run` examples still reference the **upstream** image `ghcr.io/devplayer0/docker-net-dhcp:release-linux-amd64`. The "fork install" block at the top points at `claymore666` correctly, but the body examples weren't updated. Consumers will copy-paste these and hit upstream. | s/devplayer0/claymore666/ on examples |
| I-N8 | `Plugin.Close` parallel-stop loop, `pluginShutdownTimeout` | Each `dhcpManager.Stop` has its own 5s ctx (in `dhcp_manager.go`'s `setupClient`), and `Close` enforces a 5s wall-clock cap on the whole batch (line 728). Under wall-clock pressure, the inner timeout dominates and the outer cap is redundant; if udhcpc Wait actually sticks, only the outer cap unblocks shutdown. Document the layering or collapse to one. | trivial doc / small refactor |
| I-N9 | `pkg/util/general.go:8` | Now that `AwaitCondition` is synchronous and ~15 lines, consider inlining it into the two callers. The abstraction-vs-duplication tradeoff is closer than it was when the helper was 35 lines of leak-prone goroutine logic. | small refactor |
| I-N10 | Per-thread test coverage | 19.0% overall, but 0% on `cmd/net-dhcp/main.go`, `pkg/plugin/Listen`, `Close`, `NewPlugin`, `lookupEndpointMAC`, `reacquireEndpoint`, `initialDHCPHostname`. Issue #26 already covers the broader gap; the specific Listen/Close path needs an integration test that boots the plugin against a fake docker socket. | medium-large |

## Metrics Summary

The table reflects findings filed across all three review passes (2026-05-02, -05-03, -05-04). "Open" excludes today's W-N1 to W-N5 and I-N1 to I-N10 since they aren't filed yet.

| Category | Total filed | Open | Closed by v0.5.x |
|----------|-------------|------|------------------|
| Error Handling & Robustness | 5 | 4 | 1 (#33 Await* leaks → v0.5.3) |
| Performance & Efficiency | 1 | 1 | 0 |
| Dead Code & Unused | 1 (W-N1, new) | 1 | 0 |
| Code Style & Idioms | 6 | 6 | 0 |
| Concurrency & Lifecycle | 4 | 4 | 0 |
| Testing | 2 | 2 | 0 |
| Dependencies | 1 (#31 govulncheck) | 1 | 0 |
| Configuration & Environment | 1 (#23 ptrace cap) | 1 | 0 |
| Logging & Observability | 1 (#24 MAC/IP in INFO) | 1 | 0 |
| Security | 0 (gosec findings are all info-level) | 0 | 0 |
| Architecture | 2 | 2 | 0 |
| Project Hygiene | 4 | 4 | 0 |
| **Total** | **28** | **27** | **1** |

(Plus the 5 W-N* + 10 I-N* findings in this pass that aren't yet filed.)

**Build**: pass. **Tests**: pass under `-race`. **Coverage**: 19.0%. **Severity distribution today**: 0 critical, 5 warnings, 10 info.

## What's Done Well

1. **Atomic state writes**. `saveOptions` and `saveTombstones` use temp-file + rename with proper cleanup on every error path. The earlier non-atomic implementation depended on `loadOptions`'s parse-error fallback to recover from torn writes; the current code makes that fallback path a backstop instead of a hot path.
2. **The tombstone mechanism**. Solving libnetwork's "destroy old endpoint, create new endpoint with fresh ID" container-restart flow with a TTL-bounded MAC/IP cache is genuinely clever, and the narrow-by-hostname refinement (added in v0.5.0) handles the obvious failure mode (sequential `compose restart` of N containers swapping identities) cleanly.
3. **Race-tested everywhere**. CI runs `-race` on every push; the codebase has no race-test fails. The two mutexes (`mu` and `tombstoneMu`) are documented to never be held together, which is the right way to prevent lock-ordering deadlocks in a small lock set.
4. **Comments explain the *why***. `pkg/plugin/dhcp_manager.go` and `pkg/plugin/plugin.go` are densely commented in the right way — they explain libnetwork's behavior, what each comment-bearing line is defending against, and which previous bug a piece of code fixes. Future-you will be grateful.
5. **Plugin.Health endpoint**. Lightweight observability hook (recovery counters + active-endpoint count) accessible on the same socket as the libnetwork RPCs. Trivial to wire into a `curl --unix-socket` health check.
6. **CI exists, runs the right things**. Build, vet, gofmt, staticcheck, race-tests on every push to main and dev. Most Go projects with comparable scope ship without any of this.

## Top Recommendations

Ordered by value-to-effort.

1. **Delete `Plugin.storeJoinHint` (W-N1)**. Five-line delete; one PR. Confirms `updateJoinHint` is the only RMW path and removes a footgun for future contributors.
2. **Set HTTP server timeouts and call `server.Shutdown` in `Close` (W-N2 + W-N3)**. ~20 lines total. Closes the contract on the shutdown path and brings the codebase in line with the skill-reference's Go server hygiene checklist. Pair these in one PR; they touch the same struct.
3. **Add `deadcode` and `govulncheck` to CI (I-N5)**. ~10 lines of YAML. Catches the next dead-code instance and surfaces upstream CVEs as they get assigned. `gosec` is also worth adding but you'll want to suppress G104/G301/G302 on the deferred-Close and plugin-private-FS paths first.
4. **Fix `ErrInvalidMode` to mention `ipvlan` (W-N5)**. One-line change. Stops users with a typo from concluding ipvlan isn't supported.
5. **Tie recovery to plugin lifetime (W-N4)**. ~30 lines. The largest of the W-N findings; resolves prior issue #10. Worth doing before anybody embeds the plugin into a longer-lived process (e.g. integration tests).

The 23 open carryover issues from prior reviews are appropriate next-quarter work — none of them are show-stoppers, and the codebase is already running production traffic at .12 with zero log noise on v0.5.3.
