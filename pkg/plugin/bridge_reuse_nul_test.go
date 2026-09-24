// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"testing"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func withFakeBridge(t *testing.T, name string) {
	t.Helper()
	link := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}

	prevLink, prevAddr := nlLinkByName, nlAddrList
	nlLinkByName = func(n string) (netlink.Link, error) {
		if n != name {
			return nil, errors.New("Link not found")
		}
		return link, nil
	}
	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) {
		return nil, nil
	}
	t.Cleanup(func() { nlLinkByName, nlAddrList = prevLink, prevAddr })
}

func existingDHCPNetwork(bridge string) dNetwork.Summary {
	return dNetwork.Summary{
		ID:      "net-old",
		Name:    "old",
		Driver:  testDHCPDriver,
		Options: map[string]string{"bridge": bridge},
		IPAM:    dNetwork.IPAM{Driver: "null"},
	}
}

func createBridgeNetwork(t *testing.T, p *Plugin, id, bridge string) error {
	t.Helper()
	return p.CreateNetwork(CreateNetworkRequest{
		NetworkID: id,
		Options: map[string]interface{}{
			util.OptionsKeyGeneric: map[string]interface{}{"bridge": bridge},
		},
		IPv4Data: []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	})
}

// dockerd passes a NUL in a driver option through untouched and the kernel reads
// IFLA_IFNAME as a C string, so a name stored before #705 can still carry one.
func TestCreateNetwork_BridgeReuseSurvivesANulInAStoredName(t *testing.T) {
	const bridge = "br-test"

	t.Run("a stored name with a NUL still collides", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		withFakeBridge(t, bridge)
		p := newPluginForTest()
		p.docker = &fakeDocker{listResult: []dNetwork.Summary{
			existingDHCPNetwork(bridge + "\x00evil"),
		}}

		err := createBridgeNetwork(t, p, "net-new", bridge)
		if !errors.Is(err, util.ErrBridgeUsed) {
			t.Fatalf("a second DHCP network was allowed onto a bridge already held by one whose stored name carries a NUL: err=%v", err)
		}
	})

	t.Run("a stored name with trailing junk after the NUL is the same case", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		withFakeBridge(t, bridge)
		p := newPluginForTest()
		p.docker = &fakeDocker{listResult: []dNetwork.Summary{
			existingDHCPNetwork(bridge + "\x00" + "something-else-entirely"),
		}}

		if err := createBridgeNetwork(t, p, "net-new", bridge); !errors.Is(err, util.ErrBridgeUsed) {
			t.Fatalf("err=%v, want ErrBridgeUsed", err)
		}
	})

	t.Run("an honestly different bridge is still allowed", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		withFakeBridge(t, bridge)
		p := newPluginForTest()
		p.docker = &fakeDocker{listResult: []dNetwork.Summary{
			existingDHCPNetwork("br-somewhere-else"),
		}}

		if err := createBridgeNetwork(t, p, "net-new", bridge); err != nil {
			t.Fatalf("a network on an unrelated bridge was refused: %v", err)
		}
	})

	t.Run("the plain collision is still caught", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		withFakeBridge(t, bridge)
		p := newPluginForTest()
		p.docker = &fakeDocker{listResult: []dNetwork.Summary{
			existingDHCPNetwork(bridge),
		}}

		if err := createBridgeNetwork(t, p, "net-new", bridge); !errors.Is(err, util.ErrBridgeUsed) {
			t.Fatalf("the ordinary same-bridge collision was not caught: %v — the NUL cases above prove nothing", err)
		}
	})
}
