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

// startMetricsListener brings up the optional TCP endpoint on a
// kernel-assigned port and returns its base URL.
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

// TestMetricsListener_ServesTheExposition is the happy path: the port an
// operator opened actually answers a scrape.
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

// TestMetricsListener_ExposesNothingButMetrics is the security assertion
// of this feature, and the reason the TCP endpoint is a second server
// rather than p.server on another listener.
//
// p.server routes every libnetwork RPC. This plugin runs with
// CAP_NET_ADMIN, CAP_SYS_ADMIN and CAP_SYS_PTRACE and
// "network": {"type": "host"}, so serving that mux on a TCP port would
// hand anyone who can reach the port the ability to create networks,
// join endpoints and delete them — on the host's own network namespace.
//
// The mistake this guards against is a plausible one-line "simplification"
// (reuse p.server.Handler, why build a second mux?), and nothing else in
// the suite would go red for it. A 404 here is the contract.
func TestMetricsListener_ExposesNothingButMetrics(t *testing.T) {
	p, base := startMetricsListener(t)

	// Every path the plugin serves on its socket, plus the paths the
	// daemon calls that we deliberately leave unrouted. Taken from the
	// routing table rather than a literal list so a route added later
	// is covered here automatically.
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
		// POST, because that is how libnetwork calls them — a GET-only
		// check could pass while the real method was reachable.
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

// TestMetricsListener_BadAddressFailsAtStartup pins that a malformed or
// unusable METRICS_ADDR is a startup error rather than a goroutine that
// logs and leaves the plugin running without the endpoint an operator
// asked for.
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

// TestMetricsListener_OffByDefault records that constructing a plugin
// opens no port. The default is the security posture, so it is asserted
// rather than assumed.
func TestMetricsListener_OffByDefault(t *testing.T) {
	p := &Plugin{}
	if p.metricsServer != nil || p.metricsListener != nil {
		t.Error("a plugin that was never told METRICS_ADDR has a metrics listener")
	}
}
