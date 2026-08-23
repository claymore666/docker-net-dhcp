// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	docker "github.com/docker/docker/client"
)

// PluginMetrics dials the plugin's UNIX socket and returns the raw
// /metrics exposition plus its Content-Type.
//
// Deliberately returns the body as text rather than a parsed structure.
// The point of an integration check here is that the SHIPPED plugin
// serves the real thing over the real socket — parsing it into our own
// types would re-introduce exactly the "we assert against our model of
// it" problem #644 was filed about.
func PluginMetrics(ctx context.Context, cli *docker.Client) (body, contentType string, err error) {
	sock, err := PluginSocketPath(ctx, cli)
	if err != nil {
		return "", "", err
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://plugin/metrics", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("dial plugin socket %s: %w", sock, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("/metrics returned %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read /metrics body: %w", err)
	}
	return string(b), resp.Header.Get("Content-Type"), nil
}
