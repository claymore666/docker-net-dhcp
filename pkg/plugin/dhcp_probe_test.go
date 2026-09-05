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

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// TestNewProbeMAC pins the LAA + unicast bit semantics. Stable
// even though the rest of the bytes are random — the constraint is
// what avoids collision with any manufacturer-assigned MAC on the
// upstream's reservation table.
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

	// Two consecutive calls must produce different MACs (else the
	// rand source is broken and probes on different runs would
	// collide in the dnsmasq lease table).
	m2, err := newProbeMAC()
	if err != nil {
		t.Fatalf("newProbeMAC #2: %v", err)
	}
	if mac.String() == m2.String() {
		t.Errorf("two consecutive newProbeMAC calls returned identical MAC %s — randomness broken", mac)
	}
}

// TestNewProbeLinkName guards uniqueness + the dh-probe- prefix that
// makes orphans easy to spot in `ip link` output if a probe ever
// fails to clean up.
func TestNewProbeLinkName(t *testing.T) {
	a, err := newProbeLinkName()
	if err != nil {
		t.Fatalf("newProbeLinkName: %v", err)
	}
	if !strings.HasPrefix(a, "dh-probe-") {
		t.Errorf("missing prefix; got %q", a)
	}
	// Linux's IFNAMSIZ is 16 (including null terminator) → max 15
	// printable chars. dh-probe- (9) + 8 hex = 17. We're 2 over the
	// limit and need to rely on the kernel truncating, OR we use a
	// shorter random suffix. Pin the length here so a refactor
	// doesn't regress past the limit silently.
	if len(a) > 15 {
		t.Errorf("link name %q exceeds Linux IFNAMSIZ-1 (15) — kernel will refuse it", a)
	}

	b, _ := newProbeLinkName()
	if a == b {
		t.Errorf("two consecutive newProbeLinkName calls returned identical name %q — randomness broken", a)
	}
}

// TestPreflightProbeBudget_CoversOneLostDiscover pins the budget
// against the arithmetic that sized it (#307): dhcpcd startup on a
// slow or virtualized host (~2s) + a lost first DISCOVER retransmitted
// after dhcpcd's jittered ~4s discover interval + response and
// handler round-trip (~0.5s). The 5s value this replaced satisfied
// the same "one retry must fit" intent only with subsecond startup
// and produced false "no DHCP OFFER" errors against live servers.
// Lowering the budget below this floor needs #307-grade evidence,
// not a tidy round number.
func TestPreflightProbeBudget_CoversOneLostDiscover(t *testing.T) {
	const (
		worstStartup      = 2 * time.Second
		discoverRetry     = 4 * time.Second // dhcpcd default, jittered
		responseRoundTrip = 500 * time.Millisecond
	)
	if floor := worstStartup + discoverRetry + responseRoundTrip; preflightProbeBudget < floor {
		t.Errorf("preflightProbeBudget %v is below the lost-first-DISCOVER floor %v (see #307)", preflightProbeBudget, floor)
	}
}

// TestPreflightProbeOptions_RFC5227IsOffOnTheThrowawayLease pins the
// one field in the preflight client whose wrong value is invisible
// everywhere except against a working DHCP server.
//
// preflightProbeBudget is 8s. conflict_check=wait spends up to 7.0s of
// it inside RFC 5227 section 2.1 (PROBE_WAIT 1s + two intervals of up
// to PROBE_MAX 2s + ANNOUNCE_WAIT 2s), so a probe that inherits the
// network's mode fails `docker network create -o validate_dhcp=true`
// against a server that answered correctly. MEASURED on the 2.x lane
// 2026-09-04 at 8.1s.
//
// The mode is asserted against proto.ConflictOff, and separately
// against the arithmetic, so that a future change to either the budget
// or the RFC schedule that reintroduces the overlap goes red here
// rather than on the lane.
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

	// Why this is not merely tidy: the window does not fit in the
	// budget ALONGSIDE the exchange the budget was sized for. The
	// budget covers a lost first DISCOVER and its jittered
	// retransmission (#307), which is exactly the difference between
	// dhcp.AcquisitionWindow and dhcp.ConflictWindow, so the two terms
	// below are derived from the library's constants rather than read
	// off this file's own comment.
	window := dhcp.ConflictWindow(proto.DefaultACDParams())
	exchange := dhcp.AcquisitionWindow(proto.DefaultParams(nil)) - window
	if exchange+window <= preflightProbeBudget {
		t.Errorf("this test's premise has gone stale: the RFC 5227 window (%v) plus the "+
			"DHCP exchange the budget was sized for (%v) is now %v, which fits inside the "+
			"%v budget. Re-derive the reason before relaxing anything.",
			window, exchange, exchange+window, preflightProbeBudget)
	}

	// The probe still honours the network's server policy: a server
	// this network will never lease from is not an answer to "is
	// anyone listening?" (#111, #669). Asserted here so the extraction
	// cannot quietly drop it.
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
