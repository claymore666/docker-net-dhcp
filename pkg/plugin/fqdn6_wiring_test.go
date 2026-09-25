// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// noAAAALines counts the entries that tell a register_dns network its container has no AAAA (#1029).
func noAAAALines(hook *logtest.Hook) int {
	n := 0
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "no AAAA") {
			n++
		}
	}
	return n
}

// A register_dns network on slaac, or on auto after a fallback, logs that it registers no AAAA (#1029).
func TestV6Wiring_SaysWhenRegisterDNSGetsNoAAAA(t *testing.T) {
	id6 := dhcp.Identity6{DUID: []byte{0, 4, 1, 2, 3, 4}, IAID: 0x11223344}
	cases := []struct {
		name        string
		opts        DHCPNetworkOptions
		atWiring    int
		atFallback  int
		hasFallback bool
	}{
		{"slaac with register_dns", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac", RegisterDNS: true}, 1, 0, false},
		{"slaac without register_dns", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac"}, 0, 0, false},
		{"auto with register_dns", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto", RegisterDNS: true}, 0, 1, true},
		{"auto without register_dns", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto"}, 0, 0, true},
		{"dhcp with register_dns", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", RegisterDNS: true}, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := logtest.NewLocal(log.StandardLogger())
			defer hook.Reset()
			p := &Plugin{}
			var base dhcp.DHCPClientOptions
			if err := p.v6Wiring(&base, tc.opts, id6, "rec-1", "", "0123456789abcdef"); err != nil {
				t.Fatalf("v6Wiring: %v", err)
			}
			if got := noAAAALines(hook); got != tc.atWiring {
				t.Errorf("%d no-AAAA lines at the attach, want %d: %v", got, tc.atWiring, messagesOf(hook.AllEntries()))
			}
			if (base.OnV6Fallback != nil) != tc.hasFallback {
				t.Fatalf("OnV6Fallback set = %v, want %v", base.OnV6Fallback != nil, tc.hasFallback)
			}
			if !tc.hasFallback {
				return
			}
			hook.Reset()
			base.OnV6Fallback(0)
			if got := noAAAALines(hook); got != 0 {
				t.Errorf("a zero fallback count logged %d no-AAAA lines, want none", got)
			}
			base.OnV6Fallback(1)
			if got := noAAAALines(hook); got != tc.atFallback {
				t.Errorf("%d no-AAAA lines at the fallback, want %d: %v", got, tc.atFallback,
					messagesOf(hook.AllEntries()))
			}
			if got := p.dhcpv6AutoFallbacks.Load(); got != 1 {
				t.Errorf("dhcpv6_auto_fallbacks = %d, want 1: the wrap must still run the fallback report", got)
			}
		})
	}
}
