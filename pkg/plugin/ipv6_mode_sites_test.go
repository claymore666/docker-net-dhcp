// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Three sites start a DHCPv6 client and each must pass ipv6_mode: proto.Mode6's zero value
// is Mode6DHCP, so a missed site silently runs DHCPv6 on a slaac network (#817).

const (
	v6WiringIdentityAssign  = ".Identity6 ="
	v6WiringIdentityLiteral = "Identity6:"
	v6WiringModeAssign      = ".Mode6 ="
	v6WiringModeLiteral     = "Mode6:"
	v6WiringCall            = "v6Wiring("
	v6WiringOwner           = "ipv6_mode.go"
)

var v6WiringSiteFiles = []string{"v6_acquire.go", "dhcp_manager.go"}

func pluginSourceLines(t *testing.T, name string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("reading %s: %v\nIf it was renamed, rename it in v6WiringSiteFiles "+
			"too — do not drop it.", name, err)
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			out = append(out, "")
			continue
		}
		out = append(out, line)
	}
	return out
}

func lineSetsV6Identity(line string) bool {
	return strings.Contains(line, v6WiringIdentityAssign) || strings.Contains(line, v6WiringIdentityLiteral)
}

func lineSetsV6Mode(line string) bool {
	return strings.Contains(line, v6WiringModeAssign) || strings.Contains(line, v6WiringModeLiteral)
}

func TestV6Wiring_EverySiteThatStartsAV6ClientGoesThroughTheHelper(t *testing.T) {
	for _, name := range v6WiringSiteFiles {
		calls := 0
		for i, line := range pluginSourceLines(t, name) {
			if lineSetsV6Identity(line) || lineSetsV6Mode(line) {
				t.Errorf("%s:%d sets a DHCPv6 client field directly:\n  %s\n"+
					"Call %s instead. A site that sets the identity without the mode runs "+
					"in proto.Mode6DHCP — the enum's zero value — whatever ipv6_mode the "+
					"network stores, and nothing fails.",
					name, i+1, strings.TrimSpace(line), v6WiringCall)
			}
			if strings.Contains(line, v6WiringCall) {
				calls++
			}
		}
		if calls == 0 {
			t.Errorf("%s starts a DHCPv6 client and never calls %s. Either the client "+
				"construction moved — in which case this list must follow it — or this "+
				"site no longer hands the library the network's ipv6_mode.",
				name, v6WiringCall)
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == v6WiringOwner {
			continue
		}
		for i, line := range pluginSourceLines(t, name) {
			if lineSetsV6Identity(line) || lineSetsV6Mode(line) {
				t.Errorf("%s:%d sets a DHCPv6 client field outside %s:\n  %s\n"+
					"Every client this plugin starts gets its identity and its ipv6_mode "+
					"from %s, together. See the comment on that function.",
					name, i+1, v6WiringOwner, strings.TrimSpace(line), v6WiringCall)
			}
		}
	}

	owner := strings.Join(pluginSourceLines(t, v6WiringOwner), "\n")
	if !strings.Contains(owner, v6WiringIdentityAssign) {
		t.Errorf("%s no longer contains %q, so the identity half of this test matches "+
			"nothing anywhere. Follow the rename here before trusting the sweep.",
			v6WiringOwner, v6WiringIdentityAssign)
	}
	if !strings.Contains(owner, v6WiringModeAssign) {
		t.Errorf("%s no longer contains %q, so the mode half of this test matches "+
			"nothing anywhere. Follow the rename here before trusting the sweep.",
			v6WiringOwner, v6WiringModeAssign)
	}
}

func TestV6WiringMatchers_SeeBothSpellings(t *testing.T) {
	ident := []string{
		"\t\t\t\tbase.Identity6 = identity6",
		"\t\tclientOpts.Identity6 = identity6",
		"\t\topts := dhcp.DHCPClientOptions{Identity6: id6}",
		"\t\t\tIdentity6: id6,",
	}
	mode := []string{
		"\tbase.Mode6 = mode",
		"\t\topts := dhcp.DHCPClientOptions{Mode6: proto.Mode6SLAAC}",
		"\t\t\tMode6: m,",
	}
	for _, l := range ident {
		if !lineSetsV6Identity(l) {
			t.Errorf("lineSetsV6Identity(%q) is false, so a site written that way is invisible "+
				"to the sweep and runs in the enum's zero mode", l)
		}
	}
	for _, l := range mode {
		if !lineSetsV6Mode(l) {
			t.Errorf("lineSetsV6Mode(%q) is false", l)
		}
	}
	for _, l := range []string{
		"\tid6, err := p.resolveIdentity6(r.EndpointID)",
		"\tif opts.ipv6Enabled() {",
		"",
	} {
		if lineSetsV6Identity(l) || lineSetsV6Mode(l) {
			t.Errorf("an ordinary line is reported as a wiring site: %q", l)
		}
	}
	for i, got := range pluginSourceLines(t, v6WiringOwner) {
		if strings.HasPrefix(strings.TrimSpace(got), "//") {
			t.Fatalf("%s:%d survived comment stripping: %q", v6WiringOwner, i+1, got)
		}
	}
}
