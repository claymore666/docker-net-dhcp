// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

// AddSegmentParent gives a test a parent of its own on the DHCP segment: a macvlan_mode=passthru child takes its parent
// alone, so on HostVeth it would refuse every later test's child (#905). The pair is removed at cleanup.
func AddSegmentParent(t *testing.T, name, peer string) string {
	t.Helper()
	for _, n := range []string{name, peer} {
		if l, err := netlink.LinkByName(n); err == nil {
			_ = netlink.LinkDel(l)
		}
	}
	segment, err := netlink.LinkByName(DHCPSegment)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", DHCPSegment, err)
	}
	if _, err := addParentVeth(segment, name, peer); err != nil {
		t.Fatalf("segment parent %s: %v", name, err)
	}
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(name); err == nil {
			if err := netlink.LinkDel(l); err != nil {
				t.Logf("WARN: LinkDel(%s): %v", name, err)
			}
		}
	})
	return name
}

// LinkMAC is a host link's hardware address, empty when the link is gone.
func LinkMAC(t *testing.T, name string) net.HardwareAddr {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", name, err)
	}
	return l.Attrs().HardwareAddr
}
