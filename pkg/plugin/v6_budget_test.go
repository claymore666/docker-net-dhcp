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
	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func TestV6AcquisitionDeadline_LeavesTheV4HalfRoomAndStillFitsTheDaemon(t *testing.T) {
	start := time.Now()
	budget := v6AcquisitionDeadline(start).Sub(start)

	// In run 34056785164 the response and the daemon's first line each fell within a second
	// of the plugin's verdict and first line, so the floor is two seconds (#911).
	const minimumCallMargin = 2 * time.Second

	if pluginCallBudget-budget < minimumCallMargin {
		t.Errorf("the v6 acquisition may run for %v of the daemon's %v, leaving %v for "+
			"the response; the container start then fails with the daemon's timeout "+
			"instead of the plugin's verdict, which is the shape of #868",
			budget, pluginCallBudget, pluginCallBudget-budget)
	}

	// Measured 2026-09-06 in run 34056785164 on the bridge fixture (#911).
	const measuredV4Half = 11 * time.Second
	left := budget - measuredV4Half
	if left < dhcp.RouterDiscoveryWindow(proto.DefaultParams6()) {
		t.Errorf("after the v4 half's measured %v there is %v left, which is less than "+
			"RFC 4861's router-discovery window; every absence verdict on a dual-stack "+
			"endpoint would then describe the deadline rather than the segment",
			measuredV4Half, left)
	}

	if left >= dhcp.V6AcquisitionWindow(proto.DefaultParams6()) {
		t.Errorf("the remainder after the v4 half is %v, which is not shorter than the "+
			"derived window %v; this deadline changes nothing and the 11+21.7 sum stands",
			left, dhcp.V6AcquisitionWindow(proto.DefaultParams6()))
	}
}

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
