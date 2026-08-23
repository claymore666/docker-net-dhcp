// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net"
	"strings"
	"testing"
)

func serverParams(allow, deny []string) dhcpcdParams {
	return dhcpcdParams{
		Iface:        "eth0",
		MAC:          net.HardwareAddr{0x02, 0x42, 0x0a, 0x00, 0x00, 0x02},
		AllowServers: allow,
		DenyServers:  deny,
	}
}

func linesWithPrefix(cfg, prefix string) []string {
	var out []string
	for _, l := range strings.Split(cfg, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

func TestRenderConfig_ServerLists(t *testing.T) {
	t.Run("no lists emits neither directive", func(t *testing.T) {
		cfg := renderConfig(serverParams(nil, nil))
		if got := linesWithPrefix(cfg, "whitelist"); len(got) != 0 {
			t.Fatalf("unexpected %v", got)
		}
		if got := linesWithPrefix(cfg, "blacklist"); len(got) != 0 {
			t.Fatalf("unexpected %v", got)
		}
	})

	t.Run("allow list becomes whitelist lines, in order", func(t *testing.T) {
		cfg := renderConfig(serverParams([]string{"1.1.1.1", "2.2.2.2"}, nil))
		got := linesWithPrefix(cfg, "whitelist")
		want := []string{"whitelist 1.1.1.1", "whitelist 2.2.2.2"}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("deny list alone becomes blacklist lines", func(t *testing.T) {
		cfg := renderConfig(serverParams(nil, []string{"3.3.3.3"}))
		got := linesWithPrefix(cfg, "blacklist")
		if len(got) != 1 || got[0] != "blacklist 3.3.3.3" {
			t.Fatalf("got %v", got)
		}
	})

	// dhcpcd stops consulting the blacklist once a whitelist exists
	// (src/dhcp.c:3181-3196). A config carrying both would claim a
	// denial the client never enforces, so the renderer must not write
	// one even when a caller hands it both.
	t.Run("a whitelist suppresses the blacklist entirely", func(t *testing.T) {
		cfg := renderConfig(serverParams([]string{"1.1.1.1"}, []string{"3.3.3.3"}))
		if got := linesWithPrefix(cfg, "blacklist"); len(got) != 0 {
			t.Fatalf("emitted %v alongside a whitelist; dhcpcd would never read it", got)
		}
		if got := linesWithPrefix(cfg, "whitelist"); len(got) != 1 {
			t.Fatalf("whitelist lines = %v", got)
		}
		if strings.Contains(cfg, "3.3.3.3") {
			t.Fatal("denied address still appears in the rendered config")
		}
	})

	// The generated file is fed to dhcpcd with -f; a directive it cannot
	// parse takes the client down rather than degrading. Keep the
	// emitted form to the one verified against the shipped binary.
	t.Run("directives use the bare address form", func(t *testing.T) {
		cfg := renderConfig(serverParams([]string{"1.1.1.1"}, nil))
		for _, l := range linesWithPrefix(cfg, "whitelist") {
			if fields := strings.Fields(l); len(fields) != 2 {
				t.Fatalf("line %q has %d fields, want 2", l, len(fields))
			}
		}
	})
}
