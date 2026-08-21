// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// TestRenewPhases_SkipPathsTouchNoKernelState pins the guard that each
// kernel-touching renew phase opens with. On a manager that never
// reached Start, netHandle and ctrLink are nil, so a guard evaluated
// one line too late is not a logic slip — it is a nil dereference that
// panics the plugin's event loop.
//
// This was previously unassertable: the guards lived inside renew's
// 200-line body, and TestRenew_LeaseChangedCounter could only lean on
// them (its comment says as much — "we leave them off so they don't
// try to dereference a nil m.netHandle") rather than check them. Each
// phase being its own method is what makes the check possible.
//
// A failure here shows up as a panic, not a t.Error.
func TestRenewPhases_SkipPathsTouchNoKernelState(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(m *dhcpManager) error
	}{
		{
			// PropagateMTU off, but the server did supply an MTU:
			// reading m.ctrLink.Attrs() before checking the opt-in
			// panics.
			name: "MTU propagation opted out",
			run: func(m *dhcpManager) error {
				m.propagateMTU(false, dhcp.Info{MTU: 1400})
				return nil
			},
		},
		{
			// Opted in, but the server sent no option 26.
			// dhcp-handler reports that as 0, and MTU 0 on a kernel
			// link is disallowed.
			name: "MTU propagation opted in with no option 26",
			run: func(m *dhcpManager) error {
				m.opts.PropagateMTU = true
				m.propagateMTU(false, dhcp.Info{MTU: 0})
				return nil
			},
		},
		{
			// Opted in, and the server supplied an MTU below the
			// range we will apply. The refusal must come BEFORE
			// m.ctrLink.Attrs(), like the two guards above -- and
			// with the bound removed this case dereferences nil and
			// panics, which is what makes it a check on the bound
			// rather than on the constant (#702).
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
			// DHCPv6 has no gateway option — the router advertises
			// itself — so the v6 arm must never reach netlink.
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
			// An operator-pinned gateway wins over the lease's.
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
			// Opted in, but the server supplied no servers: writing
			// an empty list would clobber the container's resolv.conf.
			name: "DNS propagation opted in with an empty server list",
			run: func(m *dhcpManager) error {
				m.opts.PropagateDNS = true
				m.propagateDNS(false, dhcp.Info{})
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Zero value: netHandle and ctrLink are nil, exactly as
			// they are before Start and after a failed attach.
			m := &dhcpManager{}
			if err := tc.run(m); err != nil {
				t.Fatalf("skip path returned an error: %v", err)
			}
		})
	}
}

// TestApplyAddressChange_NoOpWithoutAChange pins that the address
// phase leaves the link alone on the steady-state renewal — the
// overwhelmingly common case, and the one where an AddrReplace would
// churn the container's address for nothing.
//
// Same nil-dereference argument as above: a zero-value manager reaches
// netlink only if the no-change guard fails to fire.
func TestApplyAddressChange_NoOpWithoutAChange(t *testing.T) {
	addr, err := netlink.ParseAddr("192.168.0.10/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}

	t.Run("first bind has no previous address", func(t *testing.T) {
		m := &dhcpManager{}
		if err := m.applyAddressChange(false, addr); err != nil {
			t.Fatalf("applyAddressChange: %v", err)
		}
	})

	t.Run("renewal of the same address", func(t *testing.T) {
		m := &dhcpManager{}
		m.setLastIP(false, addr)
		if err := m.applyAddressChange(false, addr); err != nil {
			t.Fatalf("applyAddressChange: %v", err)
		}
	})

	t.Run("a v6 change does not consult the v4 lease", func(t *testing.T) {
		// Cross-family bleed would make every first v6 bind look
		// like a renumber, which is the busybox-IAID failure mode
		// #152 removed. lastIP is set for v4 only.
		m := &dhcpManager{plugin: &Plugin{}}
		m.setLastIP(false, addr)

		other, err := netlink.ParseAddr("192.168.0.11/24")
		if err != nil {
			t.Fatalf("ParseAddr: %v", err)
		}
		if err := m.applyAddressChange(true, other); err != nil {
			t.Fatalf("applyAddressChange: %v", err)
		}
		if got := m.plugin.leaseChanged.Load(); got != 0 {
			t.Errorf("leaseChanged = %d, want 0 (a first v6 bind is not a change)", got)
		}
	})
}

// TestLogObservedOptions_SilentUnlessSomethingWasObserved pins the
// "no noisy line per renewal" contract: a plain LAN offers none of
// these options, and this runs on every renewal of every endpoint.
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

	// One subtest per option so a field dropped from the emitter is
	// caught individually rather than masked by its neighbours.
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

// captureLog redirects the global logger for the duration of fn. The
// package logs through logrus' standard logger, and no test in this
// package runs in parallel, so swapping it is safe.
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

// TestHandleEvent_CountsDroppedOptionValues is the plugin half of #703:
// the filter runs in the dhcpcd hook process, so the only way its work
// reaches an operator is the count riding the event across the FIFO. A
// drop that leaves no trace is indistinguishable from an attack that was
// never attempted.
func TestHandleEvent_CountsDroppedOptionValues(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	// "nak" carries no lease data, so this touches no kernel state --
	// and it is the case that proves the count is folded in for every
	// event type, not just the lease-bearing ones.
	m.handleEvent(dhcp.Event{Type: "nak", UnsafeValuesDropped: 3}, false)

	if got := p.unsafeOptionValuesDropped.Load(); got != 3 {
		t.Errorf("unsafe_option_values_dropped = %d, want 3", got)
	}
}
