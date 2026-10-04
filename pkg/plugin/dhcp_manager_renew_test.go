// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func TestRenewPhases_SkipPathsTouchNoKernelState(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(m *dhcpManager) error
	}{
		{
			name: "MTU propagation opted out",
			run: func(m *dhcpManager) error {
				m.propagateMTU(false, dhcp.Info{MTU: 1400})
				return nil
			},
		},
		{
			name: "MTU propagation opted in with no option 26",
			run: func(m *dhcpManager) error {
				m.opts.PropagateMTU = true
				m.propagateMTU(false, dhcp.Info{MTU: 0})
				return nil
			},
		},
		{
			name: "MTU propagation opted in with an out-of-range option 26",
			run: func(m *dhcpManager) error {
				m.opts.PropagateMTU = true
				m.plugin = &Plugin{}
				m.propagateMTU(false, dhcp.Info{MTU: 68})
				if got := m.plugin.mtuRefused.Load(); got != 1 {
					t.Errorf("mtu_refused = %d, want 1", got)
				}
				return nil
			},
		},
		{
			// DHCPv6 has no gateway option; the router advertises itself (RFC 4861).
			name: "default route on the v6 path",
			run: func(m *dhcpManager) error {
				return m.reconcileDefaultRoute(true, dhcp.Info{Gateway: "192.168.0.1"})
			},
		},
		{
			name: "default route with no gateway offered",
			run: func(m *dhcpManager) error {
				return m.reconcileDefaultRoute(false, dhcp.Info{})
			},
		},
		{
			name: "default route with an operator override",
			run: func(m *dhcpManager) error {
				m.opts.Gateway = "192.168.0.254"
				return m.reconcileDefaultRoute(false, dhcp.Info{Gateway: "192.168.0.1"})
			},
		},
		{
			name: "DNS propagation opted out",
			run: func(m *dhcpManager) error {
				m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.168.0.1"}})
				return nil
			},
		},
		{
			name: "DNS propagation opted in with an empty server list",
			run: func(m *dhcpManager) error {
				m.opts.PropagateDNS = true
				m.propagateDNS(false, dhcp.Info{})
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &dhcpManager{}
			if err := tc.run(m); err != nil {
				t.Fatalf("skip path returned an error: %v", err)
			}
		})
	}
}

func TestApplyAddressChange_NoOpWithoutAChange(t *testing.T) {
	addr, err := netlink.ParseAddr("192.168.0.10/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}

	t.Run("first bind has no previous address", func(t *testing.T) {
		m := &dhcpManager{}
		if err := m.applyAddressChange(false, addr, dhcp.Info{}); err != nil {
			t.Fatalf("applyAddressChange: %v", err)
		}
	})

	t.Run("renewal of the same address", func(t *testing.T) {
		m := &dhcpManager{}
		m.setLastIP(false, addr)
		if err := m.applyAddressChange(false, addr, dhcp.Info{}); err != nil {
			t.Fatalf("applyAddressChange: %v", err)
		}
	})

	t.Run("a v6 change does not consult the v4 lease", func(t *testing.T) {
		m := &dhcpManager{plugin: &Plugin{}}
		m.setLastIP(false, addr)

		other, err := netlink.ParseAddr("192.168.0.11/24")
		if err != nil {
			t.Fatalf("ParseAddr: %v", err)
		}
		if err := m.applyAddressChange(true, other, dhcp.Info{}); err != nil {
			t.Fatalf("applyAddressChange: %v", err)
		}
		if got := m.plugin.leaseChangedV4.Load(); got != 0 {
			t.Errorf("leaseChangedV4 = %d, want 0 (a first v6 bind is not a change)", got)
		}
	})
}

func TestLogObservedOptions_SilentUnlessSomethingWasObserved(t *testing.T) {
	t.Run("plain lease logs nothing", func(t *testing.T) {
		out := captureLog(t, func() {
			(&dhcpManager{}).logObservedOptions(false, dhcp.Info{
				IP:      "192.168.0.10/24",
				Gateway: "192.168.0.1",
			})
		})
		if out != "" {
			t.Errorf("logged %q, want nothing for a lease carrying no observe-only options", out)
		}
	})

	for _, tc := range []struct {
		name  string
		info  dhcp.Info
		field string
	}{
		{"ntp", dhcp.Info{NTPServers: []string{"192.168.0.1"}}, "ntp"},
		{"tftp", dhcp.Info{TFTPServer: "192.168.0.2"}, "tftp"},
		{"bootfile", dhcp.Info{BootFile: "pxelinux.0"}, "bootfile"},
		{"search", dhcp.Info{SearchList: []string{"lan"}}, "search"},
		{"wpad", dhcp.Info{WPAD: "http://wpad/wpad.dat"}, "wpad"},
		{"posix timezone", dhcp.Info{PosixTimezone: "CET-1CEST,M3.5.0,M10.5.0/3"}, "posix_tz"},
		{"tzdb timezone", dhcp.Info{TZDBTimezone: "Europe/Berlin"}, "tzdb_tz"},
		{"time offset", dhcp.Info{TimeOffset: "3600"}, "time_offset"},
		{"vendor option 43", dhcp.Info{VendorSpecific: "0104c0a86301"}, "vendor_43"},
		{"vendor option 125", dhcp.Info{VendorIdentifying: []dhcp.VendorBlock{{Enterprise: 9, Data: "aabb"}}}, "vendor_125"},
		{"vendor option 17", dhcp.Info{VendorInformation: []dhcp.VendorBlock{{Enterprise: 9, Data: "00010002"}}}, "vendor_17"},
	} {
		t.Run(tc.name+" alone triggers the line and is named in it", func(t *testing.T) {
			out := captureLog(t, func() {
				(&dhcpManager{}).logObservedOptions(false, tc.info)
			})
			if !strings.Contains(out, "DHCP options received") {
				t.Fatalf("logged %q, want the observed-options line", out)
			}
			if !strings.Contains(out, tc.field) {
				t.Errorf("logged %q, want it to name field %q", out, tc.field)
			}
		})
	}
}

func TestLogObservedOptions_AnotherOptionsLineCarriesNoVendorField(t *testing.T) {
	out := captureLog(t, func() {
		(&dhcpManager{}).logObservedOptions(false, dhcp.Info{NTPServers: []string{"192.168.0.1"}})
	})
	if !strings.Contains(out, "DHCP options received") {
		t.Fatalf("logged %q, want the observed-options line", out)
	}
	for _, key := range []string{"vendor_43", "vendor_125", "vendor_17"} {
		if strings.Contains(out, key) {
			t.Errorf("logged %q, want no %s on a lease that carried no such option", out, key)
		}
	}
}

func TestLogObservedOptions_NamesTheVendorBlobsByOption(t *testing.T) {
	out := captureLog(t, func() {
		(&dhcpManager{}).logObservedOptions(false, dhcp.Info{
			VendorSpecific: "0104c0a86301",
			VendorIdentifying: []dhcp.VendorBlock{
				{Enterprise: 9, Data: "aabb"},
				{Enterprise: 3561, Data: ""},
			},
		})
	})
	if !strings.Contains(out, "vendor_43=0104c0a86301") {
		t.Errorf("logged %q, want vendor_43=0104c0a86301", out)
	}
	if !strings.Contains(out, `vendor_125="[9:aabb 3561:]"`) {
		t.Errorf("logged %q, want vendor_125 with both blocks in wire order", out)
	}
	if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
		t.Errorf("one observation logged as several lines: %q", out)
	}
}

func TestLogObservedOptions_NamesOption17BlocksByEnterpriseInWireOrder(t *testing.T) {
	out := captureLog(t, func() {
		(&dhcpManager{}).logObservedOptions(true, dhcp.Info{
			VendorInformation: []dhcp.VendorBlock{
				{Enterprise: 9, Data: "00010002aabb"},
				{Enterprise: 3561, Data: ""},
				{Enterprise: 66051, Data: "cc"},
			},
		})
	})
	if !strings.Contains(out, `vendor_17="[9:00010002aabb 3561: 66051:cc]"`) {
		t.Errorf("logged %q, want vendor_17 with all three instances in wire order", out)
	}
	for _, key := range []string{"vendor_43", "vendor_125"} {
		if strings.Contains(out, key) {
			t.Errorf("logged %q, want no %s on a lease that carried only option 17", out, key)
		}
	}
	if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
		t.Errorf("one observation logged as several lines: %q", out)
	}
}

func captureLog(t *testing.T, fn func()) string {
	t.Helper()

	std := log.StandardLogger()
	prevOut, prevLevel := std.Out, std.GetLevel()
	t.Cleanup(func() {
		std.Out = prevOut
		std.SetLevel(prevLevel)
	})

	var buf bytes.Buffer
	std.Out = &buf
	std.SetLevel(log.InfoLevel)
	fn()
	return buf.String()
}

func TestHandleEvent_CountsDroppedOptionValues(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	m.handleEvent(dhcp.Event{Type: "nak", UnsafeValuesDropped: 3}, false)

	if got := p.unsafeOptionValuesDropped.Load(); got != 3 {
		t.Errorf("unsafe_option_values_dropped = %d, want 3", got)
	}
}
