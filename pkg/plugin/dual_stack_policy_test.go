// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"sync"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func dualStackPolicyManager(t *testing.T) *dhcpManager {
	t.Helper()
	withStateDir(t, t.TempDir())
	m, _ := daemonFreeManager(t, &fakeDocker{})
	m.opts.Bridge, m.opts.IPv6Mode = "br0", "slaac"
	m.opts.DHCPServers = "192.0.2.1"
	m.ctrLink = &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: 1, Name: "lo", HardwareAddr: m.MacAddress}}
	return m
}

func recordClientOptions(t *testing.T) map[bool]*dhcp.DHCPClientOptions {
	t.Helper()
	seen := map[bool]*dhcp.DHCPClientOptions{}
	prev := newDHCPClient
	newDHCPClient = func(_ string, o *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
		c := *o
		seen[o.V6] = &c
		return nil, errors.New("no client in this test")
	}
	t.Cleanup(func() { newDHCPClient = prev })
	return seen
}

func TestDualStack_V6SetupKeepsTheV4PolicyFlag(t *testing.T) {
	m := dualStackPolicyManager(t)
	seen := recordClientOptions(t)

	_, _ = m.setupClient(false)
	if o := seen[false]; o == nil || len(o.AllowServers) == 0 {
		t.Fatal("v4 client was not given the allow-list; fixture wrong")
	}
	_, _ = m.setupClient(true)
	if o := seen[true]; o == nil || len(o.AllowServers) != 0 {
		t.Fatalf("v6 client options = %+v, want no allow-list", o)
	}

	m.handleEvent(dhcp.Event{Type: "leasefail"}, false)
	h := m.plugin.healthSnapshot()
	if h.DHCPTimeoutsV4 != 1 || h.DHCPServerPolicyTimeouts != 1 {
		t.Errorf("after one v4 leasefail dhcp_timeouts_v4=%d dhcp_server_policy_timeouts=%d, want 1 and 1",
			h.DHCPTimeoutsV4, h.DHCPServerPolicyTimeouts)
	}
}

func TestDualStack_V6OutageIsNotAPolicyTimeout(t *testing.T) {
	m := dualStackPolicyManager(t)
	recordClientOptions(t)

	_, _ = m.setupClient(false)
	_, _ = m.setupClient(true)

	m.handleEvent(dhcp.Event{Type: "leasefail"}, true)
	h := m.plugin.healthSnapshot()
	if h.DHCPTimeoutsV6 != 1 || h.DHCPTimeoutsV4 != 0 || h.DHCPServerPolicyTimeouts != 0 {
		t.Errorf("after one v6 leasefail v6=%d v4=%d policy=%d, want 1, 0, 0",
			h.DHCPTimeoutsV6, h.DHCPTimeoutsV4, h.DHCPServerPolicyTimeouts)
	}
}

func TestDualStack_V4OutageCountsOnlyUnderAnAllowList(t *testing.T) {
	for _, tc := range []struct {
		name    string
		servers string
		want    int32
	}{
		{"v4 only with an allow-list", "192.0.2.1", 1},
		{"v4 only without one", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := dualStackPolicyManager(t)
			m.opts.IPv6Mode, m.opts.DHCPServers = "", tc.servers
			recordClientOptions(t)
			_, _ = m.setupClient(false)
			m.handleEvent(dhcp.Event{Type: "leasefail"}, false)
			h := m.plugin.healthSnapshot()
			if h.DHCPTimeoutsV4 != 1 || h.DHCPServerPolicyTimeouts != tc.want {
				t.Errorf("v4=%d policy=%d, want 1 and %d", h.DHCPTimeoutsV4, h.DHCPServerPolicyTimeouts, tc.want)
			}
		})
	}
}

func TestDualStack_V6ClientGetsTheReReadLink(t *testing.T) {
	m := dualStackPolicyManager(t)
	m.netHandle = &netlink.Handle{}
	prevIdx := nlLinkByIndex
	nlLinkByIndex = func(_ *netlink.Handle, index int) (netlink.Link, error) {
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: index, Name: "eth0-renamed", HardwareAddr: m.MacAddress}}, nil
	}
	t.Cleanup(func() { nlLinkByIndex = prevIdx })
	names := map[bool]string{}
	indexes := map[bool]int{}
	prev := newDHCPClient
	newDHCPClient = func(name string, o *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
		names[o.V6], indexes[o.V6] = name, o.LinkIndex
		return nil, errors.New("no client in this test")
	}
	t.Cleanup(func() { newDHCPClient = prev })

	_, _ = m.setupClient(false)
	stored := m.ctrLink
	_, _ = m.setupClient(true)

	for _, v6 := range []bool{false, true} {
		if names[v6] != "eth0-renamed" || indexes[v6] != 1 {
			t.Errorf("v6=%v client got link %q index %d, want eth0-renamed and 1", v6, names[v6], indexes[v6])
		}
	}
	if m.ctrLink != stored {
		t.Error("the v6 set-up replaced m.ctrLink after the v4 goroutine could be reading it")
	}
}

func TestDualStack_V6SetupDoesNotRaceTheV4EventGoroutine(t *testing.T) {
	m := dualStackPolicyManager(t)
	m.netHandle = &netlink.Handle{}
	events := make(chan dhcp.Event)
	prevNew, prevStart, prevIdx, prevMTU, prevRoutes := newDHCPClient, startDHCPClient, nlLinkByIndex, nlHandleLinkSetMTU, nlHandleRouteListFiltered
	newDHCPClient = func(_ string, o *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
		if o.V6 {
			return nil, errors.New("no v6 client in this test")
		}
		return &dhcp.DHCPClient{}, nil
	}
	startDHCPClient = func(*dhcp.DHCPClient) (chan dhcp.Event, error) { return events, nil }
	nlLinkByIndex = func(_ *netlink.Handle, index int) (netlink.Link, error) {
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: index, Name: "lo", MTU: 1500, HardwareAddr: m.MacAddress}}, nil
	}
	nlHandleLinkSetMTU = func(*netlink.Handle, netlink.Link, int) error { return nil }
	nlHandleRouteListFiltered = func(*netlink.Handle, int, *netlink.Route, uint64) ([]netlink.Route, error) { return nil, nil }
	t.Cleanup(func() {
		newDHCPClient, startDHCPClient, nlLinkByIndex, nlHandleLinkSetMTU, nlHandleRouteListFiltered =
			prevNew, prevStart, prevIdx, prevMTU, prevRoutes
	})

	if _, err := m.setupClient(false); err != nil {
		t.Fatalf("v4 setup: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			events <- dhcp.Event{Type: "leasefail"}
			events <- dhcp.Event{Type: "renew", Data: dhcp.Info{MTU: 1400}}
			events <- dhcp.Event{Type: "bound", Data: dhcp.Info{MTU: 1400}}
		}
	}()
	for i := 0; i < 50; i++ {
		_, _ = m.setupClient(true)
	}
	wg.Wait()
	close(events)
}
