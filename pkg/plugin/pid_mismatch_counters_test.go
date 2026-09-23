// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"os"
	"testing"
	"time"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const noSandboxKey = ""

func TestOpenSandboxNetNS_CountsAPIDMismatch(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	ns, err := m.openSandboxNetNS(ctx, noSandboxKey, os.Getpid(), foreignCtrID, time.Millisecond)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("opened the network namespace of a PID that does not name the container")
	}
	if got := p.netnsPIDMismatches.Load(); got != 1 {
		t.Errorf("netns_pid_mismatches = %d, want 1. The refusal is correct and invisible: the error "+
			"an operator sees is a timeout, and this counter is the only thing that says the PID "+
			"belonged to something else", got)
	}
}

func TestOpenSandboxNetNS_DoesNotCountAnOrdinaryFailure(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	ns, err := m.openSandboxNetNS(ctx, noSandboxKey, 0x7FFFFFFF, foreignCtrID, time.Millisecond)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("opened a namespace for a PID that does not exist")
	}
	if got := p.netnsPIDMismatches.Load(); got != 0 {
		t.Errorf("netns_pid_mismatches = %d, want 0: this failure is not a PID mismatch, and a counter "+
			"that also rises for missing PIDs cannot separate the two cases it exists to separate", got)
	}
}

func TestOpenSandboxNetNS_CountsNothingWhenThePIDMatches(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}
	pid := os.Getpid()

	// A deadline, since errPIDNotContainer is permanent and openSandboxNetNS polls until its context ends.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ns, err := m.openSandboxNetNS(ctx, noSandboxKey, pid, selfCgroupLeaf(t, pid), time.Millisecond)
	if err != nil {
		t.Fatalf("refused a PID whose cgroup names it: %v", err)
	}
	defer closeNsHandle(ns)

	if got := p.netnsPIDMismatches.Load(); got != 0 {
		t.Errorf("netns_pid_mismatches = %d after a SUCCESSFUL open, want 0", got)
	}
}

func dnsPropagationManager(ctrID string) (*dhcpManager, *Plugin) {
	const netID, epID = "n1", "ep1"
	p := &Plugin{}
	return &dhcpManager{
		plugin:  p,
		joinReq: JoinRequest{NetworkID: netID, EndpointID: epID},
		opts:    DHCPNetworkOptions{Bridge: "br0", PropagateDNS: true},
		docker: &fakeDocker{
			inspectResult: map[string]dNetwork.Inspect{
				netID: {Containers: map[string]dNetwork.EndpointResource{
					ctrID: {EndpointID: epID},
				}},
			},
			containerResult: map[string]dContainer.InspectResponse{
				ctrID: {ContainerJSONBase: &dContainer.ContainerJSONBase{
					State: &dContainer.State{Running: true, Status: "running", Pid: os.Getpid()},
				}},
			},
		},
	}, p
}

func TestPropagateDNS_CountsAPIDMismatch(t *testing.T) {
	m, p := dnsPropagationManager(foreignCtrID)

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.168.0.1"}})

	if got := p.dnsPropagationPIDMismatches.Load(); got != 1 {
		t.Errorf("dns_propagation_pid_mismatches = %d, want 1: the write was refused because the PID "+
			"resolved to something that is not the container, and nothing else says so", got)
	}
}

func TestPropagateDNS_DoesNotCountAnOrdinaryFailure(t *testing.T) {
	pid := os.Getpid()
	m, p := dnsPropagationManager(selfCgroupLeaf(t, pid))

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.168.0.1\n"}})

	if got := p.dnsPropagationPIDMismatches.Load(); got != 0 {
		t.Errorf("dns_propagation_pid_mismatches = %d, want 0. The write was refused because every "+
			"nameserver was unusable, which has nothing to do with the PID. A counter that also rises "+
			"for that cannot separate the two cases it exists to separate, and an operator reading it "+
			"as \"the PID no longer belonged to that container\" would be reading a lie", got)
	}
}

func TestPropagateDNS_CountsNothingWhenDisabled(t *testing.T) {
	m, p := dnsPropagationManager(foreignCtrID)
	m.opts.PropagateDNS = false

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.168.0.1"}})

	if got := p.dnsPropagationPIDMismatches.Load(); got != 0 {
		t.Errorf("dns_propagation_pid_mismatches = %d with propagate_dns off, want 0", got)
	}
}
