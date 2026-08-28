// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"testing"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestV6Fixture_ModesComeUpAsRequested is the v6-modes fixture's own
// contract test: each of the four modes brings up a segment that is
// actually in that mode.
//
// It exists because the first version of that fixture came up in the
// WRONG MODE and said nothing. dnsmasq was started while the bridge's
// global IPv6 address was still tentative, so it could not send from
// it and its first router advertisement slipped from about one second
// to about nine — while logging "IPv6 router advertisement enabled"
// exactly as it does when everything is fine. Three of the four modes
// were silently degraded. The only visible symptom was a consumer
// test failing to observe behaviour that genuinely was not happening,
// and the cheapest-looking repair would have been to widen a timeout
// until the symptom went away.
//
// #815 is one consumer of this fixture; #816, #820 and #821 are the
// others. Without this test each of them would rediscover a fixture
// defect separately, as a failure in their own feature.
//
// The assertions live in the fixture (NewV6Fixture fails the test if
// the segment is not in the mode asked for), so this is the thing that
// RUNS them — and it runs them for all four modes rather than for
// whichever one a consumer happens to need today.
func TestV6Fixture_ModesComeUpAsRequested(t *testing.T) {
	for _, mode := range []harness.V6Mode{
		harness.V6Managed,
		harness.V6Stateless,
		harness.V6SLAAC,
		harness.V6NoRA,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			f := harness.NewV6Fixture(t, mode)
			t.Cleanup(func() {
				if t.Failed() {
					f.DumpLogs(func(s string) { t.Log(s) })
				}
			})
			if f.Bridge() != harness.V6BridgeName {
				t.Errorf("fixture bridge = %q, want %q", f.Bridge(), harness.V6BridgeName)
			}
			if f.Mode() != mode {
				t.Errorf("fixture mode = %s, want %s", f.Mode(), mode)
			}
		})
	}
}
