// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

type linkState int32

const (
	linkUp linkState = iota
	linkDown
	linkWithdrawn
	linkUnreadable
)

// withSandboxLink answers every read of the located link from state, with the kernel's own not-found answer for a
// withdrawn link: libnetwork moves a refused attach's link back to the host netns (#1236).
func withSandboxLink(t *testing.T, state *atomic.Int32) {
	t.Helper()
	_, kernelNotFound := netlink.LinkByIndex(math.MaxInt32)
	var nf netlink.LinkNotFoundError
	if !errors.As(kernelNotFound, &nf) {
		t.Fatalf("reading an index no link has returned %v, not a LinkNotFoundError, so a withdrawn link "+
			"cannot be driven here. This is the instrument failing", kernelNotFound)
	}
	prev := nlLinkByIndex
	nlLinkByIndex = func(_ *netlink.Handle, index int) (netlink.Link, error) {
		switch linkState(state.Load()) {
		case linkWithdrawn:
			return nil, kernelNotFound
		case linkUnreadable:
			return nil, fmt.Errorf("netlink receive: %w", syscall.EPERM)
		}
		real, err := netlink.LinkByIndex(index)
		if err != nil {
			return nil, err
		}
		attrs := *real.Attrs()
		attrs.Flags |= net.FlagUp
		if linkState(state.Load()) == linkDown {
			attrs.Flags &^= net.FlagUp
		}
		return &netlink.Device{LinkAttrs: attrs}, nil
	}
	t.Cleanup(func() { nlLinkByIndex = prev })
}

func withFailingClientStart(t *testing.T, at func()) {
	t.Helper()
	prev := startDHCPClient
	startDHCPClient = func(*dhcp.DHCPClient) (chan dhcp.Event, error) {
		at()
		return nil, fmt.Errorf("bind raw socket: %w", syscall.ENODEV)
	}
	t.Cleanup(func() { startDHCPClient = prev })
}

type attachOutcome struct{ withdrawn, startFail, gone, left int32 }

func outcomeOf(p *Plugin) attachOutcome {
	return attachOutcome{
		withdrawn: p.joinAbortedLinkWithdrawn.Load(),
		startFail: p.joinStartFailures.Load(),
		gone:      p.joinAbortedContainerGone.Load(),
		left:      p.joinAbortedEndpointLeft.Load(),
	}
}

func TestStart_ALinkTheEngineTakesBackIsAnAbortedAttachNotAStartFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		initial  linkState
		atClient linkState
		want     attachOutcome
	}{
		{"withdrawn before it came up", linkWithdrawn, linkWithdrawn, attachOutcome{withdrawn: 1}},
		{"withdrawn under the client start", linkUp, linkWithdrawn, attachOutcome{withdrawn: 1}},
		{"present and never up", linkDown, linkDown, attachOutcome{startFail: 1}},
		{"present, up, client start fails", linkUp, linkUp, attachOutcome{startFail: 1}},
		{"unreadable, not proven gone", linkUnreadable, linkUnreadable, attachOutcome{startFail: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prevAwait := linkAwaitTimeout
			linkAwaitTimeout = 3 * pollTime
			t.Cleanup(func() { linkAwaitTimeout = prevAwait })

			m, p := daemonFreeManager(t, &fakeDocker{})
			var state atomic.Int32
			state.Store(int32(tc.initial))
			withSandboxLink(t, &state)
			starts := 0
			withFailingClientStart(t, func() {
				starts++
				state.Store(int32(tc.atClient))
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := m.Start(ctx)
			if err == nil {
				t.Fatal("Start succeeded on a link that was withdrawn, down, unreadable or had no client")
			}
			if got, want := errors.Is(err, errContainerLinkWithdrawn), tc.want.withdrawn == 1; got != want {
				t.Errorf("Start returned %v; marked withdrawn = %v, want %v", err, got, want)
			}
			p.settleFailedAttach(m.joinReq, m, err)
			if got := outcomeOf(p); got != tc.want {
				t.Errorf("counted %+v, want %+v: a refused attach left no running container to renew for, "+
					"and a link that is still in the sandbox is a real start failure (#1236)", got, tc.want)
			}
			if tc.initial != linkUp && starts != 0 {
				t.Errorf("%d client start(s) on a link that never came up", starts)
			}
		})
	}
}

func TestSettleFailedAttach_AWithdrawnLinkCountsOnceBehindTheStrongerEvidence(t *testing.T) {
	withdrawn := fmt.Errorf("%w: container link eth1 was never set up: Link not found", errContainerLinkWithdrawn)
	for _, tc := range []struct {
		name  string
		err   error
		leave bool
		want  attachOutcome
	}{
		{"Leave arrived first", withdrawn, true, attachOutcome{left: 1}},
		{"the daemon says no such container", fmt.Errorf("%w: %w", withdrawn, cerrdefs.ErrNotFound), false,
			attachOutcome{gone: 1}},
		{"withdrawn alone", withdrawn, false, attachOutcome{withdrawn: 1}},
		{"a plain fault", errors.New("container link eth1 was never set up: context deadline exceeded"), false,
			attachOutcome{startFail: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			m := &dhcpManager{}
			m.attachAborted.Store(tc.leave)
			p.settleFailedAttach(JoinRequest{NetworkID: vanishNet, EndpointID: vanishEP}, m, tc.err)
			if got := outcomeOf(p); got != tc.want {
				t.Errorf("counted %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The re-read must ask the sandbox's own handle: a closed handle falls back to the caller's netns, where the
// sandbox's index does not exist, and a live link would read as withdrawn (#1236).
func TestStart_TheWithdrawnCheckAsksTheSandboxHandleWhileItIsOpen(t *testing.T) {
	prevAwait := linkAwaitTimeout
	linkAwaitTimeout = 3 * pollTime
	t.Cleanup(func() { linkAwaitTimeout = prevAwait })

	m, p := daemonFreeManager(t, &fakeDocker{})
	var sandbox *netlink.Handle
	prevNew := nlNewHandleAt
	nlNewHandleAt = func(netns.NsHandle, ...int) (*netlink.Handle, error) {
		h, err := netlink.NewHandle()
		sandbox = h
		return h, err
	}
	t.Cleanup(func() { nlNewHandleAt = prevNew })

	_, kernelNotFound := netlink.LinkByIndex(math.MaxInt32)
	prevRead := nlLinkByIndex
	nlLinkByIndex = func(h *netlink.Handle, index int) (netlink.Link, error) {
		open, _ := h.GetSocketReceiveBufferSize()
		if h != sandbox || len(open) == 0 || m.ctrLink == nil || index != m.ctrLink.Attrs().Index {
			return nil, kernelNotFound
		}
		real, err := netlink.LinkByIndex(index)
		if err != nil {
			return nil, err
		}
		attrs := *real.Attrs()
		attrs.Flags &^= net.FlagUp
		return &netlink.Device{LinkAttrs: attrs}, nil
	}
	t.Cleanup(func() { nlLinkByIndex = prevRead })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded on a link that never came up")
	}
	if sandbox == nil {
		t.Fatal("Start opened no sandbox handle, so the test drove nothing. This is the instrument failing")
	}
	p.settleFailedAttach(m.joinReq, m, err)
	if got, want := outcomeOf(p), (attachOutcome{startFail: 1}); got != want {
		t.Errorf("counted %+v, want %+v: the link is still in the sandbox, so this is a start failure (%v)",
			got, want, err)
	}
}
