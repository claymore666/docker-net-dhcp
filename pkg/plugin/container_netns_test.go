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

	"github.com/vishvananda/netns"
)

// TestOpenContainerNetNS_RefusesAPIDThatIsNotTheContainer is the #688
// guard applied to the path with the larger blast radius.
//
// The PID reaching the netns open was resolved through Docker up to 70
// seconds earlier (awaitTimeout + attachDaemonBusyGrace). The plugin
// runs in the HOST PID namespace, so if the container exited in that
// window and the kernel recycled the PID, the namespace opened belongs
// to an unrelated host task -- and unlike the resolv.conf case, what
// follows is a netlink handle carrying every address, MTU and route the
// manager applies, plus a root dhcpcd spawned into it.
//
// Driven with a live PID that is emphatically not the named container:
// the test process itself.
func TestOpenContainerNetNS_RefusesAPIDThatIsNotTheContainer(t *testing.T) {
	ns, err := openContainerNetNS(os.Getpid(), foreignCtrID)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("opened the network namespace of a PID that does not belong to the container")
	}
	if !errors.Is(err, errPIDNotContainer) {
		t.Errorf("error must be errPIDNotContainer, which is the cause netns_pid_mismatches keys off "+
			"(the counter itself is asserted in TestOpenSandboxNetNS_CountsAPIDMismatch, not here), got: %v", err)
	}
	if ns.IsOpen() {
		t.Errorf("a refused open must not return a live descriptor, got %v", ns)
	}
}

// The non-vacuity control: with an ID that DOES name the process, the
// same call must succeed and hand back this process's own netns. Without
// this, a function that refused everything would pass the test above.
func TestOpenContainerNetNS_OpensTheNamespaceOfAMatchingPID(t *testing.T) {
	pid := os.Getpid()

	ns, err := openContainerNetNS(pid, selfCgroupLeaf(t, pid))
	if err != nil {
		t.Fatalf("refused a PID whose cgroup names it: %v", err)
	}
	defer closeNsHandle(ns)

	if !ns.IsOpen() {
		t.Fatal("returned a closed handle")
	}

	// Prove it is the right namespace, not merely a descriptor: netns
	// handles compare by the inode behind them.
	own, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get: %v", err)
	}
	defer closeNsHandle(own)
	if !ns.Equal(own) {
		t.Errorf("opened %v, want this process's own netns %v", ns, own)
	}
}

// TestAwaitContainerNetNS_RefusalSurvivesTheDeadline pins the join
// between the two halves of the fix, which is the part that is easy to
// break without noticing.
//
// awaitContainerNetNS retries, so a mismatching PID is refused over and
// over and the caller finally sees a DEADLINE error. The counter in
// dhcp_manager keys off errors.Is(err, errPIDNotContainer), so if the
// wrapping ever loses that cause, an attack degrades into an ordinary
// slow-attach timeout with nothing counted -- the plugin's own opinion
// stays clean while the refusal is invisible. Both causes must survive.
func TestAwaitContainerNetNS_RefusalSurvivesTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	ns, err := awaitContainerNetNS(ctx, os.Getpid(), foreignCtrID, 10*time.Millisecond)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("expected a refusal for a PID that never names the container")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error must wrap context.DeadlineExceeded, got: %v", err)
	}
	if !errors.Is(err, errPIDNotContainer) {
		t.Errorf("error must still carry errPIDNotContainer through the deadline wrap, or the cause "+
			"netns_pid_mismatches keys off is lost (the count itself is asserted in "+
			"TestOpenSandboxNetNS_CountsAPIDMismatch): %v", err)
	}
	if !strings.Contains(err.Error(), "last attempt:") {
		t.Errorf("error must carry the last attempt's cause (#317), got: %v", err)
	}
	if ns.IsOpen() {
		t.Errorf("a refused await must not return a live descriptor, got %v", ns)
	}
}
