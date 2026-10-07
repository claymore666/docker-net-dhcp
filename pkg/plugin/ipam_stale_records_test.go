// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	dContainer "github.com/moby/moby/api/types/container"
	dNetwork "github.com/moby/moby/api/types/network"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const staleAddr = "192.168.99.60/24"

// staleRecord writes the record an endpoint leaves in the given phase, as the process the fixture has open.
func staleRecord(t *testing.T, p *Plugin, mac net.HardwareAddr, phase lease.Phase, until time.Duration) string {
	t.Helper()
	var id string
	if phase == lease.PhaseAdopted {
		id = newRecordID()
		if err := p.records.Adopted(id, ipamTestNetwork, mac, dhcp.ClientIdentity(mac)); err != nil {
			t.Fatalf("Adopted: %v", err)
		}
	} else {
		id = p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity(mac))
	}
	if id == "" {
		t.Fatal("no record was written")
	}
	if err := p.records.Observed(id, acquired(staleAddr, until), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if phase == lease.PhaseAdopted {
		return id
	}
	if err := p.records.Bound(id); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	if phase == lease.PhaseLeft {
		if err := p.records.Left(id); err != nil {
			t.Fatalf("Left: %v", err)
		}
	}
	return id
}

// staleEngine is a daemon that lists no endpoint on the IPAM network and the given running containers on it.
func staleEngine(running ...net.HardwareAddr) *fakeDocker {
	items := make([]dContainer.Summary, 0, len(running))
	for _, mac := range running {
		items = append(items, dContainer.Summary{NetworkSettings: &dContainer.NetworkSettingsSummary{
			Networks: map[string]*dNetwork.EndpointSettings{
				"lan": {NetworkID: ipamTestNetwork, MacAddress: dNetwork.HardwareAddr(mac)},
			},
		}})
	}
	return &fakeDocker{
		listResult: []dNetwork.Summary{{ID: ipamTestNetwork, Driver: testDHCPDriver}},
		inspectResult: map[string]dNetwork.Inspect{
			ipamTestNetwork: {ID: ipamTestNetwork, Driver: testDHCPDriver},
		},
		runningResult: items,
	}
}

func staleReleasesFor(sender *fakeSender, mac net.HardwareAddr) int {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	n := 0
	for _, r := range sender.recs {
		if bytes.Equal(r.CHAddr, mac) {
			n++
		}
	}
	return n
}

// staleServer is the DHCP server's view of the exchanges a reservation ran: the MAC and the address asked for.
type staleServer struct {
	mu    sync.Mutex
	asked []string
}

func (s *staleServer) install(t *testing.T, answer string) {
	t.Helper()
	prev := dhcpGetIP
	dhcpGetIP = func(_ context.Context, _ string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		s.mu.Lock()
		s.asked = append(s.asked, o.MAC.String()+" "+o.RequestedIP)
		s.mu.Unlock()
		if o.Records != nil && o.RecordID != "" {
			if err := o.Records.Observed(o.RecordID, acquired(answer, time.Hour), nil); err != nil {
				t.Errorf("Observed: %v", err)
			}
		}
		return dhcp.Info{IP: answer, Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = prev })
}

func (s *staleServer) exchanges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

func TestIPAMStrandedRecords_AnEndpointRemovedWhileThePluginWasDownIsGivenUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase lease.Phase
		until time.Duration
		want  lease.Phase
	}{
		{"joined, lease live", lease.PhaseJoined, time.Hour, lease.PhaseRetained},
		{"joined, lease expired", lease.PhaseJoined, -time.Hour, lease.PhaseClosed},
		{"left, lease live", lease.PhaseLeft, time.Hour, lease.PhaseRetained},
		{"left, lease expired", lease.PhaseLeft, -time.Hour, lease.PhaseClosed},
		{"adopted, lease live", lease.PhaseAdopted, time.Hour, lease.PhaseRetained},
		{"adopted, lease expired", lease.PhaseAdopted, -time.Hour, lease.PhaseClosed},
	} {
		t.Run(tc.name+": released at the deadline", func(t *testing.T) {
			gone := f0MAC(0x60)
			p, _, sender, journal := f0Fixture(t)
			id := staleRecord(t, p, gone, tc.phase, tc.until)
			f0Reopen(t, p, journal, "proc-2")
			p.docker = staleEngine()

			p.recoverEndpoints(context.Background(), testDaemonWait)

			if got := f0Rec(t, p, id).Phase; got != tc.want {
				t.Fatalf("phase = %v, want %v", got, tc.want)
			}
			p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle + time.Second))
			wantReleases := 0
			if tc.want == lease.PhaseRetained {
				wantReleases = 1
			}
			if got := staleReleasesFor(sender, gone); got != wantReleases {
				t.Errorf("the server saw %d RELEASE for %v, want %d under release_lease=on_remove", got, gone, wantReleases)
			}
		})

		t.Run(tc.name+": --ip for a new MAC reaches the server", func(t *testing.T) {
			gone, next := f0MAC(0x60), f0MAC(0x61)
			p, b, _, journal := f0Fixture(t)
			staleRecord(t, p, gone, tc.phase, tc.until)
			f0Reopen(t, p, journal, "proc-2")
			p.docker = staleEngine()
			p.recoverEndpoints(context.Background(), testDaemonWait)
			s2ReserveLink(t)
			srv := &staleServer{}
			srv.install(t, staleAddr)

			_, err := p.RequestAddress(context.Background(), RequestAddressRequest{
				PoolID:  b.PoolID,
				Address: "192.168.99.60",
				Options: map[string]string{ipamOptMacAddress: next.String()},
			})
			if err != nil {
				t.Fatalf("RequestAddress: %v", err)
			}
			if got := srv.exchanges(); len(got) != 1 || got[0] != next.String()+" 192.168.99.60" {
				t.Errorf("the server saw %q, want one exchange from %v asking for 192.168.99.60", got, next)
			}
		})
	}
}

func TestIPAMStrandedRecords_AJoinedRecordIsKeptWithoutTheSecondSource(t *testing.T) {
	running := f0MAC(0x60)
	for _, tc := range []struct {
		name   string
		docker func() *fakeDocker
	}{
		{"the container list still runs its MAC", func() *fakeDocker { return staleEngine(running) }},
		{"the container list failed", func() *fakeDocker {
			d := staleEngine()
			d.runningErr = errors.New("403 from the socket proxy")
			return d
		}},
		{"a running container on the network reports no MAC", func() *fakeDocker { return staleEngine(net.HardwareAddr{}) }},
		{"a running container has no network settings", func() *fakeDocker {
			d := staleEngine()
			d.runningResult = []dContainer.Summary{{}}
			return d
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, sender, journal := f0Fixture(t)
			id := staleRecord(t, p, running, lease.PhaseJoined, time.Hour)
			id6 := s2Created6(t, p, running, "2001:db8::60/64", time.Hour)
			if err := p.records.Bound(id6); err != nil {
				t.Fatalf("Bound: %v", err)
			}
			f0Reopen(t, p, journal, "proc-2")
			d := tc.docker()
			p.docker = d

			p.recoverEndpoints(context.Background(), testDaemonWait)

			if got := f0Rec(t, p, id).Phase; got != lease.PhaseJoined {
				t.Errorf("v4 phase = %v, want joined: the engine's endpoint list can be short, and nothing "+
					"else says this container is gone", got)
			}
			if got := f0Rec(t, p, id6).Phase; got != lease.PhaseJoined {
				t.Errorf("v6 phase = %v, want joined", got)
			}
			p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle + time.Second))
			if got := staleReleasesFor(sender, running); got != 0 {
				t.Errorf("the server saw %d RELEASE for a container that may still run", got)
			}
			if len(d.runningOpts) != 1 {
				t.Fatalf("the container list was asked %d times, want 1", len(d.runningOpts))
			}
			o := d.runningOpts[0]
			if o.All || !o.Filters["network"][ipamTestNetwork] || len(o.Filters) != 1 {
				t.Errorf("the container list was asked with %+v, want running containers on %s only", o, ipamTestNetwork)
			}
		})
	}
}

func TestIPAMStrandedRecords_TheJoinedV6HalfGoesWithItsV4Half(t *testing.T) {
	gone := f0MAC(0x60)
	p, _, _, journal := f0Fixture(t)
	id := staleRecord(t, p, gone, lease.PhaseJoined, time.Hour)
	id6 := s2Created6(t, p, gone, "2001:db8::60/64", time.Hour)
	if err := p.records.Bound(id6); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	f0Reopen(t, p, journal, "proc-2")
	p.docker = staleEngine()

	p.recoverEndpoints(context.Background(), testDaemonWait)

	r4, r6 := f0Rec(t, p, id), f0Rec(t, p, id6)
	if r4.Phase != lease.PhaseRetained || r6.Phase != lease.PhaseRetained {
		t.Fatalf("phases v4=%v v6=%v, want both retained", r4.Phase, r6.Phase)
	}
	if !r4.Deadline.Equal(r6.Deadline) {
		t.Errorf("the v4 tombstone runs to %v and the v6 one to %v", r4.Deadline, r6.Deadline)
	}
}

func TestIPAMStrandedRecords_ThisProcessAndCreatedRecordsNeverAskTheContainerList(t *testing.T) {
	mine, created := f0MAC(0x60), f0MAC(0x61)
	p, _, _, journal := f0Fixture(t)
	createdID := f0Created(t, p, created, f0Addr)
	f0Reopen(t, p, journal, "proc-2")
	mineID := staleRecord(t, p, mine, lease.PhaseJoined, time.Hour)
	d := staleEngine()
	p.docker = d

	p.recoverEndpoints(context.Background(), testDaemonWait)

	if got := f0Rec(t, p, mineID).Phase; got != lease.PhaseJoined {
		t.Errorf("this process's joined record is %v, want joined: a start here is listed only after "+
			"CreateEndpoint returns", got)
	}
	if got := f0Rec(t, p, createdID).Phase; got != lease.PhaseRetained {
		t.Errorf("the earlier process's created record is %v, want retained", got)
	}
	if len(d.runningOpts) != 0 {
		t.Errorf("the container list was asked %d times with no joined candidate, want 0", len(d.runningOpts))
	}
}

func TestIPAMReserve_AJournalThatCannotBeWrittenRequestsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broken bool
	}{
		{"control: journal healthy", false},
		{"journal write fails", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mac := f0MAC(0x21)
			p, b, _, _ := f0Fixture(t)
			p.docker = f0Docker()
			s2ReserveLink(t)
			srv := &staleServer{}
			srv.install(t, f0Addr)
			if tc.broken {
				_ = p.records.Close()
			}

			res, err := p.ipamReserveAddress(context.Background(), ipamTestNetwork, storedNetwork{Options: f0Options(), Binding: b}, mac, "")

			got := srv.exchanges()
			if !tc.broken {
				if err != nil || res == nil || res.record == "" || len(got) != 1 {
					t.Fatalf("control: err=%v exchanges=%q", err, got)
				}
				return
			}
			if len(got) != 0 {
				t.Errorf("the server saw %q: a lease with no record behind it is one nothing ever releases", got)
			}
			if !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "could not be written") {
				t.Errorf("err = %v, want the IPAM refusal naming the unwritten record", err)
			}
		})
	}
}

func TestIPAMStrandedRecords_AnotherNetworksEntryDoesNotKeepARecord(t *testing.T) {
	gone, neighbour := f0MAC(0x62), f0MAC(0x63)
	p, _, _, journal := f0Fixture(t)
	id := staleRecord(t, p, gone, lease.PhaseJoined, time.Hour)
	f0Reopen(t, p, journal, "proc-2")
	d := staleEngine(neighbour)
	d.runningResult[0].NetworkSettings.Networks["other"] = &dNetwork.EndpointSettings{NetworkID: "other-network", MacAddress: dNetwork.HardwareAddr(gone)}
	d.runningResult[0].NetworkSettings.Networks["l3"] = &dNetwork.EndpointSettings{NetworkID: "l3-network"}
	p.docker = d

	p.recoverEndpoints(context.Background(), testDaemonWait)

	if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
		t.Errorf("the record is %v, want retained: the MAC is in use on another network only, and an "+
			"entry with no MAC there says nothing about this one", got)
	}
}
