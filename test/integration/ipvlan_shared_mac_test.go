// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// sharedMACEndpoint is one of a container's two ipvlan endpoints and the server and segment it must lease through.
type sharedMACEndpoint struct {
	network  string
	cidr     string
	lowerIdx int
	leases   func() string
	other    func() string
	ip       string
	clientID string
}

// ipvlanClientID is the option 61 the server records for an ipvlan endpoint: type 0x00 and the endpoint id's first 8 bytes (#219).
func ipvlanClientID(t *testing.T, endpointID string) string {
	t.Helper()
	b, err := hex.DecodeString(endpointID[:16])
	if err != nil {
		t.Fatalf("endpoint id %q is not hex: %v", endpointID, err)
	}
	return "00:" + colonHex(b)
}

// dnsmasqRow returns the expiry and client-id of the row for ip in a dnsmasq lease file.
func dnsmasqRow(leases, ip string) (expiry int64, clientID string, ok bool) {
	for _, line := range strings.Split(leases, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[2] != ip {
			continue
		}
		e, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return 0, "", false
		}
		return e, strings.ToLower(f[len(f)-1]), true
	}
	return 0, "", false
}

// connectAndAwaitIP attaches a running container to a network and returns the endpoint's id and address.
func connectAndAwaitIP(t *testing.T, ctx context.Context, cli *docker.Client, id, netName string) (string, string) {
	t.Helper()
	if err := cli.NetworkConnect(ctx, netName, id, nil); err != nil {
		t.Fatalf("NetworkConnect(%s, %s): %v", netName, id[:12], err)
	}
	return endpointIDAndAddr(t, ctx, cli, id, netName)
}

// endpointIDAndAddr waits for the container's endpoint on netName to report an address and returns its id and address.
func endpointIDAndAddr(t *testing.T, ctx context.Context, cli *docker.Client, id, netName string) (string, string) {
	t.Helper()
	addr, _ := awaitEndpoint(t, ctx, cli, id, netName)
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect(%s): %v", id[:12], err)
	}
	ep := ins.NetworkSettings.Networks[netName]
	if ep == nil || len(ep.EndpointID) < 16 {
		t.Fatalf("container %s has no endpoint id on %s", id[:12], netName)
	}
	return ep.EndpointID, addr
}

// assertEachLinkHoldsItsOwnLease checks, from inside the container, that the interface on each endpoint's lower link
// holds that endpoint's address and only addresses of its own segment, and that each server's row for the address
// names that endpoint's client-id while the other server saw it never.
func assertEachLinkHoldsItsOwnLease(t *testing.T, ctx context.Context, id string, eps []*sharedMACEndpoint) {
	t.Helper()
	ifaces := strings.Fields(harness.ExecOutput(t, ctx, id, "ls", "/sys/class/net"))
	for _, ep := range eps {
		_, subnet, _ := net.ParseCIDR(ep.cidr)
		var on []string
		for _, ifc := range ifaces {
			if ifc == "lo" {
				continue
			}
			iflink := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/"+ifc+"/iflink"))
			if iflink != strconv.Itoa(ep.lowerIdx) {
				continue
			}
			on = append(on, ifc)
			addrs := harness.ExecOutput(t, ctx, id, "ip", "-4", "-o", "addr", "show", "dev", ifc)
			if !strings.Contains(addrs, " "+ep.ip+"/") {
				t.Errorf("%s, the link on %s's segment, does not hold its address %s:\n%s", ifc, ep.network, ep.ip, addrs)
			}
			for _, f := range strings.Fields(addrs) {
				if ip, _, err := net.ParseCIDR(f); err == nil && ip.To4() != nil && !subnet.Contains(ip) {
					t.Errorf("%s, the link on %s's segment, holds %s from another segment: the other endpoint's "+
						"client runs on it (#1243):\n%s", ifc, ep.network, ip, addrs)
				}
			}
		}
		if len(on) != 1 {
			t.Errorf("the container has %d links on %s's segment (%v), want one", len(on), ep.network, on)
		}
		_, cid, ok := dnsmasqRow(ep.leases(), ep.ip)
		if !ok || cid != ep.clientID {
			t.Errorf("%s's server has no row for %s with client-id %s (row found %v, client-id %q):\n%s",
				ep.network, ep.ip, ep.clientID, ok, cid, ep.leases())
		}
		if strings.Contains(strings.ToLower(ep.other()), ep.clientID) {
			t.Errorf("the other segment's server leased to %s's client-id %s, so its client sent from the "+
				"wrong link (#1243):\n%s", ep.network, ep.clientID, ep.other())
		}
	}
}

// awaitRenewals waits until every endpoint's own server has extended its row past the expiry it held on entry; the
// client renews at T1, one minute into the fixture's two-minute lease, through the link it bound to (#1243).
func awaitRenewals(t *testing.T, eps []*sharedMACEndpoint, what string) {
	t.Helper()
	before := make([]int64, len(eps))
	for i, ep := range eps {
		// dnsmasq writes the row after the ACK, not with it (#905).
		e, ok, _ := harness.AwaitSettled(5*time.Second, 100*time.Millisecond,
			func() (int64, error) { e, _, _ := dnsmasqRow(ep.leases(), ep.ip); return e, nil },
			func(e int64) bool { return e != 0 })
		if !ok {
			t.Fatalf("%s: %s's server has no row for %s:\n%s", what, ep.network, ep.ip, ep.leases())
		}
		before[i] = e
	}
	deadline := time.Now().Add(100 * time.Second)
	for {
		pending := 0
		for i, ep := range eps {
			if e, _, ok := dnsmasqRow(ep.leases(), ep.ip); !ok || e <= before[i] {
				pending++
			}
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			for i, ep := range eps {
				e, cid, ok := dnsmasqRow(ep.leases(), ep.ip)
				if !ok || e <= before[i] {
					t.Errorf("%s: %s's server never renewed %s (expiry %d, then %d, client-id %q): its client "+
						"is not on its own link (#1243):\n%s", what, ep.network, ep.ip, before[i], e, cid, ep.leases())
				}
			}
			t.FailNow()
		}
		time.Sleep(time.Second)
	}
}

// TestIPvlan_TwoEndpointsSharingTheParentMACEachLeaseOnTheirOwnLink checks that a container on an ipvlan network on a
// parent and one on its vlan sub-interface, whose children both wear the parent's MAC, runs each client on its own
// link, in both attach orders and after a plugin restart (#1243).
func TestIPvlan_TwoEndpointsSharingTheParentMACEachLeaseOnTheirOwnLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)
	v := harness.StartVlan(t)

	const netU, netT = "dh-itest-ipv2-u", "dh-itest-ipv2-t"
	harness.CreateNetwork(t, ctx, netU, "ipvlan", map[string]string{"parent": harness.VlanParent})
	harness.CreateNetwork(t, ctx, netT, "ipvlan", map[string]string{"parent": harness.VlanParent, "vlan": harness.VlanID})

	parent, err := netlink.LinkByName(harness.VlanParent)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", harness.VlanParent, err)
	}
	sub, err := netlink.LinkByName(harness.VlanSubIf)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", harness.VlanSubIf, err)
	}
	if parent.Attrs().HardwareAddr.String() != sub.Attrs().HardwareAddr.String() {
		t.Fatalf("%s (%s) and %s (%s) differ in MAC, so their ipvlan children do not share one and this test "+
			"no longer covers #1243", harness.VlanParent, parent.Attrs().HardwareAddr, harness.VlanSubIf, sub.Attrs().HardwareAddr)
	}

	newEP := func(id, network string) *sharedMACEndpoint {
		ep := &sharedMACEndpoint{network: network}
		if network == netU {
			ep.cidr, ep.lowerIdx, ep.leases, ep.other = harness.VlanUntaggedCIDR, parent.Attrs().Index, v.UntaggedLeases, v.TaggedLeases
		} else {
			ep.cidr, ep.lowerIdx, ep.leases, ep.other = harness.VlanTaggedCIDR, sub.Attrs().Index, v.TaggedLeases, v.UntaggedLeases
		}
		var epID string
		epID, ep.ip = endpointIDAndAddr(t, ctx, cli, id, network)
		ep.clientID = ipvlanClientID(t, epID)
		return ep
	}

	// The first endpoint's link is in the container when the second one's client looks for its own.
	idUT, _, _ := harness.RunContainer(t, ctx, netU, "dh-itest-ipv2-ut")
	connectAndAwaitIP(t, ctx, cli, idUT, netT)
	idTU, _, _ := harness.RunContainer(t, ctx, netT, "dh-itest-ipv2-tu")
	connectAndAwaitIP(t, ctx, cli, idTU, netU)

	ctrs := map[string][]*sharedMACEndpoint{
		idUT: {newEP(idUT, netU), newEP(idUT, netT)},
		idTU: {newEP(idTU, netT), newEP(idTU, netU)},
	}
	var all []*sharedMACEndpoint
	for _, eps := range ctrs {
		all = append(all, eps...)
	}

	awaitRenewals(t, all, "after the attach")
	for id, eps := range ctrs {
		assertEachLinkHoldsItsOwnLease(t, ctx, id, eps)
	}

	// A restarted plugin finds each child again through the same lookup.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil &&
			!strings.Contains(err.Error(), "already enabled") {
			t.Logf("WARN: cleanup PluginEnable: %v", err)
		}
	})
	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 30*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	awaitRenewals(t, all, "after a plugin restart")
	for id, eps := range ctrs {
		assertEachLinkHoldsItsOwnLease(t, ctx, id, eps)
	}
}
