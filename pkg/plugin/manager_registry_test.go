// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"sync"
	"testing"
)

// The registry has two ways in, and which one a caller uses encodes who
// wins a collision. Join registers unconditionally and stops whatever it
// displaced, because a Join is newer truth than a recovery. Recovery
// registers only if absent and yields when it loses, because it is the
// older truth and the endpoint it was about to adopt already has a
// client.
//
// Getting that backwards does not fail loudly. It leaves two dhcpcd
// processes on one interface, one of them unreachable from the registry
// and therefore unstoppable — the leak registerDHCPManager's doc comment
// has warned about since it was written, and which the recovery path
// nonetheless shipped by discarding the return value.

func TestRegisterDHCPManagerIfAbsent_RegistersWhenNobodyHolds(t *testing.T) {
	p := &Plugin{persistentDHCP: make(map[string]*dhcpManager)}
	m := &dhcpManager{}

	if !p.registerDHCPManagerIfAbsent("ep-1", m) {
		t.Fatal("registration into an empty registry reported a loss")
	}
	got, ok := p.takeDHCPManager("ep-1")
	if !ok || got != m {
		t.Errorf("registry holds %v (ok=%v), want the manager just registered", got, ok)
	}
}

func TestRegisterDHCPManagerIfAbsent_YieldsToTheIncumbent(t *testing.T) {
	p := &Plugin{persistentDHCP: make(map[string]*dhcpManager)}
	incumbent, challenger := &dhcpManager{}, &dhcpManager{}
	p.registerDHCPManager("ep-1", incumbent)

	if p.registerDHCPManagerIfAbsent("ep-1", challenger) {
		t.Fatal("registration over an existing manager reported a win")
	}
	// The point of the whole exercise: the incumbent is still reachable,
	// so whoever owns it can still stop its dhcpcd.
	got, ok := p.takeDHCPManager("ep-1")
	if !ok || got != incumbent {
		t.Errorf("registry holds %v (ok=%v), want the incumbent", got, ok)
	}
}

// Concurrent load over the registry, run under -race in CI.
//
// Read what this does and does not establish. It requires exactly one
// winner — not "at least one", which a last-write-wins registry would
// also satisfy while dropping the other — and under -race it fails an
// implementation that touches the map without the lock at all.
//
// It does NOT catch the shape this fix was actually about: a read, an
// unlock, and a later write. That was measured, not assumed — driving
// 64 racers against a deliberately two-step implementation passed, three
// runs out of three. Every racer queues on the same mutex, so by the
// time the winner releases it the losers' reads already see the entry,
// and the window never opens. Widening it would take a hook in
// production code that exists only for this test.
//
// So the atomicity itself is held by check-manager-registration.sh,
// which reads the function's structure and goes red on exactly that
// two-step — verified by making the edit and watching it fail. This
// test is here for the corruption a race detector can see; the gate is
// here for the one it cannot.
func TestRegisterDHCPManagerIfAbsent_ExactlyOneWinner(t *testing.T) {
	const racers = 64
	p := &Plugin{persistentDHCP: make(map[string]*dhcpManager)}

	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	var winner *dhcpManager

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		m := &dhcpManager{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if p.registerDHCPManagerIfAbsent("ep-1", m) {
				mu.Lock()
				winners++
				winner = m
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Fatalf("%d racers claimed the endpoint, want exactly 1", winners)
	}
	got, ok := p.takeDHCPManager("ep-1")
	if !ok || got != winner {
		t.Errorf("registry holds %v, want the one manager that reported a win", got)
	}
}
