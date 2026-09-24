// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"
)

// The library's SetHostname returns before anything is on the wire, so these assert intent; the
// integration suite asserts the name in dnsmasq's lease file (#961).
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

// register_dns sends the name in option 81 (RFC 4702), which the library takes only at construction
// and section 3.1 keeps apart from option 12, so such a network waits for the name before Start (#961).
func TestNameTheRunningClient_ZeroOnAllThreeIsNotOnlyAnIdleHost(t *testing.T) {
	p := &Plugin{}
	attaches := 0
	for i := 0; i < 5; i++ {
		m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"},
			DHCPNetworkOptions{}).withPlugin(p)
		client := &fakeJoinClient{}
		m.setHealthClient(client)

		name := "not-read-yet"
		m.nameTheRunningClient(newJoinPhases(), func() error { name = ""; return nil }, &name)
		attaches++

		if len(client.names) != 0 {
			t.Fatalf("attach %d handed the client %v: a container with no name has nothing to hand "+
				"over, and an empty handover asks the server to stop recording a name that was "+
				"never sent", i, client.names)
		}
	}

	if got := p.hostnamesAppliedLate.Load(); got != 0 {
		t.Errorf("hostnames_applied_late = %d after %d unnamed attaches, want 0", got, attaches)
	}
	if got := p.hostnameLookupFailures.Load(); got != 0 {
		t.Errorf("hostname_lookup_failures = %d, want 0: every lookup answered", got)
	}
	if got := p.hostnameApplyFailures.Load(); got != 0 {
		t.Errorf("hostname_apply_failures = %d, want 0: there was nothing to apply", got)
	}

	m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-2"},
		DHCPNetworkOptions{}).withPlugin(p)
	m.setHealthClient(&fakeJoinClient{})
	name := ""
	m.nameTheRunningClient(newJoinPhases(), func() error { name = "web1"; return nil }, &name)
	if got := p.hostnamesAppliedLate.Load(); got != 1 {
		t.Errorf("hostnames_applied_late = %d after one NAMED attach on the same plugin, want 1: if this "+
			"does not move, the zeros above measure the counter and not the host", got)
	}
}

func TestNameTheRunningClient_TheLedgerNamesOnlyWhatTheClientTook(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setErr  error
		wantLog string
	}{
		{name: "the client refuses the handover", setErr: errors.New("the request queue is full"), wantLog: ""},
		{name: "the client takes it", setErr: nil, wantLog: "web1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)
			m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"},
				DHCPNetworkOptions{AuditLog: true}).withPlugin(p)
			client := &fakeJoinClient{setErr: tc.setErr}
			m.setHealthClient(client)

			name := ""
			m.nameTheRunningClient(newJoinPhases(), func() error { name = "web1"; return nil }, &name)
			m.audit("bound", "192.168.0.10")

			rows := readLedgerLines(t, p.ledger.path)
			if len(rows) != 1 {
				t.Fatalf("the ledger has %d rows, want 1", len(rows))
			}
			if got := rows[0].Hostname; got != tc.wantLog {
				t.Errorf("the ledger's bound row names %q, want %q. The ledger and "+
					"hostname_apply_failures (%d) describe the same endpoint, and a name in "+
					"this column the client never took makes them disagree (#961)",
					got, tc.wantLog, p.hostnameApplyFailures.Load())
			}
		})
	}
}

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

// On `docker restart` libnetwork sends Leave then Join with no CreateEndpoint, so Join rebuilds the
// endpoint through daemon calls while the daemon is inside ContainerStart (#406, #961). The stub
// parent has ifindex 0, so the kernel refuses the child link and nothing is built on the host.
func TestReacquireEndpoint_AsksTheDaemonBeforeTheAttachBegins(t *testing.T) {
	for _, mode := range []string{ModeMacvlan, ModeIPvlan} {
		t.Run(mode, func(t *testing.T) {
			p := newTestPlugin(t)
			if err := saveOptions("net-1", DHCPNetworkOptions{Mode: mode, Parent: "par0"}); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}
			stubLinkByName(t, func(string) (netlink.Link, error) {
				return &fakeLink{
					typ:   "device",
					attrs: netlink.LinkAttrs{Name: "par0", Flags: net.FlagUp},
				}, nil
			})

			docker := &fakeDocker{
				inspectResult: map[string]dNetwork.Inspect{
					"net-1": {Containers: map[string]dNetwork.EndpointResource{
						"ctr-1": {EndpointID: "ep-abcdef", MacAddress: "02:42:ac:11:00:02"},
					}},
				},
				containerResult: map[string]dContainer.InspectResponse{
					"ctr-1": {Config: &dContainer.Config{Hostname: "web1"}},
				},
			}
			p.docker = docker

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = p.reacquireEndpoint(ctx, JoinRequest{NetworkID: "net-1", EndpointID: "ep-abcdef"},
				DHCPNetworkOptions{Mode: mode})

			if docker.inspectCalls == 0 {
				t.Error("the restart route rebuilt the endpoint without asking the daemon anything. " +
					"If that is so then #961's window claim covers this route too and the " +
					"documents that exclude it are wrong; if it is not, this drive has stopped " +
					"measuring the route")
			}
			if docker.containerCalls == 0 {
				t.Error("the restart route never reached the hostname lookup, so nothing here " +
					"measured the replay: with the network's options already on disk the only " +
					"daemon call this route can make is the lookup's, and it did not make it. " +
					"Either the replay stopped asking the daemon before the attach begins, which " +
					"is what #961's documents say it still does, or the route now returns before " +
					"createParentAttachedEndpoint's hostname lookup and this drive is measuring " +
					"the return")
			}
		})
	}
}
