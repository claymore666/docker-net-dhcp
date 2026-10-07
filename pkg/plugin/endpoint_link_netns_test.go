// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// sandboxLinks is a second network namespace standing in for a container's, with the links the engine moved into it.
type sandboxLinks struct {
	ns netns.NsHandle
	h  *netlink.Handle
}

func newSandbox(t *testing.T) sandboxLinks {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	host, err := netns.Get()
	if err != nil {
		t.Fatalf("read this namespace: %v", err)
	}
	defer host.Close()
	ns, err := netns.New()
	if err != nil {
		t.Fatalf("create the sandbox namespace: %v", err)
	}
	if err := netns.Set(host); err != nil {
		t.Fatalf("return to the host namespace: %v", err)
	}
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatalf("netlink handle in the sandbox: %v", err)
	}
	t.Cleanup(func() {
		h.Close()
		_ = ns.Close()
	})
	return sandboxLinks{ns: ns, h: h}
}

// moveIn does what the engine does at Join: moves the host-side child into the sandbox and renames it. It returns
// the index the link holds there, the identity a lookup is judged by, since the lookup may see it before the rename.
func (s sandboxLinks) moveIn(t *testing.T, hostName, ctrName string) int {
	t.Helper()
	if err := netlink.LinkSetNsFd(mustLinkByName(t, hostName), int(s.ns)); err != nil {
		t.Fatalf("move %s into the sandbox: %v", hostName, err)
	}
	l, err := s.h.LinkByName(hostName)
	if err != nil {
		t.Fatalf("%s after the move: %v", hostName, err)
	}
	if err := s.h.LinkSetName(l, ctrName); err != nil {
		t.Fatalf("rename %s to %s: %v", hostName, ctrName, err)
	}
	return l.Attrs().Index
}

// hostParents makes the parent NIC and a vlan sub-interface of it, which wears the same MAC.
func hostParents(t *testing.T) (parent, vlan netlink.Link) {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "p1243"}}); err != nil {
		t.Fatalf("create the parent: %v", err)
	}
	parent = mustLinkByName(t, "p1243")
	t.Cleanup(func() { _ = netlink.LinkDel(parent) })
	if err := netlink.LinkSetUp(parent); err != nil {
		t.Fatalf("set the parent up: %v", err)
	}
	err := netlink.LinkAdd(&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "p1243.7", ParentIndex: parent.Attrs().Index},
		VlanId: 7, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q})
	if err != nil {
		t.Fatalf("create the vlan sub-interface: %v", err)
	}
	vlan = mustLinkByName(t, "p1243.7")
	if err := netlink.LinkSetUp(vlan); err != nil {
		t.Fatalf("set the vlan sub-interface up: %v", err)
	}
	return parent, vlan
}

// childFor adds an endpoint's child under subLinkName on parent and tags it as CreateEndpoint does; an empty
// endpointID makes an untagged child, as the engine's built-in ipvlan or macvlan driver leaves.
func childFor(t *testing.T, endpointID, name string, opts DHCPNetworkOptions, parent netlink.Link) string {
	t.Helper()
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.ParentIndex = parent.Attrs().Index
	if opts.macvlanPassthru() {
		la.HardwareAddr = parent.Attrs().HardwareAddr
	}
	link, err := newChildLink(opts, la)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("create %s on %s: %v", name, parent.Attrs().Name, err)
	}
	if endpointID != "" {
		if err := tagEndpointLink(mustLinkByName(t, name), endpointID); err != nil {
			t.Fatalf("tag %s: %v", name, err)
		}
	}
	return name
}

func ipvlanOn(parent string) DHCPNetworkOptions {
	return DHCPNetworkOptions{Mode: ModeIPvlan, Parent: parent}
}

func passthruOn(parent string) DHCPNetworkOptions {
	return DHCPNetworkOptions{Mode: ModeMacvlan, Parent: parent, MacvlanMode: "passthru"}
}

// locate runs the Join path's link lookup for one endpoint in the sandbox and returns the link it bound to.
func (s sandboxLinks) locate(t *testing.T, endpointID string, opts DHCPNetworkOptions, mac []byte, budget time.Duration) (netlink.Link, error) {
	t.Helper()
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1243", EndpointID: endpointID}, opts)
	m.MacAddress = mac
	m.netHandle = s.h
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	err := m.locateContainerLink(ctx)
	return m.ctrLink, err
}

const (
	ep1243A = "aaaa1243aaaa1243aaaa1243aaaa1243aaaa1243aaaa1243aaaa1243aaaa1243"
	ep1243B = "bbbb1243bbbb1243bbbb1243bbbb1243bbbb1243bbbb1243bbbb1243bbbb1243"
)

// Two endpoints whose children wear one MAC sit in one container; each client binds to its own link however the
// engine ordered and renamed them (#1243).
func TestLocateContainerLink_SameMACChildrenInOneContainerEachFindTheirOwn(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for _, tc := range []struct {
		name         string
		optsA, optsB func(parent, vlan string) DHCPNetworkOptions
		onVlanB      bool
	}{
		{"two ipvlan networks on one parent",
			func(p, _ string) DHCPNetworkOptions { return ipvlanOn(p) },
			func(p, _ string) DHCPNetworkOptions { return ipvlanOn(p) }, false},
		{"ipvlan on the parent and ipvlan on its vlan",
			func(p, _ string) DHCPNetworkOptions { return ipvlanOn(p) },
			func(_, v string) DHCPNetworkOptions { return ipvlanOn(v) }, true},
		{"ipvlan on the parent and passthru on its vlan",
			func(p, _ string) DHCPNetworkOptions { return ipvlanOn(p) },
			func(_, v string) DHCPNetworkOptions { return passthruOn(v) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, vlan := hostParents(t)
			mac := parent.Attrs().HardwareAddr
			optsA := tc.optsA(parent.Attrs().Name, vlan.Attrs().Name)
			optsB := tc.optsB(parent.Attrs().Name, vlan.Attrs().Name)
			parentB := parent
			if tc.onVlanB {
				parentB = vlan
			}
			s := newSandbox(t)
			indexA := s.moveIn(t, childFor(t, ep1243A, subLinkName(ep1243A), optsA, parent), "eth0")
			indexB := s.moveIn(t, childFor(t, ep1243B, subLinkName(ep1243B), optsB, parentB), "eth1")

			for _, want := range []struct {
				endpoint, ctrName string
				index             int
				opts              DHCPNetworkOptions
			}{{ep1243B, "eth1", indexB, optsB}, {ep1243A, "eth0", indexA, optsA}} {
				got, err := s.locate(t, want.endpoint, want.opts, mac, 2*time.Second)
				if err != nil {
					t.Fatalf("endpoint %s found no link: %v", want.endpoint[:4], err)
				}
				if got.Attrs().Index != want.index {
					t.Errorf("endpoint %s's client binds to %s (index %d), the other endpoint's link; want %s (index %d)",
						want.endpoint[:4], got.Attrs().Name, got.Attrs().Index, want.ctrName, want.index)
				}
			}
		})
	}
}

// The engine's own ipvlan or macvlan link on the same parent is already in the container while this endpoint's
// child is still on the host: the first poll must not take it (#1243).
func TestLocateContainerLink_AnUntaggedSameMACLinkIsNeverTakenOnJoin(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for _, tc := range []struct {
		name string
		opts func(parent, vlan string) DHCPNetworkOptions
		vlan bool
	}{
		{"ipvlan", func(p, _ string) DHCPNetworkOptions { return ipvlanOn(p) }, false},
		{"macvlan passthru", func(_, v string) DHCPNetworkOptions { return passthruOn(v) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, vlan := hostParents(t)
			opts := tc.opts(parent.Attrs().Name, vlan.Attrs().Name)
			own := parent
			if tc.vlan {
				own = vlan
			}
			s := newSandbox(t)
			s.moveIn(t, childFor(t, "", "engine1243", ipvlanOn(parent.Attrs().Name), parent), "eth0")
			ownName := childFor(t, ep1243B, subLinkName(ep1243B), opts, own)

			type result struct {
				link netlink.Link
				err  error
			}
			done := make(chan result, 1)
			go func() {
				l, err := s.locate(t, ep1243B, opts, parent.Attrs().HardwareAddr, 5*time.Second)
				done <- result{l, err}
			}()
			select {
			case r := <-done:
				name := "none"
				if r.link != nil {
					name = r.link.Attrs().Name
				}
				t.Fatalf("the lookup returned %s (err %v) before this endpoint's child reached the sandbox: the "+
					"client would bind to the engine's link", name, r.err)
			case <-time.After(5 * pollTime):
			}
			ownIndex := s.moveIn(t, ownName, "eth1")
			r := <-done
			if r.err != nil {
				t.Fatalf("no link once the child arrived: %v", r.err)
			}
			if r.link.Attrs().Index != ownIndex {
				t.Errorf("the client binds to %s (index %d), want this endpoint's child (index %d)",
					r.link.Attrs().Name, r.link.Attrs().Index, ownIndex)
			}
		})
	}
}

// CreateEndpoint leaves on the child the identity the sandbox lookup reads: the alias the kernel holds for the link,
// read back at the moment it was written, finds this endpoint and not another one (#1243).
func TestCreateEndpoint_TheChildCarriesTheIdentityTheSandboxLookupReads(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for _, opts := range []func(parent, vlan string) DHCPNetworkOptions{
		func(p, _ string) DHCPNetworkOptions { return ipvlanOn(p) },
		func(_, v string) DHCPNetworkOptions { return passthruOn(v) },
		func(p, _ string) DHCPNetworkOptions { return DHCPNetworkOptions{Mode: ModeMacvlan, Parent: p} },
	} {
		o := opts("p1243", "p1243.7")
		t.Run(o.Mode+" "+o.MacvlanMode, func(t *testing.T) {
			hostParents(t)
			withStateDir(t, t.TempDir())
			stop := errors.New("stopped after the tag")
			var written []netlink.Link
			prev := nlLinkSetAlias
			nlLinkSetAlias = func(l netlink.Link, alias string) error {
				if err := prev(l, alias); err != nil {
					return err
				}
				written = append(written, mustLinkByName(t, l.Attrs().Name))
				return stop
			}
			t.Cleanup(func() { nlLinkSetAlias = prev })
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			_, err := p.createParentAttachedEndpoint(context.Background(), time.Now(),
				CreateEndpointRequest{NetworkID: "net-1243", EndpointID: ep1243B}, o)
			if !errors.Is(err, stop) || len(written) != 1 {
				t.Fatalf("err = %v, %d alias writes: CreateEndpoint never tagged the child, so the sandbox lookup "+
					"cannot tell it from another child wearing the same MAC", err, len(written))
			}
			child := written[0]
			if child.Attrs().Name != subLinkName(ep1243B) {
				t.Fatalf("tagged %s, want the endpoint's child %s", child.Attrs().Name, subLinkName(ep1243B))
			}
			mac := child.Attrs().HardwareAddr
			if got, err := findEndpointLink(fakeLinkLister{links: []netlink.Link{child}}, mac, ep1243B, true, false); err != nil || got != child {
				t.Errorf("the lookup does not find the child CreateEndpoint made (err %v)", err)
			}
			if got, err := findEndpointLink(fakeLinkLister{links: []netlink.Link{child}}, mac, ep1243A, true, true); err == nil {
				t.Errorf("another endpoint's lookup took this child (%s)", got.Attrs().Name)
			}
		})
	}
}
