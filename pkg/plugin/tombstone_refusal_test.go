// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestDeleteEndpoint_ARefusedHostnameIsNotAnAbsentOne pins the #726
// wildcard-write in BOTH directions, because a guard that only refuses
// is as broken as one that only permits.
//
// The tombstone store treats an empty Hostname as a WILDCARD: consume
// skips an entry only when `hostname != "" && t.Hostname != "" &&
// t.Hostname != hostname`. That is correct and load-bearing for an
// honest absence -- it is the v0.5.0 contract for containers that have
// no hostname, and a fix that stopped writing those tombstones would
// silently take MAC stability away from every one of them.
//
// It is exactly wrong for a REFUSAL. safeHostname returns ("", false)
// for a hostname the plugin will not put in a DHCP packet, and both
// CreateEndpoint paths then wrote a fingerprint carrying that "" --
// so the value we declined to trust for a NARROW match became a match
// against EVERYTHING, handing this container's MAC and IP to whichever
// unrelated container next started on the network.
//
// Direction 1 alone would pass if the fix simply stopped writing
// tombstones. Direction 2 alone would pass against the unfixed code.
// Only the pair says what the code must do.
func TestDeleteEndpoint_ARefusedHostnameIsNotAnAbsentOne(t *testing.T) {
	cases := []struct {
		name string
		// hostname/hostnameTrusted are what CreateEndpoint recorded.
		hostname        string
		hostnameTrusted bool
		// unrelatedConsumes is the damage question: can a container
		// that has nothing to do with this one pick the tombstone up?
		unrelatedConsumes bool
		// ownConsumes is the value question, checked only when the
		// tombstone survived the first: does the container it was
		// written for still get it back?
		ownConsumes bool
		reason      string
	}{
		{
			name:              "a refused hostname writes NOTHING",
			hostname:          "",
			hostnameTrusted:   false,
			unrelatedConsumes: false,
			ownConsumes:       false,
			reason: "the empty string is the matcher's wildcard, so writing it hands this endpoint's MAC and IP " +
				"to the next unrelated container on the network — the refusal would have been a match against everything",
		},
		{
			name:              "an absent hostname still writes the network-only tombstone",
			hostname:          "",
			hostnameTrusted:   true,
			unrelatedConsumes: true,
			reason: "hostname-less containers have had network-only matching since v0.5.0, and it looks identical " +
				"to the case above from inside the store; refusing to write for them would take MAC stability away " +
				"from every container that simply has no hostname",
		},
		{
			name:              "an ordinary hostname is unaffected",
			hostname:          "web",
			hostnameTrusted:   true,
			unrelatedConsumes: false,
			ownConsumes:       true,
			reason:            "the common path must not move: narrowed to its own container, and returned to it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			if err := saveOptions("n1", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}

			restore := nlLinkByName
			nlLinkByName = func(string) (netlink.Link, error) { return nil, netlink.LinkNotFoundError{} }
			t.Cleanup(func() { nlLinkByName = restore })

			p := &Plugin{
				docker:               &fakeDocker{inspectErr: errors.New("docker must not be called")},
				endpointFingerprints: make(map[string]endpointFingerprint),
			}
			p.rememberEndpoint("ep-1", endpointFingerprint{
				MAC: "02:42:ac:11:00:02", IPv4: "192.168.0.50", Hostname: tc.hostname,
			}, tc.hostnameTrusted)

			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
				NetworkID: "n1", EndpointID: "ep-1",
			}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}

			// The victim's view first. Consuming as the SAME container
			// would find its own tombstone and look correct in every
			// case, including the broken one.
			mac, ipv4, _, ok := p.tombstones.consume("n1", "unrelated-container")
			if ok != tc.unrelatedConsumes {
				t.Fatalf("an unrelated container consumed=%v (mac=%q ipv4=%q), want %v — %s",
					ok, mac, ipv4, tc.unrelatedConsumes, tc.reason)
			}
			if ok {
				return // the entry is gone; nothing left to ask.
			}

			if _, _, _, own := p.tombstones.consume("n1", tc.hostname); own != tc.ownConsumes {
				t.Errorf("the container it was written for consumed=%v, want %v — %s",
					own, tc.ownConsumes, tc.reason)
			}
		})
	}
}

// TestRememberEndpoint_TrustFlowsToTheFingerprint pins the plumbing
// separately from the behaviour above.
//
// The defect was never in the tombstone store. It was that both
// CreateEndpoint paths HELD the trust bit -- they pass it to
// consumeTombstone a screen earlier -- and then dropped it when they
// built the fingerprint. Making it an argument means a future caller
// cannot drop it silently; this checks the argument actually reaches
// the field, so the compiler's half and the runtime's half both hold.
func TestRememberEndpoint_TrustFlowsToTheFingerprint(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		p := &Plugin{endpointFingerprints: make(map[string]endpointFingerprint)}
		p.rememberEndpoint("ep-1", endpointFingerprint{MAC: "02:42:ac:11:00:02"}, trusted)

		fp, ok := p.takeEndpoint("ep-1")
		if !ok {
			t.Fatalf("trusted=%v: nothing remembered", trusted)
		}
		if fp.HostnameRefused == trusted {
			t.Errorf("trusted=%v recorded HostnameRefused=%v; the two must be opposites or DeleteEndpoint "+
				"reads the wrong instruction", trusted, fp.HostnameRefused)
		}
	}
}
