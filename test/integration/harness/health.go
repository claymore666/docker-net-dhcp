//go:build integration

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
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
