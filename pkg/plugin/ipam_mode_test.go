// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"strings"
	"testing"
)

func TestIpamRefuseIPvlan_NamesTheJoinTimeMACSetAsTheCause(t *testing.T) {
	err := ipamRefuseIPvlan(ModeIPvlan)
	if err == nil {
		t.Fatal("ipvlan was accepted in IPAM mode")
	}
	msg := err.Error()
	for _, want := range []string{
		"sets it on the container's interface at start",
		"an ipvlan interface cannot change its MAC",
		"every container would fail to start",
		"#949 tracks the engine change",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal is %q and does not say %q. The cause measured on #949 is that "+
				"Docker sets the MAC it generated for the IPAM driver on the container link at "+
				"start, and an ipvlan link refuses any MAC set, even its own", msg, want)
		}
	}
	if strings.Contains(msg, "share the parent's MAC") {
		t.Errorf("the refusal %q still gives the shared parent MAC as the cause. A per-endpoint "+
			"MAC used only as the DHCP identity would be compatible with that; the MAC set "+
			"at start is what fails (#949)", msg)
	}
}
