// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The kernel refuses a macvlan and an ipvlan child on one parent with EBUSY, both ways,
// since each claims the parent's single receive handler; many of one kind are fine (#486).

func childOn(t *testing.T, kind string, parentIndex int) netlink.Link {
	t.Helper()

	la := netlink.NewLinkAttrs()
	la.Name = "child-" + kind
	la.ParentIndex = parentIndex

	switch kind {
	case ModeMacvlan:
		return &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}
	case ModeIPvlan:
		return &netlink.IPVlan{LinkAttrs: la, Mode: netlink.IPVLAN_MODE_L2}
	default:
		t.Fatalf("childOn: unknown kind %q", kind)
		return nil
	}
}

func withLinkList(t *testing.T, links []netlink.Link, err error) {
	t.Helper()

	orig := nlLinkList
	nlLinkList = func() ([]netlink.Link, error) { return links, err }
	t.Cleanup(func() { nlLinkList = orig })
}

func TestExplainChildLinkAdd_NamesTheConflictingKind(t *testing.T) {
	const parentIdx = 7

	cases := []struct {
		name       string
		want       string
		occupant   string
		wantNamed  string
		wantMode   string
		wantParent string
	}{
		{
			name:      "ipvlan refused while macvlan children live",
			wantMode:  ModeIPvlan,
			occupant:  ModeMacvlan,
			wantNamed: ModeMacvlan,
		},
		{
			name:      "macvlan refused while ipvlan children live",
			wantMode:  ModeMacvlan,
			occupant:  ModeIPvlan,
			wantNamed: ModeIPvlan,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLinkList(t, []netlink.Link{
				childOn(t, tc.occupant, parentIdx),
			}, nil)

			err := explainChildLinkAdd(unix.EBUSY, tc.wantMode, "eth0", parentIdx)
			if err == nil {
				t.Fatal("explainChildLinkAdd returned nil for an EBUSY")
			}

			if !errors.Is(err, unix.EBUSY) {
				t.Errorf("error no longer wraps EBUSY: %v", err)
			}

			msg := err.Error()
			for _, want := range []string{tc.wantNamed, "eth0", "not both"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message does not mention %q: %s", want, msg)
				}
			}
		})
	}
}

func TestExplainChildLinkAdd_UnseenBlockerDoesNotInvent(t *testing.T) {
	withLinkList(t, nil, nil)

	err := explainChildLinkAdd(unix.EBUSY, ModeIPvlan, "eth0", 7)
	if !errors.Is(err, unix.EBUSY) {
		t.Fatalf("error no longer wraps EBUSY: %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "not both") {
		t.Errorf("claimed a mode conflict with no evidence of one: %s", msg)
	}
}

func TestExplainChildLinkAdd_IgnoresChildrenOfOtherParents(t *testing.T) {
	withLinkList(t, []netlink.Link{
		childOn(t, ModeMacvlan, 99),
	}, nil)

	if msg := explainChildLinkAdd(unix.EBUSY, ModeIPvlan, "eth0", 7).Error(); strings.Contains(msg, "not both") {
		t.Errorf("a macvlan child of a different parent was reported as the conflict: %s", msg)
	}
}

func TestExplainChildLinkAdd_NonBusyIsUnchanged(t *testing.T) {
	boom := errors.New("boom")

	err := explainChildLinkAdd(boom, ModeMacvlan, "eth0", 7)
	if !errors.Is(err, boom) {
		t.Fatalf("original error lost: %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "not both") || strings.Contains(msg, "torn down") {
		t.Errorf("EBUSY explanation applied to an unrelated error: %s", msg)
	}
}

func TestNewProbeLink_MatchesTheNetworkMode(t *testing.T) {
	mac, err := net.ParseMAC("02:11:22:33:44:55")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}

	probe := func(mode string) netlink.Link {
		t.Helper()
		link, err := newProbeLink(DHCPNetworkOptions{Mode: mode}, "dh-probe-a1b2c3", 7, mac)
		if err != nil {
			t.Fatalf("newProbeLink(%s): %v", mode, err)
		}
		return link
	}

	t.Run("ipvlan network gets an ipvlan probe", func(t *testing.T) {
		link := probe(ModeIPvlan)

		if _, ok := link.(*netlink.IPVlan); !ok {
			t.Fatalf("probe link is %T, want *netlink.IPVlan — a macvlan probe "+
				"cannot coexist with the ipvlan endpoints this network will create", link)
		}
		// The kernel rejects a MAC on an ipvlan child outright.
		if got := link.Attrs().HardwareAddr; got != nil {
			t.Errorf("ipvlan probe link carries HardwareAddr %v, want none", got)
		}
	})

	t.Run("macvlan network gets a macvlan probe with the probe MAC", func(t *testing.T) {
		link := probe(ModeMacvlan)

		if _, ok := link.(*netlink.Macvlan); !ok {
			t.Fatalf("probe link is %T, want *netlink.Macvlan", link)
		}
		if got := link.Attrs().HardwareAddr; got.String() != mac.String() {
			t.Errorf("macvlan probe MAC = %v, want %v", got, mac)
		}
	})

	t.Run("both attach to the parent given", func(t *testing.T) {
		for _, mode := range []string{ModeMacvlan, ModeIPvlan} {
			if got := probe(mode).Attrs().ParentIndex; got != 7 {
				t.Errorf("%s probe ParentIndex = %d, want 7", mode, got)
			}
		}
	})
}

func TestExplainChildLinkAdd_UnreadableLinkTableIsNotAnEmptyParent(t *testing.T) {
	withLinkList(t, nil, errors.New("netlink: operation not permitted"))

	err := explainChildLinkAdd(unix.EBUSY, ModeIPvlan, "eth0", 7)
	if !errors.Is(err, unix.EBUSY) {
		t.Fatalf("error no longer wraps EBUSY: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not be read") {
		t.Errorf("the message does not say the link table was unreadable, so a failed scan "+
			"reads to an operator exactly like a parent with no children of the other kind "+
			"(#802): %s", msg)
	}
	if strings.Contains(msg, "retry once it has finished") {
		t.Errorf("the message claims the blocker is a teardown in progress, which is a "+
			"statement about a scan that never happened: %s", msg)
	}
}

func TestChildLinkKind_DumpInterruptedStillUsesTheResults(t *testing.T) {
	const parentIdx = 7
	withLinkList(t, []netlink.Link{childOn(t, ModeMacvlan, parentIdx)}, netlink.ErrDumpInterrupted)

	kind, known := childLinkKind(parentIdx)
	if !known {
		t.Fatal("ErrDumpInterrupted was treated as a failed scan, but netlink v1.3.1 " +
			"returns it alongside a usable result set (#802)")
	}
	if kind != ModeMacvlan {
		t.Errorf("kind = %q, want %q: the results that came back with the sentinel were discarded",
			kind, ModeMacvlan)
	}
}

func TestChildLinkKind_EmptyParentIsKnown(t *testing.T) {
	withLinkList(t, nil, nil)

	kind, known := childLinkKind(7)
	if !known {
		t.Error("a successful dump over a parent with no children reported that it could not tell")
	}
	if kind != "" {
		t.Errorf("kind = %q, want empty", kind)
	}
}
