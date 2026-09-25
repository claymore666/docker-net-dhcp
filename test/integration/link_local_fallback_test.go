// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

var containerV4 = regexp.MustCompile(`inet (\d+\.\d+\.\d+\.\d+/\d+)`)

// linkV4 lists the IPv4 addresses on the container's eth0 as the kernel holds them.
func linkV4(t *testing.T, ctx context.Context, id string) []string {
	t.Helper()
	var out []string
	for _, m := range containerV4.FindAllStringSubmatch(harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0"), -1) {
		out = append(out, m[1])
	}
	return out
}

func endpointView(h *harness.HealthResponse, ep string) (harness.EndpointHealth, bool) {
	for _, e := range h.Endpoints {
		if e.Endpoint == ep {
			return e, true
		}
	}
	return harness.EndpointHealth{}, false
}

// TestLinkLocalFallback_AServerlessStartThenMovesToALease starts a container with no DHCP server on the link, then
// starts the server and waits for the running container to move from 169.254/16 to a leased address (#904).
func TestLinkLocalFallback_AServerlessStartThenMovesToALease(t *testing.T) {
	const (
		netName = "dh-itest-linklocal"
		ctrName = "dh-itest-linklocal-ctr"
		// The client retransmits a DISCOVER at most 64s apart (RFC 2131 section 4.1), plus room for a loaded runner.
		moveBudget = 150 * time.Second
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ef := harness.NewEphemeralFixture(t, harness.WithDnsmasqBackend())
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	ef.Stop()
	mark := harness.MarkPluginLog(t, ctx)
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent":              harness.EphemeralHostVeth,
		"link_local_fallback": "true",
	})
	started := time.Now()
	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container started %.1fs after the create with ip=%s mac=%s", time.Since(started).Seconds(), ip, mac)
	if parsed := net.ParseIP(ip); parsed == nil || !parsed.IsLinkLocalUnicast() || parsed.To4() == nil {
		t.Fatalf("Docker reports %q, want an IPv4 169.254/16 address with no server on the link", ip)
	}
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	onLink := linkV4(t, ctx, id)
	if len(onLink) != 1 || !strings.HasPrefix(onLink[0], ip+"/16") {
		t.Fatalf("the container's eth0 holds %v, want exactly %s/16", onLink, ip)
	}
	// With no gateway in Join the engine attaches docker_gwbridge as a second link with its own default route (#904).
	if links := harness.ExecOutput(t, ctx, id, "ip", "-o", "-4", "addr", "show"); strings.Count(strings.TrimSpace(links), "\n") != 1 ||
		!strings.Contains(links, " eth0 ") {
		t.Fatalf("the container holds IPv4 on more than lo and eth0, so the engine added its gateway bridge:\n%s", links)
	}
	if routes := harness.ExecOutput(t, ctx, id, "ip", "-4", "route", "show"); strings.Contains(routes, "default") {
		t.Fatalf("a link-local endpoint got a default route, which Join withholds for it (#904):\n%s", routes)
	}
	h := harness.WaitPluginHealthFor(t, ctx, cli, 20*time.Second, "the endpoint to read link_local",
		func(h *harness.HealthResponse) bool {
			e, ok := endpointView(h, ep)
			return ok && e.LeaseState == "link_local"
		})
	if h.LinkLocalEndpoints < 1 {
		t.Errorf("link_local_endpoints = %d with this endpoint on 169.254/16", h.LinkLocalEndpoints)
	}

	ef.StartAgain()
	deadline := time.Now().Add(moveBudget)
	var leased string
	for time.Now().Before(deadline) {
		if got := linkV4(t, ctx, id); len(got) == 1 && !strings.HasPrefix(got[0], "169.254.") {
			leased = got[0]
			break
		}
		time.Sleep(time.Second)
	}
	if leased == "" {
		t.Fatalf("the container still holds %v %s after the server came back", linkV4(t, ctx, id), moveBudget)
	}
	addr, _, _ := net.ParseCIDR(leased)
	harness.AssertEphemeralIP(t, addr.String())
	if acked := ef.LastACKAddress(mac); acked != addr.String() {
		t.Errorf("the server last ACKed %q to %s, but the container link holds %s", acked, mac, leased)
	}
	if _, ok := ef.LeaseExpiry(mac); !ok {
		t.Errorf("the server's lease file holds no lease for %s", mac)
	}
	want := "default via " + ef.ServerIP() + " dev eth0"
	routes := ""
	for time.Now().Before(deadline) {
		if routes = strings.TrimSpace(harness.ExecOutput(t, ctx, id, "ip", "-4", "route", "show", "default")); strings.HasPrefix(routes, want) {
			break
		}
		time.Sleep(time.Second)
	}
	if !strings.HasPrefix(routes, want) || strings.Contains(routes, "\n") {
		t.Errorf("after the move the container's default routes are %q, want only %q", routes, want)
	}

	logged := harness.AwaitPluginLogSince(t, ctx, mark, 20*time.Second, func(w string) bool {
		return strings.Contains(w, "dhcp renew with changed IP")
	})
	for _, want := range []string{"Endpoint is on an IPv4 link-local address", "dhcp renew with changed IP",
		"left its link-local address"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the plugin log since the network was created has no %q", want)
		}
	}
	h = harness.WaitPluginHealthFor(t, ctx, cli, 30*time.Second, "the endpoint to read bound",
		func(h *harness.HealthResponse) bool {
			e, ok := endpointView(h, ep)
			return ok && e.LeaseState == "bound"
		})
	if e, _ := endpointView(h, ep); e.Address != leased {
		t.Errorf("health shows %s at %q, want the leased %s", ep, e.Address, leased)
	}
}
