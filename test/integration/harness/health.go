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

// PluginSocketPath returns the absolute path to PluginRef's UNIX
// socket. Docker exposes plugin sockets under
// /run/docker/plugins/<plugin-id>/<sock-name>.sock; both fragments
// come from PluginInspect. Requires root to dial the socket.
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
	return filepath.Join("/run/docker/plugins", p.ID, "net-dhcp.sock"), nil
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
// It exists so a readiness poll is not written as a bare PluginHealth
// loop, which is indistinguishable at a glance from an unguarded
// measurement and is what the suite-source guard rejects (#405).
func WaitPluginHealth(t *testing.T, ctx context.Context, cli *docker.Client, budget time.Duration) *HealthResponse {
	t.Helper()
	deadline := time.Now().Add(budget)
	var lastErr error
	for time.Now().Before(deadline) {
		h, err := PluginHealth(ctx, cli)
		if err == nil {
			return h
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("plugin health never became reachable within %s; last error: %v", budget, lastErr)
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
// Plugin logs live under /var/lib/docker/plugins/<plugin-id>/rootfs/
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
	logPath := filepath.Join("/var/lib/docker/plugins", p.ID, "rootfs/var/log/net-dhcp.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return logPath, nil, fmt.Errorf("read %s: %w", logPath, err)
	}
	return logPath, data, nil
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
