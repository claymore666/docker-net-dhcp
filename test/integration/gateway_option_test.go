// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestGatewayOption_OverridesTheOfferedRouter checks that -o gateway= replaces the offered router on the macvlan, bridge and IPAM paths.
func TestGatewayOption_OverridesTheOfferedRouter(t *testing.T) {
	// Inside each fixture subnet, outside the pool, and none of the fixtures' own or tagged addresses.
	const (
		macvlanOverride = "192.168.99.251"
		bridgeOverride  = "192.168.100.251"
	)
	for _, tc := range []struct {
		name     string
		mode     string
		ipam     bool
		override string
		offered  string
	}{
		{name: "macvlan", mode: "macvlan", override: macvlanOverride, offered: harness.DefaultGateway},
		{name: "bridge", mode: "bridge", override: bridgeOverride, offered: bridgeServerIP()},
		{name: "ipam-macvlan", mode: "macvlan", ipam: true, override: macvlanOverride, offered: harness.DefaultGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			netName := "dh-itest-gw-" + tc.name
			ctrName := netName + "-ctr"

			t.Cleanup(func() {
				if t.Failed() {
					fixture.DumpLogs(func(s string) { t.Log(s) })
					fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
					harness.DumpPluginLog(t)
				}
			})

			opts := map[string]string{"gateway": tc.override}
			if tc.ipam {
				harness.CreateNetworkIPAM(t, ctx, netName, tc.mode, harness.SubnetCIDR, nil, opts)
			} else {
				harness.CreateNetwork(t, ctx, netName, tc.mode, opts)
			}
			id, ipv4, _ := harness.RunContainer(t, ctx, netName, ctrName)
			t.Logf("container ip=%s", ipv4)

			out := harness.ExecOutput(t, ctx, id, "ip", "route")
			gw := defaultRouteGateway(t, out)
			if gw == tc.offered {
				t.Fatalf("default route via %s, the router the server offers: gateway=%s was ignored:\n%s", gw, tc.override, out)
			}
			if gw != tc.override {
				t.Errorf("default route via %s, want the gateway option %s:\n%s", gw, tc.override, out)
			}
		})
	}
}
