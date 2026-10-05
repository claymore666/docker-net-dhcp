// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dContainer "github.com/moby/moby/api/types/container"
	dNetwork "github.com/moby/moby/api/types/network"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	vanishNet = "net-0123456789abcdef"
	vanishEP  = "ep-0123456789abcdef"
	vanishCtr = "ctr-0123456789abcdef"
)

// A key that was never created and a key that was unlinked read the same from the directory, so what separates a
// slow start from a vanished container is the daemon (#1186).
func TestSettleFailedAttach_AMissingKeyIsNotProofTheContainerWentAway(t *testing.T) {
	deadlineNoKey := fmt.Errorf("failed to get sandbox network namespace: %w",
		fmt.Errorf("%w (last attempt: %w)", context.DeadlineExceeded, syscall.ENOENT))
	plainFault := errors.New("failed to start DHCP client: exec format error")
	notFound := fmt.Errorf("failed to get Docker container info: %w",
		fmt.Errorf("Error response from daemon: No such container: deadbeef: %w", cerrdefs.ErrNotFound))

	state := func(status string) map[string]dContainer.InspectResponse {
		return map[string]dContainer.InspectResponse{vanishCtr: {State: &dContainer.State{Status: dContainer.ContainerState(status), Running: status == "running"}}}
	}
	attached := map[string]dNetwork.Inspect{vanishNet: {Containers: map[string]dNetwork.EndpointResource{
		vanishCtr: {EndpointID: vanishEP},
	}}}
	notAttached := map[string]dNetwork.Inspect{vanishNet: {}}

	type want struct{ gone, startFail int32 }
	vanished, faulted := want{gone: 1}, want{startFail: 1}

	for _, tc := range []struct {
		name       string
		err        error
		keyPresent bool
		fake       *fakeDocker
		want       want
		wantCalls  int
	}{
		{"deadline without a key, container running", deadlineNoKey, false,
			&fakeDocker{inspectResult: attached, containerResult: state("running")}, faulted, 1},
		{"deadline without a key, container created and not yet started", deadlineNoKey, false,
			&fakeDocker{inspectResult: attached, containerResult: state("created")}, faulted, 1},
		{"deadline without a key, daemon errors", deadlineNoKey, false,
			&fakeDocker{inspectResult: attached, containerErr: errors.New("connection refused")}, faulted, 1},
		{"deadline without a key, daemon deadline", deadlineNoKey, false,
			&fakeDocker{inspectResult: attached, containerErr: context.DeadlineExceeded}, faulted, 1},
		{"deadline without a key, network inspect errors", deadlineNoKey, false,
			&fakeDocker{inspectErr: errors.New("connection refused")}, faulted, 0},
		{"deadline without a key, endpoint not in the network", deadlineNoKey, false,
			&fakeDocker{inspectResult: notAttached}, faulted, 0},
		{"deadline without a key, daemon has no such container", deadlineNoKey, false,
			&fakeDocker{inspectResult: attached, containerErr: fmt.Errorf("inspect: %w", cerrdefs.ErrNotFound)}, vanished, 1},
		{"deadline without a key, container exited", deadlineNoKey, false,
			&fakeDocker{inspectResult: attached, containerResult: state("exited")}, vanished, 1},
		{"deadline without a key, container dead", deadlineNoKey, false,
			&fakeDocker{inspectResult: attached, containerResult: state("dead")}, vanished, 1},
		{"the key was linked then unlinked, container removed", plainFault, false,
			&fakeDocker{inspectResult: attached, containerErr: cerrdefs.ErrNotFound}, vanished, 1},
		{"the key was unlinked, container still running", plainFault, false,
			&fakeDocker{inspectResult: attached, containerResult: state("running")}, faulted, 1},
		{"the daemon already said no such container", notFound, false,
			&fakeDocker{inspectErr: errors.New("connection refused")}, vanished, 0},
		{"key present, plain fault: the daemon is never asked", plainFault, true,
			&fakeDocker{inspectResult: attached, containerErr: cerrdefs.ErrNotFound}, faulted, 0},
		{"no container claimed the endpoint", fmt.Errorf("%w: ep", util.ErrNoContainer), false,
			&fakeDocker{inspectResult: notAttached}, want{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			saved := sandboxNetnsDirs
			sandboxNetnsDirs = []string{dir}
			t.Cleanup(func() { sandboxNetnsDirs = saved })
			key := filepath.Join(dir, "netns-abc")
			if tc.keyPresent {
				if err := os.WriteFile(key, nil, 0o644); err != nil {
					t.Fatalf("seed sandbox key: %v", err)
				}
			}

			p := &Plugin{docker: tc.fake}
			r := JoinRequest{NetworkID: vanishNet, EndpointID: vanishEP, SandboxKey: key}
			m := &dhcpManager{}
			p.settleFailedAttach(r, m, tc.err)

			if tc.name == "no container claimed the endpoint" {
				if got := p.joinAbortedNoContainer.Load(); got != 1 {
					t.Errorf("join_aborted_no_container = %d, want 1", got)
				}
				return
			}
			if got := p.joinAbortedContainerGone.Load(); got != tc.want.gone {
				t.Errorf("join_aborted_container_gone = %d, want %d", got, tc.want.gone)
			}
			if got := p.joinStartFailures.Load(); got != tc.want.startFail {
				t.Errorf("join_start_failures = %d, want %d", got, tc.want.startFail)
			}
			if tc.fake.containerCalls != tc.wantCalls {
				t.Errorf("ContainerInspect calls = %d, want %d", tc.fake.containerCalls, tc.wantCalls)
			}
		})
	}
}

func TestSettleFailedAttach_AHungDaemonDoesNotHoldTheAttachPastTheBound(t *testing.T) {
	saved := vanishConfirmTimeout
	vanishConfirmTimeout = 50 * time.Millisecond
	t.Cleanup(func() { vanishConfirmTimeout = saved })

	dir := t.TempDir()
	savedDirs := sandboxNetnsDirs
	sandboxNetnsDirs = []string{dir}
	t.Cleanup(func() { sandboxNetnsDirs = savedDirs })

	f := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{vanishNet: {Containers: map[string]dNetwork.EndpointResource{
			vanishCtr: {EndpointID: vanishEP},
		}}},
		containerDelay: 10 * time.Second,
	}
	p := &Plugin{docker: f}
	r := JoinRequest{NetworkID: vanishNet, EndpointID: vanishEP, SandboxKey: filepath.Join(dir, "netns-abc")}
	err := fmt.Errorf("%w (last attempt: %w)", context.DeadlineExceeded, syscall.ENOENT)

	start := time.Now()
	p.settleFailedAttach(r, &dhcpManager{}, err)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("settleFailedAttach took %v against a 50ms bound; the inspect is not bounded", took)
	}
	if got := p.joinAbortedContainerGone.Load(); got != 0 {
		t.Errorf("a daemon that never answered was counted as a vanished container (%d)", got)
	}
	if got := p.joinStartFailures.Load(); got != 1 {
		t.Errorf("join_start_failures = %d, want 1", got)
	}
}

func TestSettleFailedAttach_ALeavingEndpointStaysFirst(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{}
	m.attachAborted.Store(true)
	p.settleFailedAttach(JoinRequest{EndpointID: vanishEP}, m, context.Canceled)
	if p.joinAbortedEndpointLeft.Load() != 1 || p.joinStartFailures.Load() != 0 || p.joinAbortedContainerGone.Load() != 0 {
		t.Errorf("an attach cancelled by Leave was not counted as endpoint-left alone")
	}
}
