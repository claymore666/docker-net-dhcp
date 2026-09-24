// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"testing"
)

// RFC 9915 section 11.1 wants the DUID "as stable as possible", so the blob is minted once and read back from the v6
// record on every restart; both directions are asserted because Parse is partial and the bytes come off disk (#1010).

func FuzzIdentity6RoundTrip(f *testing.F) {
	// RFC 9915 section 11.4 DUID-LL over Ethernet: type 0x0003, hardware type 0x0001, six MAC bytes.
	f.Add([]byte{0x00, 0x03, 0x00, 0x01, 0x02, 0x42, 0xac, 0x11, 0x00, 0x02}, uint32(0xac110002))
	// RFC 9915 section 11.5 DUID-UUID, as DUIDUUID mints for an ipvlan endpoint (#895).
	f.Add([]byte{
		0x00, 0x04,
		0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
		0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
	}, uint32(0))
	f.Add([]byte{}, uint32(0))
	f.Add([]byte{0x00, 0x00, 0x00, 0x00}, uint32(1))
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x00}, uint32(0xffffffff))
	f.Add([]byte{0x00, 0x03, 0x00, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, uint32(0xffffffff))

	f.Fuzz(func(t *testing.T, blob []byte, iaid uint32) {
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
