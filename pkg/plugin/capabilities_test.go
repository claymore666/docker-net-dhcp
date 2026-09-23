// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The client opens an AF_PACKET socket per endpoint, and socket(2) fails without
// CAP_NET_RAW (#725).
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
