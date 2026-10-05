// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	docker "github.com/moby/moby/client"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dNetwork "github.com/moby/moby/api/types/network"
)

// inspectStep is one scripted NetworkInspect answer.
type inspectStep struct {
	res dNetwork.Inspect
	err error
}

// scriptedInspectDocker answers NetworkInspect from a script, the last step repeating, and counts calls; unlike
// fakeDocker it is safe for concurrent callers (#1189).
type scriptedInspectDocker struct {
	fakeDocker
	mu     sync.Mutex
	script []inspectStep
	calls  atomic.Int32
	delay  time.Duration
}

func (s *scriptedInspectDocker) NetworkInspect(_ context.Context, _ string, _ docker.NetworkInspectOptions) (docker.NetworkInspectResult, error) {
	n := int(s.calls.Add(1))
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := n - 1
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	return docker.NetworkInspectResult{Network: s.script[i].res}, s.script[i].err
}

func listing(ctrID, epID string) dNetwork.Inspect {
	return dNetwork.Inspect{Containers: map[string]dNetwork.EndpointResource{ctrID: {EndpointID: epID}}}
}

func TestContainerID_LatchesOnlyARealAnswer(t *testing.T) {
	const (
		netID = "net-1"
		epID  = "0123456789abcdef"
		real  = "c0ffee00c0ffee00"
	)
	placeholder := "ep-" + epID
	errBoom := errors.New("daemon busy")

	cases := []struct {
		name  string
		first inspectStep
	}{
		{"the ep- placeholder is not kept", inspectStep{res: listing(placeholder, epID)}},
		{"a failed inspect is not kept", inspectStep{err: errBoom}},
		{"an endpoint not yet listed is not kept", inspectStep{res: listing("other", "another-endpoint")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &scriptedInspectDocker{script: []inspectStep{tc.first, {res: listing(real, epID)}}}
			m := newDHCPManager(d, JoinRequest{NetworkID: netID, EndpointID: epID}, DHCPNetworkOptions{})

			if got := m.containerID(); got != "" {
				t.Fatalf("first containerID() = %q, want empty: a placeholder or a failed lookup is not a container id", got)
			}
			if got := m.containerID(); got != real {
				t.Fatalf("second containerID() = %q, want %q: the first answer was latched", got, real)
			}
			for i := 0; i < 5; i++ {
				if got := m.containerID(); got != real {
					t.Fatalf("containerID() = %q, want %q", got, real)
				}
			}
			if got := d.calls.Load(); got != 2 {
				t.Errorf("NetworkInspect calls = %d, want 2: a real id is asked once and never again", got)
			}
		})
	}
}

func TestContainerID_PlaceholderNeverReachesTheLedger(t *testing.T) {
	const (
		netID = "net-1"
		epID  = "0123456789abcdef"
		real  = "c0ffee00c0ffee00"
	)
	d := &scriptedInspectDocker{script: []inspectStep{{res: listing("ep-"+epID, epID)}, {res: listing(real, epID)}}}
	var failures atomic.Int32
	p := &Plugin{}
	p.ledger = testLedger(t, &failures)
	m := newDHCPManager(d, JoinRequest{NetworkID: netID, EndpointID: epID}, DHCPNetworkOptions{AuditLog: true}).withPlugin(p)

	m.audit("bound", "192.168.99.50")
	m.audit("renew", "192.168.99.50")
	m.audit("renew", "192.168.99.50")

	rows := readLedgerLines(t, p.ledger.path)
	if len(rows) != 3 {
		t.Fatalf("ledger rows = %d, want 3", len(rows))
	}
	if rows[0].Container != "" {
		t.Errorf("row 0 container = %q, want empty: the ep- placeholder was written to the ledger", rows[0].Container)
	}
	for i, r := range rows[1:] {
		if r.Container != real {
			t.Errorf("row %d container = %q, want %q", i+1, r.Container, real)
		}
	}
	if got := d.calls.Load(); got != 2 {
		t.Errorf("NetworkInspect calls = %d, want 2 for three ledger rows", got)
	}
}

func TestContainerID_ConcurrentCallersAskOnceForARealID(t *testing.T) {
	const (
		epID = "0123456789abcdef"
		real = "c0ffee00c0ffee00"
	)
	d := &scriptedInspectDocker{script: []inspectStep{{res: listing(real, epID)}}, delay: 5 * time.Millisecond}
	m := newDHCPManager(d, JoinRequest{NetworkID: "net-1", EndpointID: epID}, DHCPNetworkOptions{})

	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([]string, 32)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i] = m.containerID()
		}()
	}
	close(start)
	wg.Wait()

	for i, g := range got {
		if g != real {
			t.Errorf("caller %d saw %q, want %q", i, g, real)
		}
	}
	if n := d.calls.Load(); n != 1 {
		t.Errorf("NetworkInspect calls = %d, want 1: concurrent callers must share one lookup", n)
	}
}

func TestContainerID_ConcurrentCallersAcrossAPlaceholder(t *testing.T) {
	const (
		epID = "0123456789abcdef"
		real = "c0ffee00c0ffee00"
	)
	d := &scriptedInspectDocker{script: []inspectStep{
		{res: listing("ep-"+epID, epID)}, {err: errors.New("busy")}, {res: listing(real, epID)},
	}}
	m := newDHCPManager(d, JoinRequest{NetworkID: "net-1", EndpointID: epID}, DHCPNetworkOptions{})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g := m.containerID(); g != "" && g != real {
				t.Errorf("containerID() = %q, want empty or %q, never a placeholder", g, real)
			}
		}()
	}
	wg.Wait()
	if g := m.containerID(); g != real {
		t.Errorf("containerID() after the answers settled = %q, want %q", g, real)
	}
	if n := d.calls.Load(); n != 3 {
		t.Errorf("NetworkInspect calls = %d, want 3: placeholder, failure, then the real id once", n)
	}
}

func TestContainerID_NilDockerStaysEmpty(t *testing.T) {
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "e"}, DHCPNetworkOptions{})
	if got := m.containerID(); got != "" {
		t.Errorf("containerID() with no docker client = %q, want empty", got)
	}
}
