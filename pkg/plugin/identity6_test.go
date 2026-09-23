// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const (
	epA        = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	epB        = "f123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
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
	ipvlan, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, epA, nil)
	if err != nil {
		t.Fatalf("resolveIdentity6: %v", err)
	}
	if !bytes.Equal(id.DUID, ipvlan.DUID) || id.IAID != ipvlan.IAID {
		t.Error("the MAC-less fallback and the ipvlan form differ")
	}
}

func TestResolveIdentity6_RefusesAnUnusableEndpointID(t *testing.T) {
	_, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, "abcd", nil)
	if err == nil {
		t.Fatal("resolveIdentity6 accepted a four-character endpoint id")
	}
	if !strings.Contains(err.Error(), "abcd") {
		t.Errorf("the error does not name the endpoint: %v", err)
	}
	if _, err := resolveIdentity6(DHCPNetworkOptions{Mode: ModeIPvlan}, strings.Repeat("z", 64), nil); err == nil {
		t.Error("resolveIdentity6 accepted a non-hex endpoint id")
	}
}

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
