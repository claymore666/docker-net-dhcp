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

// The PID was resolved through Docker up to 70 s earlier and the plugin runs in the
// host PID namespace, so a recycled PID must be refused (#688).
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

	own, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get: %v", err)
	}
	defer closeNsHandle(own)
	if !ns.Equal(own) {
		t.Errorf("opened %v, want this process's own netns %v", ns, own)
	}
}

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
