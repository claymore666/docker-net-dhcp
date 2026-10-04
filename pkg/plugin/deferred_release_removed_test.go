// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	cerrdefs "github.com/containerd/errdefs"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const (
	leftAsItIs     = "leaving the record as it is"
	closedAsRemove = "Docker no longer has"
)

func networkNotFound() error {
	return fmt.Errorf("network %s not found: %w", deferredTestNetwork, cerrdefs.ErrNotFound)
}

func heldRecord6(t *testing.T, p *Plugin, deadline time.Time) string {
	t.Helper()
	id6 := p.recordCreated6(deferredTestNetwork, deferredMAC(0x02), releaseTestIdentity6())
	if id6 == "" {
		t.Fatal("no v6 record was created")
	}
	if err := p.records.Observed(id6, acquired6("2001:db8::10/64", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Retained(id6, deadline); err != nil {
		t.Fatalf("Retained: %v", err)
	}
	return id6
}

func TestDeferredRelease_ARemovedNetworksHeldRecordsAreClosedOnceAndNeverReadAgain(t *testing.T) {
	for _, value := range []string{ReleaseNever, ReleaseOnStop} {
		t.Run(value, func(t *testing.T) {
			p, sender := deferredPlugin(t, value)
			docker := &fakeDocker{inspectErr: networkNotFound()}
			p.docker = docker
			deadline := time.Now()
			id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
			id6 := heldRecord6(t, p, deadline)

			if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: deferredTestNetwork}); err != nil {
				t.Fatalf("DeleteNetwork: %v", err)
			}
			if got := recordPhase(t, p, id); got != lease.PhaseRetained {
				t.Fatalf("DeleteNetwork left the record %v, want RETAINED, so the sweep has nothing to close", got)
			}

			out := captureDebugLog(t, func() {
				for pass := 1; pass <= 3; pass++ {
					p.sweepDeferredReleases(deadline.Add(time.Duration(pass) * time.Hour))
				}
			})

			for _, rid := range []string{id, id6} {
				if got := recordPhase(t, p, rid); got != lease.PhaseClosed {
					t.Errorf("record %s is %v after the network was removed, want CLOSED", rid, got)
				}
			}
			if got := sender.callCount(); got != 0 {
				t.Errorf("%d datagram(s) left the host under release_lease=%s, want 0", got, value)
			}
			if got := docker.inspectCalls; got != 1 {
				t.Errorf("NetworkInspect was called %d time(s) over three passes, want 1: the v4 and v6 "+
					"records share one network, and a closed record is not visited again", got)
			}
			if strings.Contains(out, leftAsItIs) {
				t.Errorf("the sweep still says it is leaving a removed network's record:\n%s", out)
			}
			if got := strings.Count(out, closedAsRemove); got != 1 {
				t.Errorf("the closing line was written %d time(s) over three passes, want 1:\n%s", got, out)
			}
			if !strings.Contains(out, "closed=2") {
				t.Errorf("the closing line does not count both records:\n%s", out)
			}
		})
	}
}

func TestDeferredRelease_AnAbsentOptionsFileIsNotARemovalUnlessDockerSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		docker *fakeDocker
	}{
		{name: "network-present", docker: &fakeDocker{}},
		{name: "daemon-unreachable", docker: &fakeDocker{inspectErr: errors.New("Cannot connect to the Docker daemon")}},
		{name: "no-client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sender := deferredPlugin(t, ReleaseOnRemove)
			if tc.docker != nil {
				p.docker = tc.docker
			}
			deadline := time.Now()
			id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
			if err := deleteOptions(deferredTestNetwork); err != nil {
				t.Fatalf("deleteOptions: %v", err)
			}

			out := captureDebugLog(t, func() {
				p.sweepDeferredReleases(deadline.Add(time.Hour))
				p.sweepDeferredReleases(deadline.Add(2 * time.Hour))
			})

			if got := recordPhase(t, p, id); got != lease.PhaseRetained {
				t.Errorf("the record is %v, want RETAINED: a null-mode network outlives a failed options "+
					"write, and a later backfill lets the sweep release it", got)
			}
			if got := sender.callCount(); got != 0 {
				t.Errorf("%d release(s) went out with no options to say how, want 0", got)
			}
			if got := strings.Count(out, leftAsItIs); got != 2 {
				t.Errorf("the sweep said it was leaving the record %d time(s) over two passes, want 2:\n%s", got, out)
			}
			if strings.Contains(out, closedAsRemove) {
				t.Errorf("the sweep reported a removal Docker did not confirm:\n%s", out)
			}
		})
	}
}

func TestDeferredRelease_AnUnreadableOptionsFileStillWaitsEvenForAGoneNetwork(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	docker := &fakeDocker{inspectErr: networkNotFound()}
	p.docker = docker
	deadline := time.Now()
	id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	path, err := stateFilePath(deferredTestNetwork)
	if err != nil {
		t.Fatalf("stateFilePath: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing a corrupt options file: %v", err)
	}

	p.sweepDeferredReleases(deadline.Add(time.Hour))

	if got := recordPhase(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the record is %v, want RETAINED: a corrupt file may be repaired, and #984 waits for it", got)
	}
	if got := docker.inspectCalls; got != 0 {
		t.Errorf("NetworkInspect was called %d time(s) for a file that exists, want 0", got)
	}
	if got := sender.callCount(); got != 0 {
		t.Errorf("%d release(s) went out, want 0", got)
	}
}

func TestDeleteNetwork_DoesNotAskDockerAboutTheNetworkItIsRemoving(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	docker := &fakeDocker{inspectErr: networkNotFound()}
	p.docker = docker
	id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", time.Now().Add(time.Hour))
	if err := deleteOptions(deferredTestNetwork); err != nil {
		t.Fatalf("deleteOptions: %v", err)
	}

	out := captureLog(t, func() {
		if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: deferredTestNetwork}); err != nil {
			t.Fatalf("DeleteNetwork: %v", err)
		}
	})

	if got := docker.inspectCalls; got != 0 {
		t.Errorf("DeleteNetwork called NetworkInspect %d time(s) from inside the daemon's removal, want 0", got)
	}
	if got := recordPhase(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the record is %v, want RETAINED as before", got)
	}
	if !strings.Contains(out, "could not be read while it was being removed") {
		t.Errorf("the removal no longer warns about the addresses it could not hand back:\n%s", out)
	}
	if got := sender.callCount(); got != 0 {
		t.Errorf("%d release(s) went out, want 0", got)
	}
}

func TestDeferredRelease_ARestartClosesRecordsOfANetworkRemovedBeforeIt(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	sender := installSender(t, nil)
	recordPath := filepath.Join(dir, recordFileName)
	deadline := time.Now()

	before, err := dhcp.OpenRecords(recordPath, "instance-before-restart")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	id := heldRecord(t, &Plugin{records: before}, deferredMAC(0x02), "192.168.99.10/24", deadline)
	if err := before.Close(); err != nil {
		t.Fatalf("closing the first process's records: %v", err)
	}

	after, err := dhcp.OpenRecords(recordPath, "instance-after-restart")
	if err != nil {
		t.Fatalf("reopening the records as the next process would: %v", err)
	}
	t.Cleanup(func() { _ = after.Close() })
	p := &Plugin{records: after, docker: &fakeDocker{inspectErr: networkNotFound()}}

	p.sweepDeferredReleases(deadline.Add(time.Hour))

	if got := recordPhase(t, p, id); got != lease.PhaseClosed {
		t.Errorf("a record of a network removed before the restart is %v, want CLOSED", got)
	}
	if got := sender.callCount(); got != 0 {
		t.Errorf("%d release(s) went out, want 0", got)
	}
}

func TestDeferredRelease_ARemovedNetworksRecordIsNotClosedInsideItsRestartWindow(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseNever)
	docker := &fakeDocker{inspectErr: networkNotFound()}
	p.docker = docker
	deadline := time.Now().Add(time.Hour)
	id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	if err := deleteOptions(deferredTestNetwork); err != nil {
		t.Fatalf("deleteOptions: %v", err)
	}

	p.sweepDeferredReleases(deadline.Add(releaseSettle - time.Second))

	if got := recordPhase(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the record is %v before its deadline and the settle, want RETAINED", got)
	}
	if got := docker.inspectCalls; got != 0 {
		t.Errorf("NetworkInspect was called %d time(s) for a record not yet due, want 0", got)
	}

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	if got := recordPhase(t, p, id); got != lease.PhaseClosed {
		t.Errorf("the record is %v once due, want CLOSED", got)
	}
	if got := sender.callCount(); got != 0 {
		t.Errorf("%d release(s) went out, want 0", got)
	}
}
