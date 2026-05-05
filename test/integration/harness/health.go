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
	"testing"
	"time"

	docker "github.com/docker/docker/client"
)

// HealthResponse mirrors pkg/plugin.HealthResponse. Duplicated here
// so the integration package doesn't pull on pkg/plugin internals.
type HealthResponse struct {
	Healthy                bool    `json:"healthy"`
	UptimeSeconds          float64 `json:"uptime_seconds"`
	ActiveEndpoints        int     `json:"active_endpoints"`
	PendingHints           int     `json:"pending_hints"`
	RecoveredOK            int32   `json:"recovered_ok"`
	RecoveryFailed         int32   `json:"recovery_failed"`
	TombstoneWriteFailures int32   `json:"tombstone_write_failures"`
	LeaseChanged           int32   `json:"lease_changed"`
	LeasesObtained         int32   `json:"leases_obtained"`
	LeasesRenewed          int32   `json:"leases_renewed"`
	DHCPTimeouts           int32   `json:"dhcp_timeouts"`
	LeaseReleaseFailures   int32   `json:"lease_release_failures"`
}

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

// ReadPluginLog returns the current contents of the plugin's
// /var/log/net-dhcp.log as a string, or an empty string with a t.Logf
// note on error. Useful when a test wants to assert on a specific log
// line emitted by the plugin during a bound/renew event (e.g. T2-2
// surfaces NTP / TFTP / search-list values at info level there).
//
// Path resolution mirrors DumpPluginLog. ctx is used for the
// PluginInspect round-trip; the local ReadFile is unbounded.
func ReadPluginLog(t *testing.T, ctx context.Context) string {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Logf("ReadPluginLog: docker client: %v", err)
		return ""
	}
	defer cli.Close()
	p, _, err := cli.PluginInspectWithRaw(ctx, PluginRef)
	if err != nil {
		t.Logf("ReadPluginLog: PluginInspect: %v", err)
		return ""
	}
	logPath := filepath.Join("/var/lib/docker/plugins", p.ID, "rootfs/var/log/net-dhcp.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Logf("ReadPluginLog: read %s: %v", logPath, err)
		return ""
	}
	return string(data)
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

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Logf("DumpPluginLog: docker client: %v", err)
		return
	}
	defer cli.Close()

	p, _, err := cli.PluginInspectWithRaw(ctx, PluginRef)
	if err != nil {
		t.Logf("DumpPluginLog: PluginInspect: %v", err)
		return
	}
	logPath := filepath.Join("/var/lib/docker/plugins", p.ID, "rootfs/var/log/net-dhcp.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Logf("DumpPluginLog: read %s: %v", logPath, err)
		return
	}
	t.Logf("--- net-dhcp plugin log (%s) ---\n%s", logPath, data)
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
