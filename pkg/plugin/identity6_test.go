// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

const (
	// Two Docker endpoint ids: 64 hex characters, differing in the
	// first byte, so a seed cut from the front separates them.
	epA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	epB = "f123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// And a third that differs only AFTER the first sixteen bytes, which
	// is the population a prefix-cut seed cannot separate. It is here to
	// bound the claim, not to pass it.
	epLateDiff = "0123456789abcdef0123456789abcdefffffffffffffffffffffffffffffffff"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return mac
}

// The DHCPv6 identity is derived from the MAC in every mode where the
// MAC is per-endpoint, and from the endpoint id in the one mode where it
// is not (D30 Q4).
//
// WHY THE SPLIT EXISTS. An ipvlan L2 slave inherits the parent link's
// MAC by kernel design, so every container on one ipvlan network has the
// SAME hardware address. A MAC-derived DUID there is one identity shared
// by every endpoint: they all claim one binding, the server hands the
// same address out repeatedly, and the containers fight over it. That is
// #895, the v6 form of what #219 named for v4 -- and it is silent, since
// each container comes up with an address that looks fine until a second
// one starts.
//
// WHY NOT THE ENDPOINT FORM EVERYWHERE. Because 1.9.0 handed dhcpcd the
// MAC-derived DUID on bridge and macvlan (P-8.6), and an endpoint
// upgraded from 1.x has to present the identity the server already holds
// a binding for or it loses its address on the upgrade.
func TestResolveIdentity6_ModeDecidesTheShape(t *testing.T) {
	mac := mustMAC(t, "02:42:ac:11:00:02")

	for _, mode := range []string{"", ModeBridge, ModeMacvlan} {
		t.Run("mac-derived/"+mode, func(t *testing.T) {
			id, err := resolveIdentity6(DHCPNetworkOptions{Mode: mode}, epA, mac)
			if err != nil {
				t.Fatalf("resolveIdentity6: %v", err)
			}
			// RFC 9915 section 11.4: type 3, hardware type 1, the MAC.
			want := []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
			if !bytes.Equal(id.DUID, want) {
				t.Errorf("DUID = %x, want %x (DUID-LL over the endpoint MAC)", id.DUID, want)
			}
			if id.IAID != 0xac110002 {
				t.Errorf("IAID = %#x, want %#x (the MAC's low four bytes)", id.IAID, 0xac110002)
			}
			// The endpoint id must not reach a MAC-derived identity, or
			// the 1.x upgrade path silently changes DUID.
			other, err := resolveIdentity6(DHCPNetworkOptions{Mode: mode}, epB, mac)
			if err != nil {
				t.Fatalf("resolveIdentity6: %v", err)
			}
			if !bytes.Equal(other.DUID, id.DUID) || other.IAID != id.IAID {
				t.Error("the identity moved when only the endpoint id changed; an endpoint " +
					"upgraded from 1.9.0 would present a DUID the server has no binding for")
			}
		})
	}

	t.Run("ipvlan", func(t *testing.T) {
		a, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, epA, mac)
		if err != nil {
			t.Fatalf("resolveIdentity6: %v", err)
		}
		// RFC 9915 section 11.5: type 4 and sixteen bytes.
		if len(a.DUID) != 2+16 || a.DUID[0] != 0 || a.DUID[1] != 4 {
			t.Errorf("DUID = %x, want a DUID-UUID (0004 then sixteen bytes)", a.DUID)
		}
		if bytes.Contains(a.DUID, mac) {
			t.Errorf("the ipvlan DUID %x carries the parent MAC %v, which every endpoint "+
				"on this network shares", a.DUID, mac)
		}

		// THE PROPERTY: two endpoints on ONE ipvlan parent, same MAC,
		// different identities.
		b, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, epB, mac)
		if err != nil {
			t.Fatalf("resolveIdentity6: %v", err)
		}
		if bytes.Equal(a.DUID, b.DUID) {
			t.Fatalf("two ipvlan endpoints sharing the parent MAC derive one DUID %x: "+
				"they claim one binding and the server hands both the same address (#895)", a.DUID)
		}
		if a.IAID == b.IAID {
			t.Errorf("two ipvlan endpoints derive one IAID %#x", a.IAID)
		}

		// THE BOUND on that property, stated rather than hidden: the
		// seed is a PREFIX of the endpoint id, so two ids that agree on
		// their first sixteen bytes collide. Docker's ids are random
		// 32-byte hex, so this is not reachable in practice -- but the
		// claim above is "different endpoint ids", and this is the
		// population it does not cover.
		late, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, epLateDiff, mac)
		if err != nil {
			t.Fatalf("resolveIdentity6: %v", err)
		}
		if !bytes.Equal(a.DUID, late.DUID) {
			t.Log("endpoint ids differing only after the seed no longer collide; the bound " +
				"documented here has moved and the comment above is stale")
		}
	})
}

// A caller with no MAC falls back to the endpoint form rather than to no
// identity at all.
//
// buildParams6 refuses the zero identity, so "no MAC" would otherwise
// refuse the endpoint outright -- and the endpoint id is always there.
// It is the same fallback resolveClientID takes for v4, for the same
// reason.
func TestResolveIdentity6_FallsBackWhenThereIsNoMAC(t *testing.T) {
	id, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeBridge}, epA, nil)
	if err != nil {
		t.Fatalf("resolveIdentity6: %v", err)
	}
	if id.IsZero() {
		t.Fatal("a MAC-less endpoint got no identity; buildParams6 refuses that and the " +
			"container never starts")
	}
	if len(id.DUID) != 2+16 || id.DUID[1] != 4 {
		t.Errorf("DUID = %x, want the endpoint-derived DUID-UUID", id.DUID)
	}
	// Matching what ipvlan derives from the same endpoint is the point:
	// one fallback, not two shapes of it.
	ipvlan, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, epA, nil)
	if err != nil {
		t.Fatalf("resolveIdentity6: %v", err)
	}
	if !bytes.Equal(id.DUID, ipvlan.DUID) || id.IAID != ipvlan.IAID {
		t.Error("the MAC-less fallback and the ipvlan form differ")
	}
}

// An endpoint id too short to cut a UUID from is an error naming the
// endpoint, not a silently short DUID.
//
// A truncated seed would produce a DUID-UUID of the wrong length, which
// the library refuses anyway -- but it refuses it after the chassis has
// been asked for a client, with a message about a UUID length. The
// refusal here names the endpoint, and it is loud in exactly the case
// that reaches it: a test or an engine handing over a short id.
func TestResolveIdentity6_RefusesAnUnusableEndpointID(t *testing.T) {
	_, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, "abcd", nil)
	if err == nil {
		t.Fatal("resolveIdentity6 accepted a four-character endpoint id")
	}
	if !strings.Contains(err.Error(), "abcd") {
		t.Errorf("the error does not name the endpoint: %v", err)
	}
	// Not hex, right length: hex.DecodeString is what rejects it, and
	// the caller still gets a named endpoint rather than a decode error.
	if _, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, strings.Repeat("z", 64), nil); err == nil {
		t.Error("resolveIdentity6 accepted a non-hex endpoint id")
	}
}

// The seed is a prefix of the endpoint id and the exact width of a UUID.
//
// Both halves matter. Sixteen bytes is RFC 9915 section 11.5's payload
// width, and the library refuses anything else; a PREFIX rather than a
// hash keeps the identity legible in the server's log beside the
// endpoint it belongs to, which is what an operator matching a binding
// to a container actually does.
func TestEndpointSeed(t *testing.T) {
	seed := endpointSeed(epA)
	if len(seed) != uuidBytes {
		t.Fatalf("seed is %d bytes, want %d (RFC 9915 section 11.5's DUID-UUID payload)",
			len(seed), uuidBytes)
	}
	if got, want := seed[0], byte(0x01); got != want {
		t.Errorf("the seed does not start at the endpoint id: %x", seed)
	}
	if endpointSeed(epA[:uuidBytes*2-1]) != nil {
		t.Error("endpointSeed cut a short id rather than refusing it")
	}
	if endpointSeed("") != nil {
		t.Error("endpointSeed produced a seed from an empty endpoint id")
	}
	// The identity built from it is the one dhcp.Identity6 accepts back.
	id, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, epA, nil)
	if err != nil {
		t.Fatalf("resolveIdentity6: %v", err)
	}
	round, err := dhcp.ParseIdentity6(id.Bytes())
	if err != nil {
		t.Fatalf("the derived identity does not round-trip through the record: %v", err)
	}
	if !bytes.Equal(round.DUID, id.DUID) || round.IAID != id.IAID {
		t.Errorf("the derived identity round-tripped to %+v, want %+v", round, id)
	}
}
