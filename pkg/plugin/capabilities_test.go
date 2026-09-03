// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestManifestsGrantCapNetRaw pins the capability the DHCP transport
// needs, in every manifest that ships a plugin.
//
// WHY A TEST AND NOT A COMMENT. 2.0 opens an AF_PACKET socket per
// endpoint: the exchange has to reach a link the kernel has no address
// on, which is the whole reason a DHCP client is not an ordinary UDP
// program. Without CAP_NET_RAW that socket fails at socket(2), so the
// failure arrives on the first `docker run` against a plugin whose unit
// tests, gates and image build were all green.
//
// It is pinned in BOTH directions of the mistake that produced it. The
// capability was NOT granted before this change — config.json listed
// CAP_NET_ADMIN, CAP_SYS_ADMIN and CAP_SYS_PTRACE — while issue #725's
// title asserts that "CAP_NET_RAW is already granted". A claim about a
// manifest that the manifest contradicts is exactly what a test is for,
// and the second half of the assertion below is that the manifest is
// read rather than the claim.
func TestManifestsGrantCapNetRaw(t *testing.T) {
	const want = "CAP_NET_RAW"

	for _, name := range pluginManifests {
		b, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var m struct {
			Linux struct {
				Capabilities []string `json:"capabilities"`
			} `json:"linux"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		// An empty list would satisfy "contains nothing forbidden" and
		// must not satisfy this: the manifest has to name capabilities
		// at all before the presence of one means anything.
		if len(m.Linux.Capabilities) == 0 {
			t.Fatalf("%s declares no capabilities at all; the check below would pass vacuously", name)
		}

		found := false
		for _, c := range m.Linux.Capabilities {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not grant %s (has %v). The DHCP transport opens an AF_PACKET "+
				"socket per endpoint — it must send on a link with no address, which is what "+
				"AF_PACKET is for — and socket(2) fails without it. Every container on every "+
				"network of this plugin would fail to get an address, and nothing before the "+
				"first `docker run` would say so.", name, want, m.Linux.Capabilities)
		}
	}
}
