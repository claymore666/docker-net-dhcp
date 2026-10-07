// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net/netip"
	"testing"
)

// TestMustMAC_EmptyMeansNoMAC pins "" as unset: a table row with no --mac-address panicked MustMAC (#178).
func TestMustMAC_EmptyMeansNoMAC(t *testing.T) {
	if hw := MustMAC(""); hw != nil {
		t.Fatalf("MustMAC(\"\") = %v, want nil", hw)
	}
	if got := MustMAC("02:00:00:10:36:05").String(); got != "02:00:00:10:36:05" {
		t.Fatalf("MustMAC round trip = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustMAC(\"zz\") did not panic")
		}
	}()
	MustMAC("zz")
}

func TestAddrAndPrefixString_UnsetIsEmpty(t *testing.T) {
	if got := AddrString(netip.Addr{}); got != "" {
		t.Fatalf("AddrString(zero) = %q", got)
	}
	if got := PrefixString(netip.Prefix{}); got != "" {
		t.Fatalf("PrefixString(zero) = %q", got)
	}
	if got := AddrString(netip.MustParseAddr("192.0.2.1")); got != "192.0.2.1" {
		t.Fatalf("AddrString = %q", got)
	}
}
