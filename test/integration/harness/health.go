// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"
)

// HealthResponse and the floor that reads it live in healthfloor.go,
// which is untagged so the decision logic is unit-testable.

// pluginExecRoot is where the daemon exposes managed-plugin sockets.
//
// It looks like it should follow --exec-root, and it does NOT: moby
// hardcodes it (daemon/daemon_linux.go, getPluginExecRoot returns
// "/run/docker/plugins" and ignores its config argument entirely; the
// Windows build is the one that derives it from the data-root). A
// second daemon on the same host therefore puts its plugin sockets
// HERE, beside the first daemon's, distinguished only by plugin id.
//
// Verified the expensive way (#125): pointing this at a daemon's own
// --exec-root made the health floor report an unreachable plugin on a
// run in which the plugin was serving perfectly well.
const pluginExecRoot = "/run/docker/plugins"

// dockerDataRoot asks the daemon where its data-root is rather than
// assuming /var/lib/docker. Derived, not transcribed: a second daemon
// on the same host must have its own, and a transcribed constant is
// silently wrong there instead of loudly.
//
// The Info call needs a daemon and so stays here; the choice it feeds
// is chooseDataRoot in dataroot.go, which is untagged so that both of
// its branches — the answer and the fallback — are covered by the
// ordinary test lane rather than by nothing.
func dockerDataRoot(ctx context.Context, cli *docker.Client) string {
	info, err := cli.Info(ctx)
	return chooseDataRoot(info.DockerRootDir, err)
}

// PluginSocketPath returns the absolute path to PluginRef's UNIX
// socket. Docker exposes plugin sockets under
// /run/docker/plugins/<plugin-id>/<sock-name>.sock; the id comes from
// PluginInspect, the directory from pluginExecRoot. Requires root to
// dial the socket.
func PluginSocketPath(ctx context.Context, cli *docker.Client) (string, error) {
	p, _, err := cli.PluginInspectWithRaw(ctx, PluginRef)
	if err != nil {
		return "", fmt.Errorf("PluginInspect: %w", err)
	}
	if !p.Enabled {
		return "", fmt.Errorf("plugin %q is not currently enabled — its socket is gone", PluginRef)
	}
	// The plugin manifest declares a single socket; net-dhcp.sock is
	// the canonical name in this fork's config.json.
	return filepath.Join(pluginExecRoot, p.ID, "net-dhcp.sock"), nil
}

// PluginHealth dials the plugin's socket and returns its
// /Plugin.Health payload.
func PluginHealth(ctx context.Context, cli *docker.Client) (*HealthResponse, error) {
	sock, err := PluginSocketPath(ctx, cli)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://plugin/Plugin.Health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dial plugin socket %s: %w", sock, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Plugin.Health returned %s", resp.Status)
	}
	var out HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode Plugin.Health: %w", err)
	}
	return &out, nil
}

// WaitPluginHealth polls until the plugin's socket answers, or budget
// is spent, and fails the test if it never does.
//
// This is for *readiness* only — the gap after a deliberate recycle
// where Plugin.Enabled has flipped but the socket is not listening yet.
// It deliberately makes no claim about counters, and comparing two of
// its results is not a measurement: use BeginCounterWindow for that.
//
// It is also the suite's ONE-READING reader, for the same reason: a
// cell that reads a field of the document rather than a change in one
// (the `endpoints` array, `status`, the build identity) has no window
// to belong to, and taking a lone PluginHealth for it is what
// counterwindow_guard_test.go refuses.
//
// It exists so a readiness poll is not written as a bare PluginHealth
// loop, which is indistinguishable at a glance from an unguarded
// measurement and is what the suite-source guard rejects (#405).
func WaitPluginHealth(t *testing.T, ctx context.Context, cli *docker.Client, budget time.Duration) *HealthResponse {
	t.Helper()
	return WaitPluginHealthFor(t, ctx, cli, budget, "the plugin socket to answer", nil)
}

// WaitPluginHealthFor is WaitPluginHealth with a precondition on the
// state the document describes: it polls until the socket answers AND
// cond accepts, and fails naming what it was waiting for.
//
// WHY A ONE-READING CELL NEEDS ONE. A manager is registered in
// persistentDHCP when the Join returns and its renewal client binds
// after that, so there is a window in which `endpoints` truthfully
// reports the container's entry as `acquiring` with no address. A cell
// that reads the document once lands in it (measured in CI, run
// 33938855928, on a container whose start had already returned).
//
// STILL NOT A MEASUREMENT, and two rules keep it from becoming the
// unguarded before/after pair #405 found: cond sees ONE document, so
// there is nothing to subtract; and cond must be written on a DIFFERENT
// field from the ones the caller then asserts on. A cond that waits for
// the answer the test wants makes the test a report that the answer was
// eventually produced, which is the failure `--- FAIL` cannot show you.
func WaitPluginHealthFor(t *testing.T, ctx context.Context, cli *docker.Client, budget time.Duration, what string, cond func(*HealthResponse) bool) *HealthResponse {
	t.Helper()
	deadline := time.Now().Add(budget)
	var lastErr error
	var last *HealthResponse
	for time.Now().Before(deadline) {
		h, err := PluginHealth(ctx, cli)
		if err != nil {
			lastErr = err
		} else {
			last = h
			if cond == nil || cond(h) {
				return h
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("plugin health never became reachable within %s while waiting for %s; last error: %v",
			budget, what, lastErr)
	}
	t.Fatalf("waited %s for %s and it never happened. The last document reported %d endpoint(s): %+v",
		budget, what, len(last.Endpoints), last.Endpoints)
	return nil
}

// ReadPluginLog returns the current contents of the plugin's
// /var/log/net-dhcp.log as a string, or an empty string with a t.Logf
// note on error. Useful when a test wants to assert on a specific log
// line emitted by the plugin during a bound/renew event (e.g. T2-2
// surfaces NTP / TFTP / search-list values at info level there).
//
// A thin t-flavoured wrapper over PluginLog: swallowing the error into
// a log note is what a mid-test assertion helper wants, and is exactly
// what the health floor must not do.
func ReadPluginLog(t *testing.T, ctx context.Context) string {
	t.Helper()
	_, data, err := PluginLog(ctx)
	if err != nil {
		t.Logf("ReadPluginLog: %v", err)
		return ""
	}
	return string(data)
}

// CountPluginLogLines returns how many lines of the plugin log contain
// every one of subs.
//
// The motivating use is attribution (#278). Health counters are
// plugin-WIDE: `dhcp_timeouts` climbing proves that *some* client saw a
// failure, not that the endpoint under test did. Every counter bump in
// dhcpManager sits next to a log line carrying that manager's
// `endpoint=<short id>` field, so passing an endpoint id plus the
// message text turns a global observation into an endpoint-scoped one
// without adding per-endpoint counters to the health surface.
//
// Counts, not booleans, so callers can assert on a DELTA across a
// window and stay immune to start-up churn already in the log.
func CountPluginLogLines(t *testing.T, ctx context.Context, subs ...string) int {
	t.Helper()
	if len(subs) == 0 {
		return 0
	}
	n := 0
	for _, line := range strings.Split(ReadPluginLog(t, ctx), "\n") {
		matched := true
		for _, sub := range subs {
			if !strings.Contains(line, sub) {
				matched = false
				break
			}
		}
		if matched {
			n++
		}
	}
	return n
}

// DumpPluginLog tails the plugin's /var/log/net-dhcp.log into t.Log.
// Plugin logs live under <data-root>/plugins/<plugin-id>/rootfs/
// (Docker's standard layout for managed plugins). The plugin id comes
// from PluginInspect; its rootfs is read directly from the host
// filesystem (the test process runs as root). Useful as a t.Cleanup
// hook on tests that depend on plugin-side state changes — without
// it, a failure surfaces as "expected X, got Y" with no insight into
// what the plugin actually did.
//
// Best-effort: missing log file or unresolvable plugin id is logged
// as a Logf, never a Fatal — we don't want a missing log to cascade
// into the diagnostic noise that hid the original failure.
func DumpPluginLog(t *testing.T) {
	t.Helper()
	// Cleanup runs after the test's deferred cancel(), so we derive
	// a fresh context — passing the test's ctx in would arrive
	// already canceled and PluginInspect would fail with
	// context.Canceled. 5s is enough for the local-socket call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logPath, data, err := PluginLog(ctx)
	if err != nil {
		t.Logf("DumpPluginLog: %v", err)
		return
	}
	t.Logf("--- net-dhcp plugin log (%s) ---\n%s", logPath, data)
}

// PluginLog returns the plugin's on-disk log and the path it came
// from. Split out of DumpPluginLog so the health floor can reach it
// too: the floor runs in TestMain after m.Run() and has no *testing.T
// to log into, which is why a floor failure used to report a counter
// and no evidence at all (#385).
//
// Errors are returned rather than logged so each caller can decide how
// loud to be — DumpPluginLog stays best-effort, the floor says plainly
// that its evidence is missing.
func PluginLog(ctx context.Context) (string, []byte, error) {
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return "", nil, fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()

	p, _, err := cli.PluginInspectWithRaw(ctx, PluginRef)
	if err != nil {
		return "", nil, fmt.Errorf("PluginInspect: %w", err)
	}
	logPath := filepath.Join(dockerDataRoot(ctx, cli), "plugins", p.ID, "rootfs/var/log/net-dhcp.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return logPath, nil, fmt.Errorf("read %s: %w", logPath, err)
	}
	return logPath, data, nil
}

// PluginLogSize returns the current size of the plugin's on-disk log,
// for use as a baseline offset.
//
// WHY AN OFFSET AND NOT A COUNTER SNAPSHOT. The censuses read the whole
// log on purpose: the plugin's counters reset when the plugin process
// does, so a floor judging counters alone sees only the last restart's
// worth of a run — which is how a run with three failed Joins went
// green (#385, #406). The log does not reset, and neither does a byte
// offset into it, so scoping by offset keeps that property while still
// answering "during THIS process".
//
// A missing or unreadable log yields 0, meaning "scope to the whole
// log". That is the safe direction: it can only make the floor judge
// more than this process caused, never less, so a broken baseline
// cannot quietly narrow what gets judged.
func PluginLogSize(ctx context.Context) int64 {
	_, data, err := PluginLog(ctx)
	if err != nil {
		return 0
	}
	return int64(len(data))
}

// LogSince returns the portion of data after off, for scoping a census
// to one test process.
//
// An offset past the end means the log was TRUNCATED or replaced since
// the baseline — a plugin reinstall, or a rotation. Falling back to the
// whole log is deliberate: the alternative is judging nothing, and a
// census that silently judges nothing is the failure mode this whole
// mechanism exists to prevent.
func LogSince(data []byte, off int64) []byte {
	if off <= 0 || off > int64(len(data)) {
		return data
	}
	return data[off:]
}

// WaitPluginEnabled polls PluginInspect until p.Enabled matches want
// or budget elapses. Use after PluginEnable / PluginDisable to know
// when the daemon has reflected the state change.
func WaitPluginEnabled(ctx context.Context, cli *docker.Client, want bool, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		p, _, err := cli.PluginInspectWithRaw(ctx, PluginRef)
		if err == nil && p.Enabled == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("plugin did not reach enabled=%v within %v", want, budget)
}

// PluginHealthOrNil reads the health surface and returns nil on any
// error, for callers that want a baseline rather than an assertion.
//
// Deliberately NOT a variant that fails: a baseline is an optimisation
// on top of a correct-but-wider judgement, so a plugin that is not
// answering yet must not turn into a run-level error here. The floor
// itself already fails loudly if health is unreadable at the END of a
// run, which is where an unreachable plugin actually matters.
func PluginHealthOrNil(ctx context.Context) *HealthResponse {
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return nil
	}
	defer cli.Close()
	h, err := PluginHealth(ctx, cli)
	if err != nil {
		return nil
	}
	return h
}
