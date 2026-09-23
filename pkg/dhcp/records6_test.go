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

// A dual-stack endpoint has one hardware address on one network, so without the scope suffix the two families' records
// collide (#911).

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

// RFC 9915 section 11: a DUID "SHOULD NOT change over time if at all possible"; on ipvlan only the file keeps it
// (#895).

func TestRecords6_IdentitySurvivesAReopen(t *testing.T) {
	r, path := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
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

// Measured 2026-09-06: the library drops an identity-less v6 record at fold time and the write still succeeds (#911).

func TestRecords6_ARecordWithNoIdentityIsRefusedAtTheCall(t *testing.T) {
	r, _ := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}

	if err := r.Created6("ep-v6", network, mac, nil); err == nil {
		t.Fatal("Created6 accepted an empty identity")
	}
	if id, _, _, ok := r.Resume6(network, mac, time.Now()); ok {
		t.Errorf("a refused Created6 left record %q behind", id)
	}

	if _, err := buildParams6(&DHCPClientOptions{V6: true}, false); err == nil {
		t.Error("buildParams6 accepted the zero identity a record-less endpoint resumes")
	}
}

// The identity blob is opaque to the library (D10), so nothing below the chassis validates it (#911).

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

// A v6 scope carries a '#' the state-file path check refuses, which left DHCPv6 addresses unreleased (#984).

func TestNetworkOfScope_IsScope6Backwards(t *testing.T) {
	for _, network := range []string{
		"0123456789abcdef",
		"n1",
		"abcv6",
		"v6",
	} {
		t.Run(network, func(t *testing.T) {
			if got, v6 := NetworkOfScope(Scope6(network)); got != network || !v6 {
				t.Errorf("NetworkOfScope(Scope6(%q)) = (%q, %v), want (%q, true)",
					network, got, v6, network)
			}
			if got, v6 := NetworkOfScope(network); got != network || v6 {
				t.Errorf("NetworkOfScope(%q) = (%q, %v), want (%q, false): a network id is "+
					"its own v4 scope", network, got, v6, network)
			}
		})
	}
}
