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

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// TestIpamReserve_ASecondRequestJoinsTheFirst is defeat row 14 and the
// reason ipamReserves exists at all.
//
// A plugin enabled with a --timeout below the reserve's budget has its
// RequestAddress re-sent with the SAME body while the first exchange is
// still running. Two exchanges for one endpoint means two DISCOVERs, two
// leases on the server and one container, and the second lease is never
// released: nothing holds it and no DHCPRELEASE goes on the wire.
//
// Driven through the real entry point, with the exchange occupied by
// hand: the netlink and DHCP half needs a parent NIC and a server, and
// the concurrency half is what this is about.
func TestIpamReserve_ASecondRequestJoinsTheFirst(t *testing.T) {
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

	joined := make(chan *ipamReservation, 1)
	go func() {
		res, err := p.ipamReserveAddress(context.Background(), ipamTestNetwork, sn, mac, "")
		if err != nil {
			joined <- nil
			return
		}
		joined <- res
	}()

	// Let the joiner reach the wait before the result lands, so the test
	// drives the joining path rather than the already-finished one.
	deadline := time.Now().Add(2 * time.Second)
	for p.ipamReserveJoined.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := p.ipamReserveJoined.Load(); n != 1 {
		t.Fatalf("ipam_reserve_joined = %d, want 1 — the second request started its own "+
			"exchange, which is a second lease for one container", n)
	}

	p.ipamReserves.finish(key, first, ipamReservation{
		addr:   netip.MustParsePrefix("192.168.99.10/24"),
		info:   dhcp.Info{IP: "192.168.99.10/24", Gateway: "192.168.99.1"},
		record: "rec-1",
	}, nil)

	select {
	case got := <-joined:
		if got == nil {
			t.Fatal("the joining request failed; it must receive the first one's result")
		}
		if got.addr.String() != "192.168.99.10/24" {
			t.Errorf("the joiner got %v, the exchange won 192.168.99.10/24", got.addr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the joining request never woke; a re-sent body would hang the daemon's retry")
	}
}

// TestIpamReserves_AFailedReservationIsNotRemembered. The next attempt
// has to run a fresh exchange rather than be handed this one's error
// forever.
func TestIpamReserves_AFailedReservationIsNotRemembered(t *testing.T) {
	s := newIPAMReserves()
	res, _ := s.begin("k", time.Now())
	s.finish("k", res, ipamReservation{}, context.DeadlineExceeded)
	if n := s.len(); n != 0 {
		t.Errorf("%d failed reservation(s) kept; the next attempt would be answered with a "+
			"stale error instead of running an exchange", n)
	}
}

// TestIpamReserves_TakeOnlyCompletes. CreateEndpoint must not consume a
// reservation whose exchange is still running: it would read a zero
// address and bind the endpoint to nothing.
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

// TestSweepIPAMReservations_RetainsWhatDockerNeverBuilt.
//
// Only a RETAINED record carries a deadline, so a RESERVED one with a
// live lease and no endpoint -- the daemon spent every retry, or fell
// over between RequestAddress and CreateEndpoint -- would keep answering
// address lookups until the network is deleted.
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

	// Nothing is swept while the endpoint could still arrive. The
	// control matters: a sweeper that retained immediately would take
	// the address away from the CreateEndpoint on its way in.
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

// TestRetainOrphanedReservations_AtStartUpEveryReservationIsOrphaned.
//
// The age does not need measuring here, and that is what makes this arm
// different from the sweeper. A RESERVED record is one an address was
// answered for and no CreateEndpoint ever bound a link to; the process
// that could still have bound it is gone, and the fold admits no Create
// from another.
func TestRetainOrphanedReservations_AtStartUpEveryReservationIsOrphaned(t *testing.T) {
	p, _ := ipamFixture(t)
	mac, _ := net.ParseMAC(ipamTestMAC)
	ident := dhcp.ClientIdentity([]byte{7})

	reserved := p.recordReserved(ipamTestNetwork, mac, ident)
	if err := p.records.Observed(reserved, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	// A live endpoint beside it, which must be left alone: retaining a
	// JOINED record would have restart recovery decline to resume a
	// container that is running.
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

// TestIpamRebindCandidate_AmbiguityIsCountedNotGuessed.
//
// A RequestAddress carries no hostname and no endpoint id, so when
// several containers on one network restart together there is nothing to
// match a request to a previous lease on. Picking one would hand a
// container's address to a different container.
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

// TestSaveNetwork_StampsTheIPAMSchemaVersion is the other half of
// TestSaveOptions_StampsSchemaVersion: a file carrying a pool binding
// says so in its version, so a 2.0 build refuses it rather than reading
// the options out of it and serving the network on the null path.
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

// TestIpamACKIsTheOneAsked is the design's §3 rule for the `--ip`
// shape, and it is a rule about libnetwork rather than about DHCP.
//
// Option 50 is a REQUEST: a server may answer another address because
// the one asked for is reserved for a different client, already leased,
// or outside the range it serves. libnetwork does not compare the
// driver's answer to the address it preferred -- it adopts whatever
// comes back -- so an unchecked ACK makes `docker run --ip A` publish B
// and exit 0, with nothing anywhere saying the pin did not take.
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
			// The operator has to be able to tell which address they
			// asked for from which one the server offered.
			for _, want := range []string{c.demanded, c.got} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is %q and does not name %q", err, want)
				}
			}
		})
	}
}

// TestIpamGiveUpRecord_AFailedExchangeLeavesTheCandidate.
//
// ipamRebindCandidate writes OpRebind before any packet goes out --
// the exchange has to run under the identity the server already has a
// lease filed under -- and that fold clears the tombstone deadline. So
// a reserve that then fails has taken the candidate off the board. If
// it closes the record, a container restarted on its own during a brief
// outage finds nothing to re-bind on the retry seconds later, takes a
// fresh address, and no counter moves: ipam_rebind_ambiguous is about
// two candidates, not none.
func TestIpamGiveUpRecord_AFailedExchangeLeavesTheCandidate(t *testing.T) {
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

		// The exchange fails -- the server is unreachable, or the ACK
		// is refused by the subnet rule.
		p.ipamGiveUpRecord(id, true)

		// The retry, well inside the window.
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
		p.ipamGiveUpRecord(id, false)
		if got := ipamPhaseOf(t, p, id); got != lease.PhaseClosed {
			t.Errorf("phase = %v, want Closed. Nothing is owed to an address the plugin never "+
				"held, and a reservation left open answers address lookups forever.", got)
		}
	})
}

// TestIpamExchangeAddresses is the line between a pin and a preference.
//
// `--ip` is a demand and an ACK for another address is refused. A
// tombstone's address is how a restarted container keeps what it had,
// and the server answering otherwise is the documented limit: refusing
// there would turn "your address moved" into "your container will not
// start", on the one path that exists to make restarts survivable.
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

// TestIpamExchangeClientID is the fix for the address a restarted
// container lost.
//
// The lane measured it end to end: a container re-bound the tombstone
// that held 192.168.99.82 and came back on 192.168.99.54, because the
// exchange went out under a client-id derived from the MAC Docker had
// just minted for the new endpoint. The server had the lease filed
// under the old identity and had no reason to hand it to a client it
// had never heard of.
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
		// A DUID-shaped value: a type byte that is not the opaque one.
		if got := ipamExchangeClientID(fresh, []byte{0xff, 1, 2, 3}); !bytes.Equal(got, fresh) {
			t.Errorf("sent %x, derived from an identity in a shape no record here writes. "+
				"Trimming its first byte would put a value on the wire that nothing describes.", got)
		}
		if got := ipamExchangeClientID(fresh, []byte{0x00}); !bytes.Equal(got, fresh) {
			t.Errorf("sent %x for a type byte with no payload behind it", got)
		}
	})
}

// TestIpamReserveLinkNames is the fix for every bridge-mode reservation.
//
// IFNAMSIZ is 16 including the terminator. The first edition named the
// link (14 characters) and glued "-p" on at the LinkAdd, which is 16,
// and netlink answered a bare ERANGE -- "numerical result out of range"
// -- for every bridge reservation the lane ran. Both names come from
// one function so that one test measures the pair.
func TestIpamReserveLinkNames(t *testing.T) {
	const maxIfname = 15 // IFNAMSIZ - 1

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

// TestIpamReserveVeth_TheClientHalfCarriesTheMAC is the bridge-mode
// reserve's direction of travel, which the first edition had backwards.
//
// The caller hands the DHCP client the link called `name`. A frame
// transmitted on a bridge port leaves the port rather than entering the
// bridge, so the half the client runs on must NOT be the one enslaved:
// with the roles swapped the DISCOVER goes to the dangling end of the
// veth and nothing on the segment ever sees it. What made that hard to
// see in the lane is that the direction which DOES work carried the
// kernel's own IPv6 router solicitation from the dangling end onto the
// bridge, so the server logged the reservation's MAC and the link
// looked present while no DHCP ever left it.
//
// Two halves, asserted separately because a mutant can get one right:
// the MAC belongs to `name`, and the peer must not carry it -- a peer
// wearing the endpoint's MAC would put that address on the bridge from
// the wrong side and a server answering it would answer the wrong link.
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

// TestIpamAcceptedReservation_NoACKBecomesAReservationUnchecked drives
// the APPLICATION of the two acceptance rules, not the rules.
//
// Both rules had unit tests already and both were applied on the
// success path of runIPAMReserve, which needs netlink and a live DHCP
// server to enter. So the unit suite could refuse a bad address in
// isolation while a deleted call site -- the shape a cleanup takes --
// changed nothing it could see. The rules now live in the constructor
// that BUILDS the reservation, which is why this test can reach them,
// and why removing one no longer compiles.
//
// The last case fixes the order. When an ACK breaks both rules the
// operator is told about the network, because the other remedy --
// asking for a different --ip -- would not help on a network whose
// server serves a different subnet.
func TestIpamAcceptedReservation_NoACKBecomesAReservationUnchecked(t *testing.T) {
	for _, c := range []struct {
		name, ackIP, pool, demanded string
		wantErr                     string
		// notErr is the OTHER rule's wording, for the case that breaks
		// both. Without it "the network cause wins" is a sentence in a
		// comment: the --ip refusal quotes the offered address too, so
		// an assertion on the address alone passes either order.
		notErr string
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

// TestIpamRecordAnswersFor is the guard between an address replay and
// `docker run --ip` for an address someone else already holds.
//
// libnetwork injects the hardware address only when it is CREATING an
// endpoint, so a creating request for an address a running container
// holds is byte-for-byte that container's replay apart from the MAC.
// Answering it allocates one address to two endpoints and moves
// ipam_replay_hits for something that is not a replay.
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
		// An empty-but-present MAC against an empty CHAddr. Dropping
		// the length guard from the match leaves bytes.Equal comparing
		// two empty slices, which is TRUE: a record belonging to nobody
		// would answer a request identifying nobody, and every other
		// case in this table still passes. That is the whole mutant.
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
