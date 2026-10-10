// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestIPAMv6Pool_TypedSubnetCreatesAndInspectShowsBothPools checks that a typed IPv6 --subnet becomes a pool of
// this driver and the daemon's record carries both pools (#1132); starting a container on it is the address side.
func TestIPAMv6Pool_TypedSubnetCreatesAndInspectShowsBothPools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName = "dh-itest-ipam6-pool"
	v4 := netip.MustParsePrefix(harness.V6SubnetCIDR)
	v6 := netip.MustParsePrefix(harness.V6SubnetV6CIDR)
	on := true
	res, err := cli.NetworkCreate(ctx, netName, docker.NetworkCreateOptions{
		Driver:     harness.DriverName,
		EnableIPv6: &on,
		IPAM: &network.IPAM{
			Driver: harness.DriverName,
			Config: []network.IPAMConfig{{Subnet: v4}, {Subnet: v6}},
		},
		Options: map[string]string{"mode": "bridge", "bridge": f.Bridge(), "ipv6_mode": "dhcp"},
	})
	if err != nil {
		t.Fatalf("NetworkCreate(%s, subnets %s and %s): %v", netName, v4, v6, err)
	}
	t.Cleanup(func() {
		_, _ = cli.NetworkRemove(context.Background(), res.ID, docker.NetworkRemoveOptions{})
	})

	insp := ipamNetworkInspect(t, ctx, cli, netName)
	if insp.IPAM.Driver != harness.DriverName {
		t.Errorf("the network's IPAM driver is %q, want %q", insp.IPAM.Driver, harness.DriverName)
	}
	got := map[netip.Prefix]bool{}
	for _, c := range insp.IPAM.Config {
		got[c.Subnet] = true
	}
	for _, want := range []netip.Prefix{v4, v6} {
		if !got[want] {
			t.Errorf("docker network inspect shows no pool %s; the IPAM config is %v", want, insp.IPAM.Config)
		}
	}
	if !insp.EnableIPv6 {
		t.Errorf("docker network inspect shows EnableIPv6 false on a network created with --ipv6")
	}
}
