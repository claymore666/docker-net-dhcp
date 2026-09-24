// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/vishvananda/netlink"
)

// An empty tombstone hostname is a wildcard, so a refused hostname must not be recorded
// as one while an absent one still is (#726).
func TestDeleteEndpoint_ARefusedHostnameIsNotAnAbsentOne(t *testing.T) {
	cases := []struct {
		name              string
		hostname          string
		hostnameTrusted   bool
		unrelatedConsumes bool
		ownConsumes       bool
		reason            string
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
				MAC: "02:42:ac:11:00:02", IPv4: "192.168.0.50",
			}, dhcpHostname{name: tc.hostname, refused: !tc.hostnameTrusted})

			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
				NetworkID: "n1", EndpointID: "ep-1",
			}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}

			mac, ipv4, _, ok := p.tombstones.consume("n1", "unrelated-container")
			if ok != tc.unrelatedConsumes {
				t.Fatalf("an unrelated container consumed=%v (mac=%q ipv4=%q), want %v — %s",
					ok, mac, ipv4, tc.unrelatedConsumes, tc.reason)
			}
			if ok {
				return
			}

			if _, _, _, own := p.tombstones.consume("n1", tc.hostname); own != tc.ownConsumes {
				t.Errorf("the container it was written for consumed=%v, want %v — %s",
					own, tc.ownConsumes, tc.reason)
			}
		})
	}
}

func TestRememberEndpoint_TrustFlowsToTheFingerprint(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		p := &Plugin{endpointFingerprints: make(map[string]endpointFingerprint)}
		p.rememberEndpoint("ep-1", endpointFingerprint{MAC: "02:42:ac:11:00:02"},
			dhcpHostname{name: "web", refused: !trusted})

		fp, ok := p.takeEndpoint("ep-1")
		if !ok {
			t.Fatalf("trusted=%v: nothing remembered", trusted)
		}
		if fp.HostnameRefused == trusted {
			t.Errorf("trusted=%v recorded HostnameRefused=%v; the two must be opposites or DeleteEndpoint "+
				"reads the wrong instruction", trusted, fp.HostnameRefused)
		}
		if fp.Hostname != "web" {
			t.Errorf("trusted=%v recorded Hostname=%q, want \"web\" -- rememberEndpoint fills the field from "+
				"the hostname value now, so dropping the name is a live way to break narrow matching", trusted, fp.Hostname)
		}
	}
}
