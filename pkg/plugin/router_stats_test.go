// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"math"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func TestAddRouterStats_EachDeltaReachesItsOwnCounter(t *testing.T) {
	p := &Plugin{}
	p.addRouterStats(dhcp.RouterStats{
		SolicitsSent:   11,
		AdvertsSeen:    22,
		AdvertsRefused: 33,
		OptionsIgnored: 44,
		EntriesDropped: 55,
		EntriesEvicted: 66,
	})
	for _, tc := range []struct {
		name string
		got  int32
		want int32
	}{
		{"router_solicits_sent", p.routerSolicitsSent.Load(), 11},
		{"router_adverts_seen", p.routerAdvertsSeen.Load(), 22},
		{"router_adverts_refused", p.routerAdvertsRefused.Load(), 33},
		{"router_advert_options_ignored", p.routerAdvertOptionsIgnored.Load(), 44},
		{"router_table_entries_dropped", p.routerTableEntriesDropped.Load(), 55},
		{"router_table_entries_evicted", p.routerTableEntriesEvicted.Load(), 66},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d; the fold is writing another counter", tc.name, tc.got, tc.want)
		}
	}
}

func TestAddRouterStats_EverySixDeltasAccumulate(t *testing.T) {
	p := &Plugin{}
	first := dhcp.RouterStats{
		SolicitsSent: 1, AdvertsSeen: 2, AdvertsRefused: 3,
		OptionsIgnored: 4, EntriesDropped: 5, EntriesEvicted: 6,
	}
	second := dhcp.RouterStats{
		SolicitsSent: 10, AdvertsSeen: 20, AdvertsRefused: 30,
		OptionsIgnored: 40, EntriesDropped: 50, EntriesEvicted: 60,
	}
	p.addRouterStats(first)
	p.addRouterStats(second)
	for _, tc := range []struct {
		name string
		got  int32
		want int32
	}{
		{"router_solicits_sent", p.routerSolicitsSent.Load(), 11},
		{"router_adverts_seen", p.routerAdvertsSeen.Load(), 22},
		{"router_adverts_refused", p.routerAdvertsRefused.Load(), 33},
		{"router_advert_options_ignored", p.routerAdvertOptionsIgnored.Load(), 44},
		{"router_table_entries_dropped", p.routerTableEntriesDropped.Load(), 55},
		{"router_table_entries_evicted", p.routerTableEntriesEvicted.Load(), 66},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d after two managers reported into it, want %d; a counter that "+
				"takes the last delta instead of the sum falls when a container stops",
				tc.name, tc.got, tc.want)
		}
	}
}

func TestAddRouterStats_SaturatesRatherThanGoingNegative(t *testing.T) {
	p := &Plugin{}
	p.addRouterStats(dhcp.RouterStats{AdvertsSeen: math.MaxInt32 + 1})
	if got := p.routerAdvertsSeen.Load(); got != math.MaxInt32 {
		t.Errorf("a delta above the surface's range left the counter at %d, want %d",
			got, int32(math.MaxInt32))
	}
}

func TestV6Wiring_ArmsTheRouterStatsCallbackInEveryMode(t *testing.T) {
	id6 := dhcp.Identity6{DUID: []byte{0, 4, 1, 2, 3, 4}, IAID: 0x11223344}
	for _, tc := range []struct {
		name string
		opts DHCPNetworkOptions
	}{
		{"dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp"}},
		{"ipv6=true alone", DHCPNetworkOptions{Bridge: "br0", IPv6: true}},
		{"slaac", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac"}},
		{"auto", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			var base dhcp.DHCPClientOptions
			if err := p.v6Wiring(&base, tc.opts, id6, "rec-1", "", "endpoint-1"); err != nil {
				t.Fatalf("v6Wiring: %v", err)
			}
			if base.OnRouterStats == nil {
				t.Fatal("no router-discovery callback on a client this plugin started; every " +
					"IPv6 mode solicits and listens, so this reads as a link with no router")
			}
			base.OnRouterStats(dhcp.RouterStats{AdvertsSeen: 1})
			if got := p.routerAdvertsSeen.Load(); got != 1 {
				t.Errorf("the callback left the counter at %d, want 1; it is pointing "+
					"somewhere other than this plugin's counter", got)
			}
		})
	}
}

func TestV6Wiring_ANilPluginArmsNoCallbackAndStillCarriesTheMode(t *testing.T) {
	var p *Plugin
	var base dhcp.DHCPClientOptions
	if err := p.v6Wiring(&base, DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac"},
		dhcp.Identity6{DUID: []byte{0, 4, 1}, IAID: 1}, "rec-1", "", "endpoint-1"); err != nil {
		t.Fatalf("v6Wiring: %v", err)
	}
	if base.OnRouterStats != nil {
		t.Error("a client with no plugin behind it was given a callback into one")
	}
	if base.Mode6 != proto.Mode6SLAAC {
		t.Errorf("Mode6 = %v, want slaac; the mode reaches the wire with no plugin", base.Mode6)
	}
}

func TestHealth_ServesTheRouterDiscoveryCounters(t *testing.T) {
	p := &Plugin{}
	p.addRouterStats(dhcp.RouterStats{
		SolicitsSent: 1, AdvertsSeen: 2, AdvertsRefused: 3,
		OptionsIgnored: 4, EntriesDropped: 5, EntriesEvicted: 6,
	})
	h := p.healthSnapshot()
	for _, tc := range []struct {
		name string
		got  int32
		want int32
	}{
		{"router_solicits_sent", h.RouterSolicitsSent, 1},
		{"router_adverts_seen", h.RouterAdvertsSeen, 2},
		{"router_adverts_refused", h.RouterAdvertsRefused, 3},
		{"router_advert_options_ignored", h.RouterAdvertOptionsIgnored, 4},
		{"router_table_entries_dropped", h.RouterTableEntriesDropped, 5},
		{"router_table_entries_evicted", h.RouterTableEntriesEvicted, 6},
	} {
		if tc.got != tc.want {
			t.Errorf("%s served %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}
