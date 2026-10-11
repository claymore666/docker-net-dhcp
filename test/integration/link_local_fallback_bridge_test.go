// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestLinkLocalFallback_ABridgeWithNoServerStartsOnALinkLocalAddress is the bridge case of the macvlan test (#1323).
// The bridge has no DHCP server and no other port, so no lease can arrive; the macvlan test covers the move to one.
func TestLinkLocalFallback_ABridgeWithNoServerStartsOnALinkLocalAddress(t *testing.T) {
	const (
		brName  = "dh-itest-brll"
		netName = "dh-itest-linklocal-br"
		ctrName = "dh-itest-linklocal-br-ctr"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	la := netlink.NewLinkAttrs()
	la.Name = brName
	br := &netlink.Bridge{LinkAttrs: la}
	if err := netlink.LinkAdd(br); err != nil {
		t.Fatalf("LinkAdd(%s bridge): %v", brName, err)
	}
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(brName); err == nil {
			_ = netlink.LinkDel(l)
		}
	})
	// The kernel's 15 s STP forward delay would hold the container's port out of forwarding during the probes (#1323).
	if err := os.WriteFile(filepath.Join("/sys/class/net", brName, "bridge/forward_delay"), []byte("0"), 0o644); err != nil {
		t.Fatalf("forward_delay on %s: %v", brName, err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		t.Fatalf("LinkSetUp(%s): %v", brName, err)
	}

	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	t.Cleanup(func() {
		if t.Failed() {
			harness.DumpPluginLog(t)
		}
	})
	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
		"bridge":              brName,
		"link_local_fallback": "true",
	})
	started := time.Now()
	id, ip, _ := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container started %.1fs after the create with ip=%s", time.Since(started).Seconds(), ip)
	if parsed := net.ParseIP(ip); parsed == nil || !parsed.IsLinkLocalUnicast() || parsed.To4() == nil {
		t.Fatalf("Docker reports %q, want an IPv4 169.254/16 address with no server on the bridge (#1323)", ip)
	}
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	onLink := linkV4(t, ctx, id)
	if len(onLink) != 1 || !strings.HasPrefix(onLink[0], ip+"/16") {
		t.Fatalf("the container's eth0 holds %v, want exactly %s/16 (#1323)", onLink, ip)
	}
	if links := harness.ExecOutput(t, ctx, id, "ip", "-o", "-4", "addr", "show"); strings.Count(strings.TrimSpace(links), "\n") != 1 ||
		!strings.Contains(links, " eth0 ") {
		t.Fatalf("the container holds IPv4 on more than lo and eth0, so the engine added its gateway bridge (#1323):\n%s", links)
	}
	if routes := harness.ExecOutput(t, ctx, id, "ip", "-4", "route", "show"); strings.Contains(routes, "default") {
		t.Fatalf("a link-local endpoint got a default route, which Join withholds for it (#1323):\n%s", routes)
	}
	// The address came through the bridge path: the container's host-side port sits on the bridge we made.
	ports, brIndex := 0, mustLinkIndex(t, brName)
	links, err := util.DumpResult(netlink.LinkList())
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	for _, l := range links {
		if l.Attrs().MasterIndex == brIndex {
			ports++
		}
	}
	if ports != 1 {
		t.Errorf("%s has %d ports, want exactly the container's one (#1323)", brName, ports)
	}

	h := harness.WaitPluginHealthFor(t, ctx, cli, 20*time.Second, "the endpoint to read link_local",
		func(h *harness.HealthResponse) bool {
			e, ok := endpointView(h, ep)
			return ok && e.LeaseState == "link_local"
		})
	if h.LinkLocalEndpoints < 1 {
		t.Errorf("link_local_endpoints = %d with this endpoint on 169.254/16 (#1323)", h.LinkLocalEndpoints)
	}
	if e, _ := endpointView(h, ep); !strings.HasPrefix(e.Address, ip+"/16") {
		t.Errorf("health shows %s at %q, want the %s/16 the container holds (#1323)", ep, e.Address, ip)
	}
}

func mustLinkIndex(t *testing.T, name string) int {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", name, err)
	}
	return l.Attrs().Index
}
