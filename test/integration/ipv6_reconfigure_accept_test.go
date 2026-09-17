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

// #925's opt-in, measured on the wire.
//
// RFC 9915 section 21.20 makes one option the whole of whether a
// server-initiated Reconfigure can ever reach this client: "A client
// uses the Reconfigure Accept option to announce to the server whether
// the client is willing to accept Reconfigure messages ... In the
// absence of this option, the default behavior is that the client is
// unwilling to accept Reconfigure messages." A client that does not
// announce is never sent one, and every other part of #925 — the key,
// the authentication, the Renew the server asks for — is unreachable
// behind that.
//
// WHY THIS IS NOT THE UNIT TEST WE ALREADY HAVE.
// pkg/dhcp/v6mode_test.go's TestBuildParams6_CarriesTheModeTheLinkAddressAndReconfigure
// asks buildParams6 for its AcceptReconfigure field, which is the
// plugin's INTENTION. It passes over an encoder that drops the option,
// over a library condition the plugin does not meet, and over a client
// that never put a datagram on the link at all. This reads the octets
// that left the host. The two are different claims and only the second
// is what a DHCPv6 server acts on.
//
// WHAT THIS DOES NOT SHOW, stated rather than left to be assumed. It
// does not show a Reconfigure being accepted, because the fixture
// cannot send one: dnsmasq defines RFC 9915's Reconfigure constants in
// src/dhcp6-protocol.h and references none of them anywhere else in its
// source (MEASURED on 2.91 and on 2.92rel2, the two versions this
// runner has been measured at), so it never sends a Reconfigure, never
// reads this option, and implements no Reconfiguration Key
// Authentication Protocol. The acceptance path is driven in the
// library's own unit tests. What the fixture CAN answer is whether this
// plugin's client asked to be reconfigured, and that is the assertion
// here.

// reconfigureAcceptBudget bounds the wait for the client's Solicit and
// Request to reach the capture.
//
// It is the ordinary acquisition budget and not a number of its own:
// the messages under test ARE the acquisition's first two, so a client
// that has acquired has sent both, and a client that has not is late
// for a reason this test does not own.
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

	// BEFORE the container, and that ordering is the test. A capture
	// opened afterwards has no Solicit to show and cannot say whether
	// it was ever able to see this client's frames at all.
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

	// THE ADDRESS FIRST, and not as decoration. Without it, a capture
	// holding no client message could be read as "the plugin does not
	// announce" when what actually happened is that no DHCPv6 exchange
	// ever ran, and the finding would name the wrong defect.
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

	// The verdict itself lives in the untagged half of the harness and
	// is driven in both directions in the fast lane
	// (harness/dhcpv6_parse_test.go), so what runs here is the same
	// function a non-privileged run has already exercised against a
	// silent Solicit, a silent Request, a missing Request and an empty
	// capture.
	if findings := harness.ReconfigureAcceptFindings(msgs, want...); len(findings) > 0 {
		t.Errorf("this plugin's DHCPv6 client did not announce RFC 9915 section 21.20's "+
			"Reconfigure Accept option (#925): %s\n%s\ncaptured:\n%s",
			strings.Join(findings, "; "), cap6.SeenTally(), harness.FormatDHCPv6Messages(msgs))
	}
}

// The third announcing kind, on the only segment that sends it.
//
// RFC 9915 section 20.4.2 names three exchanges a server may choose a
// reconfigure key in -- "the Request/Reply, Solicit/Reply, or
// Information-request/Reply message exchange" -- and a stateless
// endpoint performs exactly one of them. It sends no Solicit and no
// Request, so the Information-request is its ONLY chance to announce,
// and a client that did not announce there can never be reconfigured
// at all.
//
// IT IS ALSO THE ARM WITH THE WORST FAILURE. The library's own bound
// (proto/doc.go) is that a client holding a lease escapes a stuck
// Reconfigure exchange at its T1, and that "a STATELESS client has
// neither timer". So on this segment the announcement is both the only
// way in and the case with no timer to recover on, which is why it is
// read on the wire here and not left to the managed case above.
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

	// THE EXCHANGE FIRST, for the reason the managed case reads the
	// address first: a capture holding no client message could be read
	// as "the plugin does not announce" when nothing ran at all, and
	// the finding would name the wrong defect. On this segment there is
	// no address to read, and the counter that moves when the stateless
	// Reply is received is the evidence that one happened (#815).
	if _, ok := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.DHCPv6ConfigOnly > before.DHCPv6ConfigOnly
	}); !ok {
		t.Fatalf("dhcpv6_config_only did not move within 30s, so no stateless DHCPv6 exchange "+
			"completed and there is nothing on the wire to read. This is not #925.\n%s",
			cap6.SeenTally())
	}

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
