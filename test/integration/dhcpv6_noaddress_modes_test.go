// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestDHCPv6_NoAddressModes_RefuseTheEndpoint PINS A DEFECT (#868).
//
// It asserts the WRONG answer on purpose. On an IPv6 network whose
// router advertisements do not hand out addresses over DHCPv6 —
// stateless, or SLAAC-only — no container can start at all, because
// CreateEndpoint treats a DHCPv6 acquisition failure as fatal for the
// whole endpoint and on those modes there is no DHCPv6 address by
// definition. That is a user-facing product defect and it is on `dev`
// today; #868 carries it.
//
// The test is written this way rather than deleted so the defect
// cannot be quietly forgotten: it goes RED the moment #868 is fixed,
// and whoever fixes it flips these assertions rather than discovering
// this file as a mystery failure.
//
// WHAT THIS MEANS FOR #815. #815 is about a stateless DHCPv6
// configuration reply being received and discarded, and the
// event-builder half of it is fixed in this branch with unit coverage.
// That fix is UNREACHABLE IN PRODUCTION until #868 is fixed: the
// `config` event needs an endpoint and its persistent client, and on
// exactly the networks #815 is about the endpoint is never created.
// So there is deliberately no integration test here asserting that the
// configuration is applied — it cannot be, yet. That test belongs to
// #868, where the assertions below flip.
func TestDHCPv6_NoAddressModes_RefuseTheEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// startOn brings up a container on a fresh network over the given
	// fixture's bridge and returns the ContainerStart error, or nil.
	// It does not fail the test on a start error — the error is the
	// measurement.
	startOn := func(t *testing.T, f *harness.V6Fixture, netName string) error {
		t.Helper()
		harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
			"bridge":        f.Bridge(),
			"ipv6":          "true",
			"propagate_dns": "true",
		})
		ctrName := netName + "-ctr"
		create, err := cli.ContainerCreate(ctx,
			&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
			harness.HostConfig(),
			&network.NetworkingConfig{
				EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
			},
			nil, ctrName)
		if err != nil {
			t.Fatalf("ContainerCreate(%s): %v", ctrName, err)
		}
		t.Cleanup(func() {
			_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
		})
		return cli.ContainerStart(ctx, create.ID, container.StartOptions{})
	}

	// The two modes with no DHCPv6 addressing. Each gets its own
	// segment and its own container, and asserts against those — there
	// is no aggregate claim here, because "these modes refuse" is not
	// the same statement as "each of them refuses".
	for _, tc := range []struct {
		name string
		mode harness.V6Mode
		net  string
	}{
		{"stateless", harness.V6Stateless, "dh-itest-v6sl"},
		{"slaac", harness.V6SLAAC, "dh-itest-v6slaac"},
	} {
		t.Run(tc.name+" refuses the endpoint (#868)", func(t *testing.T) {
			f := harness.NewV6Fixture(t, tc.mode)
			t.Cleanup(func() {
				if t.Failed() {
					f.DumpLogs(func(s string) { t.Log(s) })
					harness.DumpPluginLog(t)
				}
			})

			err := startOn(t, f, tc.net)
			if err == nil {
				t.Fatalf("the container STARTED on a %s segment — #868 appears to be fixed. "+
					"That is good news and this test is now wrong: flip these assertions to "+
					"assert the container starts and its resolver carries %s and %s, which is "+
					"the integration proof #815 is still missing.",
					tc.mode, harness.V6DNSServer, harness.V6SearchDomain)
			}
			if !strings.Contains(err.Error(), "via DHCPv6") {
				t.Fatalf("the container failed to start on a %s segment, but not for the reason "+
					"#868 describes — this may be a different defect:\n%v", tc.mode, err)
			}

			// Outside evidence that the segment itself was working and
			// the client was present: the fixture advertised, and the
			// container's client solicited. Without this the subtest
			// passes just as well against a segment that is simply
			// broken, and then it is not pinning a defect — it is
			// observing nothing and calling it a finding.
			if n := f.CountLogLines("RTR-ADVERT("); n == 0 {
				t.Errorf("the %s segment sent no router advertisements at all; "+
					"the refusal above cannot be attributed to #868", tc.mode)
			}
			if n := f.CountLogLines("RTR-SOLICIT("); n == 0 {
				t.Errorf("no router solicitation reached the %s segment; the container's "+
					"client never came up, so the refusal is not the one #868 describes", tc.mode)
			}
		})
	}

	// The preservation control. Managed DHCPv6 differs from stateless
	// in exactly one thing — whether the server hands out addresses —
	// and a container must still start there. Without this, every
	// assertion above is satisfied by a plugin that refuses IPv6
	// networks outright, and the fix for #868 could be "stop requiring
	// v6 anywhere" while this file stayed green.
	t.Run("managed still starts (control)", func(t *testing.T) {
		f := harness.NewV6Fixture(t, harness.V6Managed)
		t.Cleanup(func() {
			if t.Failed() {
				f.DumpLogs(func(s string) { t.Log(s) })
				harness.DumpPluginLog(t)
			}
		})
		if err := startOn(t, f, "dh-itest-v6managed"); err != nil {
			t.Fatalf("a container failed to start on a MANAGED DHCPv6 segment, where a "+
				"DHCPv6 address is available — this is not #868, it is a regression in the "+
				"working case:\n%v", err)
		}
	})
}
