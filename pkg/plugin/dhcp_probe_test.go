// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func TestNewProbeMAC(t *testing.T) {
	mac, err := newProbeMAC()
	if err != nil {
		t.Fatalf("newProbeMAC: %v", err)
	}
	if len(mac) != 6 {
		t.Fatalf("MAC length = %d, want 6", len(mac))
	}
	if mac[0]&0x02 != 0x02 {
		t.Errorf("LAA bit not set on first byte (%#x); upstream may treat this as a manufacturer MAC", mac[0])
	}
	if mac[0]&0x01 != 0x00 {
		t.Errorf("multicast bit set on first byte (%#x); not a valid unicast address", mac[0])
	}

	m2, err := newProbeMAC()
	if err != nil {
		t.Fatalf("newProbeMAC #2: %v", err)
	}
	if mac.String() == m2.String() {
		t.Errorf("two consecutive newProbeMAC calls returned identical MAC %s — randomness broken", mac)
	}
}

func TestNewProbeLinkName(t *testing.T) {
	a, err := newProbeLinkName()
	if err != nil {
		t.Fatalf("newProbeLinkName: %v", err)
	}
	if !strings.HasPrefix(a, "dh-probe-") {
		t.Errorf("missing prefix; got %q", a)
	}
	// IFNAMSIZ is 16 including the NUL, so a link name holds at most 15 characters.
	if len(a) > 15 {
		t.Errorf("link name %q exceeds Linux IFNAMSIZ-1 (15) — kernel will refuse it", a)
	}

	b, _ := newProbeLinkName()
	if a == b {
		t.Errorf("two consecutive newProbeLinkName calls returned identical name %q — randomness broken", a)
	}
}

func TestPreflightProbeBudget_CoversOneLostDiscover(t *testing.T) {
	const (
		worstStartup      = 2 * time.Second
		discoverRetry     = 4 * time.Second // RFC 2131 section 4.1: 4 s, randomised by up to 1 s
		responseRoundTrip = 500 * time.Millisecond
	)
	if floor := worstStartup + discoverRetry + responseRoundTrip; preflightProbeBudget < floor {
		t.Errorf("preflightProbeBudget %v is below the lost-first-DISCOVER floor %v (see #307)", preflightProbeBudget, floor)
	}
}

// conflict_check=wait spends up to 7 s of the 8 s probe budget in RFC 5227 section 2.1,
// and a probe that inherited it failed validate_dhcp at 8.1 s (measured 2026-09-04).
func TestPreflightProbeOptions_RFC5227IsOffOnTheThrowawayLease(t *testing.T) {
	mac, err := newProbeMAC()
	if err != nil {
		t.Fatalf("newProbeMAC: %v", err)
	}

	o := preflightProbeOptions(mac, serverPolicy{})
	if o.ConflictMode != proto.ConflictOff {
		t.Errorf("the preflight probe runs conflict_check=%v; it must be %v. "+
			"Section 2.1 answers \"may I use this address\", and this address is released "+
			"milliseconds later on a link deleted with it.",
			o.ConflictMode, proto.ConflictOff)
	}

	window := dhcp.ConflictWindow(proto.DefaultACDParams())
	exchange := dhcp.AcquisitionWindow(proto.DefaultParams(nil)) - window
	if exchange+window <= preflightProbeBudget {
		t.Errorf("this test's premise has gone stale: the RFC 5227 window (%v) plus the "+
			"DHCP exchange the budget was sized for (%v) is now %v, which fits inside the "+
			"%v budget. Re-derive the reason before relaxing anything.",
			window, exchange, exchange+window, preflightProbeBudget)
	}

	pol := serverPolicy{Prefer: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}
	o = preflightProbeOptions(mac, pol)
	if len(o.AllowServers) != 1 || o.AllowServers[0] != "192.0.2.1" {
		t.Errorf("the preflight probe dropped the network's server allow list: %v", o.AllowServers)
	}
	if !bytesEqualMAC(o.MAC, mac) {
		t.Errorf("the preflight probe's MAC is %v, not the probe link's %v", o.MAC, mac)
	}
}

func bytesEqualMAC(a, b net.HardwareAddr) bool { return a.String() == b.String() }
