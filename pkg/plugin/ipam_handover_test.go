// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// h1Exchange records what each exchange sent and how many releases and pauses preceded it.
type h1Exchange struct {
	clientID  []byte
	requested string
	record    string
	releases  int
	waits     int
	answer    string
	fail      error
	calls     int
	sender    *fakeSender
	waited    *[]time.Duration
}

func h1Install(t *testing.T, x *h1Exchange) {
	t.Helper()
	prev := dhcpGetIP
	dhcpGetIP = func(_ context.Context, _ string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		x.calls++
		x.clientID = append([]byte(nil), o.ClientID...)
		x.requested, x.record = o.RequestedIP, o.RecordID
		x.releases, x.waits = x.sender.callCount(), len(*x.waited)
		if x.fail != nil {
			return dhcp.Info{}, dhcp.RAObservation{}, x.fail
		}
		if o.Records != nil {
			if err := o.Records.Observed(o.RecordID, acquired(x.answer, time.Hour), nil); err != nil {
				t.Errorf("Observed: %v", err)
			}
		}
		return dhcp.Info{IP: x.answer, Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = prev })
}

func h1Fixture(t *testing.T) (*Plugin, *ipamBinding, *h1Exchange, string) {
	t.Helper()
	p, b, sender, journal := f0Fixture(t)
	s2ReserveLink(t)
	x := &h1Exchange{answer: f0Addr, sender: sender, waited: f0HandOverWait(t)}
	h1Install(t, x)
	return p, b, x, journal
}

func TestIPAMHandOver_ReleasesUnderTheTombstonesMACThenAsksUnderTheOwnID(t *testing.T) {
	removed, restarted := f0MAC(0x01), f0MAC(0x02)
	p, b, x, _ := h1Fixture(t)
	old := f0Tombstone(t, p, removed, f0Addr)

	res, err := s2Reserve(t, p, b, f0Options(), restarted)
	if err != nil {
		t.Fatalf("the reserve failed: %v", err)
	}
	if x.releases != 1 || x.waits != 1 {
		t.Fatalf("the exchange ran after %d releases and %d pauses, want 1 and 1", x.releases, x.waits)
	}
	if got := (*x.waited)[0]; got != 2*time.Second {
		t.Fatalf("the hand-over paused %v, want 2s", got)
	}
	rel := x.sender.recs[0]
	if rel.ID != old || !bytes.Equal(rel.CHAddr, removed) || !bytes.Equal(rel.Identity, dhcp.ClientIdentity(removed)) {
		t.Fatalf("the release named record %q on %v with identity %x, want the tombstone %q on %v with its own identity",
			rel.ID, net.HardwareAddr(rel.CHAddr), rel.Identity, old, removed)
	}
	if own := resolveClientID(f0Options(), "", restarted); !bytes.Equal(x.clientID, own) {
		t.Fatalf("the exchange sent client id %x, want the new MAC's own %x", x.clientID, own)
	}
	if x.requested != "192.168.99.10" {
		t.Fatalf("the exchange asked for %q, want the tombstone's address", x.requested)
	}
	if x.record == old || res.record != x.record {
		t.Fatalf("the exchange wrote to %q and the reservation carries %q, want one fresh record beside %q", x.record, res.record, old)
	}
	if r := f0Rec(t, p, old); r.Phase != lease.PhaseClosed {
		t.Fatalf("the handed-over tombstone is %v, want closed", r.Phase)
	}
	if r := f0Rec(t, p, res.record); r.Phase != lease.PhaseCreated || !bytes.Equal(r.CHAddr, restarted) {
		t.Fatalf("the fresh record is %v on %v, want created on %v", r.Phase, net.HardwareAddr(r.CHAddr), restarted)
	}
	if renew, ok := dhcp.ClientIDPayload(f0Rec(t, p, res.record).Identity); !ok || !bytes.Equal(renew, x.clientID) {
		t.Fatalf("the fresh record renews under client id %x, want the %x the exchange was ACKed under", renew, x.clientID)
	}
	if !res.rebound {
		t.Fatal("the reservation is not marked re-bound, so a refused endpoint would close an address it holds")
	}
	if got := p.releasesSentV4.Load(); got != 1 {
		t.Fatalf("releases_sent = %d, want 1: the hand-over RELEASE is on the wire", got)
	}
	if got := p.ipamRebindMoved.Load(); got != 0 {
		t.Fatalf("ipam_rebind_moved = %d, want 0 for an ACK on the tombstone's address", got)
	}
}

func TestIPAMHandOver_AFailedExchangeKeepsTheTombstoneAndARetryHandsItOverAgain(t *testing.T) {
	removed, first, second := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)
	p, b, x, _ := h1Fixture(t)
	old := f0Tombstone(t, p, removed, f0Addr)

	x.fail = errors.New("no answer from the server")
	before := time.Now()
	if _, err := s2Reserve(t, p, b, f0Options(), first); err == nil {
		t.Fatal("the reserve succeeded")
	}
	fresh := x.record
	if r := f0Rec(t, p, fresh); r.Phase != lease.PhaseClosed {
		t.Fatalf("the fresh record is %v after a failed exchange, want closed", r.Phase)
	}
	if r := f0Rec(t, p, old); r.Phase != lease.PhaseRetained || r.Deadline.Before(before.Add(tombstoneTTL)) {
		t.Fatalf("the tombstone is %v to %v, want retained on a fresh window", r.Phase, r.Deadline)
	}

	x.fail = nil
	res, err := s2Reserve(t, p, b, f0Options(), second)
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if n := x.sender.callCount(); n != 2 {
		t.Fatalf("%d releases, want one per attempt", n)
	}
	// The claim moved the tombstone to the first attempt's MAC, so the retry releases under it, not the original.
	if got := x.sender.recs[1].CHAddr; !bytes.Equal(got, first) {
		t.Fatalf("the retry released under %v, want the first attempt's %v", net.HardwareAddr(got), first)
	}
	if r := f0Rec(t, p, old); r.Phase != lease.PhaseClosed || res.record == old || res.record == fresh {
		t.Fatalf("the tombstone is %v and the reservation carries %q, want closed and a new record", r.Phase, res.record)
	}
}

func TestIPAMHandOver_NotTakenKeepsTodaysExchange(t *testing.T) {
	removed, restarted := f0MAC(0x01), f0MAC(0x02)
	for _, tc := range []struct {
		name      string
		mac       net.HardwareAddr
		opts      func() DHCPNetworkOptions
		sendErr   error
		wantSends int
	}{
		{name: "the same MAC", mac: removed, opts: f0Options},
		{name: "a network client_id", mac: restarted, opts: func() DHCPNetworkOptions {
			o := f0Options()
			o.ClientID = "fixed-id"
			return o
		}},
		{name: "ipvlan", mac: restarted, opts: func() DHCPNetworkOptions {
			o := f0Options()
			o.Mode, o.Bridge, o.Parent = ModeIPvlan, "", f0Bridge
			return o
		}},
		{name: "the release not sent", mac: restarted, opts: f0Options, sendErr: errors.New("no route"), wantSends: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, x, _ := h1Fixture(t)
			x.sender.err = tc.sendErr
			old := f0Tombstone(t, p, removed, f0Addr)

			res, err := s2Reserve(t, p, b, tc.opts(), tc.mac)
			if err != nil {
				t.Fatalf("the reserve failed: %v", err)
			}
			if n := x.sender.callCount(); n != tc.wantSends || len(*x.waited) != 0 {
				t.Fatalf("%d releases and %d pauses, want %d and none", n, len(*x.waited), tc.wantSends)
			}
			want, _ := dhcp.ClientIDPayload(dhcp.ClientIdentity(removed))
			if !bytes.Equal(x.clientID, want) || res.record != old || x.record != old {
				t.Fatalf("the exchange sent %x on %q, reservation %q; want the tombstone's id %x on %q",
					x.clientID, x.record, res.record, want, old)
			}
			if got := p.releasesSentV4.Load(); got != 0 {
				t.Fatalf("releases_sent = %d, want 0", got)
			}
		})
	}
}

func TestIPAMHandOver_TwoTombstonesReleaseNothing(t *testing.T) {
	p, b, x, _ := h1Fixture(t)
	f0Tombstone(t, p, f0MAC(0x01), f0Addr)
	f0Tombstone(t, p, f0MAC(0x02), f0Addr2)

	if _, err := s2Reserve(t, p, b, f0Options(), f0MAC(0x03)); err != nil {
		t.Fatalf("the reserve failed: %v", err)
	}
	if n := x.sender.callCount(); n != 0 {
		t.Fatalf("%d releases went on the wire for a restart that could not tell which lease was its own", n)
	}
}

func TestIPAMHandOver_AnotherAddressInTheACKIsCountedNotRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		wantMoved int32
	}{
		{name: "the server moved it", wantMoved: 1},
		{name: "an --ip demand", requested: "192.168.99.11", wantMoved: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, x, _ := h1Fixture(t)
			x.answer = f0Addr2
			old := f0Tombstone(t, p, f0MAC(0x01), f0Addr)

			res, err := p.ipamReserveAddress(context.Background(), ipamTestNetwork,
				storedNetwork{Options: f0Options(), Binding: b}, f0MAC(0x02), tc.requested)
			if err != nil {
				t.Fatalf("the reserve failed: %v", err)
			}
			if res.record == old || res.addr.Addr().String() != "192.168.99.11" {
				t.Fatalf("the reservation is %v on %q, want %s on a fresh record", res.addr, res.record, f0Addr2)
			}
			if got := p.ipamRebindMoved.Load(); got != tc.wantMoved {
				t.Fatalf("ipam_rebind_moved = %d, want %d", got, tc.wantMoved)
			}
		})
	}
}

func TestIPAMHandOver_ACancelledPauseAsksNothingAndKeepsTheTombstone(t *testing.T) {
	p, b, x, _ := h1Fixture(t)
	old := f0Tombstone(t, p, f0MAC(0x01), f0Addr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.ipamReserveAddress(ctx, ipamTestNetwork, storedNetwork{Options: f0Options(), Binding: b}, f0MAC(0x02), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the reserve returned %v, want the cancellation", err)
	}
	if x.calls != 0 {
		t.Fatalf("%d exchanges ran after a cancelled pause", x.calls)
	}
	if r := f0Rec(t, p, old); r.Phase != lease.PhaseRetained {
		t.Fatalf("the tombstone is %v, want retained for the next start", r.Phase)
	}
}

func TestIPAMHandOverWait_ReturnsAtTheGapOrAtTheCancellation(t *testing.T) {
	if err := ipamHandOverWait(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("an uncancelled pause returned %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := ipamHandOverWait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled pause returned %v, want the cancellation", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("a cancelled pause took %v", waited)
	}
}

func TestIPAMHandOver_APluginRestartDuringTheExchangeKeepsTheTombstone(t *testing.T) {
	p, b, x, journal := h1Fixture(t)
	old := f0Tombstone(t, p, f0MAC(0x01), f0Addr)
	crashed := filepath.Join(t.TempDir(), recordFileName)
	inner := dhcpGetIP
	dhcpGetIP = func(ctx context.Context, n string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		raw, err := os.ReadFile(journal)
		if err != nil {
			t.Fatalf("reading the journal: %v", err)
		}
		if err := os.WriteFile(crashed, raw, 0o600); err != nil {
			t.Fatalf("copying the journal: %v", err)
		}
		return inner(ctx, n, o)
	}
	if _, err := s2Reserve(t, p, b, f0Options(), f0MAC(0x02)); err != nil {
		t.Fatalf("the reserve failed: %v", err)
	}

	f0Reopen(t, p, crashed, "proc-2")
	now := time.Now()
	if n := p.giveUpStrandedIPAMRecords(context.Background(), ipamTestNetwork, nil, now); n != 2 {
		t.Fatalf("the rule gave up %d records, want the tombstone and the fresh record", n)
	}
	if r := f0Rec(t, p, old); r.Phase != lease.PhaseRetained {
		t.Fatalf("the handed-over tombstone is %v after a restart mid-exchange, want retained", r.Phase)
	}
	if r := f0Rec(t, p, x.record); r.Phase != lease.PhaseClosed {
		t.Fatalf("the fresh record is %v, want closed: it holds nothing", r.Phase)
	}
	if id, addr, _, _ := p.ipamRebindCandidate(ipamTestNetwork, f0MAC(0x03), false, nil); id != old || addr != "192.168.99.10" {
		t.Fatalf("the next start re-bound (%q, %q), want the tombstone %q", id, addr, old)
	}
}
