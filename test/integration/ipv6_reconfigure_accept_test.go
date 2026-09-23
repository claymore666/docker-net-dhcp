// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// Without the Reconfigure Accept option a client is never sent a Reconfigure (RFC 9915 section 21.20), so this reads
// the octets that left the host; TestBuildParams6_CarriesTheModeTheLinkAddressAndReconfigure reads only the plugin's
// intent. dnsmasq 2.91 and 2.92rel2 define the Reconfigure constants in src/dhcp6-protocol.h and use none, so the
// fixture cannot send a Reconfigure; acceptance is tested in the library (#925).

// reconfigureAcceptBudget is the acquisition budget, since the Solicit and Request under test are the acquisition's first two messages.
func reconfigureAcceptBudget() time.Duration { return harness.IPAcquisitionBudget }

func TestDHCPv6_TheClientAnnouncesReconfigureAcceptOnTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	// The capture opens before the container, or it could not show the Solicit.
	cap6 := f.StartDHCPv6Capture()
	t.Cleanup(func() {
		if t.Failed() {
			cap6.Dump(func(s string) { t.Log(s) })
		}
	})

	const netName = "dh-itest-v6reconf"
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, netName,
		map[string]string{"ipv6": "", "ipv6_mode": "dhcp"})
	if err != nil {
		t.Fatalf("a container failed to start on a managed DHCPv6 segment: %v", err)
	}

	// The address first: an empty capture without it would blame the announcement when no exchange ran.
	if addr := inspectV6(t, ctx, cli, id, netName); addr == "" {
		t.Fatalf("the container has no IPv6 address on a managed DHCPv6 segment, so no DHCPv6 "+
			"exchange ran and there is nothing on the wire to read. This is not #925: it is the "+
			"managed acquisition failing.\n%s", cap6.SeenTally())
	} else if !strings.HasPrefix(addr, harness.V6Prefix) {
		t.Fatalf("the container's IPv6 address %q is not from this segment's prefix %q, so the "+
			"messages captured below are not this fixture's exchange", addr, harness.V6Prefix)
	}

	want := []uint8{harness.DHCPv6Solicit, harness.DHCPv6Request}
	msgs, ok := cap6.AwaitClientMessages(want, reconfigureAcceptBudget())
	if !ok {
		t.Fatalf("the capture did not see both a SOLICIT and a REQUEST from the client within "+
			"%s, so there is nothing to assert about what they announced.\n%s\ncaptured:\n%s",
			reconfigureAcceptBudget(), cap6.SeenTally(), harness.FormatDHCPv6Messages(msgs))
	}

	// ReconfigureAcceptFindings is untagged and driven both ways in harness/dhcpv6_parse_test.go.
	if findings := harness.ReconfigureAcceptFindings(msgs, want...); len(findings) > 0 {
		t.Errorf("this plugin's DHCPv6 client did not announce RFC 9915 section 21.20's "+
			"Reconfigure Accept option (#925): %s\n%s\ncaptured:\n%s",
			strings.Join(findings, "; "), cap6.SeenTally(), harness.FormatDHCPv6Messages(msgs))
	}
}

// A stateless endpoint sends only an Information-request, one of the three exchanges a server may choose a
// reconfigure key in (RFC 9915 section 20.4.2), and has no T1 to escape a stuck Reconfigure (proto/doc.go) (#925).

// TestDHCPv6_Stateless_TheClientAnnouncesReconfigureAcceptOnTheWire checks that a stateless client's Information-request carries Reconfigure Accept (#925).
func TestDHCPv6_Stateless_TheClientAnnouncesReconfigureAcceptOnTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6Stateless)
	dumpOnFailure(t, f)

	cap6 := f.StartDHCPv6Capture()
	t.Cleanup(func() {
		if t.Failed() {
			cap6.Dump(func(s string) { t.Log(s) })
		}
	})

	w := harness.BeginCounterWindow(t, ctx, cli, "dhcpv6_config_only")
	if _, err := startOnV6Segment(t, ctx, cli, f, "dh-itest-v6reconfsl"); err != nil {
		t.Fatalf("a container failed to start on a stateless DHCPv6 segment: %v", err)
	}

	// On this segment the stateless Reply counter is the evidence that an exchange ran (#815).
	if _, ok := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.DHCPv6ConfigOnly > before.DHCPv6ConfigOnly
	}); !ok {
		t.Fatalf("dhcpv6_config_only did not move within 30s, so no stateless DHCPv6 exchange "+
			"completed and there is nothing on the wire to read. This is not #925.\n%s",
			cap6.SeenTally())
	}

	// End runs the plugin-instance check and reports a window never closed, which first failed Integration 35169552839 (#925).
	w.End()

	want := []uint8{harness.DHCPv6InformationRequest}
	msgs, ok := cap6.AwaitClientMessages(want, reconfigureAcceptBudget())
	if !ok {
		t.Fatalf("the capture did not see an INFORMATION-REQUEST from the client within %s, so "+
			"there is nothing to assert about what it announced.\n%s\ncaptured:\n%s",
			reconfigureAcceptBudget(), cap6.SeenTally(), harness.FormatDHCPv6Messages(msgs))
	}

	if findings := harness.ReconfigureAcceptFindings(msgs, want...); len(findings) > 0 {
		t.Errorf("this plugin's DHCPv6 client did not announce RFC 9915 section 21.20's "+
			"Reconfigure Accept option in its Information-request, which is the only message a "+
			"stateless client can announce in (#925): %s\n%s\ncaptured:\n%s",
			strings.Join(findings, "; "), cap6.SeenTally(), harness.FormatDHCPv6Messages(msgs))
	}
}
