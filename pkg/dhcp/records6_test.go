// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
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

// The v1.2.0 library writes the Reconfigure key, the replay value and whether one exists into the lease record and reads
// them back on a resume (dhcp-golib#28); the plugin hands the record's lease to the client whole (#1137).

func TestRecords6_TheReconfigureKeySurvivesAReopen(t *testing.T) {
	r, path := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	id6 := testIdentity6(t, "02:42:ac:11:00:02")
	key := bytes.Repeat([]byte{0x7c}, 16)
	now := time.Now()

	held := lease.Lease{
		Addr:                  netip.MustParsePrefix("2001:db8::5/128"),
		Addrs:                 []lease.Addr6{{Addr: netip.MustParsePrefix("2001:db8::5/128"), Preferred: now.Add(time.Hour), Valid: now.Add(2 * time.Hour)}},
		Acquired:              now,
		Renew:                 now.Add(30 * time.Minute),
		Rebind:                now.Add(time.Hour),
		Expire:                now.Add(2 * time.Hour),
		ServerDUID:            []byte{0, 3, 0, 1, 2, 2, 2, 2, 2, 2},
		ReconfigureKey:        key,
		ReconfigureReplay:     0,
		ReconfigureReplaySeen: true,
	}
	if err := r.Created6("ep-v6", network, mac, id6.Bytes()); err != nil {
		t.Fatalf("Created6: %v", err)
	}
	if err := r.Bound("ep-v6"); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	if err := r.Observed("ep-v6", lease.Event{Kind: lease.Acquired, Lease: held}, nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := r.Observed("ep-v6", lease.Event{Kind: lease.Lost, Reason: proto.ReasonStopped}, nil); err != nil {
		t.Fatalf("Observed(stopped): %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenRecords(path, "instance-b")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	_, res, _, ok := reopened.Resume6(network, mac, now.Add(time.Minute))
	if !ok {
		t.Fatal("the record did not resume after a reopen")
	}
	if res.Lease == nil {
		t.Fatal("the resumed record carries no lease: the client would Solicit instead of Confirm")
	}
	if !bytes.Equal(res.Lease.ReconfigureKey, key) {
		t.Errorf("reconfigure key came back as %x, want %x: a restarted client would discard every Reconfigure "+
			"until a Reply brought the key again", res.Lease.ReconfigureKey, key)
	}
	if !res.Lease.ReconfigureReplaySeen || res.Lease.ReconfigureReplay != 0 {
		t.Errorf("replay floor came back as (%d, seen=%v), want (0, seen=true): a recorded zero is a floor, not none",
			res.Lease.ReconfigureReplay, res.Lease.ReconfigureReplaySeen)
	}
}

// releaseTo is one Release datagram's Server Identifier and IA_PD count.
type releaseTo struct {
	server []byte
	pds    int
}

// dhcp-golib v1.4.3 keeps the server that delegated a prefix beside the address server (dhcp-golib#70); the plugin's
// record-built Release sends one datagram per server, so the journal must keep both through a compaction (#214).
func TestRecords6_ASplitLeaseKeepsItsPrefixServerThroughACompactionAndAReopen(t *testing.T) {
	addrServer, prefixServer := []byte{0, 3, 0, 1, 2, 2, 2, 2, 2, 2}, []byte{0, 3, 0, 1, 4, 4, 4, 4, 4, 4}
	for _, tc := range []struct {
		name      string
		pdServer  []byte
		wantGrams []releaseTo
	}{
		{"split", prefixServer, []releaseTo{{addrServer, 0}, {prefixServer, 1}}},
		{"one server, as every v1.4.2 record", nil, []releaseTo{{addrServer, 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, path := testRecords(t)
			mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
			id6 := testIdentity6(t, "02:42:ac:11:00:02")
			now := time.Now()
			held := func(at time.Time) lease.Lease {
				return lease.Lease{
					Addr:             netip.MustParsePrefix("2001:db8::5/128"),
					Acquired:         at,
					Renew:            at.Add(10 * time.Second),
					Rebind:           at.Add(16 * time.Second),
					Expire:           at.Add(time.Hour),
					ServerDUID:       addrServer,
					IAID:             id6.IAID,
					Prefixes:         []lease.Addr6{{Addr: netip.MustParsePrefix("2001:db8:1:100::/64"), Valid: at.Add(time.Hour)}},
					PrefixServerDUID: tc.pdServer,
				}
			}
			mustRecord(t, r.Created6("ep-v6", "net-1", mac, id6.Bytes()))
			mustRecord(t, r.Bound("ep-v6"))
			mustRecord(t, r.Observed("ep-v6", lease.Event{Kind: lease.Acquired, Lease: held(now)}, nil))
			mustRecord(t, r.Observed("ep-v6", lease.Event{Kind: lease.Renewed, Lease: held(now.Add(10 * time.Second))}, nil))
			mustRecord(t, r.Observed("ep-v6", lease.Event{Kind: lease.Renewed, Lease: held(now.Add(20 * time.Second))}, nil))
			if err := compactNow(t, r, now.Add(time.Minute)); err != nil {
				t.Fatalf("compact: %v", err)
			}
			mustRecord(t, r.Close())

			reopened, err := OpenRecords(path, "instance-b")
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			rb, err := reopened.Rebuilt()
			if err != nil {
				t.Fatalf("Rebuilt: %v", err)
			}
			rec, ok := rb.ByID("ep-v6")
			if !ok {
				t.Fatal("the record did not survive the compaction")
			}
			grams, err := lease.BuildReleases(rec, 0x00abcdef)
			if err != nil || len(grams) != len(tc.wantGrams) {
				t.Fatalf("the reopened record releases as %d datagram(s), %v; want %d", len(grams), err, len(tc.wantGrams))
			}
			for i, want := range tc.wantGrams {
				msg, err := wire.DecodeV6(grams[i].Payload)
				if err != nil {
					t.Fatalf("datagram %d: %v", i, err)
				}
				sid, _ := msg.Options.First(wire.OptV6ServerID)
				if pds := msg.Options.Count(wire.OptV6IAPD); !bytes.Equal(sid, want.server) || pds != want.pds {
					t.Errorf("datagram %d goes to %x with %d IA_PD; want %x with %d (RFC 8415 section 18.2.7)",
						i, sid, pds, want.server, want.pds)
				}
			}
		})
	}
}
