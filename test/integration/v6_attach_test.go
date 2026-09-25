// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// v6Attach is the endpoint shape a v6-segment test runs on; each *_Macvlan test is its twin on the macvlan shape,
// which reaches the DHCPv6 one-shot through parent_attached.go instead of network.go (#960).
type v6Attach struct {
	mode string
	kind string
}

var (
	onV6Bridge  = v6Attach{mode: "bridge", kind: "veth"}
	onV6Macvlan = v6Attach{mode: "macvlan", kind: "macvlan"}
)

func (a v6Attach) net(name string) string {
	if a == onV6Macvlan {
		return name + "m"
	}
	return name
}

// attachEvidenceBudget bounds the read of the server's DHCPv4 log line, written during the endpoint's creation.
const attachEvidenceBudget = 10 * time.Second

// startOnV6SegmentAs is startOnV6SegmentWithOpts on the given shape; the macvlan shape's parent is a port of the
// fixture's bridge, the segment whose server runs the mode under test (#960).
func startOnV6SegmentAs(t *testing.T, ctx context.Context, cli *docker.Client, f *harness.V6Fixture, at v6Attach, netName string, extra map[string]string) (string, error) {
	t.Helper()
	opts := map[string]string{
		"ipv6":          "true",
		"propagate_dns": "true",
	}
	if at == onV6Macvlan {
		opts["parent"] = f.MacvlanParent()
	} else {
		opts["bridge"] = f.Bridge()
	}
	for k, v := range extra {
		if v == "" {
			delete(opts, k)
			continue
		}
		opts[k] = v
	}
	return startContainerOn(t, ctx, cli, netName, at.mode, opts)
}

// assertAttachedAs checks that a started container's link has the shape's kind and that the fixture's own server
// leased v4 to that link's MAC on its bridge, so the run was on this segment through this copy (#960).
func assertAttachedAs(t *testing.T, ctx context.Context, f *harness.V6Fixture, id string, at v6Attach) {
	t.Helper()
	kind, _, mac := harness.ChildLinkMode(t, ctx, id, "eth0")
	if kind != at.kind {
		t.Errorf("the container's link is a %q, want %q: this run did not go through the %s "+
			"copy of CreateEndpoint it exists to cover", kind, at.kind, at.mode)
	}
	if n := f.AwaitLogLines(attachEvidenceBudget, "DHCPACK("+f.Bridge()+")", mac); n < 1 {
		t.Errorf("the fixture's server logged no DHCPACK on %s for the container's MAC %s "+
			"within %s, so the container was not on the segment whose mode is under test",
			f.Bridge(), mac, attachEvidenceBudget)
	}
}

// assertServedOnSegment is the evidence for a container that never started: its link is gone, so only the server's
// v4 lease on the bridge shows that the endpoint was on this segment (#960).
func assertServedOnSegment(t *testing.T, f *harness.V6Fixture) {
	t.Helper()
	if n := f.AwaitLogLines(attachEvidenceBudget, "DHCPACK("+f.Bridge()+")"); n < 1 {
		t.Errorf("the fixture's server logged no DHCPACK on %s within %s, so the endpoint "+
			"was not on the segment whose mode is under test", f.Bridge(), attachEvidenceBudget)
	}
}
