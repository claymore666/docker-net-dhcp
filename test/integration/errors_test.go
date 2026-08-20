// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// errorCases drive TestErrors_NetworkCreateValidation. Each case
// exercises a validation branch in pkg/plugin/network.go (via the
// libnetwork remote-driver protocol) — the plugin should refuse the
// network up-front, before any DHCP traffic is ever attempted.
//
// `opts` is the COMPLETE driver-options map; no auto-injection. That
// keeps each row honest about exactly which combination it's hitting,
// at the cost of re-typing parent= for the macvlan rows.
//
// `wantSubstr` is matched case-insensitively as a substring of the
// dockerd-wrapped error string. Loose-matching keeps these tests
// stable across libnetwork wording changes; tightening to exact
// equality would buy nothing.
var errorCases = []struct {
	name       string
	opts       map[string]string
	ipam       *network.IPAM // nil → null IPAM (the supported case)
	wantSubstr string
}{
	{
		name:       "InvalidMode",
		opts:       map[string]string{"mode": "moonbridge"},
		wantSubstr: "invalid mode",
	},
	{
		name:       "MacvlanMissingParent",
		opts:       map[string]string{"mode": "macvlan"},
		wantSubstr: "parent required",
	},
	{
		// Sets `bridge=foo` on a macvlan network. The plugin's
		// validator checks `Parent != ""` first then refuses any
		// foreign option for the chosen mode, so this exercises the
		// ErrModeMismatch branch (bridge cannot be set in
		// mode=macvlan).
		name: "MacvlanWithBridge",
		opts: map[string]string{
			"mode":   "macvlan",
			"parent": harness.HostVeth,
			"bridge": "foo",
		},
		wantSubstr: "does not apply to selected mode",
	},
	{
		name: "IPAMNotNull",
		opts: map[string]string{
			"mode":   "macvlan",
			"parent": harness.HostVeth,
		},
		ipam:       &network.IPAM{Driver: "default"},
		wantSubstr: "null IPAM driver",
	},
	{
		// dhcp_servers / dhcp_deny_servers are validated before any
		// mode-specific check, so these rows use the cheapest valid
		// mode rather than saying anything about macvlan.
		name: "DHCPServersNotAnIP",
		opts: map[string]string{
			"mode":         "macvlan",
			"parent":       harness.HostVeth,
			"dhcp_servers": "dhcp.example.com",
		},
		wantSubstr: "is not an IP address",
	},
	{
		// A v6 entry is refused rather than ignored: dhcpcd's
		// whitelist/blacklist are DHCPv4-only, so accepting it would
		// leave the operator believing a server was ranked when
		// nothing had been (#111).
		name: "DHCPServersIPv6",
		opts: map[string]string{
			"mode":         "macvlan",
			"parent":       harness.HostVeth,
			"dhcp_servers": "fd00::1",
		},
		wantSubstr: "DHCPv4-only",
	},
	{
		name: "DenyServersNotAnIP",
		opts: map[string]string{
			"mode":              "macvlan",
			"parent":            harness.HostVeth,
			"dhcp_deny_servers": "192.168.100.1/24",
		},
		wantSubstr: "is not an IP address",
	},
	{
		// Denying every preference leaves no server at all. Accepting
		// it would degrade the network into "any server will do",
		// which is the opposite of what both options were set to
		// achieve (#669).
		name: "DenyEmptiesPreference",
		opts: map[string]string{
			"mode":              "macvlan",
			"parent":            harness.HostVeth,
			"dhcp_servers":      "192.168.100.1",
			"dhcp_deny_servers": "192.168.100.1",
		},
		wantSubstr: "leaving no server to lease from",
	},
}

// TestErrors_NetworkCreateValidation walks the validation matrix.
// These cases never reach DHCP, so the only setup required is that
// the plugin is enabled (TestMain enforces) and the host-side veth
// exists for the macvlan cases (the fixture also creates it). On
// expected-failure rows no network is persisted; on regression we
// clean up so the next test run starts fresh.
func TestErrors_NetworkCreateValidation(t *testing.T) {
	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatalf("docker client: %v", err)
			}
			defer cli.Close()

			ipam := tc.ipam
			if ipam == nil {
				ipam = &network.IPAM{Driver: "null"}
			}

			netName := "dh-itest-err-" + strings.ToLower(tc.name)
			res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
				Driver:  harness.DriverName,
				IPAM:    ipam,
				Options: tc.opts,
			})
			if err == nil {
				_ = cli.NetworkRemove(context.Background(), res.ID)
				t.Fatalf("expected NetworkCreate to fail with %q substring, got success", tc.wantSubstr)
			}
			msg := err.Error()
			if !strings.Contains(strings.ToLower(msg), strings.ToLower(tc.wantSubstr)) {
				t.Errorf("error message missing expected substring %q\nactual: %s", tc.wantSubstr, msg)
			} else {
				t.Logf("✓ %s rejected: %s", tc.name, msg)
			}
		})
	}
}
