// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// A second exchange under one MAC files a second lease no DHCPRELEASE ever returns (#962), and libnetwork only
// repeats a MAC the operator set, so the loser is refused, not parked (#110).
func TestIpamReserve_OneExchangePerHardwareAddress(t *testing.T) {
	p, b := ipamFixture(t)
	mac, _ := net.ParseMAC(ipamTestMAC)
	key := ipamReserveKey(b.PoolID, mac)

	first, mine := p.ipamReserves.begin(key, time.Now())
	if !mine {
		t.Fatal("the first caller does not own the exchange")
	}

	sn, err := ipamNetwork(ipamTestNetwork)
	if err != nil {
		t.Fatalf("ipamNetwork: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := p.ipamReserveAddress(context.Background(), ipamTestNetwork, sn, mac, "")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a second exchange was started under a hardware address one is already " +
				"running for; that is two leases at the server for one MAC, and the second " +
				"is never released")
		}
		if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("error %v does not wrap util.ErrIPAM", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second request neither ran nor was refused; it is waiting on the first, " +
			"and the daemon's own call budget is what would end it")
	}

	if n := p.ipamReserveDuplicateMAC.Load(); n != 1 {
		t.Errorf("ipam_reserve_duplicate_mac = %d, want 1", n)
	}
	if n := p.ipamReserves.len(); n != 1 {
		t.Errorf("%d reservations held, want 1: the refused request left one behind", n)
	}

	p.ipamReserves.finish(key, first, ipamReservation{
		addr:   netip.MustParsePrefix("192.168.99.10/24"),
		info:   dhcp.Info{IP: "192.168.99.10/24", Gateway: "192.168.99.1"},
		record: "rec-1",
	}, nil)
	got, ok := p.ipamReserves.take(key)
	if !ok || got.addr.String() != "192.168.99.10/24" {
		t.Error("the refused request disturbed the exchange that owns the key")
	}
}

func TestIpamReserves_AFailedReservationIsNotRemembered(t *testing.T) {
	s := newIPAMReserves()
	res, _ := s.begin("k", time.Now())
	s.finish("k", res, ipamReservation{}, context.DeadlineExceeded)
	if n := s.len(); n != 0 {
		t.Errorf("%d failed reservation(s) kept; the next attempt would be answered with a "+
			"stale error instead of running an exchange", n)
	}
}

func TestIpamReserves_TakeOnlyCompletes(t *testing.T) {
	s := newIPAMReserves()
	res, _ := s.begin("k", time.Now())
	if _, ok := s.take("k"); ok {
		t.Fatal("an in-flight reservation was taken")
	}
	s.finish("k", res, ipamReservation{addr: netip.MustParsePrefix("192.168.99.10/24"), record: "r"}, nil)
	if _, ok := s.take("k"); !ok {
		t.Error("a completed reservation was not taken")
	}
	if _, ok := s.take("k"); ok {
		t.Error("a reservation was taken twice; two endpoints would share one address")
	}
}

func TestSweepIPAMReservations_RetainsWhatDockerNeverBuilt(t *testing.T) {
	p, b := ipamFixture(t)
	mac, _ := net.ParseMAC(ipamTestMAC)
	id := p.recordReserved(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
	if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	key := ipamReserveKey(b.PoolID, mac)
	started := time.Now()
	res, _ := p.ipamReserves.begin(key, started)
	p.ipamReserves.finish(key, res, ipamReservation{
		addr: netip.MustParsePrefix("192.168.99.10/24"), record: id,
	}, nil)

	if n := p.sweepIPAMReservations(started.Add(time.Second)); n != 0 {
		t.Fatalf("%d reservation(s) swept a second after they were made; CreateEndpoint had "+
			"not run yet", n)
	}
	if got := ipamPhaseOf(t, p, id); got != lease.PhaseReserved {
		t.Fatalf("the record is in phase %v before the sweep, want %v", got, lease.PhaseReserved)
	}

	if n := p.sweepIPAMReservations(started.Add(tombstoneTTL + time.Second)); n != 1 {
		t.Fatalf("%d reservation(s) swept after the tombstone TTL, want 1", n)
	}
	if got := ipamPhaseOf(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the swept record is in phase %v, want %v — a reservation nothing retains "+
			"answers address lookups for the life of the network", got, lease.PhaseRetained)
	}
	if n := p.ipamReserves.len(); n != 0 {
		t.Errorf("%d reservation(s) left in memory after the sweep", n)
	}
}

func TestRetainOrphanedReservations_AtStartUpEveryReservationIsOrphaned(t *testing.T) {
	p, _ := ipamFixture(t)
	mac, _ := net.ParseMAC(ipamTestMAC)
	ident := dhcp.ClientIdentity([]byte{7})

	reserved := p.recordReserved(ipamTestNetwork, mac, ident)
	if err := p.records.Observed(reserved, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	live := p.recordCreated(ipamTestNetwork, mac, ident)
	if err := p.records.Observed(live, acquired("192.168.99.11/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Bound(live); err != nil {
		t.Fatalf("Bound: %v", err)
	}

	if n := retainOrphanedReservations(p.records, time.Now()); n != 1 {
		t.Fatalf("retained %d record(s), want 1", n)
	}
	if got := ipamPhaseOf(t, p, reserved); got != lease.PhaseRetained {
		t.Errorf("the orphaned reservation is in phase %v, want %v", got, lease.PhaseRetained)
	}
	if got := ipamPhaseOf(t, p, live); got != lease.PhaseJoined {
		t.Errorf("a live endpoint's record moved to %v at start-up", got)
	}
}

// Both exchanges now speak as the record, and the server keeps one binding per option-61 identity, so re-binding a
// record whose container still runs would take the old container's address. Keyed on MAC and address (#1047).
func TestIpamRebindCandidate_LeavesARunningEndpointsAddressAlone(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)
	ident := dhcp.ClientIdentity([]byte{7})
	newMAC, _ := net.ParseMAC("02:42:c0:a8:63:0b")

	tombstone := func(t *testing.T, p *Plugin, chaddr net.HardwareAddr, addr string) string {
		t.Helper()
		id := p.recordCreated(ipamTestNetwork, chaddr, ident)
		if err := p.records.Observed(id, acquired(addr, time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		if err := p.records.Retained(id, time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("Retained: %v", err)
		}
		return id
	}
	holds := func(p *Plugin, endpointID string, chaddr net.HardwareAddr, addr string) {
		p.rememberEndpoint(endpointID, endpointFingerprint{MAC: chaddr.String(), IPv4: addr}, dhcpHostname{})
	}

	t.Run("a live endpoint's record is not offered", func(t *testing.T) {
		p, _ := ipamFixture(t)
		id := tombstone(t, p, mac, "192.168.99.10/24")
		holds(p, "endpoint-still-running", mac, "192.168.99.10")

		gotID, gotAddr, gotIdent := p.ipamRebindCandidate(ipamTestNetwork, newMAC)
		if gotID != "" || gotAddr != "" || gotIdent != nil {
			t.Errorf("re-bound (%q, %q) from a record a running endpoint still holds. Both "+
				"clients would then renew under one identity and the running container "+
				"would be answered for somebody else's address", gotID, gotAddr)
		}
		if got := ipamPhaseOf(t, p, id); got != lease.PhaseRetained {
			t.Errorf("the record moved to %v; it was not taken, so nothing should have been "+
				"written to it", got)
		}
		if n := p.ipamRebindAmbiguous.Load(); n != 0 {
			t.Errorf("ipam_rebind_ambiguous = %d; skipping a held record is not an ambiguity", n)
		}
	})

	t.Run("the same hardware address on another address still re-binds", func(t *testing.T) {
		p, _ := ipamFixture(t)
		id := tombstone(t, p, mac, "192.168.99.10/24")
		holds(p, "endpoint-elsewhere", mac, "192.168.99.99")

		gotID, gotAddr, _ := p.ipamRebindCandidate(ipamTestNetwork, newMAC)
		if gotID != id || gotAddr != "192.168.99.10" {
			t.Errorf("re-bound (%q, %q), want (%q, 192.168.99.10). The guard keys on the "+
				"pair; keyed on the hardware address alone it refuses re-binds that are "+
				"correct", gotID, gotAddr, id)
		}
	})

	t.Run("the ordinary teardown still re-binds", func(t *testing.T) {
		p, _ := ipamFixture(t)
		id := tombstone(t, p, mac, "192.168.99.10/24")
		holds(p, "endpoint-gone", mac, "192.168.99.10")
		p.takeEndpoint("endpoint-gone")

		gotID, gotAddr, gotIdent := p.ipamRebindCandidate(ipamTestNetwork, newMAC)
		if gotID != id || gotAddr != "192.168.99.10" {
			t.Fatalf("re-bound (%q, %q), want (%q, 192.168.99.10) — this is the whole feature",
				gotID, gotAddr, id)
		}
		if !bytes.Equal(gotIdent, ident) {
			t.Errorf("identity %x, want %x", gotIdent, ident)
		}
	})

	t.Run("one held and one free candidate is not an ambiguity", func(t *testing.T) {
		p, _ := ipamFixture(t)
		other, _ := net.ParseMAC("02:42:c0:a8:63:0c")
		held := tombstone(t, p, mac, "192.168.99.10/24")
		free := tombstone(t, p, other, "192.168.99.11/24")
		holds(p, "endpoint-still-running", mac, "192.168.99.10")

		gotID, gotAddr, _ := p.ipamRebindCandidate(ipamTestNetwork, newMAC)
		if gotID != free || gotAddr != "192.168.99.11" {
			t.Errorf("re-bound (%q, %q), want (%q, 192.168.99.11)", gotID, gotAddr, free)
		}
		if n := p.ipamRebindAmbiguous.Load(); n != 0 {
			t.Errorf("ipam_rebind_ambiguous = %d; one of the two was never a candidate", n)
		}
		if got := ipamPhaseOf(t, p, held); got != lease.PhaseRetained {
			t.Errorf("the held record moved to %v", got)
		}
	})
}

// A RequestAddress carries no hostname or endpoint id, so several candidates are counted, not guessed (#110).
func TestIpamRebindCandidate_AmbiguityIsCountedNotGuessed(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)
	ident := dhcp.ClientIdentity([]byte{7})

	tombstone := func(t *testing.T, p *Plugin, addr string) string {
		t.Helper()
		id := p.recordCreated(ipamTestNetwork, mac, ident)
		if err := p.records.Observed(id, acquired(addr, time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		if err := p.records.Retained(id, time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("Retained: %v", err)
		}
		return id
	}

	t.Run("exactly one candidate is re-bound", func(t *testing.T) {
		p, _ := ipamFixture(t)
		id := tombstone(t, p, "192.168.99.10/24")
		newMAC, _ := net.ParseMAC("02:42:c0:a8:63:0b")
		gotID, gotAddr, gotIdent := p.ipamRebindCandidate(ipamTestNetwork, newMAC)
		if gotID != id {
			t.Errorf("re-bound record %q, want %q", gotID, id)
		}
		if gotAddr != "192.168.99.10" {
			t.Errorf("asking for %q, the tombstone holds 192.168.99.10", gotAddr)
		}
		if !bytes.Equal(gotIdent, ident) {
			t.Errorf("the candidate came back with identity %x, want %x. The exchange goes "+
				"out under this value: the MAC is a fresh one Docker minted, so an identity "+
				"derived from it asks the server as a client it has never seen and the "+
				"tombstone's address is handed to nobody.", gotIdent, ident)
		}
		if n := p.ipamRebindAmbiguous.Load(); n != 0 {
			t.Errorf("ipam_rebind_ambiguous = %d with one candidate", n)
		}
	})

	t.Run("two candidates are counted and neither is taken", func(t *testing.T) {
		p, _ := ipamFixture(t)
		tombstone(t, p, "192.168.99.10/24")
		tombstone(t, p, "192.168.99.11/24")
		newMAC, _ := net.ParseMAC("02:42:c0:a8:63:0b")
		gotID, gotAddr, gotIdent := p.ipamRebindCandidate(ipamTestNetwork, newMAC)
		if gotID != "" || gotAddr != "" || gotIdent != nil {
			t.Errorf("chose (%q, %q) between two candidates; there is nothing to choose on, "+
				"so one container would take another's address", gotID, gotAddr)
			_ = gotIdent
		}
		if n := p.ipamRebindAmbiguous.Load(); n != 1 {
			t.Errorf("ipam_rebind_ambiguous = %d, want 1 — the documented limit is only a "+
				"limit if it is visible", n)
		}
	})
}

func TestSaveNetwork_StampsTheIPAMSchemaVersion(t *testing.T) {
	p, b := ipamFixture(t)
	_ = p
	sn, err := loadNetwork(ipamTestNetwork)
	if err != nil {
		t.Fatalf("loadNetwork: %v", err)
	}
	if !sn.IPAMMode() {
		t.Fatal("the network loaded without its binding")
	}
	if sn.Binding.PoolID != b.PoolID {
		t.Errorf("binding PoolID = %q, want %q", sn.Binding.PoolID, b.PoolID)
	}
	path, err := stateFilePath(ipamTestNetwork)
	if err != nil {
		t.Fatalf("stateFilePath: %v", err)
	}
	v := schemaVersionOfFile(t, path)
	if v != stateSchemaVersion {
		t.Errorf(`"v" = %d, want %d — a 2.0 build must refuse this file rather than read `+
			`the options out of it and serve the network as null-IPAM`, v, stateSchemaVersion)
	}
	if stateSchemaVersion == stateSchemaVersionBase {
		t.Error("the IPAM schema version equals the base one, so nothing distinguishes a " +
			"file an older build may read from one it must refuse")
	}
}

func schemaVersionOfFile(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var vo versionedOptions
	if err := json.Unmarshal(raw, &vo); err != nil {
		t.Fatalf("%s is not a versioned options file: %v", path, err)
	}
	return vo.V
}

// Option 50 is a request, and libnetwork adopts whatever address comes back, so an unchecked ACK makes `--ip A`
// publish B (#110).
func TestIpamACKIsTheOneAsked(t *testing.T) {
	for _, c := range []struct {
		name, demanded, got string
		refuse              bool
	}{
		{"nothing was demanded, so the server chooses", "", "192.168.99.50", false},
		{"the server answered the address asked for", "192.168.99.50", "192.168.99.50", false},
		{"the server answered a different address", "192.168.99.50", "192.168.99.51", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := netip.ParseAddr(c.got)
			if err != nil {
				t.Fatal(err)
			}
			err = ipamACKIsTheOneAsked(got, c.demanded)
			if !c.refuse {
				if err != nil {
					t.Fatalf("refused %v against %q (%v); the address the server chose is the "+
						"product everywhere no --ip was typed", got, c.demanded, err)
				}
				return
			}
			if err == nil {
				t.Fatal("an ACK for another address was accepted. libnetwork adopts whatever " +
					"the driver returns, so `docker run --ip` succeeds with an address the " +
					"operator did not ask for and nothing reports the substitution.")
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("error %v does not wrap util.ErrIPAM", err)
			}
			for _, want := range []string{c.demanded, c.got} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is %q and does not name %q", err, want)
				}
			}
		})
	}
}

// OpRebind is written before any packet and clears the tombstone deadline, so a failed exchange must leave the
// candidate (#1047).
func TestIpamGiveUpAttempt_AFailedExchangeLeavesTheCandidate(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)
	ident := dhcp.ClientIdentity([]byte{7})

	t.Run("a re-bound record goes back to being a tombstone", func(t *testing.T) {
		p, _ := ipamFixture(t)
		id := p.recordCreated(ipamTestNetwork, mac, ident)
		if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		if err := p.records.Retained(id, time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("Retained: %v", err)
		}
		restarted, _ := net.ParseMAC("02:42:c0:a8:63:0b")
		gotID, gotAddr, _ := p.ipamRebindCandidate(ipamTestNetwork, restarted)
		if gotID != id || gotAddr != "192.168.99.10" {
			t.Fatalf("the candidate was not taken: (%q, %q)", gotID, gotAddr)
		}

		p.ipamGiveUpAttempt(id, true, time.Now())

		againID, againAddr, _ := p.ipamRebindCandidate(ipamTestNetwork, restarted)
		if againID != id {
			t.Errorf("the retry found candidate %q, want %q. The failed attempt consumed the "+
				"tombstone, so this container takes a fresh address and the documented "+
				"restart stability is gone with nothing counting it.", againID, id)
		}
		if againAddr != "192.168.99.10" {
			t.Errorf("the retry asks for %q, want 192.168.99.10", againAddr)
		}
	})

	t.Run("a record that was never a tombstone is closed", func(t *testing.T) {
		p, _ := ipamFixture(t)
		id := p.recordReserved(ipamTestNetwork, mac, ident)
		if id == "" {
			t.Fatal("recordReserved returned no record")
		}
		p.ipamGiveUpAttempt(id, false, time.Now())
		if got := ipamPhaseOf(t, p, id); got != lease.PhaseClosed {
			t.Errorf("phase = %v, want Closed. Nothing is owed to an address the plugin never "+
				"held, and a reservation left open answers address lookups forever.", got)
		}
	})
}

func TestIpamExchangeAddresses(t *testing.T) {
	for _, c := range []struct {
		name, requested, rebind string
		ask, demand             string
	}{
		{"--ip alone", "192.168.99.50", "", "192.168.99.50", "192.168.99.50"},
		{"a tombstone alone", "", "192.168.99.10", "192.168.99.10", ""},
		{"--ip wins over a tombstone, and is still the demand", "192.168.99.50", "192.168.99.10", "192.168.99.50", "192.168.99.50"},
		{"neither", "", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ask, demand := ipamExchangeAddresses(c.requested, c.rebind)
			if ask != c.ask {
				t.Errorf("asks for %q, want %q", ask, c.ask)
			}
			if demand != c.demand {
				t.Errorf("demands %q, want %q. A re-bind address demanded is a restarted "+
					"container refused when the server hands it a different one.", demand, c.demand)
			}
		})
	}
}

// Measured on the lane: a container re-binding the tombstone for 192.168.99.82 came back on .54, because the
// client-id derived from the MAC Docker had just minted (#1047).
func TestIpamExchangeClientID(t *testing.T) {
	fresh := []byte("fresh-from-the-mac")

	t.Run("a re-bind goes out under the record's identity", func(t *testing.T) {
		got := ipamExchangeClientID(fresh, dhcp.ClientIdentity([]byte{9, 9, 9}))
		if !bytes.Equal(got, []byte{9, 9, 9}) {
			t.Errorf("the exchange sends %x; the record's payload is 090909. A re-bind under "+
				"any other identity asks the server as a new client, and the address the "+
				"tombstone promised goes to nobody.", got)
		}
	})

	t.Run("no candidate leaves the fresh identity alone", func(t *testing.T) {
		if got := ipamExchangeClientID(fresh, nil); !bytes.Equal(got, fresh) {
			t.Errorf("a reservation with no tombstone sent %x, want the MAC-derived %x", got, fresh)
		}
	})

	t.Run("an identity this chassis did not write is refused, not truncated", func(t *testing.T) {
		if got := ipamExchangeClientID(fresh, []byte{0xff, 1, 2, 3}); !bytes.Equal(got, fresh) {
			t.Errorf("sent %x, derived from an identity in a shape no record here writes. "+
				"Trimming its first byte would put a value on the wire that nothing describes.", got)
		}
		if got := ipamExchangeClientID(fresh, []byte{0x00}); !bytes.Equal(got, fresh) {
			t.Errorf("sent %x for a type byte with no payload behind it", got)
		}
	})
}

// IFNAMSIZ is 16 with the terminator: a 14-character name plus "-p" got ERANGE for every bridge reservation (#110).
func TestIpamReserveLinkNames(t *testing.T) {
	const maxIfname = 15

	name, peer, err := ipamReserveLinkNames()
	if err != nil {
		t.Fatalf("ipamReserveLinkNames: %v", err)
	}
	for what, n := range map[string]string{"link": name, "peer": peer} {
		if len(n) > maxIfname {
			t.Errorf("the %s name %q is %d characters; the kernel refuses anything over %d "+
				"with ERANGE, which arrives at the operator as `numerical result out of range`",
				what, n, len(n), maxIfname)
		}
		if n == "" {
			t.Errorf("the %s half was not named", what)
		}
	}
	if name == peer {
		t.Errorf("both halves are called %q; a veth pair needs two names", name)
	}
	if !strings.HasPrefix(name, "dh-ipam-") {
		t.Errorf("link name %q does not carry the dh-ipam- prefix an operator greps for", name)
	}
	if !strings.HasSuffix(peer, "-ipam-dh") {
		t.Errorf("peer name %q does not carry the -ipam-dh suffix an operator greps for", peer)
	}

	other, otherPeer, err := ipamReserveLinkNames()
	if err != nil {
		t.Fatalf("ipamReserveLinkNames: %v", err)
	}
	if other == name || otherPeer == peer {
		t.Errorf("two reservations were named the same (%q/%q); concurrent reserves would "+
			"collide on EEXIST", name, peer)
	}
}

// A frame sent on a bridge port leaves the port, so the client's half must not be the enslaved one. On the lane,
// the reversed pair still carried the kernel's router solicitation onto the bridge while no DHCP left it (#110).
func TestIpamReserveVeth_TheClientHalfCarriesTheMAC(t *testing.T) {
	mac, err := net.ParseMAC("02:00:00:00:99:95")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	v := ipamReserveVeth("dh-ipam-aabbcc", "aabbcc-ipam-dh", mac)

	if v.LinkAttrs.Name != "dh-ipam-aabbcc" {
		t.Errorf("the veth is named %q, not the name the client is given", v.LinkAttrs.Name)
	}
	if got := v.LinkAttrs.HardwareAddr.String(); got != mac.String() {
		t.Errorf("the half the DHCP client runs on carries MAC %q, want the endpoint's %q. "+
			"The reservation exists to ask the server under the endpoint's own hardware "+
			"address; asking under any other address reserves an address for nobody.",
			got, mac)
	}
	if v.PeerHardwareAddr != nil {
		t.Errorf("the peer carries MAC %q. The peer is the BRIDGE PORT: the endpoint's MAC "+
			"on it puts that address on the segment from the side the client is not "+
			"listening on, which is the inversion that made every bridge reserve time out.",
			v.PeerHardwareAddr)
	}
	if v.PeerName != "aabbcc-ipam-dh" {
		t.Errorf("the peer is named %q, not the name the bridge half was given", v.PeerName)
	}
}

func TestIpamAcceptedReservation_NoACKBecomesAReservationUnchecked(t *testing.T) {
	for _, c := range []struct {
		name, ackIP, pool, demanded string
		wantErr                     string
		notErr                      string
	}{
		{
			name:  "in the pool and the address asked for",
			ackIP: "192.168.99.50/24", pool: "192.168.99.0/24", demanded: "192.168.99.50",
		},
		{
			name:  "no subnet and no --ip, so the server decides",
			ackIP: "10.0.0.7/8", pool: "0.0.0.0/0",
		},
		{
			name:  "an ACK outside the subnet the operator typed",
			ackIP: "10.0.0.7/8", pool: "192.168.99.0/24",
			wantErr: "10.0.0.7",
		},
		{
			name:  "an ACK for an address other than the --ip",
			ackIP: "192.168.99.51/24", pool: "192.168.99.0/24", demanded: "192.168.99.50",
			wantErr: "--ip",
		},
		{
			name:  "not an address with a prefix",
			ackIP: "192.168.99.50", pool: "0.0.0.0/0",
			wantErr: "not an address with a prefix",
		},
		{
			name:  "outside the subnet AND not the --ip: the network is the cause to print",
			ackIP: "10.0.0.7/8", pool: "192.168.99.0/24", demanded: "192.168.99.50",
			wantErr: "outside this network's subnet", notErr: "--ip asked for",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := ipamAcceptedReservation(dhcp.Info{IP: c.ackIP}, "rec-1", c.pool, c.demanded)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("refused %s against pool %q/demanded %q: %v", c.ackIP, c.pool, c.demanded, err)
				}
				if res.addr.String() != c.ackIP {
					t.Errorf("the reservation carries %v, the ACK was %s", res.addr, c.ackIP)
				}
				if res.record != "rec-1" {
					t.Errorf("the reservation lost its record id: %q", res.record)
				}
				return
			}
			if err == nil {
				t.Fatalf("an ACK of %s was accepted on pool %q with --ip %q. libnetwork adopts "+
					"whatever this returns, so the container comes up at an address the "+
					"operator neither asked for nor could have predicted.", c.ackIP, c.pool, c.demanded)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("the refusal is %q; it does not mention %q, which is what the "+
					"operator has to act on", err, c.wantErr)
			}
			if c.notErr != "" && strings.Contains(err.Error(), c.notErr) {
				t.Errorf("the refusal is %q, which is the --ip rule answering for a network "+
					"whose server serves a different subnet. The remedy it offers -- ask for "+
					"a different --ip -- cannot work here; the subnet is the thing to change.", err)
			}
			if res.addr.IsValid() {
				t.Errorf("a refused ACK still produced an address (%v); a caller that ignores "+
					"the error publishes it", res.addr)
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("the refusal is not an ErrIPAM, so the daemon does not render it as "+
					"an IPAM failure: %v", err)
			}
		})
	}
}

// libnetwork injects the MAC only when creating an endpoint, so a creating request is a replay apart from the MAC
// (#110).
func TestIpamRecordAnswersFor(t *testing.T) {
	addr := netip.MustParseAddr("192.168.99.50")
	mine := net.HardwareAddr{0x02, 0x42, 0x00, 0x00, 0x00, 0x01}
	theirs := net.HardwareAddr{0x02, 0x42, 0x00, 0x00, 0x00, 0x02}

	for _, c := range []struct {
		name   string
		rec    lease.Record
		mac    net.HardwareAddr
		refuse bool
	}{
		{"the replay shape carries no MAC, and the address is what Docker stored",
			lease.Record{ID: "r1", CHAddr: mine}, nil, false},
		{"the creating endpoint is the one the record belongs to",
			lease.Record{ID: "r1", CHAddr: mine}, mine, false},
		{"another endpoint holds it",
			lease.Record{ID: "r1", CHAddr: theirs}, mine, true},
		{"the record cannot say whose it is",
			lease.Record{ID: "r1"}, mine, true},
		{"neither the record nor the request says whose it is",
			lease.Record{ID: "r1"}, net.HardwareAddr{}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := ipamRecordAnswersFor(c.rec, c.mac, addr)
			if !c.refuse {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("record %q (CHAddr %v) was handed to %v. One address, two endpoints, "+
					"and the contradiction surfaces later at CreateEndpoint wearing a message "+
					"about a plugin restart that never happened.", c.rec.ID, c.rec.CHAddr, c.mac)
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("the refusal is not an ErrIPAM: %v", err)
			}
		})
	}
}

// The daemon does not pass `docker plugin enable --timeout` to the plugin, so budgets derive from the 30 s default
// (#110).
func TestIpamReserveBudget_IsSizedToTheDefaultCallBudget(t *testing.T) {
	if pluginCallBudget != 30*time.Second {
		t.Errorf("pluginCallBudget is %v, but docs/reference.md and pkg/util's empty-body "+
			"refusal both tell the operator 30s. Update the prose with the constant, or the "+
			"advice sends them to a value this plugin cannot work at.", pluginCallBudget)
	}
	got := ipamReserveBudget()
	if got >= pluginCallBudget {
		t.Errorf("one reservation may spend %v of a %v call budget, leaving nothing to write "+
			"the response in. The daemon stops listening first, and the call it re-sends "+
			"carries no body and is refused, which is the failure this margin exists to "+
			"avoid.", got, pluginCallBudget)
	}
	if got <= 0 {
		t.Fatalf("the reservation budget is %v, so every reserve is out of time before it "+
			"starts", got)
	}
}
