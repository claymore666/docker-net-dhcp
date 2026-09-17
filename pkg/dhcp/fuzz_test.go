// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"testing"
)

// FuzzIdentity6RoundTrip drives both halves of the DHCPv6 identity codec
// against each other.
//
// The blob this codec writes is the only thing that survives a plugin
// restart on the v6 path: RFC 9915 section 11.1 requires a DUID to be
// "as stable as possible", so the identity is minted once, written into
// the endpoint's v6 record and read back forever after. A decode that
// disagrees with the encode is an endpoint that asks the server for a
// binding under a name the server has never seen, and the address
// changes under a container that was promised it would not.
//
// Both directions are asserted because neither implies the other. Bytes
// is total and Parse is partial, so Parse(Bytes(x)) == x can hold while
// Bytes(Parse(b)) != b for a b no Bytes call would ever have produced —
// and b comes off disk, where an older build, a truncated write or a
// hand edit put it.
//
// The oracle is byte equality with the input, not a property the
// package computes: nothing here asks Identity6 whether it is happy.
func FuzzIdentity6RoundTrip(f *testing.F) {
	// RFC 9915 section 11.4 DUID-LL over Ethernet: type 0x0003, hardware
	// type 0x0001, then the six MAC bytes. What DUIDLL mints.
	f.Add([]byte{0x00, 0x03, 0x00, 0x01, 0x02, 0x42, 0xac, 0x11, 0x00, 0x02}, uint32(0xac110002))
	// RFC 9915 section 11.5 DUID-UUID: type 0x0004 and sixteen octets.
	// What DUIDUUID mints for an ipvlan endpoint (#895).
	f.Add([]byte{
		0x00, 0x04,
		0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
		0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
	}, uint32(0))
	// The boundary Parse refuses, and one byte past it.
	f.Add([]byte{}, uint32(0))
	f.Add([]byte{0x00, 0x00, 0x00, 0x00}, uint32(1))
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x00}, uint32(0xffffffff))
	// A DUID whose own tail looks like an IAID, which is the shape a
	// split with no length field has to get right.
	f.Add([]byte{0x00, 0x03, 0x00, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, uint32(0xffffffff))

	f.Fuzz(func(t *testing.T, blob []byte, iaid uint32) {
		// Decode direction: whatever came off disk either fails, and
		// then it was too short to hold a DUID at all, or it re-encodes
		// to the same bytes.
		got, err := ParseIdentity6(blob)
		switch {
		case err != nil:
			if len(blob) > iaidLen {
				t.Fatalf("ParseIdentity6 refused %d bytes, which is more than the %d an IAID needs: %v", len(blob), iaidLen, err)
			}
		default:
			if len(blob) <= iaidLen {
				t.Fatalf("ParseIdentity6 accepted %d bytes, which cannot carry a DUID and an IAID", len(blob))
			}
			if back := got.Bytes(); !bytes.Equal(back, blob) {
				t.Fatalf("re-encoding a parsed identity changed it: got %x, want %x", back, blob)
			}
			if got.IsZero() {
				t.Fatalf("ParseIdentity6 accepted %x and returned the identity that was never minted", blob)
			}
		}

		// Encode direction: an identity that was minted survives the
		// record store, and one that was not writes nothing that could
		// be read back as an identity.
		id := Identity6{DUID: blob, IAID: iaid}
		enc := id.Bytes()
		if id.IsZero() {
			if enc != nil {
				t.Fatalf("the empty identity wrote %x", enc)
			}
			if _, err := ParseIdentity6(enc); err == nil {
				t.Fatal("what the empty identity wrote parsed back as an identity")
			}
			return
		}
		back, err := ParseIdentity6(enc)
		if err != nil {
			t.Fatalf("an identity this package minted did not parse back: %v", err)
		}
		if !bytes.Equal(back.DUID, id.DUID) {
			t.Fatalf("DUID changed across the record store: got %x, want %x", back.DUID, id.DUID)
		}
		if back.IAID != id.IAID {
			t.Fatalf("IAID changed across the record store: got %#x, want %#x", back.IAID, id.IAID)
		}
	})
}
