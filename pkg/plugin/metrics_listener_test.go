// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startMetricsListener(t *testing.T) (*Plugin, string) {
	t.Helper()
	p := &Plugin{
		startTime:      time.Now(),
		instanceID:     "test-instance",
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	if err := p.ListenMetrics("127.0.0.1:0"); err != nil {
		t.Fatalf("ListenMetrics: %v", err)
	}
	t.Cleanup(func() {
		if p.metricsServer != nil {
			_ = p.metricsServer.Close()
		}
	})
	return p, "http://" + p.metricsListener.Addr().String()
}

func TestMetricsListener_ServesTheExposition(t *testing.T) {
	_, base := startMetricsListener(t)

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain exposition", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `net_dhcp_build_info{instance_id="test-instance",`) {
		t.Errorf("exposition does not carry this instance's identity:\n%s", body)
	}
}

// The TCP endpoint has its own mux because the socket's mux routes every libnetwork RPC.
func TestMetricsListener_ExposesNothingButMetrics(t *testing.T) {
	p, base := startMetricsListener(t)

	var paths []string
	for _, r := range p.routes() {
		if r.path == "/metrics" {
			continue
		}
		paths = append(paths, r.path)
	}
	paths = append(paths, unroutedRPCs()...)
	if len(paths) < 2 {
		t.Fatalf("routing table yielded %d paths to check; the table is the input to this test", len(paths))
	}

	for _, path := range paths {
		// libnetwork calls every RPC with POST.
		resp, err := http.Post(base+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s is REACHABLE over the metrics port: status %d, body %q.\n"+
				"The metrics listener must serve /metrics and nothing else — this port is on the host network namespace "+
				"and these RPCs mutate host networking.", path, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
}

func TestMetricsListener_BadAddressFailsAtStartup(t *testing.T) {
	p := &Plugin{}
	err := p.ListenMetrics("this is not an address")
	if err == nil {
		if p.metricsServer != nil {
			_ = p.metricsServer.Close()
		}
		t.Fatal("a malformed METRICS_ADDR was accepted")
	}
	if !strings.Contains(err.Error(), "this is not an address") {
		t.Errorf("error does not name the bad address: %v", err)
	}
}

func TestMetricsListener_OffByDefault(t *testing.T) {
	p := &Plugin{}
	if p.metricsServer != nil || p.metricsListener != nil {
		t.Error("a plugin that was never told METRICS_ADDR has a metrics listener")
	}
}
