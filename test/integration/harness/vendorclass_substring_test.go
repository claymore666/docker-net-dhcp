// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"strings"
	"testing"
)

// dnsmasq's --dhcp-vendorclass matches by substring, so a class that contains another class gets both tags (#1030).
func TestVendorClassesAreNotSubstringsOfEachOther(t *testing.T) {
	classes := map[string]string{
		"TestVendorClass":             TestVendorClass,
		"TestClasslessVendorClass":    TestClasslessVendorClass,
		"TestClassless249VendorClass": TestClassless249VendorClass,
	}
	for an, a := range classes {
		for bn, b := range classes {
			if an != bn && strings.Contains(a, b) {
				t.Errorf("%s %q contains %s %q: dnsmasq would tag a %s client with both", an, a, bn, b, an)
			}
		}
	}
}
