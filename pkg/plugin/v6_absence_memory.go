// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// defaultDHCPv6AbsenceMemory is the Options.DHCPv6AbsenceMemory fallback and config.json's DHCPV6_ABSENCE_MEMORY
// default (#1038).
const defaultDHCPv6AbsenceMemory = 10 * time.Minute

// absenceWindowFor reads Options.DHCPv6AbsenceMemory: nil is the default and zero is off, never the default (#1038).
func absenceWindowFor(opts Options) time.Duration {
	if opts.DHCPv6AbsenceMemory == nil {
		return defaultDHCPv6AbsenceMemory
	}
	return *opts.DHCPv6AbsenceMemory
}

// v6AbsenceMemory holds, per network, when an ipv6_mode=auto attach last fell back because no DHCPv6 server answered.
type v6AbsenceMemory struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time
	at     map[string]time.Time
}

func (m *v6AbsenceMemory) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *v6AbsenceMemory) remember(networkID string) {
	if m.window <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.at == nil {
		m.at = make(map[string]time.Time)
	}
	m.at[networkID] = m.clock()
}

func (m *v6AbsenceMemory) forget(networkID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.at, networkID)
}

// age returns how old the network's memory is, and false once it is as old as the window or there is none.
func (m *v6AbsenceMemory) age(networkID string) (time.Duration, bool) {
	if m.window <= 0 {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.at[networkID]
	if !ok {
		return 0, false
	}
	age := m.clock().Sub(at)
	if age >= m.window {
		delete(m.at, networkID)
		return 0, false
	}
	return age, true
}

// v6AbsenceServe marks an auto endpoint served while its network remembers a silent server, so v6Wiring runs both
// of its clients as slaac. The library reads Params6.AutoFallback zero as its 6 s default and negative as strict, so
// no value skips the Solicit; Mode6SLAAC does (#1038).
func (p *Plugin) v6AbsenceServe(opts DHCPNetworkOptions, networkID, endpointID string) bool {
	if mode, err := opts.ipv6Mode(); err != nil || mode != proto.Mode6Auto || opts.IPv6AutoStrict {
		return false
	}
	age, ok := p.v6Absence.age(networkID)
	if !ok {
		return false
	}
	p.v6AbsenceServed.Store(endpointID, struct{}{})
	p.dhcpv6AbsenceRemembered.Add(1)
	log.WithFields(log.Fields{
		"network":  shortID(networkID),
		"endpoint": shortID(endpointID),
		"age":      age.Round(time.Second).String(),
		"window":   p.v6Absence.window.String(),
	}).Info("ipv6_mode=auto: an earlier attach on this network found no DHCPv6 server inside the fallback window, " +
		"so this endpoint forms its address from the router's advertised prefix without soliciting")
	if opts.RegisterDNS {
		noAAAAAfterFallback(endpointID, func(uint64) {})(1)
	}
	return true
}

// v6AbsenceRecord makes an auto attach's fallback set its network's memory; the persistent client's never does.
func (p *Plugin) v6AbsenceRecord(base *dhcp.DHCPClientOptions, networkID string) {
	if base.Mode6 != proto.Mode6Auto || base.StrictAuto6 {
		return
	}
	report := base.OnV6Fallback
	base.OnV6Fallback = func(n uint64) {
		if report != nil {
			report(n)
		}
		if n != 0 {
			p.v6Absence.remember(networkID)
		}
	}
}

func (p *Plugin) v6AbsenceServedEndpoint(endpointID string) bool {
	_, served := p.v6AbsenceServed.Load(endpointID)
	return served
}
