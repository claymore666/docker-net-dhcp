// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"net"
	"testing"
)

// The blob has no self-describing DUID length, so the split is positional: the IAID is the last four bytes (D10, #911).

func TestIdentity6_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		id   Identity6
	}{
		// DUID-LL from a six-byte MAC (RFC 9915 section 11.4): the bridge and macvlan shape.
		{"duid-ll", Identity6{DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0, 2}, IAID: 0xac110002}},
		// DUID-UUID: the ipvlan shape, where every endpoint on one parent shares the MAC (#895).
		{"duid-uuid", Identity6{
			DUID: append([]byte{0, 4}, bytes.Repeat([]byte{0xab}, 16)...),
			IAID: 1,
		}},
		{"one-byte duid", Identity6{DUID: []byte{0x7f}, IAID: 0}},
		{"max iaid", Identity6{DUID: []byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 6}, IAID: 0xffffffff}},
		{"asymmetric iaid", Identity6{DUID: []byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 6}, IAID: 0x01020304}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.id.Bytes()
			if len(b) != len(tc.id.DUID)+4 {
				t.Fatalf("Bytes() is %d long for a %d-byte DUID, want %d",
					len(b), len(tc.id.DUID), len(tc.id.DUID)+4)
			}
			got, err := ParseIdentity6(b)
			if err != nil {
				t.Fatalf("ParseIdentity6(%x): %v", b, err)
			}
			if !bytes.Equal(got.DUID, tc.id.DUID) {
				t.Errorf("DUID round-tripped to %x, want %x", got.DUID, tc.id.DUID)
			}
			if got.IAID != tc.id.IAID {
				t.Errorf("IAID round-tripped to %#x, want %#x", got.IAID, tc.id.IAID)
			}
		})
	}
}

func TestIdentity6_BytesDoesNotAliasTheDUID(t *testing.T) {
	duid := []byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 6}
	id := Identity6{DUID: duid, IAID: 7}
	b := id.Bytes()
	duid[0] = 0xff
	if b[0] != 0 {
		t.Errorf("Bytes() aliases the caller's DUID: mutating it changed the blob to %x", b)
	}
}

func TestParseIdentity6_DoesNotAliasTheBlob(t *testing.T) {
	blob := []byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 6, 0, 0, 0, 7}
	id, err := ParseIdentity6(blob)
	if err != nil {
		t.Fatalf("ParseIdentity6: %v", err)
	}
	blob[0] = 0xff
	if id.DUID[0] != 0 {
		t.Errorf("ParseIdentity6 aliases the blob: mutating it changed the DUID to %x", id.DUID)
	}
}

func TestParseIdentity6_RefusesABlobWithNoDUID(t *testing.T) {
	for n := 0; n <= 4; n++ {
		if _, err := ParseIdentity6(make([]byte, n)); err == nil {
			t.Errorf("ParseIdentity6 accepted a %d-byte blob, want an error: "+
				"anything up to four bytes carries no DUID", n)
		}
	}
	if _, err := ParseIdentity6(make([]byte, 5)); err != nil {
		t.Errorf("ParseIdentity6 refused a five-byte blob (%v), which is a one-byte "+
			"DUID and an IAID and is the first accepting length", err)
	}
}

func TestIdentity6_IsZero(t *testing.T) {
	if !(Identity6{}).IsZero() {
		t.Error("the zero Identity6 does not report itself zero")
	}
	// Zero is a legitimate IAID, so only the DUID decides (#911).
	if !(Identity6{IAID: 42}).IsZero() {
		t.Error("an Identity6 with an IAID and no DUID reports itself non-zero; " +
			"the DUID is the part RFC 9915 section 11 says must persist")
	}
	if (Identity6{DUID: []byte{0}}).IsZero() {
		t.Error("an Identity6 with a DUID reports itself zero")
	}
	if (Identity6{}).Bytes() != nil {
		t.Error("the zero Identity6 encodes to a non-nil blob, which a record " +
			"store would persist as a real identity")
	}
}

func TestDUIDConstructors(t *testing.T) {
	mac, err := net.ParseMAC("02:42:ac:11:00:02")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	duid, err := DUIDLL(mac)
	if err != nil {
		t.Fatalf("DUIDLL: %v", err)
	}
	// RFC 9915 section 11.4: type 3, then a 16-bit hardware type (1 is Ethernet), then the link-layer address.
	want := []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	if !bytes.Equal(duid, want) {
		t.Errorf("DUIDLL(%v) = %x, want %x", mac, duid, want)
	}
	if _, err := DUIDLL(nil); err == nil {
		t.Error("DUIDLL accepted an empty hardware address")
	}

	uuid := bytes.Repeat([]byte{0x5a}, 16)
	duid, err = DUIDUUID(uuid)
	if err != nil {
		t.Fatalf("DUIDUUID: %v", err)
	}
	// RFC 9915 section 11.5: type 4 then exactly 128 bits.
	if !bytes.Equal(duid, append([]byte{0, 4}, uuid...)) {
		t.Errorf("DUIDUUID = %x, want 0004 followed by the UUID", duid)
	}
	if _, err := DUIDUUID(uuid[:15]); err == nil {
		t.Error("DUIDUUID accepted a 15-byte UUID")
	}
}

// The high bytes of a MAC are the OUI shared on one Docker network, and RFC 9915 section 12 reads equal IAIDs as one
// client (#911).

func TestIAIDDerivations_TakeFromOppositeEnds(t *testing.T) {
	mac, err := net.ParseMAC("02:42:ac:11:00:02")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	iaid, err := IAIDFromMAC(mac)
	if err != nil {
		t.Fatalf("IAIDFromMAC: %v", err)
	}
	if iaid != 0xac110002 {
		t.Errorf("IAIDFromMAC(%v) = %#x, want %#x (the low four bytes)", mac, iaid, 0xac110002)
	}

	seed := []byte{0xde, 0xad, 0xbe, 0xef, 0x11, 0x22}
	iaid, err = IAIDFromBytes(seed)
	if err != nil {
		t.Fatalf("IAIDFromBytes: %v", err)
	}
	if iaid != 0xdeadbeef {
		t.Errorf("IAIDFromBytes(%x) = %#x, want %#x (the first four bytes)", seed, iaid, 0xdeadbeef)
	}

	other, err := net.ParseMAC("02:42:ac:11:00:03")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	a, _ := IAIDFromMAC(mac)
	b, _ := IAIDFromMAC(other)
	if a == b {
		t.Errorf("two MACs on one parent derive the same IAID %#x", a)
	}

	if _, err := IAIDFromMAC(mac[:3]); err == nil {
		t.Error("IAIDFromMAC accepted a three-byte address")
	}
	if _, err := IAIDFromBytes([]byte{1, 2, 3}); err == nil {
		t.Error("IAIDFromBytes accepted a three-byte seed")
	}
}
