//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// errorCases drive TestErrors_NetworkCreateValidation. Each case
// exercises a validation branch in pkg/plugin/network.go (via the
// libnetwork remote-driver protocol) — the plugin should refuse the
// network up-front, before any DHCP traffic is ever attempted. The
// `wantSubstr` is a fragment of the error string the plugin emits;
// it travels through dockerd, so we match loosely on substring to
// stay tolerant of dockerd's wrapping.
var errorCases = []struct {
	name       string
	mode       string
	opts       map[string]string
	ipam       *network.IPAM // nil → null IPAM (the supported case)
	wantSubstr string
}{
	{
		name:       "InvalidMode",
		mode:       "moonbridge",
		wantSubstr: "invalid mode",
	},
	{
		name:       "MacvlanMissingParent",
		mode:       "macvlan",
		opts:       map[string]string{}, // explicitly clear parent
		wantSubstr: "parent required",
	},
	{
		name:       "BridgeWithParent",
		mode:       "bridge",
		opts:       map[string]string{"parent": "lo"},
		wantSubstr: "does not apply to selected mode",
	},
	{
		name:       "IPAMNotNull",
		mode:       "macvlan",
		ipam:       &network.IPAM{Driver: "default"},
		wantSubstr: "null IPAM driver",
	},
}

// TestErrors_NetworkCreateValidation walks the validation matrix.
// These cases never reach DHCP, so the only setup required is that
// the plugin is enabled (TestMain enforces) and the host-side veth
// exists for the macvlan cases (the fixture also creates it). No
// network is actually persisted on success of the assertion — we
// expect each create to fail.
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

			opts := map[string]string{"mode": tc.mode}
			// Only inject the harness parent if the case didn't set
			// the parent option (or absence of it) explicitly. The
			// MacvlanMissingParent case wants no parent at all.
			if tc.mode == "macvlan" || tc.mode == "ipvlan" {
				if _, ok := tc.opts["parent"]; ok || tc.opts == nil {
					opts["parent"] = harness.HostVeth
				}
			}
			for k, v := range tc.opts {
				opts[k] = v
			}

			ipam := tc.ipam
			if ipam == nil {
				ipam = &network.IPAM{Driver: "null"}
			}

			netName := "dh-itest-err-" + strings.ToLower(tc.name)
			res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
				Driver:  harness.DriverName,
				IPAM:    ipam,
				Options: opts,
			})
			// Belt + braces: if a regression somehow lets the create
			// succeed, clean it up so the next test run starts fresh.
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
