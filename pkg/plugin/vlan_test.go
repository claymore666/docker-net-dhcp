// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	vlanTestParentIndex = 2
	vlanTestLinkIndex   = 5
)

var (
	vlanNetA = strings.Repeat("a", 64)
	vlanNetB = strings.Repeat("b", 64)
)

func vlanTestParent() *netlink.Device {
	return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: vlanTestParentIndex, Flags: net.FlagUp}}
}

func vlanTestLink(alias string) *netlink.Vlan {
	return &netlink.Vlan{
		LinkAttrs:    netlink.LinkAttrs{Name: "eth0.100", Index: vlanTestLinkIndex, ParentIndex: vlanTestParentIndex, Alias: alias},
		VlanId:       100,
		VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
	}
}

func upVlan(v *netlink.Vlan) *netlink.Vlan {
	v.Flags |= net.FlagUp
	return v
}

func vlanOpts(id string) DHCPNetworkOptions {
	return DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0", Vlan: id}
}

// vlanKernel is the host's links as the seams see them; deleted and added record what the plugin asked the kernel
// to do, the only outside evidence a unit test has (#902).
type vlanKernel struct {
	links     map[string]netlink.Link
	deleted   []string
	added     []netlink.Link
	addErr    error
	trialErr  map[string]error
	aliasErr  error
	aliasSets []string
	v6Off     []string
	v6OffErr  error
	newIndex  int
}

func stubVlanKernel(t *testing.T, links ...netlink.Link) *vlanKernel {
	t.Helper()
	k := &vlanKernel{links: map[string]netlink.Link{}}
	for _, l := range links {
		k.links[l.Attrs().Name] = l
	}
	prevBy, prevDel, prevUp, prevList, prevAlias := nlLinkByName, nlLinkDel, nlLinkSetUp, nlLinkList, nlLinkSetAlias
	prevAdd, prevTrial, prevV6 := vlanLinkAdd, vlanTrialAdd, vlanHostIPv6Off
	t.Cleanup(func() {
		nlLinkByName, nlLinkDel, nlLinkSetUp, nlLinkList, nlLinkSetAlias = prevBy, prevDel, prevUp, prevList, prevAlias
		vlanLinkAdd, vlanTrialAdd, vlanHostIPv6Off = prevAdd, prevTrial, prevV6
	})
	vlanHostIPv6Off = func(name string) error {
		state := "down"
		if l, ok := k.links[name]; ok && l.Attrs().Flags&net.FlagUp != 0 {
			state = "up"
		}
		k.v6Off = append(k.v6Off, name+" while "+state)
		return k.v6OffErr
	}
	nlLinkByName = func(name string) (netlink.Link, error) {
		if l, ok := k.links[name]; ok {
			return l, nil
		}
		return nil, netlink.LinkNotFoundError{}
	}
	nlLinkDel = func(l netlink.Link) error {
		k.deleted = append(k.deleted, l.Attrs().Name)
		delete(k.links, l.Attrs().Name)
		return nil
	}
	nlLinkSetUp = func(l netlink.Link) error {
		l.Attrs().Flags |= net.FlagUp
		return nil
	}
	nlLinkList = func() ([]netlink.Link, error) {
		var out []netlink.Link
		for _, l := range k.links {
			out = append(out, l)
		}
		return out, nil
	}
	nlLinkSetAlias = func(l netlink.Link, alias string) error {
		k.aliasSets = append(k.aliasSets, l.Attrs().Name+"="+alias)
		if k.aliasErr != nil {
			return k.aliasErr
		}
		l.Attrs().Alias = alias
		return nil
	}
	vlanLinkAdd = func(_ *parentGuard, l netlink.Link) error {
		k.added = append(k.added, l)
		if k.addErr != nil {
			return k.addErr
		}
		if l.Attrs().Index == 0 {
			l.Attrs().Index = vlanTestLinkIndex
			if k.newIndex != 0 {
				l.Attrs().Index = k.newIndex
			}
		}
		// The kernel gives a new vlan its lower's mtu, measured on Linux 6.12 (#902).
		for _, lower := range k.links {
			if lower.Attrs().Index == l.Attrs().ParentIndex && l.Attrs().MTU == 0 {
				l.Attrs().MTU = lower.Attrs().MTU
			}
		}
		k.links[l.Attrs().Name] = l
		return nil
	}
	vlanTrialAdd = func(_ *parentGuard, l netlink.Link) error {
		k.added = append(k.added, l)
		if err := k.trialErr[l.Type()]; err != nil {
			return err
		}
		k.links[l.Attrs().Name] = l
		return nil
	}
	return k
}

func (k *vlanKernel) removed(name string) bool {
	for _, d := range k.deleted {
		if d == name {
			return true
		}
	}
	return false
}

func TestParseVlanID(t *testing.T) {
	for v, want := range map[string]int{"": 0, "1": 1, "100": 100, "4094": 4094} {
		if got, err := parseVlanID(v); err != nil || got != want {
			t.Errorf("parseVlanID(%q) = %d, %v; want %d, nil", v, got, err, want)
		}
	}
	for _, v := range []string{"0", "4095", "-1", "abc", "010", "+5", " 5", "5 ", "1e2", "99999999999999999999"} {
		if got, err := parseVlanID(v); !errors.Is(err, util.ErrIPAM) {
			t.Errorf("parseVlanID(%q) = %d, %v; want a refusal. 0 and 4095 are reserved by 802.1Q, and a "+
				"non-canonical spelling names a link other than the one the option reads as (#902).", v, got, err)
		}
	}
}

func TestLinkParent(t *testing.T) {
	if got := vlanOpts("").linkParent(); got != "eth0" {
		t.Errorf("linkParent without vlan = %q, want eth0", got)
	}
	if got := vlanOpts("100").linkParent(); got != "eth0.100" {
		t.Errorf("linkParent with vlan=100 = %q, want eth0.100, the name Docker's own macvlan driver uses", got)
	}
}

func TestValidateVlanOption(t *testing.T) {
	ok := []DHCPNetworkOptions{
		{Mode: ModeBridge, Bridge: "br0"},
		vlanOpts("100"),
		{Mode: ModeIPvlan, Parent: "eth0", Vlan: "4094"},
		{Mode: ModeMacvlan, Parent: "abcdefghij", Vlan: "4094"},
	}
	for _, o := range ok {
		if err := validateVlanOption(o); err != nil {
			t.Errorf("validateVlanOption(%+v) = %v, want nil", o, err)
		}
	}
	if err := validateVlanOption(DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", Vlan: "100"}); !errors.Is(err, util.ErrModeMismatch) {
		t.Errorf("vlan in bridge mode: err = %v, want ErrModeMismatch; a bridge has no parent to tag on, and "+
			"accepting it would leave the containers untagged without a word (#902)", err)
	}
	if err := validateVlanOption(vlanOpts("0")); !errors.Is(err, util.ErrIPAM) {
		t.Errorf("vlan=0: err = %v, want a refusal", err)
	}
	err := validateVlanOption(DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "abcdefghijk", Vlan: "4094"})
	if err == nil || !strings.Contains(err.Error(), "abcdefghijk.4094") || !strings.Contains(err.Error(), "15 bytes") {
		t.Errorf("a 16-byte sub-interface name: err = %v, want a refusal naming the link and the 15-byte limit; "+
			"the kernel would refuse it only at the first create, as a bare EINVAL (#902)", err)
	}
}

func TestVlanVerdict(t *testing.T) {
	good := vlanTestLink("")
	if err := vlanVerdict(good, vlanTestParentIndex, 100); err != nil {
		t.Errorf("a matching 802.1Q vlan: err = %v, want nil (adopt)", err)
	}
	ad := vlanTestLink("")
	ad.VlanProtocol = netlink.VLAN_PROTOCOL_8021AD
	otherID := vlanTestLink("")
	otherID.VlanId = 200
	otherParent := vlanTestLink("")
	otherParent.ParentIndex = 9
	for name, l := range map[string]netlink.Link{
		"a dummy of that name": &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0.100"}},
		"another vlan id":      otherID,
		"another parent":       otherParent,
		"802.1ad":              ad,
	} {
		if err := vlanVerdict(l, vlanTestParentIndex, 100); !errors.Is(err, util.ErrParentInvalid) {
			t.Errorf("%s: err = %v, want ErrParentInvalid; endpoints on it would lease on another segment (#902)", name, err)
		}
	}
}

func TestVlanUsers(t *testing.T) {
	stored := map[string]DHCPNetworkOptions{
		vlanNetA: vlanOpts("100"),
		vlanNetB: vlanOpts("200"),
	}
	if got := vlanUsers("eth0.100", vlanNetA, stored, nil); len(got) != 0 {
		t.Errorf("the deleting network counted itself: %v", got)
	}
	if got := vlanUsers("eth0.200", vlanNetA, stored, nil); len(got) != 1 {
		t.Errorf("a stored network on eth0.200: users = %v, want one", got)
	}
	nets := []dNetwork.Summary{
		{ID: "native1", Name: "native-tagged", Driver: "macvlan", Options: map[string]string{"parent": "eth0.100"}},
		{ID: "native2", Name: "native-plain", Driver: "macvlan", Options: map[string]string{"parent": "eth0"}},
		{ID: vlanNetB, Name: "stored-b", Driver: "ghcr.io/claymore666/docker-net-dhcp:latest", Options: map[string]string{"mode": "macvlan", "parent": "eth0", "vlan": "100"}},
	}
	got := vlanUsers("eth0.100", vlanNetA, stored, nets)
	if len(got) != 1 || got[0] != "native-tagged" {
		t.Errorf("users = %v, want [native-tagged]: Docker's own macvlan on eth0.100 dies with the link, the untagged "+
			"one does not, and a stored network is judged by its record, not by Docker's copy (#902)", got)
	}
}

func TestStackedOn(t *testing.T) {
	sub := vlanTestLink("")
	child := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: "c1", Index: 7, ParentIndex: vlanTestLinkIndex}}
	other := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: "c2", Index: 8, ParentIndex: vlanTestParentIndex}}
	got := stackedOn([]netlink.Link{vlanTestParent(), sub, child, other}, vlanTestLinkIndex)
	if len(got) != 1 || got[0] != "c1" {
		t.Errorf("stackedOn = %v, want [c1]", got)
	}
}

func TestVlanOccupied(t *testing.T) {
	busy := errors.New("device or resource busy")
	for _, tc := range []struct {
		name       string
		refuse     string
		wantHolder string
	}{
		{"nothing on it", "", ""},
		{"a hidden macvlan child refuses the ipvlan trial", "ipvlan", "a macvlan or macvtap child"},
		{"a hidden ipvlan child refuses the macvlan trial", "macvlan", "an ipvlan child, a macvlan passthru child or a bridge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := stubVlanKernel(t, vlanTestParent(), vlanTestLink(vlanOwnerAlias))
			if tc.refuse != "" {
				k.trialErr = map[string]error{tc.refuse: busy}
			}
			holder, err := vlanOccupied(&parentGuard{}, vlanTestLink(vlanOwnerAlias))
			if (err != nil) != (tc.refuse != "") || holder != tc.wantHolder {
				t.Fatalf("vlanOccupied = %q, %v; want holder %q", holder, err, tc.wantHolder)
			}
			for _, l := range k.added {
				if l.Attrs().ParentIndex != vlanTestLinkIndex || !strings.HasPrefix(l.Attrs().Name, "dh-probe-") {
					t.Errorf("trial %s on index %d, want a dh-probe- child on the sub-interface", l.Attrs().Name, l.Attrs().ParentIndex)
				}
				if _, left := k.links[l.Attrs().Name]; left {
					t.Errorf("trial child %s left behind", l.Attrs().Name)
				}
			}
			if tc.refuse == "" && len(k.added) != 2 {
				t.Errorf("trials = %d, want 2: one kind alone misses the other kind's hidden child (P1-P7)", len(k.added))
			}
		})
	}
}

func vlanRetirePlugin(t *testing.T, nets []dNetwork.Summary, listErr error) *Plugin {
	t.Helper()
	p := newTestPlugin(t)
	p.docker = &fakeDocker{listResult: nets, listErr: listErr}
	return p
}

func TestRetireVlanLink(t *testing.T) {
	busy := errors.New("device or resource busy")
	for _, tc := range []struct {
		name       string
		alias      string
		pending    bool
		storeOther bool
		nets       []dNetwork.Summary
		listErr    error
		extra      []netlink.Link
		trialErr   map[string]error
		wantGone   bool
	}{
		{name: "last user of a marked link removes it", alias: vlanOwnerAlias, wantGone: true},
		{name: "an unmarked link stays", alias: ""},
		{name: "a create in flight keeps it", alias: vlanOwnerAlias, pending: true},
		{name: "another stored network keeps it", alias: vlanOwnerAlias, storeOther: true},
		{name: "Docker's own macvlan on it keeps it", alias: vlanOwnerAlias,
			nets: []dNetwork.Summary{{ID: "n", Name: "native", Driver: "macvlan", Options: map[string]string{"parent": "eth0.100"}}}},
		{name: "an unreadable Docker list keeps it", alias: vlanOwnerAlias, listErr: errors.New("daemon gone")},
		{name: "a host link on it keeps it", alias: vlanOwnerAlias,
			extra: []netlink.Link{&netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: "user0", Index: 9, ParentIndex: vlanTestLinkIndex}}}},
		{name: "a child hidden in a container keeps it", alias: vlanOwnerAlias, trialErr: map[string]error{"ipvlan": busy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := vlanRetirePlugin(t, tc.nets, tc.listErr)
			links := append([]netlink.Link{vlanTestParent(), vlanTestLink(tc.alias)}, tc.extra...)
			k := stubVlanKernel(t, links...)
			k.trialErr = tc.trialErr
			if tc.storeOther {
				if err := saveOptions(vlanNetB, vlanOpts("100")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.pending {
				defer p.beginVlanCreate(vlanOpts("100"))()
			}
			p.retireVlanLink(context.Background(), vlanNetA, vlanOpts("100"), "delete_network")
			if got := k.removed("eth0.100"); got != tc.wantGone {
				t.Errorf("eth0.100 removed = %v, want %v (deleted: %v)", got, tc.wantGone, k.deleted)
			}
		})
	}

	t.Run("an unreadable state dir keeps it", func(t *testing.T) {
		p := vlanRetirePlugin(t, nil, nil)
		withStateDir(t, "/nonexistent-902")
		k := stubVlanKernel(t, vlanTestParent(), vlanTestLink(vlanOwnerAlias))
		p.retireVlanLink(context.Background(), vlanNetA, vlanOpts("100"), "delete_network")
		if k.removed("eth0.100") {
			t.Error("removed eth0.100 without knowing who else uses it")
		}
	})

	t.Run("no vlan touches nothing", func(t *testing.T) {
		p := vlanRetirePlugin(t, nil, nil)
		k := stubVlanKernel(t, vlanTestParent())
		p.retireVlanLink(context.Background(), vlanNetA, vlanOpts(""), "delete_network")
		if len(k.deleted) != 0 || p.docker.(*fakeDocker).listCalls != 0 {
			t.Errorf("a network without vlan deleted %v", k.deleted)
		}
	})
}

func TestEnsureVlanLink(t *testing.T) {
	t.Run("creates, marks and raises a missing link", func(t *testing.T) {
		p := newTestPlugin(t)
		k := stubVlanKernel(t, vlanTestParent())
		created, err := p.ensureVlanLink(context.Background(), vlanOpts("100"), "create_network")
		if err != nil || !created {
			t.Fatalf("ensure = %v, %v; want created", created, err)
		}
		v, ok := k.links["eth0.100"].(*netlink.Vlan)
		if !ok || v.VlanId != 100 || v.ParentIndex != vlanTestParentIndex || v.VlanProtocol != netlink.VLAN_PROTOCOL_8021Q {
			t.Fatalf("created %#v, want an 802.1Q vlan 100 on eth0", k.links["eth0.100"])
		}
		if v.Alias != vlanOwnerAlias || v.Flags&net.FlagUp == 0 {
			t.Errorf("alias %q, up %v; want the mark and up, else it is never removed and passes no traffic", v.Alias, v.Flags&net.FlagUp != 0)
		}
		if len(k.v6Off) != 1 || k.v6Off[0] != "eth0.100 while down" {
			t.Errorf("host IPv6 off calls %v, want one on eth0.100 while down: once up, the host takes a link-local, "+
				"a SLAAC address and the vlan router's default route (#902)", k.v6Off)
		}
	})

	t.Run("a failed host IPv6 off removes the link it made", func(t *testing.T) {
		p := newTestPlugin(t)
		k := stubVlanKernel(t, vlanTestParent())
		k.v6OffErr = errors.New("read-only file system")
		if _, err := p.ensureVlanLink(context.Background(), vlanOpts("100"), "create_network"); err == nil {
			t.Fatal("ensure succeeded with the host still on the vlan over IPv6")
		}
		if !k.removed("eth0.100") {
			t.Error("the link was left behind with host IPv6 on")
		}
	})

	t.Run("a vlan of that ID under another name is named", func(t *testing.T) {
		for _, c := range []struct {
			name      string
			other     *netlink.Vlan
			wantNamed bool
		}{
			{"same parent and ID", &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100", Index: 9, ParentIndex: vlanTestParentIndex}, VlanId: 100, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q}, true},
			{"another ID", &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100", Index: 9, ParentIndex: vlanTestParentIndex}, VlanId: 101, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q}, false},
			{"another parent", &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100", Index: 9, ParentIndex: 3}, VlanId: 100, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q}, false},
			{"802.1ad", &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100", Index: 9, ParentIndex: vlanTestParentIndex}, VlanId: 100, VlanProtocol: netlink.VLAN_PROTOCOL_8021AD}, false},
		} {
			t.Run(c.name, func(t *testing.T) {
				p := newTestPlugin(t)
				k := stubVlanKernel(t, vlanTestParent(), c.other)
				k.addErr = unix.EEXIST
				_, err := p.ensureVlanLink(context.Background(), vlanOpts("100"), "create_network")
				named := errors.Is(err, util.ErrParentInvalid) && strings.Contains(err.Error(), "as vlan100")
				if err == nil || named != c.wantNamed {
					t.Errorf("err = %v, named %v; want named %v: the kernel says only file exists (#902)", err, named, c.wantNamed)
				}
			})
		}
	})

	t.Run("adopts a matching link without marking it", func(t *testing.T) {
		p := newTestPlugin(t)
		k := stubVlanKernel(t, vlanTestParent(), vlanTestLink(""))
		created, err := p.ensureVlanLink(context.Background(), vlanOpts("100"), "create_network")
		if err != nil || created || len(k.added) != 0 || len(k.aliasSets) != 0 {
			t.Errorf("ensure = %v, %v, added %d, aliases %v; want adopted untouched, so a user's link is never removed",
				created, err, len(k.added), k.aliasSets)
		}
	})

	t.Run("refuses a foreign link of that name", func(t *testing.T) {
		p := newTestPlugin(t)
		stubVlanKernel(t, vlanTestParent(), &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0.100"}})
		if _, err := p.ensureVlanLink(context.Background(), vlanOpts("100"), "create_network"); !errors.Is(err, util.ErrParentInvalid) {
			t.Errorf("err = %v, want ErrParentInvalid", err)
		}
	})

	t.Run("a link made by someone else during the create is judged", func(t *testing.T) {
		p := newTestPlugin(t)
		k := stubVlanKernel(t, vlanTestParent())
		vlanLinkAdd = func(_ *parentGuard, l netlink.Link) error {
			k.links[l.Attrs().Name] = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: l.Attrs().Name}}
			return unix.EEXIST
		}
		if _, err := p.ensureVlanLink(context.Background(), vlanOpts("100"), "create_network"); !errors.Is(err, util.ErrParentInvalid) {
			t.Errorf("err = %v, want the foreign-link refusal", err)
		}
	})

	t.Run("a failed setup removes the link it made", func(t *testing.T) {
		p := newTestPlugin(t)
		k := stubVlanKernel(t, vlanTestParent())
		k.aliasErr = errors.New("alias refused")
		if _, err := p.ensureVlanLink(context.Background(), vlanOpts("100"), "create_network"); err == nil {
			t.Fatal("ensure succeeded with the mark unset")
		}
		if !k.removed("eth0.100") {
			t.Error("an unmarked link the plugin made was left behind; nothing would ever remove it")
		}
	})

	t.Run("a down parent is refused", func(t *testing.T) {
		p := newTestPlugin(t)
		parent := vlanTestParent()
		parent.Flags = 0
		k := stubVlanKernel(t, parent)
		if _, err := p.ensureVlanLink(context.Background(), vlanOpts("100"), "create_network"); !errors.Is(err, util.ErrParentDown) {
			t.Errorf("err = %v, want ErrParentDown", err)
		}
		if len(k.added) != 0 {
			t.Error("created a sub-interface on a down parent")
		}
	})
}

func TestLockParent_AVlanCoexistsWithEveryChildKind(t *testing.T) {
	giveUp := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	for _, pair := range [][2]string{
		{ModeMacvlan, parentGateKindVlan},
		{ModeIPvlan, parentGateKindVlan},
		{parentGateKindVlan, ModeMacvlan},
		{parentGateKindVlan, ModeIPvlan},
	} {
		p := &Plugin{}
		holder := p.lockParent(context.Background(), "eth0", pair[0], "preflight_probe")
		p.lockParent(giveUp(), "eth0", pair[1], "create_network").Unlock()
		holder.Unlock()
		if p.parentLinkWaitTimeouts.Load() != 0 || p.parentLinkWaits.Load() != 1 {
			t.Errorf("%s behind %s: timeouts %d, waits %d; want 0 and 1. The kernel accepted a vlan beside every "+
				"child kind (K1-K5), so a WARN that it may refuse this is false (#902).",
				pair[1], pair[0], p.parentLinkWaitTimeouts.Load(), p.parentLinkWaits.Load())
		}
	}

	p := &Plugin{}
	release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", "", 0)
	if !ok {
		t.Fatal("could not take an uncontended gate")
	}
	p.lockParent(giveUp(), "eth0", parentGateKindVlan, "create_network").Unlock()
	release()
	if got := p.parentLinkWaitTimeouts.Load(); got != 1 {
		t.Errorf("vlan behind an unknown holder: timeouts = %d, want 1; an unknown kind is no evidence (#110)", got)
	}
}

func TestEnsureVlanLink_TheCreateReportsTheVlanKind(t *testing.T) {
	p := newTestPlugin(t)
	k := stubVlanKernel(t, vlanTestParent())
	holder := p.lockParent(context.Background(), "eth0", ModeMacvlan, "preflight_probe")
	defer holder.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ensureVlanLink(ctx, vlanOpts("100"), "create_network"); err != nil {
		t.Fatal(err)
	}
	if len(k.added) != 1 || p.parentLinkWaitTimeouts.Load() != 0 || p.parentLinkWaits.Load() != 1 {
		t.Errorf("added %d, timeouts %d, waits %d; want 1, 0, 1: the create waits on the physical parent's gate "+
			"and reports a kind the kernel accepts beside a macvlan probe (K1, #902)",
			len(k.added), p.parentLinkWaitTimeouts.Load(), p.parentLinkWaits.Load())
	}
}

func vlanCreate(p *Plugin, opts map[string]interface{}, ipv4 *IPAMData) error {
	if ipv4 == nil {
		ipv4 = &IPAMData{AddressSpace: "null", Pool: "0.0.0.0/0"}
	}
	return p.CreateNetwork(CreateNetworkRequest{
		NetworkID: vlanNetA,
		Options:   map[string]interface{}{util.OptionsKeyGeneric: opts},
		IPv4Data:  []*IPAMData{ipv4},
	})
}

func TestCreateNetwork_Vlan(t *testing.T) {
	jumbo := func() *netlink.Device {
		l := vlanTestParent()
		l.MTU = 9000
		return l
	}

	t.Run("creates the sub-interface and stores the option", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		k := stubVlanKernel(t, vlanTestParent())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		if err := vlanCreate(p, map[string]interface{}{"mode": "macvlan", "parent": "eth0", "vlan": "100"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := k.links["eth0.100"].(*netlink.Vlan); !ok {
			t.Error("no eth0.100 after the create")
		}
		if o, err := loadOptions(vlanNetA); err != nil || o.Vlan != "100" {
			t.Errorf("stored %+v, %v; want vlan 100, or a restart forgets which link the network owns", o, err)
		}
	})

	t.Run("bridge mode refuses vlan and creates nothing", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		k := stubVlanKernel(t, vlanTestParent())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := vlanCreate(p, map[string]interface{}{"mode": "bridge", "bridge": "br0", "vlan": "100"}, nil)
		if !errors.Is(err, util.ErrModeMismatch) || len(k.added) != 0 {
			t.Errorf("err = %v, added %d; want ErrModeMismatch and nothing created", err, len(k.added))
		}
	})

	t.Run("mtu is checked against the sub-interface", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		sub := vlanTestLink("")
		sub.MTU = 1500
		stubVlanKernel(t, jumbo(), sub)
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := vlanCreate(p, map[string]interface{}{"mode": "macvlan", "parent": "eth0", "vlan": "100", "mtu": "9000"}, nil)
		if err == nil || !strings.Contains(err.Error(), "eth0.100") {
			t.Errorf("mtu=9000 on a 1500 sub-interface of a 9000 parent: err = %v, want a refusal naming eth0.100; "+
				"a child above its lower's mtu drops every large frame (#902)", err)
		}
	})

	t.Run("release_lease is refused on a sub-interface the plugin makes", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			link    *netlink.Vlan
			release string
			refused bool
		}{
			{"missing, on_stop", nil, "on_stop", true},
			{"missing, on_remove", nil, "on_remove", true},
			{"marked, on_stop", vlanTestLink(vlanOwnerAlias), "on_stop", true},
			{"missing, never", nil, "never", false},
			{"adopted, on_stop", upVlan(vlanTestLink("")), "on_stop", false},
		} {
			t.Run(c.name, func(t *testing.T) {
				withStateDir(t, t.TempDir())
				links := []netlink.Link{vlanTestParent()}
				if c.link != nil {
					links = append(links, c.link)
				}
				k := stubVlanKernel(t, links...)
				p := newPluginForTest()
				p.docker = &fakeDocker{}
				err := vlanCreate(p, map[string]interface{}{"mode": "macvlan", "parent": "eth0", "vlan": "100", "release_lease": c.release}, nil)
				refused := errors.Is(err, util.ErrIPAM) && strings.Contains(err.Error(), "release_lease="+c.release+" is refused on eth0.100")
				if refused != c.refused || (!c.refused && err != nil) {
					t.Errorf("err = %v, want refused %v: the release is sent from the host's address on the link, "+
						"and the host has none on one the plugin makes (#902, #962)", err, c.refused)
				}
				if c.refused && len(k.added) != 0 {
					t.Error("the refused create made the sub-interface anyway")
				}
			})
		}
	})

	t.Run("a down link it did not make is refused and left as it is", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		sub := vlanTestLink("")
		stubVlanKernel(t, vlanTestParent(), sub)
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := vlanCreate(p, map[string]interface{}{"mode": "macvlan", "parent": "eth0", "vlan": "100"}, nil)
		if !errors.Is(err, util.ErrParentDown) || !strings.Contains(err.Error(), "eth0.100") {
			t.Errorf("err = %v, want ErrParentDown naming eth0.100, as for any down parent (#902)", err)
		}
		if sub.Flags&net.FlagUp != 0 || sub.Alias != "" {
			t.Errorf("up %v, alias %q; want an operator's link untouched", sub.Flags&net.FlagUp != 0, sub.Alias)
		}
	})

	t.Run("a failed create removes the sub-interface it made", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		k := stubVlanKernel(t, vlanTestParent())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := vlanCreate(p, map[string]interface{}{"mode": "macvlan", "parent": "eth0", "vlan": "100", "mtu": "9000"}, nil)
		if err == nil {
			t.Fatal("mtu 9000 over a 1500 parent was accepted")
		}
		if !k.removed("eth0.100") {
			t.Error("the failed create left the sub-interface it made behind")
		}
	})

	t.Run("a failed create keeps a sub-interface it adopted", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		k := stubVlanKernel(t, vlanTestParent(), vlanTestLink(vlanOwnerAlias))
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		if err := vlanCreate(p, map[string]interface{}{"mode": "macvlan", "parent": "eth0", "vlan": "100", "mtu": "9000"}, nil); err == nil {
			t.Fatal("mtu 9000 over a 1500 sub-interface was accepted")
		}
		if k.removed("eth0.100") {
			t.Error("a failed create removed a sub-interface it did not make, which another network may use")
		}
	})

	t.Run("IPAM mode binds the pool to the sub-interface", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubVlanKernel(t, vlanTestParent())
		const pool = "192.168.101.0/24"
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		p.ipamPools, p.ipamIndex = newIssuedPools(), newIPAMIndex()
		req := RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: pool, Options: map[string]string{"parent": "eth0"}}
		if _, err := p.RequestPool(req); err != nil {
			t.Fatal(err)
		}
		data := &IPAMData{AddressSpace: ipamLocalAddressSpace, Pool: pool}
		opts := map[string]interface{}{"mode": "macvlan", "parent": "eth0", "vlan": "100"}
		if err := vlanCreate(p, opts, data); err == nil || !strings.Contains(err.Error(), "eth0.100") {
			t.Errorf("--ipam-opt parent=eth0 on a vlan network: err = %v, want the mismatch naming eth0.100; two vlan "+
				"networks on eth0 would otherwise share one pool identity (#902)", err)
		}
		req.Options = map[string]string{"parent": "eth0.100"}
		if _, err := p.RequestPool(req); err != nil {
			t.Fatal(err)
		}
		if err := vlanCreate(p, opts, data); err != nil {
			t.Fatalf("--ipam-opt parent=eth0.100: %v", err)
		}
		if sn, err := loadNetwork(vlanNetA); err != nil || sn.Binding == nil || sn.Options.Vlan != "100" {
			t.Errorf("stored %+v, %v; want an IPAM binding and vlan 100", sn, err)
		}
	})
}

func TestVlanSites_ReadTheSubInterface(t *testing.T) {
	o := vlanOpts("100")
	if got := o.hostLink(); got != "eth0.100" {
		t.Errorf("hostLink = %q, want eth0.100: a DHCPRELEASE from the physical parent goes out untagged, to "+
			"another server or none (#902)", got)
	}
	o.MacvlanMode = "passthru"
	stubVlanKernel(t, vlanTestParent(), &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{
		Name: "eth0.100", Index: vlanTestLinkIndex, ParentIndex: vlanTestParentIndex, Flags: net.FlagUp,
		HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0x01, 0x00}}, VlanId: 100})
	mac, err := recoveredMAC(o, "")
	if err != nil || mac.String() != "02:00:00:00:01:00" {
		t.Errorf("recoveredMAC on passthru = %v, %v; want the sub-interface's MAC, which the child wears (#902)", mac, err)
	}
}

func TestCreateNetwork_VlanSiblingCheckReadsTheSubInterface(t *testing.T) {
	for _, c := range []struct {
		name    string
		sibling dNetwork.Summary
		refused bool
	}{
		{"passthru on eth0.100 beside this plugin's macvlan on eth0", dNetwork.Summary{ID: "n0", Name: "lan-a",
			Driver: testDHCPDriver, Options: map[string]string{"mode": "macvlan", "parent": "eth0"}}, false},
		{"passthru on eth0.100 beside this plugin's macvlan on eth0.100", dNetwork.Summary{ID: "n0", Name: "lan-a",
			Driver: testDHCPDriver, Options: map[string]string{"mode": "macvlan", "parent": "eth0", "vlan": "100"}}, true},
		{"passthru on eth0.100 beside Docker's macvlan on eth0.100", dNetwork.Summary{ID: "n0", Name: "lan-a",
			Driver: "macvlan", Options: map[string]string{"parent": "eth0.100"}}, true},
		{"passthru on eth0.100 beside this plugin's macvlan on eth0.200", dNetwork.Summary{ID: "n0", Name: "lan-a",
			Driver: testDHCPDriver, Options: map[string]string{"mode": "macvlan", "parent": "eth0", "vlan": "200"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			stubVlanKernel(t, vlanTestParent())
			p := newPluginForTest()
			p.docker = &fakeDocker{listResult: []dNetwork.Summary{c.sibling}}
			err := vlanCreate(p, map[string]interface{}{"mode": "macvlan", "parent": "eth0", "vlan": "100", "macvlan_mode": "passthru"}, nil)
			if c.refused != (err != nil) {
				t.Fatalf("err = %v, refused want %v: each sub-interface has its own rx_handler (K5), so the "+
					"kernel's one-mode rule applies per sub-interface, not per physical parent (#902)", err, c.refused)
			}
			if c.refused && !strings.Contains(err.Error(), "eth0.100") {
				t.Errorf("the refusal %q does not name eth0.100", err)
			}
		})
	}
}

func TestNetOptions_RefusesAStoredVlanTheCreateWouldRefuse(t *testing.T) {
	for _, opts := range []DHCPNetworkOptions{
		{Mode: ModeBridge, Bridge: "br0", Vlan: "100"},
		vlanOpts("4095"),
		{Mode: ModeMacvlan, Parent: "abcdefghijkl", Vlan: "100"},
	} {
		withStateDir(t, t.TempDir())
		if err := saveOptions(vlanNetA, opts); err != nil {
			t.Fatal(err)
		}
		p := &Plugin{docker: &fakeDocker{inspectErr: errors.New("docker must not be called")}}
		if _, err := p.netOptions(context.Background(), vlanNetA); err == nil || p.networkOptionsRejected.Load() != 1 {
			t.Errorf("stored %+v was served (err %v, rejected %d)", opts, err, p.networkOptionsRejected.Load())
		}
	}
}

func TestDeleteNetwork_RetiresTheVlan(t *testing.T) {
	for _, c := range []struct {
		name     string
		opts     DHCPNetworkOptions
		wantGone bool
	}{
		{"the last network removes the link it made", vlanOpts("100"), true},
		{"a refused stored vlan touches nothing", DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", Vlan: "100"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPlugin(t)
			p.docker = &fakeDocker{}
			k := stubVlanKernel(t, vlanTestParent(), vlanTestLink(vlanOwnerAlias))
			if err := saveOptions(vlanNetA, c.opts); err != nil {
				t.Fatal(err)
			}
			if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: vlanNetA}); err != nil {
				t.Fatal(err)
			}
			if got := k.removed("eth0.100"); got != c.wantGone {
				t.Errorf("eth0.100 removed = %v, want %v", got, c.wantGone)
			}
			if _, err := loadOptions(vlanNetA); err == nil {
				t.Error("the options outlived the delete")
			}
		})
	}
}

// vlanSiteKernel stubs a parent and its sub-interface at ifindexes no host has, so a site that gets past the gate
// fails its real LinkAdd with ENODEV or EPERM and creates nothing (#902).
func vlanSiteKernel(t *testing.T, subUp bool) DHCPNetworkOptions {
	t.Helper()
	const parentIndex, subIndex = 2147480000, 2147480001
	var flags net.Flags
	if subUp {
		flags = net.FlagUp
	}
	stubVlanKernel(t,
		&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "dh-902-nop", Index: parentIndex, Flags: net.FlagUp}},
		&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "dh-902-nop.100", Index: subIndex, ParentIndex: parentIndex,
			Flags: flags, Alias: vlanOwnerAlias}, VlanId: 100, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q})
	return DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "dh-902-nop", Vlan: "100"}
}

func TestDisableHostIPv6Under(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "eth0.100"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := ipv6DisablePath(dir, "eth0.100")
	if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := disableHostIPv6Under(dir, "eth0.100"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != "1" {
		t.Errorf("disable_ipv6 = %q, want 1", b)
	}
	if err := disableHostIPv6Under(dir, "eth0.101"); err == nil {
		t.Error("a missing link's sysctl passed; the host would stay on the vlan unnoticed")
	}
	if err := disableHostIPv6Under(filepath.Join(dir, "absent"), "eth0.100"); err != nil {
		t.Errorf("err = %v, want nil on a kernel with IPv6 off at boot, which has no conf tree", err)
	}
}

func TestVlanSites_TheChildSitesTakeTheSubInterface(t *testing.T) {
	sites := []struct {
		name string
		call func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error
	}{
		{"the endpoint", func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error {
			_, err := p.createParentAttachedEndpoint(ctx, time.Now(), CreateEndpointRequest{
				NetworkID: vlanNetA, EndpointID: strings.Repeat("e", 64)}, opts)
			return err
		}},
		{"the IPAM endpoint", func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error {
			_, err := p.addIPAMEndpointLink(ctx, strings.Repeat("e", 64), ModeMacvlan, opts, nil)
			return err
		}},
		{"the IPAM reservation", func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error {
			_, err := p.addIPAMReserveLink(ctx, "dh-902-ra", "dh-902-rb", ModeMacvlan, opts, nil)
			return err
		}},
		{"the validate_dhcp probe", func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error {
			return p.runDHCPProbe(ctx, opts, serverPolicy{})
		}},
	}
	for _, s := range sites {
		t.Run(s.name+" waits on the sub-interface's gate", func(t *testing.T) {
			withStateDir(t, t.TempDir())
			opts := vlanSiteKernel(t, true)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			holder := p.lockParent(context.Background(), opts.linkParent(), ModeMacvlan, "test-holder")
			defer holder.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = s.call(ctx, p, opts)
			if got := p.parentLinkWaits.Load(); got != 1 {
				t.Errorf("parent_link_waits = %d, want 1: a child on the sub-interface must queue behind a holder "+
					"of the sub-interface, the link whose rx_handler it takes (#902)", got)
			}
		})
	}
	for _, s := range sites[:3] {
		t.Run(s.name+" re-creates a vanished sub-interface", func(t *testing.T) {
			withStateDir(t, t.TempDir())
			k := stubVlanKernel(t, &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "dh-902-nop", Index: 2147480000, Flags: net.FlagUp}})
			k.newIndex = 2147480001
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			_ = s.call(context.Background(), p, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "dh-902-nop", Vlan: "100"})
			if v, ok := k.links["dh-902-nop.100"].(*netlink.Vlan); !ok || v.Alias != vlanOwnerAlias {
				t.Errorf("links %v; want dh-902-nop.100 made and marked: a host reboot or an ip link del loses it "+
					"while Docker keeps the network (#902)", k.links)
			}
		})
		t.Run(s.name+" refuses a down sub-interface", func(t *testing.T) {
			withStateDir(t, t.TempDir())
			opts := vlanSiteKernel(t, false)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			err := s.call(context.Background(), p, opts)
			if !errors.Is(err, util.ErrParentDown) || !strings.Contains(err.Error(), "dh-902-nop.100") {
				t.Errorf("err = %v, want ErrParentDown naming dh-902-nop.100: the parent checks run on the link the "+
					"child attaches to (#902)", err)
			}
		})
	}
}

func TestJoin_TheRouteCopyReadsTheSubInterface(t *testing.T) {
	withStateDir(t, t.TempDir())
	opts := vlanSiteKernel(t, true)
	if err := saveOptions("net-902", opts); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	var read []int
	prev := nlRouteListFiltered
	nlRouteListFiltered = func(_ int, filter *netlink.Route, _ uint64) ([]netlink.Route, error) {
		read = append(read, filter.LinkIndex)
		return nil, nil
	}
	t.Cleanup(func() { nlRouteListFiltered = prev })
	a4, _ := netlink.ParseAddr("192.168.111.61/24")
	mac, _ := net.ParseMAC("02:42:c0:a8:6f:3d")
	p := &Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
		joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)}
	p.storeJoinHint("ep-902", joinHint{IPv4: a4, MacAddress: mac})
	if _, err := p.Join(context.Background(), JoinRequest{NetworkID: "net-902", EndpointID: "ep-902"}); err != nil {
		t.Fatalf("Join: %v", err)
	}
	p.mu.Lock()
	m := p.persistentDHCP["ep-902"]
	p.mu.Unlock()
	if m != nil {
		defer func() { m.attachCancel(); <-m.startedCh }()
	}
	if len(read) == 0 || read[0] != 2147480001 {
		t.Errorf("the route copy read link indexes %v, want the sub-interface's 2147480001 first: the container "+
			"gets the routes of the link its frames leave on (#902)", read)
	}
}
