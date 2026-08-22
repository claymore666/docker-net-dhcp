// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"testing"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// withFakeBridge points the netlink seam at a synthetic bridge so
// CreateNetwork's bridge-reuse guard — pure Go over values Docker hands
// us — is reachable without CAP_NET_ADMIN. It carries one address so the
// address-overlap arm below the guard has something to compare.
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

// existingDHCPNetwork is what NetworkList reports for a network this
// plugin already serves. IPAM driver "null" is what this plugin
// requires, and it matters here: the address-overlap arm skips null-IPAM
// networks outright, so the bridge-name comparison is the ONLY thing
// standing between two DHCP networks and the same bridge.
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

// TestCreateNetwork_BridgeReuseSurvivesANulInAStoredName is the
// regression test for the escape #705's fix left open.
//
// #705 added ValidIfaceName to validateModeOptions, which CreateNetwork
// is the only caller of. That validates the name of the network being
// created. It does nothing about the names of the networks it is
// COMPARED AGAINST: the reuse guard decodes every other DHCP network's
// options straight out of Docker's record and compares Go strings.
//
// Both halves of the bypass are measured on this project's own hardware
// and recorded in #705: a NUL in a driver option transports through
// dockerd untouched, and netlink resolves "docker0\x00evil" to docker0
// because the kernel reads IFLA_IFNAME as a C string. So a network
// created on a build older than #705 keeps a NUL-bearing bridge name in
// Docker's netdb, survives the upgrade, and is then compared as a whole
// Go string against the truncated name a new network asks for:
//
//	"br-test" == "br-test\x00evil"  ->  false  ->  no ErrBridgeUsed
//
// Two DHCP networks then share one bridge, which is the exact outcome
// ErrBridgeUsed exists to prevent, reached past the fix that was
// supposed to prevent it.
//
// The last subtest is what stops this passing vacuously: if the guard
// were broken outright, the first case would "pass" while proving
// nothing.
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
		// The suffix is attacker-chosen and arbitrary; nothing about the
		// bypass depends on what follows the NUL, so the test must not
		// either.
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
		// The guard must fail in one direction only. A fix that answered
		// ErrBridgeUsed for every other network would pass the cases
		// above and make a second DHCP network impossible to create.
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
