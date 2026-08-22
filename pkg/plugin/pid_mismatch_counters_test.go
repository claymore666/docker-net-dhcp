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

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// Two counters that were incremented in exactly one place each, exposed
// in HealthResponse, documented as operator-facing, and executed by
// nothing. Proven by mutation before these tests existed: commenting
// out both Add(1) lines left `go test ./pkg/plugin/` green.
//
// The sentinel they key off IS tested — four times, in
// container_netns_test.go and pid_revalidation_test.go. Two of those
// assertions say the counter is the point in their own message ("so the
// counter can fire", "or the mismatch is never counted") and neither
// asserts it. That is the same defect as a test whose name claims a fix
// it does not execute: the suite proved the PRECONDITION and read that
// as proving the effect.
//
// netns_pid_mismatches matters most. docs/reference.md says of it: "the
// error reads like a slow container start, and only this counter says
// the PID belonged to something else." A counter declared to be the
// SOLE discriminator, on the path that carries CAP_NET_ADMIN,
// addressing, routes and a root DHCP client into a namespace, reads
// zero as "did not happen".

// TestOpenSandboxNetNS_CountsAPIDMismatch drives the real refusal with
// a live PID that is emphatically not the named container -- the test
// process itself -- and asserts the counter, not the error.
func TestOpenSandboxNetNS_CountsAPIDMismatch(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	ns, err := m.openSandboxNetNS(ctx, os.Getpid(), foreignCtrID, time.Millisecond)
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

// The non-vacuity control, and the one that matters more: an ordinary
// failure must NOT be counted. A counter that rises on every slow
// container start says nothing at all, and its documented meaning is
// precisely that it separates the two.
func TestOpenSandboxNetNS_DoesNotCountAnOrdinaryFailure(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	// A PID that cannot exist: /proc/<pid> is absent, so the open fails
	// for a reason that has nothing to do with identity.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	ns, err := m.openSandboxNetNS(ctx, 0x7FFFFFFF, foreignCtrID, time.Millisecond)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("opened a namespace for a PID that does not exist")
	}
	if got := p.netnsPIDMismatches.Load(); got != 0 {
		t.Errorf("netns_pid_mismatches = %d, want 0: this failure is not a PID mismatch, and a counter "+
			"that also rises for missing PIDs cannot separate the two cases it exists to separate", got)
	}
}

// The success control. Without it, a wrapper that refused everything
// would satisfy both tests above.
func TestOpenSandboxNetNS_CountsNothingWhenThePIDMatches(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}
	pid := os.Getpid()

	ns, err := m.openSandboxNetNS(context.Background(), pid, selfCgroup(t, pid), time.Millisecond)
	if err != nil {
		t.Fatalf("refused a PID whose cgroup names it: %v", err)
	}
	defer closeNsHandle(ns)

	if got := p.netnsPIDMismatches.Load(); got != 0 {
		t.Errorf("netns_pid_mismatches = %d after a SUCCESSFUL open, want 0", got)
	}
}

// dnsPropagationManager builds a manager whose Docker answers resolve
// the endpoint to this test process, under the container ID given --
// so writeContainerResolvConf either accepts (the ID names us) or
// refuses with errPIDNotContainer (it does not).
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

// TestPropagateDNS_CountsAPIDMismatch is the same shape for the
// resolv.conf path. Lower stakes than the netns one -- what is written
// is a file rather than a namespace handle -- but the refusal is
// equally silent, and the counter equally unread until now.
func TestPropagateDNS_CountsAPIDMismatch(t *testing.T) {
	m, p := dnsPropagationManager(foreignCtrID)

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.168.0.1"}})

	if got := p.dnsPropagationPIDMismatches.Load(); got != 1 {
		t.Errorf("dns_propagation_pid_mismatches = %d, want 1: the write was refused because the PID "+
			"resolved to something that is not the container, and nothing else says so", got)
	}
}

// TestPropagateDNS_DoesNotCountAnOrdinaryFailure is the DNS sibling of
// the netns control above, and it is the case that makes this counter
// mean anything. Without it, widening the check from the sentinel to
// `err != nil` passes -- and the netns control dying while this one did
// not is "one fix does not reach the copies", twelve lines apart.
//
// writeContainerResolvConf has six non-sentinel failures the widened
// branch would swallow: the empty-resolv.conf refusal, `open self mnt
// ns`, `openat container ns/mnt`, `unshare CLONE_FS`, both `setns`
// calls, and the write itself. Every one of them would then increment a
// counter docs/reference.md defines as "the container PID no longer
// belonged to that container".
//
// The trap is that one of the six is genuinely that event: `openat
// ns/mnt` failing because the container exited just after the cgroup
// check passed IS a PID going away, arriving without the sentinel. So
// someone widening this branch has a plausible reason and nothing red
// to stop them. That ambiguity is precisely why the discriminating case
// has to be written down rather than left obvious.
//
// The trigger is the empty-resolv.conf refusal, reached with a PID
// whose cgroup DOES name it, so the only thing that fails is the list.
// An unreachable PID would not work: openContainerProc wraps a failed
// cgroup read AS errPIDNotContainer (fail-closed, and correct), so a
// bad PID lands on the counted side by design.
func TestPropagateDNS_DoesNotCountAnOrdinaryFailure(t *testing.T) {
	pid := os.Getpid()
	m, p := dnsPropagationManager(selfCgroup(t, pid))

	// Non-empty at propagateDNS's guard, empty by the time
	// writeContainerResolvConf checks: resolvSafe drops it.
	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.168.0.1\n"}})

	if got := p.dnsPropagationPIDMismatches.Load(); got != 0 {
		t.Errorf("dns_propagation_pid_mismatches = %d, want 0. The write was refused because every "+
			"nameserver was unusable, which has nothing to do with the PID. A counter that also rises "+
			"for that cannot separate the two cases it exists to separate, and an operator reading it "+
			"as \"the PID no longer belonged to that container\" would be reading a lie", got)
	}
}

// The guard control: propagation that is not attempted counts nothing.
// It pins the guard as well as the counter -- an increment moved above
// the PropagateDNS check would still pass the positive above. It does
// NOT reach the branch, so it is not a substitute for the case above:
// it proves the code did not run, not that the code chose correctly.
func TestPropagateDNS_CountsNothingWhenDisabled(t *testing.T) {
	m, p := dnsPropagationManager(foreignCtrID)
	m.opts.PropagateDNS = false

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.168.0.1"}})

	if got := p.dnsPropagationPIDMismatches.Load(); got != 0 {
		t.Errorf("dns_propagation_pid_mismatches = %d with propagate_dns off, want 0", got)
	}
}
