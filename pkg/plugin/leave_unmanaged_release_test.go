// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
)

// reference.md, join_start_failures: on a release_lease=on_stop network an endpoint whose persistent client never
// started "still hands its address back at Leave", from its lease record (#962). settleFailedAttach has removed the
// manager by then, which is the state this test builds.
func TestLeave_OnStopReleasesAnEndpointWhoseClientNeverStarted(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnStop)
	if p.endpointFingerprints == nil {
		p.endpointFingerprints = make(map[string]endpointFingerprint)
	}
	mac, _ := net.ParseMAC("02:42:ac:11:00:07")
	liveRecord(t, p, mac, "192.168.99.70/24")
	p.rememberEndpoint("ep-start-failed", endpointFingerprint{
		MAC: mac.String(), IPv4: "192.168.99.70", RecordKey: mac,
	}, dhcpHostname{name: "web"})

	leaveErr := p.Leave(context.Background(), LeaveRequest{NetworkID: deferredTestNetwork, EndpointID: "ep-start-failed"})
	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: deferredTestNetwork, EndpointID: "ep-start-failed",
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + time.Hour))

	if got := sender.callCount(); got == 0 {
		t.Errorf("no DHCPRELEASE was sent at Leave (it returned %v), at DeleteEndpoint or by the record sweep; "+
			"the address stays leased until the server's expiry", leaveErr)
	}
}

func leaveUnmanagedAndDelete(t *testing.T, p *Plugin, mac net.HardwareAddr) {
	t.Helper()
	if p.endpointFingerprints == nil {
		p.endpointFingerprints = make(map[string]endpointFingerprint)
	}
	p.rememberEndpoint("ep-start-failed", endpointFingerprint{
		MAC: mac.String(), IPv4: "192.168.99.70", RecordKey: mac,
	}, dhcpHostname{name: "web"})
	_ = p.Leave(context.Background(), LeaveRequest{NetworkID: deferredTestNetwork, EndpointID: "ep-start-failed"})
	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: deferredTestNetwork, EndpointID: "ep-start-failed",
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
}

func TestDeleteEndpoint_AnUnmanagedOnStopEndpointHandsBothFamiliesBack(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnStop)
	mac, _ := net.ParseMAC("02:42:ac:11:00:08")
	id4 := liveRecord(t, p, mac, "192.168.99.70/24")
	id6 := p.recordCreated6(deferredTestNetwork, mac, releaseTestIdentity6())
	if err := p.records.Observed(id6, acquired6("2001:db8::70/64", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	leaveUnmanagedAndDelete(t, p, mac)

	if got := sender.callCount(); got != 2 {
		t.Errorf("%d release(s) at DeleteEndpoint for an endpoint holding both families, want 2", got)
	}
	for _, id := range []string{id4, id6} {
		if got := recordPhase(t, p, id); got != lease.PhaseClosed {
			t.Errorf("record %s is %v after its release, want CLOSED", id, got)
		}
	}
	tombs, err := loadTombstones()
	if err != nil {
		t.Fatalf("loadTombstones: %v", err)
	}
	if len(tombs) != 0 {
		t.Errorf("a released endpoint left %d tombstone(s), want none: the server may hand the address on", len(tombs))
	}
}

func TestDeleteEndpoint_AnUnmanagedLeaveReleasesNothingOffOnStop(t *testing.T) {
	for _, value := range []string{ReleaseNever, ReleaseOnRemove} {
		t.Run(value, func(t *testing.T) {
			p, sender := deferredPlugin(t, value)
			mac, _ := net.ParseMAC("02:42:ac:11:00:09")
			id := liveRecord(t, p, mac, "192.168.99.70/24")

			leaveUnmanagedAndDelete(t, p, mac)

			if got := sender.callCount(); got != 0 {
				t.Errorf("release_lease=%s sent %d release(s) at DeleteEndpoint, want 0", value, got)
			}
			if got := recordPhase(t, p, id); got != lease.PhaseRetained {
				t.Errorf("the record is %v, want RETAINED for the restart window", got)
			}
		})
	}
}

func TestDeleteEndpoint_AFailedUnmanagedReleaseKeepsTheAddressHeld(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnStop)
	sender.err = errors.New("sendto: network is down")
	mac, _ := net.ParseMAC("02:42:ac:11:00:0a")
	id := liveRecord(t, p, mac, "192.168.99.70/24")

	leaveUnmanagedAndDelete(t, p, mac)

	if got := sender.callCount(); got != 1 {
		t.Errorf("%d release attempt(s), want 1", got)
	}
	if got := recordPhase(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the record is %v after a release that did not leave, want RETAINED: the server still holds the address", got)
	}
	if got := p.releaseFailuresV4.Load(); got != 1 {
		t.Errorf("release_failures_v4 = %d, want 1", got)
	}
}

func TestDeleteEndpoint_AnUnmanagedLeaveSendsNothingForAClosedRecord(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnStop)
	mac, _ := net.ParseMAC("02:42:ac:11:00:0b")
	id := liveRecord(t, p, mac, "192.168.99.70/24")
	p.closeRecord(id)

	leaveUnmanagedAndDelete(t, p, mac)

	if got := sender.callCount(); got != 0 {
		t.Errorf("%d release(s) for a record already CLOSED, want 0: the address was handed back before", got)
	}
}
