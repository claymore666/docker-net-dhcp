// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"net"
	"testing"
)

// The identity survives the round trip through the record store byte for
// byte, for every DUID length the chassis can mint.
//
// WHY A ROUND TRIP AND NOT TWO SEPARATE ASSERTIONS. The store holds one
// opaque blob (D10: identity is caller-supplied bytes), so Bytes and
// ParseIdentity6 are a codec whose only contract is that they compose to
// the identity. A DUID has no self-describing length on the wire in this
// blob, so the split is positional -- the IAID is the LAST four bytes --
// and a codec that agreed with itself while splitting in the wrong place
// would pass two independent assertions and hand the server a different
// DUID after every restart.
func TestIdentity6_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		id   Identity6
	}{
		// DUID-LL from a six-byte MAC: 2 bytes of type, 2 of hardware
		// type, 6 of address. The bridge and macvlan shape.
		{"duid-ll", Identity6{DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0, 2}, IAID: 0xac110002}},
		// DUID-UUID: 2 bytes of type, 16 of UUID. The ipvlan shape,
		// where every endpoint on one parent shares the MAC (#895).
		{"duid-uuid", Identity6{
			DUID: append([]byte{0, 4}, bytes.Repeat([]byte{0xab}, 16)...),
			IAID: 1,
		}},
		// The boundary: the shortest blob ParseIdentity6 accepts is
		// five bytes, one of DUID and four of IAID. Nothing mints one,
		// which is exactly why it is here -- the length check is `<=`
		// and an off-by-one there turns a one-byte DUID into a zero
		// identity that buildParams6 refuses.
		{"one-byte duid", Identity6{DUID: []byte{0x7f}, IAID: 0}},
		// Every bit of the IAID set: a big-endian encode/decode pair
		// that agreed on little-endian would pass a symmetric value.
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

// Bytes copies. The blob goes to a record store that outlives the
// options struct it came from, and a shared backing array means a later
// append to the DUID rewrites a persisted identity in place.
func TestIdentity6_BytesDoesNotAliasTheDUID(t *testing.T) {
	duid := []byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 6}
	id := Identity6{DUID: duid, IAID: 7}
	b := id.Bytes()
	duid[0] = 0xff
	if b[0] != 0 {
		t.Errorf("Bytes() aliases the caller's DUID: mutating it changed the blob to %x", b)
	}
}

// And so does ParseIdentity6, in the other direction: the blob it is
// handed comes straight off a record read.
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

// A blob too short to hold both halves is an error, not a truncated
// identity.
//
// FOUR AND BELOW, not "empty": four bytes parse cleanly as an IAID with
// an empty DUID, and an empty DUID is the zero identity -- which
// buildParams6 refuses, but only after the caller has already logged
// "resumed the endpoint's identity". The refusal belongs at the read.
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
	// The IAID alone is not an identity: zero is a legitimate IAID
	// value, so only the DUID can decide.
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

// The two DUID constructors produce the shapes RFC 9915 section 11
// defines, and the wrapper does not lose the library's refusals.
func TestDUIDConstructors(t *testing.T) {
	mac, err := net.ParseMAC("02:42:ac:11:00:02")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	duid, err := DUIDLL(mac)
	if err != nil {
		t.Fatalf("DUIDLL: %v", err)
	}
	// RFC 9915 section 11.4: type 3, then a 16-bit hardware type, then
	// the link-layer address. Hardware type 1 is Ethernet.
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

// The IAID derivations take from opposite ends, and that is the whole
// point of having two of them.
//
// IAIDFromMAC takes the LOW four bytes because the high two of a MAC are
// the OUI: every container on one Docker network shares them, so a
// high-end derivation would hand every endpoint on the segment the same
// IAID and the server would read them as one client's several
// interfaces (RFC 9915 section 12). IAIDFromBytes takes the FIRST four
// because its seed is a random endpoint id with no structure to avoid.
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

	// Two endpoints on one macvlan parent differ only in the low half
	// of the MAC; that is the population IAIDFromMAC has to separate.
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
