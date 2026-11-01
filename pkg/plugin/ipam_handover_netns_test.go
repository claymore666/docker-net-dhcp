// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"testing"

	"github.com/claymore666/dhcp-golib/runtime"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// The CI fixture's dnsmasq held its address in the plugin's namespace: the release left bound to the parent, the ARP
// for the server went unanswered, the write returned nil and the address moved (run 41157408536, #1416).
func TestIPAMHandOver_AServerInThePluginsNamespaceIsNotHandedOver(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p, _, _, _ := h1Fixture(t)
	removed, restarted := f0MAC(0x01), f0MAC(0x02)
	old := f0Tombstone(t, p, removed, f0Addr)
	nlLinkByName, nlAddrList, rtSendRelease = netlink.LinkByName, netlink.AddrList, runtime.SendRelease

	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	addLink(t, h, &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "hovp0"}, PeerName: "hovs0"})
	for name, cidr := range map[string]string{"hovp0": "192.168.99.2/24", "hovs0": "192.168.99.1/24"} {
		link, err := h.LinkByName(name)
		if err != nil {
			t.Fatalf("LinkByName %s: %v", name, err)
		}
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatalf("ParseAddr %s: %v", cidr, err)
		}
		if err := h.AddrAdd(link, addr); err != nil {
			t.Fatalf("AddrAdd %s: %v", name, err)
		}
		if err := h.LinkSetUp(link); err != nil {
			t.Fatalf("LinkSetUp %s: %v", name, err)
		}
	}

	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	prior, ok := rb.ByID(old)
	if !ok {
		t.Fatalf("the tombstone %q is not in the journal", old)
	}
	claim := ipamClaim{id: old, addr: "192.168.99.10", prior: prior}
	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "hovp0"}
	if fresh := p.ipamHandOver(ipamTestNetwork, claim, opts, restarted, dhcp.ClientIdentity(restarted)); fresh != "" {
		t.Fatalf("the hand-over opened record %q for a server at %v on this host's own link; the release cannot "+
			"reach it, so the exchange must keep the previous client id", fresh, prior.Lease.ServerID)
	}
	if got := p.releasesSentV4.Load(); got != 0 {
		t.Fatalf("releases_sent = %d, want 0", got)
	}
}
