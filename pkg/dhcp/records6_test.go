// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func testIdentity6(t *testing.T, mac string) Identity6 {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	duid, err := DUIDLL(hw)
	if err != nil {
		t.Fatalf("DUIDLL: %v", err)
	}
	iaid, err := IAIDFromMAC(hw)
	if err != nil {
		t.Fatalf("IAIDFromMAC: %v", err)
	}
	return Identity6{DUID: duid, IAID: iaid}
}

// The two families of one dual-stack endpoint do not share a record.
//
// WHAT GOES WRONG WITHOUT THE SCOPE SUFFIX. A record is indexed by scope
// AND hardware address, and a dual-stack endpoint has ONE hardware
// address on ONE network -- so a v6 record filed under the network id
// collides with the v4 record of the same endpoint exactly. The fold
// takes the newest match, so whichever family bound last would answer
// both Resume calls: the v4 manager would be handed a /128 to
// INIT-REBOOT, or the v6 manager would Confirm a v4 address. Both are
// silent at the record layer and only fail on the wire.
func TestRecords6_TheFamiliesDoNotShareARecord(t *testing.T) {
	r, _ := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	id6 := testIdentity6(t, "02:42:ac:11:00:02")

	if Scope6(network) == network {
		t.Fatal("Scope6 is the identity function: the two families would share a record")
	}

	if err := r.Created("ep-v4", network, mac, []byte{1, 2, 3}); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if err := r.Created6("ep-v6", network, mac, id6.Bytes()); err != nil {
		t.Fatalf("Created6: %v", err)
	}

	now := time.Now()
	gotV4, _, ok := r.Resume(network, mac, now)
	if !ok {
		t.Fatal("the v4 record did not resume")
	}
	if gotV4 != "ep-v4" {
		t.Errorf("the v4 scope resumed %q, want ep-v4", gotV4)
	}
	gotV6, _, ident, ok := r.Resume6(network, mac, now)
	if !ok {
		t.Fatal("the v6 record did not resume")
	}
	if gotV6 != "ep-v6" {
		t.Errorf("the v6 scope resumed %q, want ep-v6", gotV6)
	}
	if !bytes.Equal(ident.DUID, id6.DUID) || ident.IAID != id6.IAID {
		t.Errorf("Resume6 returned identity %+v, want %+v", ident, id6)
	}
}

// The identity survives the file, which is the entire reason the record
// carries it.
//
// RFC 9915 section 11: a DUID "SHOULD NOT change over time if at all
// possible". A plugin restart that re-derives the identity is fine as
// long as it derives the SAME one -- and on ipvlan it cannot, because
// every endpoint on the parent shares the MAC and the identity is seeded
// from the endpoint id (#895). So the file is the only thing that makes
// an ipvlan endpoint the same DHCPv6 client after a restart, and this
// reopens it to prove the identity is in the bytes and not in the
// process.
func TestRecords6_IdentitySurvivesAReopen(t *testing.T) {
	r, path := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	// A DUID-UUID, the ipvlan shape: nothing about the link can
	// reproduce it.
	duid, err := DUIDUUID(bytes.Repeat([]byte{0x5a}, 16))
	if err != nil {
		t.Fatalf("DUIDUUID: %v", err)
	}
	want := Identity6{DUID: duid, IAID: 0x0a0b0c0d}

	if err := r.Created6("ep-v6", network, mac, want.Bytes()); err != nil {
		t.Fatalf("Created6: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenRecords(path, "instance-b")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	_, _, got, ok := reopened.Resume6(network, mac, time.Now())
	if !ok {
		t.Fatal("the record did not resume after a reopen")
	}
	if !bytes.Equal(got.DUID, want.DUID) || got.IAID != want.IAID {
		t.Errorf("identity came back as %+v, want %+v: the server would see a new client",
			got, want)
	}
}

// A v6 record with no identity is refused at the call, not at the
// rebuild.
//
// WHY THIS IS NOT LEFT TO THE LIBRARY. The library does refuse it --
// RFC 9915 section 11, a v6 record carries the DUID as sent -- but it
// refuses at FOLD time, and the fold drops the offending record and
// carries on. So the write succeeds, Rebuilt succeeds, the rest of the
// file is intact, and the endpoint just quietly has no v6 record: every
// plugin restart mints a fresh DUID, the server files each one as a new
// client, and the operator sees an address that changes for no reason
// with nothing in the log. Measured on this tree, 2026-09-06.
func TestRecords6_ARecordWithNoIdentityIsRefusedAtTheCall(t *testing.T) {
	r, _ := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}

	if err := r.Created6("ep-v6", network, mac, nil); err == nil {
		t.Fatal("Created6 accepted an empty identity")
	}
	// And nothing was written: a refused call that still appended would
	// burn the record id.
	if id, _, _, ok := r.Resume6(network, mac, time.Now()); ok {
		t.Errorf("a refused Created6 left record %q behind", id)
	}

	// The zero identity is refused again on the way to the wire, which
	// is the second half of the same guarantee -- a record written by an
	// older build carries none, and Resume6 hands that back as the zero
	// value rather than a guess.
	if _, err := buildParams6(&DHCPClientOptions{V6: true}, false); err == nil {
		t.Error("buildParams6 accepted the zero identity a record-less endpoint resumes")
	}
}

// A record whose identity blob is too short to split is reported as
// absent, not as a truncated DUID.
//
// The blob is opaque to the library (D10), so nothing below the chassis
// validates it; a corrupted line would otherwise become a DUID one byte
// shorter than the one the server has, which is a NEW client that gets a
// new address and no diagnosis.
func TestRecords6_ACorruptIdentityIsNotTruncated(t *testing.T) {
	r, _ := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}

	if err := r.Created6("ep-v6", network, mac, []byte{1, 2, 3}); err != nil {
		t.Fatalf("Created6: %v", err)
	}
	_, _, ident, ok := r.Resume6(network, mac, time.Now())
	if !ok {
		t.Fatal("the record did not resume")
	}
	if !ident.IsZero() {
		t.Errorf("a three-byte identity blob parsed to %+v; four of its bytes would "+
			"have to be the IAID and there are only three", ident)
	}
}

// Resume6 on a network with no v6 record says so, rather than falling
// back to the v4 one.
func TestRecords6_NoRecordIsNotTheV4Record(t *testing.T) {
	r, _ := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}

	if err := r.Created("ep-v4", network, mac, []byte{1, 2, 3}); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if id, _, ident, ok := r.Resume6(network, mac, time.Now()); ok {
		t.Errorf("Resume6 answered %q (identity %+v) on a network with only a v4 record",
			id, ident)
	}
}
