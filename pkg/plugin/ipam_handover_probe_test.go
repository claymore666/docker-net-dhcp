// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/wire"
)

func probeRecord(server, gateway string) lease.Record {
	l := lease.Lease{Addr: netip.MustParsePrefix("192.168.99.10/24"), ServerID: netip.MustParseAddr(server)}
	if gateway != "" {
		l.Gateway = netip.MustParseAddr(gateway)
	}
	return lease.Record{Family: lease.FamilyV4, Lease: l}
}

func TestHopAnswersProbe_OnlyAReplyFromTheNextHopCounts(t *testing.T) {
	hopMAC, _ := net.ParseMAC("02:00:00:00:00:fe")
	reply := func(from string, hw net.HardwareAddr, op wire.ARPOp) func(f *fakeARPLink, pkt *wire.ARPPacket) {
		return func(f *fakeARPLink, pkt *wire.ARPPacket) {
			f.inject(t, &wire.ARPPacket{Op: op, SenderHW: hw, SenderIP: netip.MustParseAddr(from),
				TargetHW: f.mac, TargetIP: netip.IPv4Unspecified()})
		}
	}
	for _, tc := range []struct {
		name    string
		rec     lease.Record
		onSend  func(f *fakeARPLink, pkt *wire.ARPPacket)
		openErr error
		want    bool
		probed  string
	}{
		{name: "the server answers", rec: probeRecord("192.168.99.1", ""),
			onSend: reply("192.168.99.1", hopMAC, wire.ARPReply), want: true, probed: "192.168.99.1"},
		{name: "nothing answers", rec: probeRecord("192.168.99.1", ""), probed: "192.168.99.1"},
		{name: "another host answers", rec: probeRecord("192.168.99.1", ""),
			onSend: reply("192.168.99.7", hopMAC, wire.ARPReply), probed: "192.168.99.1"},
		{name: "a request, not a reply", rec: probeRecord("192.168.99.1", ""),
			onSend: reply("192.168.99.1", hopMAC, wire.ARPRequest), probed: "192.168.99.1"},
		{name: "a broadcast sender MAC", rec: probeRecord("192.168.99.1", ""),
			onSend: reply("192.168.99.1", net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, wire.ARPReply),
			probed: "192.168.99.1"},
		{name: "a relayed server is reached through the gateway", rec: probeRecord("10.0.0.5", "192.168.99.254"),
			onSend: reply("192.168.99.254", hopMAC, wire.ARPReply), want: true, probed: "192.168.99.254"},
		{name: "a relayed server with no gateway", rec: probeRecord("10.0.0.5", "")},
		{name: "the link does not open", rec: probeRecord("192.168.99.1", ""), openErr: errors.New("EPERM")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := newFakeARPLink("02:00:00:00:00:02")
			link.onSend = tc.onSend
			prevOpen, prevBound := openARPLink, handOverProbeBound
			opened := 0
			openARPLink = func(string) (arpLink, error) {
				opened++
				if tc.openErr != nil {
					return nil, tc.openErr
				}
				return link, nil
			}
			handOverProbeBound = 50 * time.Millisecond
			t.Cleanup(func() { openARPLink, handOverProbeBound = prevOpen, prevBound })

			if got := hopAnswersProbe(tc.rec, "eth9"); got != tc.want {
				t.Fatalf("hopAnswersProbe = %v, want %v", got, tc.want)
			}
			if tc.probed == "" {
				if len(link.sent) != 0 {
					t.Fatalf("sent %d probes, want none", len(link.sent))
				}
				return
			}
			if len(link.sent) != 1 {
				t.Fatalf("sent %d probes, want one", len(link.sent))
			}
			p := link.sent[0]
			if p.Op != wire.ARPRequest || p.TargetIP.String() != tc.probed || p.SenderIP != netip.IPv4Unspecified() {
				t.Fatalf("probe %v asked for %v from %v, want an RFC 5227 probe for %s from 0.0.0.0",
					p.Op, p.TargetIP, p.SenderIP, tc.probed)
			}
			if !link.closed {
				t.Fatal("the ARP link was left open")
			}
		})
	}
}
