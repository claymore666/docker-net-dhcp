// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

func recordingPlugin(t *testing.T) *Plugin {
	t.Helper()
	r, err := dhcp.OpenRecords(filepath.Join(t.TempDir(), recordFileName), "test-instance")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return &Plugin{records: r}
}

func acquired(addr string, until time.Duration) lease.Event {
	return lease.Event{
		Kind: lease.Acquired,
		Lease: lease.Lease{
			Addr:     netip.MustParsePrefix(addr),
			Gateway:  netip.MustParseAddr("192.168.99.1"),
			ServerID: netip.MustParseAddr("192.168.99.1"),
			Expire:   time.Now().Add(until),
		},
	}
}

// TestRecordLifecycle_TheJoinManagerResumesTheOneShotsLease is the
// whole reason the record exists, driven end to end without a network.
//
// CreateEndpoint's one-shot wins a lease and stops; the stop arrives as
// Lost{ReasonStopped}, which is this chassis's own cancellation and NOT
// a loss. If it were folded as one, the Join manager below would find
// nothing to resume and would DISCOVER — and the container would come
// up on whatever address the server offered next, silently.
func TestRecordLifecycle_TheJoinManagerResumesTheOneShotsLease(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0a")

	id := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{1, 2, 3}))
	if id == "" {
		t.Fatal("no record was created")
	}
	if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed(acquired): %v", err)
	}
	if err := p.records.Observed(id, lease.Event{Kind: lease.Lost, Reason: proto.ReasonStopped}, nil); err != nil {
		t.Fatalf("Observed(stopped): %v", err)
	}

	m := &dhcpManager{plugin: p, joinReq: JoinRequest{NetworkID: "net-1"}}
	m.MacAddress = mac
	gotID, res := m.resumeFromRecord()
	if gotID != id {
		t.Fatalf("Join resumed record %q, CreateEndpoint wrote %q", gotID, id)
	}
	if res.Lease == nil {
		t.Fatal("nothing to resume: the Join manager would DISCOVER and can be handed a different address")
	}
	if got := res.Lease.Addr.String(); got != "192.168.99.10/24" {
		t.Errorf("resuming %s, the one-shot won 192.168.99.10/24", got)
	}
	if res.Prefer != "" {
		t.Errorf("both a resume and a preference (%q); they are mutually exclusive by construction", res.Prefer)
	}

	// resumeFromRecord binds as well, and the bind is what a second
	// plugin process must not repeat.
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	rec, _ := rb.ByID(id)
	if rec.Phase != lease.PhaseJoined {
		t.Errorf("phase after Join = %s, want joined", rec.Phase)
	}
	if rec.Counters.Rejects != 0 {
		t.Fatalf("the fold refused %d event(s); last %v", rec.Counters.Rejects, rec.LastReject)
	}
}

// TestRecordBound_ARecoveredRecordIsNotBoundTwice is the silent trap.
//
// The fold accepts a bind only from CREATED or ADOPTED. A plugin
// restart resumes a record a previous process left JOINED, and a bind
// written unconditionally there is REFUSED — with no error to the
// writer, because a rejected event still folds into a record with its
// Rejects counter bumped and nothing else moved. The only observable
// is the counter, so that is what this asserts.
func TestRecordBound_ARecoveredRecordIsNotBoundTwice(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0b")

	id := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{4, 5, 6}))
	if err := p.records.Observed(id, acquired("192.168.99.11/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	m := &dhcpManager{plugin: p, joinReq: JoinRequest{NetworkID: "net-1"}}
	m.MacAddress = mac
	if _, res := m.resumeFromRecord(); res.Lease == nil {
		t.Fatal("first Join found nothing to resume")
	}
	// The restart: a second manager on the same record, which is now
	// JOINED.
	if _, res := m.resumeFromRecord(); res.Lease == nil {
		t.Fatal("the restart found nothing to resume")
	}

	rb, _ := p.records.Rebuilt()
	rec, _ := rb.ByID(id)
	if rec.Counters.Rejects != 0 {
		t.Errorf("the second Join's bind was refused (%d rejects, last %v); "+
			"recovery after a plugin restart would leave the record with a hole in its history",
			rec.Counters.Rejects, rec.LastReject)
	}
	if rec.Phase != lease.PhaseJoined {
		t.Errorf("phase = %s, want joined", rec.Phase)
	}
}

// TestRetainRecordFor_TombstonesTheIdentity closes the other end: a
// record left JOINED after its endpoint is gone would have plugin-start
// recovery resume a lease for a container that no longer exists.
func TestRetainRecordFor_TombstonesTheIdentity(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0c")

	id := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{7, 8, 9}))
	if err := p.records.Observed(id, acquired("192.168.99.12/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	m := &dhcpManager{plugin: p, joinReq: JoinRequest{NetworkID: "net-1"}}
	m.MacAddress = mac
	if _, res := m.resumeFromRecord(); res.Lease == nil {
		t.Fatal("nothing to resume")
	}
	p.recordLeft(id)
	p.retainRecordFor("net-1", mac.String())

	rb, _ := p.records.Rebuilt()
	rec, _ := rb.ByID(id)
	if rec.Phase != lease.PhaseRetained {
		t.Fatalf("phase = %s, want retained", rec.Phase)
	}
	if rec.Counters.Rejects != 0 {
		t.Fatalf("the fold refused %d event(s); last %v", rec.Counters.Rejects, rec.LastReject)
	}

	// A tombstone's address was GIVEN UP. It may be asked for as a
	// preference in a DISCOVER — which is what makes a restarted
	// container keep its address — but it must not be claimed with an
	// INIT-REBOOT, which asserts a lease this identity no longer holds.
	_, res, ok := p.records.Resume("net-1", mac, time.Now())
	if !ok {
		t.Fatal("the tombstone answered nothing at all")
	}
	if res.Lease != nil {
		t.Error("a tombstoned record offered an INIT-REBOOT: the address was given up and claiming it back asserts a lease it does not hold")
	}
	if res.Prefer != "192.168.99.12" {
		t.Errorf("preference = %q, want 192.168.99.12 — a restarted container would not get its address back", res.Prefer)
	}
}

// TestRecordCreated_ASecondEndpointDoesNotShareTheFirstsRecord pins
// the index the whole scheme is keyed on. An index on the MAC alone
// would collapse one machine on two networks into one record; an index
// on the address alone would collapse two networks handing out the same
// private address.
func TestRecordCreated_ASecondEndpointDoesNotShareTheFirstsRecord(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0d")

	a := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{1}))
	b := p.recordCreated("net-2", mac, dhcp.ClientIdentity([]byte{1}))
	if a == b || a == "" || b == "" {
		t.Fatalf("record ids %q and %q", a, b)
	}
	if err := p.records.Observed(a, acquired("192.168.99.13/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	id, res, ok := p.records.Resume("net-2", mac, time.Now())
	if !ok {
		t.Fatal("net-2 has no record")
	}
	if id != b {
		t.Errorf("net-2 resumed net-1's record")
	}
	if res.Lease != nil {
		t.Errorf("net-2 offered net-1's lease %s", res.Lease.Addr)
	}
}
