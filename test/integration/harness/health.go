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

// pluginExecRoot is where the daemon exposes managed-plugin sockets. moby hardcodes it on Linux (getPluginExecRoot in
// daemon/daemon_linux.go ignores --exec-root), so a second daemon's sockets land here too, told apart by plugin id; a
// daemon's own --exec-root made the health floor report a serving plugin unreachable (#125).
const pluginExecRoot = "/run/docker/plugins"

// dockerDataRoot asks the daemon for its data-root, since a second daemon on the host has its own.
func dockerDataRoot(ctx context.Context, cli *docker.Client) string {
	info, err := cli.Info(ctx)
	return chooseDataRoot(info.DockerRootDir, err)
}

// PluginSocketPath returns the path of PluginRef's UNIX socket, which needs root to dial.
func PluginSocketPath(ctx context.Context, cli *docker.Client) (string, error) {
	p, _, err := cli.PluginInspectWithRaw(ctx, PluginRef)
	if err != nil {
		return "", fmt.Errorf("PluginInspect: %w", err)
	}
	if !p.Enabled {
		return "", fmt.Errorf("plugin %q is not currently enabled — its socket is gone", PluginRef)
	}
	return filepath.Join(pluginExecRoot, p.ID, "net-dhcp.sock"), nil
}

// PluginHealth dials the plugin's socket and returns its /Plugin.Health payload.
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

// WaitPluginHealth polls until the plugin's socket answers or budget is spent, failing the test if it never does; it is
// for readiness and one-reading cells, never a counter measurement (#405).
func WaitPluginHealth(t *testing.T, ctx context.Context, cli *docker.Client, budget time.Duration) *HealthResponse {
	t.Helper()
	return WaitPluginHealthFor(t, ctx, cli, budget, "the plugin socket to answer", nil)
}

// WaitPluginHealthFor is WaitPluginHealth that also waits until cond accepts the document. An endpoint reads
// `acquiring` with no address between Join's return and its client binding, measured in CI run 33938855928 (#910).
// cond must test a different field from the ones the caller asserts on (#405).
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

// ReadWholePluginLog returns the plugin's whole log, which spans the suite; most callers want MarkPluginLog (#933).
func ReadWholePluginLog(t *testing.T, ctx context.Context) string {
	t.Helper()
	_, data, err := PluginLog(ctx)
	if err != nil {
		t.Logf("ReadWholePluginLog: %v", err)
		return ""
	}
	return string(data)
}

// MarkPluginLog returns the plugin log's current size for ReadPluginLogSince, failing the test if it cannot be read.
func MarkPluginLog(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	path, data, err := PluginLog(ctx)
	if err != nil {
		t.Fatalf("marking the plugin log (%s): %v\n"+
			"Without a mark the window is the whole log, and an assertion over the whole log "+
			"is satisfied by another test's lines.", path, err)
	}
	return int64(len(data))
}

// ReadPluginLogSince returns the plugin log written after mark, empty when the log cannot be read.
func ReadPluginLogSince(t *testing.T, ctx context.Context, mark int64) string {
	t.Helper()
	_, data, err := PluginLog(ctx)
	if err != nil {
		t.Logf("ReadPluginLogSince: %v", err)
		return ""
	}
	return string(PluginLogWindow(data, mark))
}

// AwaitPluginLogSince polls the window after mark until want accepts it and returns the last read; a single read races
// the plugin's write (#933).
func AwaitPluginLogSince(t *testing.T, ctx context.Context, mark int64, budget time.Duration,
	want func(window string) bool) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		got := ReadPluginLogSince(t, ctx, mark)
		if want(got) {
			return got
		}
		if !time.Now().Before(deadline) {
			t.Logf("the plugin log window was still incomplete after %s; asserting on what it holds", budget)
			return got
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// CountPluginLogLines returns how many plugin log lines contain every one of subs, for endpoint attribution (#278).
func CountPluginLogLines(t *testing.T, ctx context.Context, subs ...string) int {
	t.Helper()
	if len(subs) == 0 {
		return 0
	}
	n := 0
	for _, line := range strings.Split(ReadWholePluginLog(t, ctx), "\n") {
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

// DumpPluginLog logs the plugin's /var/log/net-dhcp.log into t.Log, best-effort.
func DumpPluginLog(t *testing.T) {
	t.Helper()
	// Cleanup runs after the test's deferred cancel, so it needs a fresh context.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logPath, data, err := PluginLog(ctx)
	if err != nil {
		t.Logf("DumpPluginLog: %v", err)
		return
	}
	t.Logf("--- net-dhcp plugin log (%s) ---\n%s", logPath, data)
}

// PluginLog returns the plugin's on-disk log and its path, for TestMain's health floor which has no *testing.T (#385).
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

// PluginLogSize returns the plugin log's size as a baseline offset, or 0 when unreadable. The counters reset with the
// plugin process and the log does not, so a counter-only floor let three failed Joins go green (#385, #406).
func PluginLogSize(ctx context.Context) int64 {
	_, data, err := PluginLog(ctx)
	if err != nil {
		return 0
	}
	return int64(len(data))
}

// WaitPluginEnabled polls PluginInspect until Enabled matches want or budget elapses.
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

// PluginHealthOrNil returns the health document, or nil on any error, for callers taking a baseline.
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
