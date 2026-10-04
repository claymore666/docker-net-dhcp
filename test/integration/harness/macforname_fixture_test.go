// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import "testing"

// Every fixture parent pair shares one segment, so no two of their ends may share an address (#1147).
func TestMACForName_GivesEveryFixtureLinkItsOwnAddress(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{
		HostVeth, hostVethPeer, IpvlanParent, ipvlanParentPeer, VlanParent, vlanPeer, "dh-itest-pt", "dh-itest-ptp",
	} {
		mac := macForName(name).String()
		if other, ok := seen[mac]; ok {
			t.Errorf("%q and %q both get %s", name, other, mac)
		}
		seen[mac] = name
	}
}
