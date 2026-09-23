// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"sync"
	"testing"
)

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
	got, ok := p.takeDHCPManager("ep-1")
	if !ok || got != incumbent {
		t.Errorf("registry holds %v (ok=%v), want the incumbent", got, ok)
	}
}

// A read-unlock-write registration passes this test; check-manager-registration.sh holds that shape.
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
