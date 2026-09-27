// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func macRebindUntouched(t *testing.T, p *Plugin, id string, chaddr net.HardwareAddr) {
	t.Helper()
	rec := f0Rec(t, p, id)
	if rec.Phase != lease.PhaseRetained || !bytes.Equal(rec.CHAddr, chaddr) {
		t.Errorf("record %s is %v on %v, want still retained on %v: a request under another MAC took it",
			id, rec.Phase, net.HardwareAddr(rec.CHAddr), chaddr)
	}
}

func macRebindAmbiguous(t *testing.T, p *Plugin, want int32) {
	t.Helper()
	if n := p.ipamRebindAmbiguous.Load(); n != want {
		t.Errorf("ipam_rebind_ambiguous = %d, want %d", n, want)
	}
}

func macRebindMinted(mac net.HardwareAddr) []byte {
	return dhcp.ClientIdentity(resolveClientID(f0Options(), "", mac))
}

func macRebindMintedIf(requireMAC bool, mac net.HardwareAddr) []byte {
	if requireMAC {
		return macRebindMinted(mac)
	}
	return nil
}

func TestIpamRebindCandidate_TheRequestingMACDecides(t *testing.T) {
	own, neighbour, other, fresh := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03), f0MAC(0x04)

	for _, requireMAC := range []bool{false, true} {
		name := "require_mac=false"
		if requireMAC {
			name = "require_mac=true"
		}
		t.Run(name+"/the own tombstone wins over a neighbour's and is not ambiguous", func(t *testing.T) {
			p, _, _, _ := f0Fixture(t)
			mine := f0Tombstone(t, p, own, f0Addr)
			theirs := f0Tombstone(t, p, neighbour, f0Addr2)
			gotID, gotAddr, gotIdent, _ := p.ipamRebindCandidate(ipamTestNetwork, own, requireMAC, macRebindMintedIf(requireMAC, own))
			if gotID != mine || gotAddr != "192.168.99.10" || !bytes.Equal(gotIdent, dhcp.ClientIdentity(own)) {
				t.Fatalf("re-bound (%q, %q, %x), want the requesting MAC's own (%q, 192.168.99.10)",
					gotID, gotAddr, gotIdent, mine)
			}
			macRebindUntouched(t, p, theirs, neighbour)
			macRebindAmbiguous(t, p, 0)
		})

		t.Run(name+"/two tombstones of the requesting MAC are counted and neither is taken", func(t *testing.T) {
			p, _, _, _ := f0Fixture(t)
			first := f0Tombstone(t, p, own, f0Addr)
			second := f0Tombstone(t, p, own, f0Addr2)
			f0Tombstone(t, p, neighbour, "192.168.99.12/24")
			if gotID, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, own, requireMAC, macRebindMintedIf(requireMAC, own)); gotID != "" {
				t.Fatalf("re-bound %q out of two records of one MAC; nothing tells them apart", gotID)
			}
			macRebindUntouched(t, p, first, own)
			macRebindUntouched(t, p, second, own)
			macRebindAmbiguous(t, p, 1)
		})

		t.Run(name+"/a tombstone of the requesting MAC a running endpoint holds is not taken", func(t *testing.T) {
			p, _, _, _ := f0Fixture(t)
			held := f0Tombstone(t, p, own, f0Addr)
			theirs := f0Tombstone(t, p, neighbour, f0Addr2)
			p.rememberEndpoint("still-running", endpointFingerprint{MAC: own.String(), IPv4: "192.168.99.10"}, dhcpHostname{})
			want := theirs
			if requireMAC {
				want = ""
			}
			if gotID, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, own, requireMAC, macRebindMintedIf(requireMAC, own)); gotID != want {
				t.Fatalf("re-bound %q, want %q: a held record is no candidate, so the MAC finds none among the rest",
					gotID, want)
			}
			macRebindUntouched(t, p, held, own)
			macRebindAmbiguous(t, p, 0)
		})
	}

	t.Run("require_mac=true/a single neighbour tombstone is not taken", func(t *testing.T) {
		p, _, _, _ := f0Fixture(t)
		theirs := f0Tombstone(t, p, neighbour, f0Addr)
		gotID, gotAddr, gotIdent, got6 := p.ipamRebindCandidate(ipamTestNetwork, own, true, macRebindMinted(own))
		if gotID != "" || gotAddr != "" || gotIdent != nil || got6 != "" {
			t.Fatalf("re-bound (%q, %q, %x, %q), a neighbour's identity, on a network where every MAC is "+
				"the user's", gotID, gotAddr, gotIdent, got6)
		}
		macRebindUntouched(t, p, theirs, neighbour)
		macRebindAmbiguous(t, p, 0)
	})

	t.Run("require_mac=true/several neighbour tombstones are not an ambiguity", func(t *testing.T) {
		p, _, _, _ := f0Fixture(t)
		f0Tombstone(t, p, neighbour, f0Addr)
		f0Tombstone(t, p, other, f0Addr2)
		if gotID, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, own, true, macRebindMinted(own)); gotID != "" {
			t.Fatalf("re-bound %q, a neighbour's", gotID)
		}
		macRebindAmbiguous(t, p, 0)
	})

	t.Run("require_mac=true/an own-MAC tombstone carrying a neighbour's identity is not taken", func(t *testing.T) {
		p, _, _, journal := f0Fixture(t)
		carried := f0Tombstone(t, p, neighbour, f0Addr)
		if gotID, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, own, false, nil); gotID != carried {
			t.Fatalf("setup: re-bound %q, want %q handed to the pinned MAC as an earlier build did", gotID, carried)
		}
		p.ipamGiveUpAttempt(carried, true, time.Now())
		f0Reopen(t, p, journal, "proc-2")
		if rec := f0Rec(t, p, carried); !bytes.Equal(rec.CHAddr, own) || !bytes.Equal(rec.Identity, macRebindMinted(neighbour)) {
			t.Fatalf("setup: record is on %v under %x, want the pinned MAC under the neighbour's identity",
				net.HardwareAddr(rec.CHAddr), rec.Identity)
		}
		for _, mac := range []net.HardwareAddr{own, neighbour} {
			gotID, _, gotIdent, _ := p.ipamRebindCandidate(ipamTestNetwork, mac, true, macRebindMinted(mac))
			if gotID != "" {
				t.Fatalf("%v re-bound %q under %x; each pinned MAC must lease under its own client id", mac, gotID, gotIdent)
			}
		}
		macRebindUntouched(t, p, carried, own)
		macRebindAmbiguous(t, p, 0)
	})

	t.Run("require_mac=false/a pinned MAC keeps a carried identity, the documented plain-network limit", func(t *testing.T) {
		p, _, _, _ := f0Fixture(t)
		carried := f0Tombstone(t, p, neighbour, f0Addr)
		if gotID, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, own, false, nil); gotID != carried {
			t.Fatalf("setup: re-bound %q, want %q", gotID, carried)
		}
		p.ipamGiveUpAttempt(carried, true, time.Now())
		f0Tombstone(t, p, f0MAC(0x03), f0Addr2)
		if gotID, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, own, false, nil); gotID != carried {
			t.Fatalf("re-bound %q, want the MAC's own record %q beside another tombstone", gotID, carried)
		}
		macRebindAmbiguous(t, p, 0)
	})

	t.Run("require_mac=false/an unpinned restart still claims the single tombstone", func(t *testing.T) {
		p, _, _, _ := f0Fixture(t)
		id := f0Tombstone(t, p, neighbour, f0Addr)
		gotID, gotAddr, gotIdent, _ := p.ipamRebindCandidate(ipamTestNetwork, fresh, false, nil)
		if gotID != id || gotAddr != "192.168.99.10" || !bytes.Equal(gotIdent, dhcp.ClientIdentity(neighbour)) {
			t.Fatalf("re-bound (%q, %q, %x), want (%q, 192.168.99.10): a restart comes back under a MAC "+
				"Docker minted fresh, and this is the normal case", gotID, gotAddr, gotIdent, id)
		}
		macRebindAmbiguous(t, p, 0)
	})

	t.Run("require_mac=false/two neighbour tombstones and no MAC match are still ambiguous", func(t *testing.T) {
		p, _, _, _ := f0Fixture(t)
		f0Tombstone(t, p, neighbour, f0Addr)
		f0Tombstone(t, p, other, f0Addr2)
		if gotID, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, fresh, false, nil); gotID != "" {
			t.Fatalf("re-bound %q with two candidates and no MAC match", gotID)
		}
		macRebindAmbiguous(t, p, 1)
	})
}

func TestIpamRebindCandidate_TheV6TwinFollowsTheMACChosenV4Record(t *testing.T) {
	own, neighbour := f0MAC(0x01), f0MAC(0x02)

	t.Run("the own pair wins over a neighbour's v4 tombstone", func(t *testing.T) {
		p, _, _, _ := f0Fixture(t)
		mine4 := f0Tombstone(t, p, own, f0Addr)
		mine6 := s2Tombstone6(t, p, own, s2Addr6)
		theirs := f0Tombstone(t, p, neighbour, f0Addr2)
		got4, _, _, got6 := p.ipamRebindCandidate(ipamTestNetwork, own, false, nil)
		if got4 != mine4 || got6 != mine6 {
			t.Fatalf("re-bound (%q, %q), want the own pair (%q, %q)", got4, got6, mine4, mine6)
		}
		macRebindUntouched(t, p, theirs, neighbour)
		macRebindAmbiguous(t, p, 0)
	})

	t.Run("require_mac=true, an own v6 tombstone does not pull a neighbour's v4 one", func(t *testing.T) {
		p, _, _, _ := f0Fixture(t)
		theirs := f0Tombstone(t, p, neighbour, f0Addr)
		mine6 := s2Tombstone6(t, p, own, s2Addr6)
		got4, _, _, got6 := p.ipamRebindCandidate(ipamTestNetwork, own, true, macRebindMinted(own))
		if got4 != "" || got6 != "" {
			t.Fatalf("re-bound (%q, %q); the only v4 candidate is a neighbour's", got4, got6)
		}
		macRebindUntouched(t, p, theirs, neighbour)
		macRebindUntouched(t, p, mine6, own)
	})

	t.Run("require_mac=false, a neighbour's v4 tombstone does not pair the own v6 one", func(t *testing.T) {
		p, _, _, _ := f0Fixture(t)
		theirs := f0Tombstone(t, p, neighbour, f0Addr)
		mine6 := s2Tombstone6(t, p, own, s2Addr6)
		got4, _, _, got6 := p.ipamRebindCandidate(ipamTestNetwork, own, false, nil)
		if got4 != theirs || got6 != "" {
			t.Fatalf("re-bound (%q, %q), want (%q, none)", got4, got6, theirs)
		}
		macRebindUntouched(t, p, mine6, own)
	})
}

// The exchange's client id is what the server files the lease under, so the wiring from the stored network's
// require_mac is read there and not from ipamRebindCandidate's return (#1118).
func TestIpamReserveAddress_RequireMACSendsTheOwnClientIDBesideANeighboursTombstone(t *testing.T) {
	own, neighbour := f0MAC(0x01), f0MAC(0x02)

	for _, tc := range []struct {
		name       string
		requireMAC bool
		wantRebind bool
	}{
		{"require_mac=true", true, false},
		{"require_mac=false", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, _, journal := f0Fixture(t)
			s2ReserveLink(t)
			theirs := f0Tombstone(t, p, neighbour, f0Addr)

			var sent []byte
			prev := dhcpGetIP
			dhcpGetIP = func(_ context.Context, _ string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
				sent = append([]byte(nil), o.ClientID...)
				return dhcp.Info{IP: f0Addr2, Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
			}
			t.Cleanup(func() { dhcpGetIP = prev })

			opts := f0Options()
			opts.RequireMAC = tc.requireMAC
			from := len(s2Lines(t, journal))
			res, err := s2Reserve(t, p, b, opts, own)
			if err != nil {
				t.Fatalf("the reserve failed: %v", err)
			}

			ownID := resolveClientID(opts, "", own)
			if tc.wantRebind {
				if !bytes.Equal(sent, neighbour) || res.record != theirs {
					t.Fatalf("sent client id %x under record %q, want the single tombstone's %x under %q",
						sent, res.record, []byte(neighbour), theirs)
				}
				return
			}
			if !bytes.Equal(sent, ownID) {
				t.Fatalf("sent client id %x, want the requesting MAC's own %x; the stored network's "+
					"require_mac did not reach the re-bind", sent, ownID)
			}
			if res.record == theirs {
				t.Fatalf("the reservation took the neighbour's record %q", theirs)
			}
			for _, ev := range s2Lines(t, journal)[from:] {
				if ev.ID == theirs {
					t.Errorf("the neighbour's record gained %v", ev.Op)
				}
			}
			macRebindUntouched(t, p, theirs, neighbour)
		})
	}
}

func TestIpamReserveAddress_RequireMACReBindsTheOwnTombstoneBesideANeighbours(t *testing.T) {
	own, neighbour := f0MAC(0x01), f0MAC(0x02)
	p, b, _, _ := f0Fixture(t)
	s2ReserveLink(t)
	mine := f0Tombstone(t, p, own, f0Addr)
	theirs := f0Tombstone(t, p, neighbour, f0Addr2)

	var sent []byte
	var asked string
	prev := dhcpGetIP
	dhcpGetIP = func(_ context.Context, _ string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		sent, asked = append([]byte(nil), o.ClientID...), o.RequestedIP
		return dhcp.Info{IP: f0Addr, Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = prev })

	opts := f0Options()
	opts.RequireMAC = true
	res, err := s2Reserve(t, p, b, opts, own)
	if err != nil {
		t.Fatalf("the reserve failed: %v", err)
	}
	if res.record != mine || !bytes.Equal(sent, resolveClientID(opts, "", own)) || asked != "192.168.99.10" {
		t.Fatalf("reserved under record %q with client id %x asking for %q, want the own record %q, the own client "+
			"id and 192.168.99.10", res.record, sent, asked, mine)
	}
	macRebindUntouched(t, p, theirs, neighbour)
	macRebindAmbiguous(t, p, 0)
}

func TestIpamReserveAddress_RequireMACWithAClientIDReBindsTheOwnTombstone(t *testing.T) {
	own := f0MAC(0x01)
	p, b, _, _ := f0Fixture(t)
	s2ReserveLink(t)
	opts := f0Options()
	opts.RequireMAC, opts.ClientID = true, "site-wide-id"
	minted := dhcp.ClientIdentity(resolveClientID(opts, "", own))
	mine := p.recordCreated(ipamTestNetwork, own, minted)
	if err := p.records.Observed(mine, acquired(f0Addr, time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Bound(mine); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	p.recordRetained(mine, time.Now().Add(tombstoneTTL))

	var asked string
	prev := dhcpGetIP
	dhcpGetIP = func(_ context.Context, _ string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		asked = o.RequestedIP
		return dhcp.Info{IP: f0Addr, Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = prev })

	res, err := s2Reserve(t, p, b, opts, own)
	if err != nil {
		t.Fatalf("the reserve failed: %v", err)
	}
	if res.record != mine || asked != "192.168.99.10" {
		t.Fatalf("reserved under record %q asking for %q, want the own record %q minted under the network's "+
			"client_id and 192.168.99.10", res.record, asked, mine)
	}
}
