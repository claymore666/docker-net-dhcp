// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const censusParentIdx = 7

func censusManager(endpoint, network, mode, parent string, started time.Time) *dhcpManager {
	m := newDHCPManager(nil, JoinRequest{
		NetworkID:  network,
		EndpointID: endpoint,
		SandboxKey: "/var/run/docker/netns/5a5a5a5a5a5a",
	}, DHCPNetworkOptions{Mode: mode, Parent: parent})
	m.started = started
	return m
}

func censusChild(name, kind, alias string, parentIndex int) netlink.Link {
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.Alias = alias
	la.ParentIndex = parentIndex
	if kind == ModeIPvlan {
		return &netlink.IPVlan{LinkAttrs: la, Mode: netlink.IPVLAN_MODE_L2}
	}
	return &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}
}

func mustContainAll(t *testing.T, msg string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(msg, w) {
			t.Errorf("message does not carry %q: %s", w, msg)
		}
	}
}

func mustContainNone(t *testing.T, msg string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(msg, w) {
			t.Errorf("message carries %q: %s", w, msg)
		}
	}
}

func TestExplainChildLinkAdd_EBUSYNamesTheEndpointsInContainersOnThatParent(t *testing.T) {
	withLinkList(t, nil, nil)
	p := newPluginForTest()
	held := censusManager("aaaaaaaaaaaa1111", "nnnnnnnnnnnn1111", ModeIPvlan, "eth0", time.Now().Add(-90*time.Second))
	held.ctrID = "c0c0c0c0c0c01111"
	p.persistentDHCP["aaaaaaaaaaaa1111"] = held
	p.persistentDHCP["bbbbbbbbbbbb2222"] = censusManager("bbbbbbbbbbbb2222", "nnnnnnnnnnnn2222", ModeIPvlan, "eth1", time.Now())
	p.persistentDHCP["cccccccccccc3333"] = censusManager("cccccccccccc3333", "nnnnnnnnnnnn3333", ModeBridge, "eth0", time.Now())

	var err error
	logged := captureLog(t, func() { err = p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx) })
	if !errors.Is(err, unix.EBUSY) {
		t.Fatalf("error no longer wraps EBUSY: %v", err)
	}
	msg := err.Error()
	census := []string{"endpoint aaaaaaaaaaaa (network nnnnnnnnnnnn, ipvlan, container c0c0c0c0c0c0, age 1m30s)",
		"0 other children on it"}
	mustContainAll(t, msg, append(census, "retry once it has finished", "holds 1 child on \"eth0\"")...)
	mustContainNone(t, msg, "bbbbbbbbbbbb", "cccccccccccc", "tagged by this plugin")
	mustContainAll(t, logged, append(census, "held=1", "unheld=0", "parent=eth0")...)
	mustContainNone(t, logged, "bbbbbbbbbbbb", "cccccccccccc")
}

func TestExplainChildLinkAdd_EBUSYTellsALeftoverFromAForeignChildAndFromAHeldOne(t *testing.T) {
	p := newPluginForTest()
	p.creatingEndpoints.Store("dddddddddddd4444", pendingEndpoint{network: "nnnnnnnnnnnn4444", since: time.Now().Add(-3 * time.Second)})
	p.creatingEndpoints.Store("eeeeeeeeeeee5555", pendingEndpoint{network: "nnnnnnnnnnnn5555", since: time.Now()})
	p.joinHints["ffffffffffff6666"] = joinHint{}
	p.persistentDHCP["777777777777cccc"] = censusManager("777777777777cccc", "nnnnnnnnnnnn7777", ModeMacvlan, "eth0", time.Time{})
	withLinkList(t, []netlink.Link{
		censusChild(subLinkName("777777777777cccc"), ModeMacvlan, endpointAliasPrefix+"777777777777cccc", censusParentIdx),
		censusChild(subLinkName("dddddddddddd4444"), ModeMacvlan, endpointAliasPrefix+"dddddddddddd4444", censusParentIdx),
		censusChild(subLinkName("eeeeeeeeeeee5555"), ModeMacvlan, "", censusParentIdx),
		censusChild(subLinkName("ffffffffffff6666"), ModeMacvlan, endpointAliasPrefix+"ffffffffffff6666", censusParentIdx),
		censusChild(subLinkName("999999999999aaaa"), ModeMacvlan, endpointAliasPrefix+"999999999999aaaa", censusParentIdx),
		censusChild("mv-foreign", ModeMacvlan, "", censusParentIdx),
		censusChild(subLinkName("888888888888bbbb"), ModeMacvlan, endpointAliasPrefix+"888888888888bbbb", 99),
	}, nil)

	msg := p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx).Error()
	mustContainAll(t, msg,
		"holds 4 children",
		"endpoint 777777777777 (network nnnnnnnnnnnn, macvlan, sandbox 5a5a5a5a5a5a)",
		"endpoint dddddddddddd (network nnnnnnnnnnnn, macvlan, host link dh-dddddddddddd, age 3s)",
		"endpoint eeeeeeeeeeee (network nnnnnnnnnnnn, macvlan, host link dh-eeeeeeeeeeee",
		"endpoint ffffffffffff (macvlan, host link dh-ffffffffffff)",
		"2 other children on it in the host namespace, 1 of them tagged by this plugin for an endpoint it no longer holds")
	mustContainNone(t, msg, "999999999999", "mv-foreign", "888888888888", "host link dh-777777777777")
}

func TestExplainChildLinkAdd_EBUSYListIsSortedAndCapped(t *testing.T) {
	withLinkList(t, nil, nil)
	p := newPluginForTest()
	for i := 7; i >= 1; i-- {
		id := fmt.Sprintf("%012d", i)
		p.persistentDHCP[id] = censusManager(id, "nnnnnnnnnnnn", ModeIPvlan, "eth0", time.Time{})
	}

	msg := p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx).Error()
	mustContainAll(t, msg, "holds 7 children", "and 2 more")
	mustContainNone(t, msg, "000000000006", "000000000007", "age ")
	if a, b := strings.Index(msg, "000000000001"), strings.Index(msg, "000000000005"); a < 0 || b < a {
		t.Errorf("the listed endpoints are not the first five in id order: %s", msg)
	}
}

func TestExplainChildLinkAdd_EBUSYNamesNoneWhenThePluginHoldsNothing(t *testing.T) {
	withLinkList(t, nil, nil)

	msg := newPluginForTest().explainChildLinkAdd(unix.EBUSY, ModeIPvlan, "eth0", censusParentIdx).Error()
	mustContainAll(t, msg, "this plugin holds no child on \"eth0\"; 0 other children on it in the host namespace")
}

func TestExplainChildLinkAdd_OtherKindAndUnreadableTableCarryNoCensus(t *testing.T) {
	p := newPluginForTest()
	p.persistentDHCP["aaaaaaaaaaaa1111"] = censusManager("aaaaaaaaaaaa1111", "nnnnnnnnnnnn1111", ModeIPvlan, "eth0", time.Now())

	withLinkList(t, []netlink.Link{censusChild("iv0", ModeIPvlan, "", censusParentIdx)}, nil)
	mustContainNone(t, p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx).Error(), "this plugin holds", "aaaaaaaaaaaa")

	withLinkList(t, nil, errors.New("netlink: operation not permitted"))
	mustContainNone(t, p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx).Error(), "this plugin holds", "aaaaaaaaaaaa")
}

func TestExplainChildLinkAdd_EBUSYDoesNotWaitOnAContainerLookup(t *testing.T) {
	withLinkList(t, nil, nil)
	p := newPluginForTest()
	m := censusManager("aaaaaaaaaaaa1111", "nnnnnnnnnnnn1111", ModeIPvlan, "eth0", time.Now())
	m.ctrID = "c0c0c0c0c0c01111"
	p.persistentDHCP["aaaaaaaaaaaa1111"] = m
	m.ctrIDMu.Lock()
	defer m.ctrIDMu.Unlock()

	done := make(chan string, 1)
	go func() { done <- p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx).Error() }()
	select {
	case msg := <-done:
		mustContainAll(t, msg, "endpoint aaaaaaaaaaaa", "sandbox 5a5a5a5a5a5a")
	case <-time.After(5 * time.Second):
		t.Fatal("the EBUSY census waited on a container lookup in progress")
	}
}

func TestExplainChildLinkAdd_EBUSYNamesAnEndpointBetweenLeaveAndDeleteEndpoint(t *testing.T) {
	withLinkList(t, nil, nil)
	p := newPluginForTest()
	m := censusManager("aaaaaaaaaaaa1111", "nnnnnnnnnnnn1111", ModeIPvlan, "eth0", time.Now().Add(-time.Minute))
	m.startErr = errors.New("start failed")
	close(m.startedCh)
	p.persistentDHCP["aaaaaaaaaaaa1111"] = m
	p.endpointFingerprints["aaaaaaaaaaaa1111"] = endpointFingerprint{}
	for _, o := range []struct{ id, mode, parent string }{
		{"bbbbbbbbbbbb2222", ModeIPvlan, "eth1"},
		{"cccccccccccc3333", ModeBridge, "eth0"},
	} {
		other := censusManager(o.id, "nnnnnnnnnnnn2222", o.mode, o.parent, time.Now())
		other.startErr = errors.New("start failed")
		close(other.startedCh)
		p.persistentDHCP[o.id] = other
		p.endpointFingerprints[o.id] = endpointFingerprint{}
	}

	for _, id := range []string{"aaaaaaaaaaaa1111", "bbbbbbbbbbbb2222", "cccccccccccc3333"} {
		_ = p.Leave(context.Background(), LeaveRequest{NetworkID: "n", EndpointID: id})
	}
	if len(p.persistentDHCP) != 0 {
		t.Fatalf("Leave left a manager registered: %v", p.persistentDHCP)
	}

	msg := p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx).Error()
	mustContainAll(t, msg, "holds 1 child on \"eth0\"",
		"endpoint aaaaaaaaaaaa (network nnnnnnnnnnnn, ipvlan, left its container ", " ago, age 1m")
	mustContainNone(t, msg, "bbbbbbbbbbbb", "cccccccccccc")

	rejoined := censusManager("aaaaaaaaaaaa1111", "nnnnnnnnnnnn1111", ModeIPvlan, "eth0", time.Now())
	rejoined.ctrID = "c0c0c0c0c0c01111"
	p.persistentDHCP["aaaaaaaaaaaa1111"] = rejoined
	msg = p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx).Error()
	mustContainAll(t, msg, "holds 1 child on \"eth0\"", "ipvlan, container c0c0c0c0c0c0, age ")
	mustContainNone(t, msg, "left its container")
	delete(p.persistentDHCP, "aaaaaaaaaaaa1111")

	_, _ = p.takeEndpoint("aaaaaaaaaaaa1111")
	mustContainAll(t, p.explainChildLinkAdd(unix.EBUSY, ModeMacvlan, "eth0", censusParentIdx).Error(),
		"this plugin holds no child on \"eth0\"")
}

func TestExplainChildLinkAdd_EBUSYTellsARecoveredManagersAgeFromItsOwn(t *testing.T) {
	withLinkList(t, nil, nil)
	p := newPluginForTest()
	m := censusManager("aaaaaaaaaaaa1111", "nnnnnnnnnnnn1111", ModeMacvlan, "eth0", time.Now().Add(-2*time.Minute))
	m.recovered = true
	p.persistentDHCP["aaaaaaaaaaaa1111"] = m

	msg := p.explainChildLinkAdd(unix.EBUSY, ModeIPvlan, "eth0", censusParentIdx).Error()
	mustContainAll(t, msg, "endpoint aaaaaaaaaaaa (network nnnnnnnnnnnn, macvlan, sandbox 5a5a5a5a5a5a, "+
		"recovered by the plugin restart 2m")
	mustContainNone(t, msg, "age ")
}
