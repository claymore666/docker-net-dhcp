// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// TestV6AcquisitionDeadline_LeavesTheV4HalfRoomAndStillFitsTheDaemon is
// the arithmetic #868's fix was defeated by, driven as arithmetic.
//
// The three failures it describes were all one sum: the v4 half of a
// dual-stack CreateEndpoint costs about 11s on the 2.x bridge fixture
// (MEASURED 2026-09-06, three v4-only endpoints in run 34056785164),
// dhcp.V6AcquisitionWindow is 21.7s, and 11 + 21.7 is past the 30s the
// daemon waits. Each bound below is one half of what the deadline has
// to be true of at once.
func TestV6AcquisitionDeadline_LeavesTheV4HalfRoomAndStillFitsTheDaemon(t *testing.T) {
	// Measured off the ARITHMETIC and not off the clock: a
	// time.Until() here is short by however long the call took, and a
	// margin of zero then reads as a margin of a few microseconds --
	// MEASURED, the mutant that deletes the margin survived exactly
	// that.
	start := time.Now()
	budget := v6AcquisitionDeadline(start).Sub(start)

	// minimumCallMargin is what has to be left under the daemon's
	// deadline AFTER the v6 verdict, and it is a floor derived from
	// measurement rather than a restatement of pluginCallMargin --
	// reading that constant here would make this check true by
	// construction. In run 34056785164 the response and its status
	// line were logged in the same second as the verdict, and the
	// daemon's own CreateEndpoint line was in the same second as the
	// plugin's first: both under a second, so the floor is two.
	const minimumCallMargin = 2 * time.Second

	if pluginCallBudget-budget < minimumCallMargin {
		t.Errorf("the v6 acquisition may run for %v of the daemon's %v, leaving %v for "+
			"the response; the container start then fails with the daemon's timeout "+
			"instead of the plugin's verdict, which is the shape of #868",
			budget, pluginCallBudget, pluginCallBudget-budget)
	}

	// MEASURED, not assumed: the v4 half's cost on the bridge fixture.
	// The v6 half must still be able to reach a verdict after it.
	const measuredV4Half = 11 * time.Second
	left := budget - measuredV4Half
	if left < dhcp.RouterDiscoveryWindow(proto.DefaultParams6()) {
		t.Errorf("after the v4 half's measured %v there is %v left, which is less than "+
			"RFC 4861's router-discovery window; every absence verdict on a dual-stack "+
			"endpoint would then describe the deadline rather than the segment",
			measuredV4Half, left)
	}

	// The other direction: a deadline so generous that it does not
	// bound the derived window at all would leave the sum unchanged.
	if left >= dhcp.V6AcquisitionWindow(proto.DefaultParams6()) {
		t.Errorf("the remainder after the v4 half is %v, which is not shorter than the "+
			"derived window %v; this deadline changes nothing and the 11+21.7 sum stands",
			left, dhcp.V6AcquisitionWindow(proto.DefaultParams6()))
	}
}

// TestWithV6AcquisitionDeadline_NeverExtendsTheCallersDeadline is the
// guard's other direction: it is a CEILING, not a replacement.
func TestWithV6AcquisitionDeadline_NeverExtendsTheCallersDeadline(t *testing.T) {
	t.Run("a shorter caller wins", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		ctx, end := withV6AcquisitionDeadline(parent, time.Now())
		defer end()

		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("no deadline on the v6 acquisition context")
		}
		if time.Until(dl) > 3*time.Second {
			t.Errorf("the v6 context outlives its caller by %v; a lease_timeout the "+
				"operator lowered would stop bounding the v6 half", time.Until(dl))
		}
	})

	t.Run("an unbounded caller is bounded", func(t *testing.T) {
		ctx, end := withV6AcquisitionDeadline(context.Background(), time.Now())
		defer end()

		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("a caller with no deadline of its own produced a v6 acquisition with " +
				"no deadline either; the daemon's is then the only one, and it belongs " +
				"to a client that has already stopped listening")
		}
	})
}

// TestV6Budget_EveryAcquisitionSiteCarriesTheDeadline is the wiring
// half, and it exists for the reason its sibling in
// v6_absence_sites_test.go does: neither of the two CreateEndpoint
// bodies can be executed without root, a netns and a parent NIC, so a
// site the fix does not reach is invisible to the unit lane.
//
// KEYED ON THE PROPERTY. The subject is "a site that acquires a lease",
// found by the same call spelling the classifier check uses, and the
// obligation is that the deadline helper is named in the same file. A
// site that reached the helper through a wrapper would read as a
// violation here; a file that names the helper in a comment and not in
// code would read as compliant. It is the weaker instrument, stated as
// such, and the integration lane is the strong one.
func TestV6Budget_EveryAcquisitionSiteCarriesTheDeadline(t *testing.T) {
	for _, name := range v6AbsenceSiteFiles {
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		src := string(body)
		if !strings.Contains(src, v6AbsenceAcquireCall) {
			t.Fatalf("%s no longer contains %q: this test's domain is empty for that file, "+
				"and an empty domain satisfies the rule below without checking anything",
				name, v6AbsenceAcquireCall)
		}
		if !strings.Contains(src, "withV6AcquisitionDeadline(") {
			t.Errorf("%s acquires a lease but never bounds the v6 half by the daemon's "+
				"deadline; on a dual-stack endpoint the v4 half has already spent part "+
				"of it, and the verdict arrives after the daemon has stopped listening",
				name)
		}
	}
}
