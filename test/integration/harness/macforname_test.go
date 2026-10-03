// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"bytes"
	"testing"
)

func TestMACForName_IsAStableLocalUnicastAddress(t *testing.T) {
	for _, name := range []string{"dh-itest-host", "dh-itest-hostp", "dh-itest-pt", "a", ""} {
		mac := macForName(name)
		if len(mac) != 6 {
			t.Fatalf("macForName(%q) = %v, want 6 bytes", name, mac)
		}
		if mac[0]&0x02 == 0 {
			t.Errorf("macForName(%q) = %v: the locally administered bit is clear", name, mac)
		}
		if mac[0]&0x01 != 0 {
			t.Errorf("macForName(%q) = %v: the multicast bit is set, a kernel refuses it on a link", name, mac)
		}
		if again := macForName(name); !bytes.Equal(mac, again) {
			t.Errorf("macForName(%q) = %v then %v: it must not vary between calls", name, mac, again)
		}
	}
}

func TestMACForName_TwoNamesGetTwoAddresses(t *testing.T) {
	if a, b := macForName("dh-itest-host"), macForName("dh-itest-hostp"); bytes.Equal(a, b) {
		t.Errorf("a veth and its peer both get %v", a)
	}
}
