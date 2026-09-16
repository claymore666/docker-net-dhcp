// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
)

// The four outcomes of a name that arrives after its client is already
// leasing (#961), driven one at a time.
//
// The attach reaches this step with the container leasing and the whole
// attach already successful, so NOTHING HERE MAY FAIL THE ATTACH and
// every arm is silent unless it is counted. These drives are what makes
// each arm's counter the thing that distinguishes it: the same endpoint,
// the same running client, four different reasons the DHCP server's
// table does or does not learn the name.
//
// WHAT THEY CANNOT SAY: that the server was told. The library's
// SetHostname returns before anything is on the wire and says so, so a
// handover here is intent and not effect. The effect is asserted on the
// dnsmasq lease file in test/integration.
func aNamedEndpoint(t *testing.T) (*dhcpManager, *Plugin, *fakeJoinClient) {
	t.Helper()
	p := &Plugin{}
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"}, DHCPNetworkOptions{}).withPlugin(p)
	client := &fakeJoinClient{}
	m.setHealthClient(client)
	return m, p, client
}

func TestNameTheRunningClient_HandsTheNameToTheClientThatIsAlreadyLeasing(t *testing.T) {
	m, p, client := aNamedEndpoint(t)
	phases := newJoinPhases()

	name := ""
	m.nameTheRunningClient(phases, func() error { name = "web1"; return nil }, &name)

	if got := client.names; len(got) != 1 || got[0] != "web1" {
		t.Errorf("the running client was given %v, want exactly [web1]. The name is the whole point of "+
			"the lookup, and a client that is never told keeps sending the option it started with, "+
			"which is none (#961)", got)
	}
	if got := p.hostnamesAppliedLate.Load(); got != 1 {
		t.Errorf("hostnames_applied_late = %d, want 1: it is the domain the two failure counters are "+
			"read against, and a zero here makes their zeros mean nothing", got)
	}
	if got := p.hostnameApplyFailures.Load(); got != 0 {
		t.Errorf("hostname_apply_failures = %d, want 0 on a client that took the name", got)
	}
	if got := p.hostnameLookupFailures.Load(); got != 0 {
		t.Errorf("hostname_lookup_failures = %d, want 0 on a lookup that answered", got)
	}
	if got := m.hostnameOnTheWire(); got != "web1" {
		t.Errorf("m.hostname = %q, want %q: the ledger reads this field, so an endpoint whose client "+
			"was told and whose record was not describes two different containers", got, "web1")
	}
	if !strings.Contains(phases.summary(), "hostname=") {
		t.Errorf("phase summary %q has no hostname phase: the wait for the name is the longest thing "+
			"left in the attach and a reader cannot see it", phases.summary())
	}
}

func TestNameTheRunningClient_ALookupThatNeverAnswersLeavesTheClientRunning(t *testing.T) {
	m, p, client := aNamedEndpoint(t)
	phases := newJoinPhases()

	name := ""
	m.nameTheRunningClient(phases, func() error { return errors.New("daemon is inside ContainerStart") }, &name)

	if len(client.names) != 0 {
		t.Errorf("the client was given %v after a lookup that failed: the only name available then is "+
			"one nothing decided", client.names)
	}
	if got := p.hostnameLookupFailures.Load(); got != 1 {
		t.Errorf("hostname_lookup_failures = %d, want 1", got)
	}
	if got := p.hostnamesAppliedLate.Load(); got != 0 {
		t.Errorf("hostnames_applied_late = %d, want 0: nothing was applied", got)
	}
	if strings.Contains(phases.summary(), "hostname=") {
		t.Errorf("phase summary %q records a completed hostname phase for a lookup that failed", phases.summary())
	}
}

func TestNameTheRunningClient_ARefusedNameIsNotHandedOver(t *testing.T) {
	m, p, client := aNamedEndpoint(t)

	name := ""
	m.nameTheRunningClient(newJoinPhases(), func() error { name = "web1\nduid 00:03:00:01"; return nil }, &name)

	if len(client.names) != 0 {
		t.Errorf("the client was given %v: safeHostname refused this name, and handing the refusal on as "+
			"an empty string would make the plugin ask the server to stop recording a name it never "+
			"sent", client.names)
	}
	if got := p.unsafeHostnamesRejected.Load(); got != 1 {
		t.Errorf("unsafe_hostnames_rejected = %d, want 1: the late path has to go through the same "+
			"refusal as the pre-start one, or a container can put a control character on the wire by "+
			"starting on a host that takes the sandbox-key route", got)
	}
	if got := p.hostnamesAppliedLate.Load(); got != 0 {
		t.Errorf("hostnames_applied_late = %d, want 0: nothing reached the client", got)
	}
	if got := p.hostnameApplyFailures.Load(); got != 0 {
		t.Errorf("hostname_apply_failures = %d, want 0: the name never got as far as the client, and "+
			"charging a refusal to the client hides a hostile container behind a broken one", got)
	}
}

// TestNameTheRunningClient_AContainerWithNoNameIsNotAFailure keeps the
// positive counter's domain honest.
//
// A container started without --hostname has an empty Config.Hostname,
// which is an ordinary container and not a fault. Handing the empty
// string over would tell the library to stop sending option 12, which
// this client is already not sending, and would make
// hostnames_applied_late rise once per container on every host.
func TestNameTheRunningClient_AContainerWithNoNameIsNotAFailure(t *testing.T) {
	m, p, client := aNamedEndpoint(t)

	name := ""
	m.nameTheRunningClient(newJoinPhases(), func() error { return nil }, &name)

	if len(client.names) != 0 {
		t.Errorf("the client was given %v for a container with no name", client.names)
	}
	for _, c := range []struct {
		field string
		got   int32
	}{
		{"hostnames_applied_late", p.hostnamesAppliedLate.Load()},
		{"hostname_lookup_failures", p.hostnameLookupFailures.Load()},
		{"hostname_apply_failures", p.hostnameApplyFailures.Load()},
		{"unsafe_hostnames_rejected", p.unsafeHostnamesRejected.Load()},
	} {
		if c.got != 0 {
			t.Errorf("%s = %d, want 0: a container started without --hostname is not an event", c.field, c.got)
		}
	}
}

func TestNameTheRunningClient_AClientThatRefusesTheNameIsCountedApart(t *testing.T) {
	m, p, client := aNamedEndpoint(t)
	client.setErr = errors.New("the request queue is full and the hostname was not delivered")

	name := ""
	m.nameTheRunningClient(newJoinPhases(), func() error { name = "web1"; return nil }, &name)

	if got := p.hostnameApplyFailures.Load(); got != 1 {
		t.Errorf("hostname_apply_failures = %d, want 1: the client refusing the handover and the daemon "+
			"not answering leave the same endpoint in the same state, and their remedies are at "+
			"opposite ends of the host", got)
	}
	if got := p.hostnameLookupFailures.Load(); got != 0 {
		t.Errorf("hostname_lookup_failures = %d, want 0: the daemon answered", got)
	}
	if got := p.hostnamesAppliedLate.Load(); got != 0 {
		t.Errorf("hostnames_applied_late = %d, want 0: the client would not take it", got)
	}
}

// TestNameTheRunningClient_NoClientIsAnApplyFailure covers the arm that
// has no client at all.
//
// It is reachable: the manager publishes the v4 client inside
// setupClient, and a future path that names an endpoint whose client was
// never published would otherwise return silently with the name in a
// field nobody sends.
func TestNameTheRunningClient_NoClientIsAnApplyFailure(t *testing.T) {
	p := &Plugin{}
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"}, DHCPNetworkOptions{}).withPlugin(p)

	name := ""
	m.nameTheRunningClient(newJoinPhases(), func() error { name = "web1"; return nil }, &name)

	if got := p.hostnameApplyFailures.Load(); got != 1 {
		t.Errorf("hostname_apply_failures = %d, want 1 when there is no client to give the name to", got)
	}
	if got := p.hostnamesAppliedLate.Load(); got != 0 {
		t.Errorf("hostnames_applied_late = %d, want 0: nothing was applied", got)
	}
}

// TestStart_ARegisterDNSNetworkTakesTheNameBeforeTheClientStarts is
// #961's bound, and it is asserted here so the claim cannot grow.
//
// register_dns puts the container's name in RFC 4702's option 81, which
// the library takes when the client is constructed and offers no setter
// for, and section 3.1 forbids the Host Name option beside it. So such a
// network keeps the pre-start wait: a client started before the name
// arrived would carry no option 81 for its whole life, and the late
// SetHostname would put the name in option 12 instead -- a different
// option, asking the server for nothing.
//
// The sample is taken at the socket, because "it waited" and "it did not
// wait" leave the same totals when Start returns.
func TestStart_ARegisterDNSNetworkTakesTheNameBeforeTheClientStarts(t *testing.T) {
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				"ctr-1": {EndpointID: "ep-abcdef"},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			"ctr-1": {
				ContainerJSONBase: &dContainer.ContainerJSONBase{State: &dContainer.State{Pid: os.Getpid()}},
				Config:            &dContainer.Config{Hostname: "web1"},
			},
		},
	}
	m, p := daemonFreeManager(t, docker)
	m.opts.RegisterDNS = true

	inspectsAtSocket := -1
	withStartedClient(t, func() {
		if inspectsAtSocket < 0 {
			inspectsAtSocket = docker.containerCalls
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if inspectsAtSocket != 1 {
		t.Errorf("the container had been inspected %d times when the client's socket was opened, want 1. "+
			"On a register_dns network the name has to be in hand at construction: option 81 is built "+
			"from it and cannot be set afterwards", inspectsAtSocket)
	}
	if got := m.hostnameOnTheWire(); got != "web1" {
		t.Errorf("m.hostname = %q, want %q: the name the client was constructed with is the only one "+
			"option 81 can carry", got, "web1")
	}
	if got := p.hostnamesAppliedLate.Load(); got != 0 {
		t.Errorf("hostnames_applied_late = %d, want 0: the name was already in the client's parameters, "+
			"and the library refuses SetHostname on a client sending option 81 anyway", got)
	}
	if got := docker.containerCalls; got != 1 {
		t.Errorf("the container was inspected %d times for one attach, want 1: the pre-start lookup and "+
			"the late one are the same answer, and a second call is a second wait on a daemon that is "+
			"inside ContainerStart (#406)", got)
	}
}

// TestStart_TheDefaultNetworkTakesTheNameAfterTheClientStarts is the
// preservation control's opposite number: the same fixture with
// register_dns off must take the other branch.
//
// Without it, a carve-out widened to every network would pass every
// other drive in this file.
func TestStart_TheDefaultNetworkTakesTheNameAfterTheClientStarts(t *testing.T) {
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				"ctr-1": {EndpointID: "ep-abcdef"},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			"ctr-1": {
				ContainerJSONBase: &dContainer.ContainerJSONBase{State: &dContainer.State{Pid: os.Getpid()}},
				Config:            &dContainer.Config{Hostname: "web1"},
			},
		},
	}
	m, p := daemonFreeManager(t, docker)
	if m.opts.RegisterDNS {
		t.Fatal("this fixture is meant to be the default network; register_dns is set")
	}

	// The substitute publishes a client that can be asked what it was
	// told, at the instant a real one becomes live. Without it the
	// manager holds the library client whose socket this lane could not
	// open, and every arm below reads as an apply failure.
	client := &fakeJoinClient{}
	inspectsAtSocket := -1
	withStartedClient(t, func() {
		if inspectsAtSocket < 0 {
			inspectsAtSocket = docker.containerCalls
		}
		m.setHealthClient(client)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := client.names; len(got) != 1 || got[0] != "web1" {
		t.Errorf("the running client was told %v, want exactly [web1]: the name reaching the plugin's "+
			"own field is not the name reaching the server's table (#961)", got)
	}
	if inspectsAtSocket != 0 {
		t.Errorf("the container had been inspected %d times when the client's socket was opened, want 0 "+
			"on a network that does not need the name at construction (#961)", inspectsAtSocket)
	}
	if got := m.hostnameOnTheWire(); got != "web1" {
		t.Errorf("m.hostname = %q, want %q", got, "web1")
	}
	if got := p.hostnamesAppliedLate.Load(); got != 1 {
		t.Errorf("hostnames_applied_late = %d, want 1: the name arrived after the client and was given "+
			"to it, which is the whole of #961", got)
	}
}
