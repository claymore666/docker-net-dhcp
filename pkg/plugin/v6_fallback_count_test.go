// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestV6FallbackReporter_TwoClientsOnOneEndpointCountOnceAndARecreatedEndpointCountsAgain(t *testing.T) {
	const netID = "net-fallback"
	const epA = "aaaaaaaaaaaa0000000000000000000000000000000000000000000000000001"
	const epB = "bbbbbbbbbbbb0000000000000000000000000000000000000000000000000002"
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})
	joinClient, persistentClient := p.v6FallbackReporter(epA), p.v6FallbackReporter(epA)
	joinClient(1)
	persistentClient(1)
	if got := p.dhcpv6AutoFallbacks.Load(); got != 1 {
		t.Errorf("dhcpv6_auto_fallbacks = %d after the Join and persistent clients of one endpoint fell back, want 1", got)
	}
	if n := len(hook.AllEntries()); n != 2 {
		t.Errorf("%d fallback warnings for two client runs, want one per run", n)
	}

	p.v6FallbackReporter(epB)(1)
	if got := p.dhcpv6AutoFallbacks.Load(); got != 2 {
		t.Errorf("dhcpv6_auto_fallbacks = %d after a second endpoint fell back, want 2", got)
	}

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{NetworkID: netID, EndpointID: epA}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	p.v6FallbackReporter(epA)(1)
	if got := p.dhcpv6AutoFallbacks.Load(); got != 3 {
		t.Errorf("dhcpv6_auto_fallbacks = %d after endpoint %s was deleted, recreated and fell back, want 3", got, epA[:12])
	}
	p.v6FallbackReporter(epB)(1)
	if got := p.dhcpv6AutoFallbacks.Load(); got != 3 {
		t.Errorf("dhcpv6_auto_fallbacks = %d after the undeleted endpoint fell back again, want 3", got)
	}
}
