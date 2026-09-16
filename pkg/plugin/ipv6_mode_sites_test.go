// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This plugin starts a DHCPv6 client at three places: the bridge attach
// path (network.go), the parent-attached attach path
// (parent_attached.go), and the persistent client the manager runs
// (dhcp_manager.go). Each one has to hand the library the network's
// `ipv6_mode` (#817).
//
// WHAT GOES WRONG IF ONE IS MISSED IS NOTHING VISIBLE. proto.Mode6's
// zero value is Mode6DHCP, so a site that sets the identity and forgets
// the mode starts a perfectly healthy client running the behaviour that
// shipped before the option existed. The network is stored with
// `ipv6_mode=slaac`, `docker network inspect` shows it, the reference
// documents it, and the endpoint asks a DHCPv6 server for an address.
// No error, no counter, no log line. On top of that the two attach
// paths are structurally identical copies and neither compiles against
// the other, and the integration fixture exercises the bridge one -- so
// "macvlan is silently dhcp" ships green.
//
// The defence in the code is that the mode travels on the same call as
// the identity, which buildParams6 refuses when empty; this test is the
// defence against a later edit that takes them apart again. It reads
// source because the alternative needs a netns, a parent NIC and a
// DHCPv6 server; it is the weaker instrument and it is the one that
// runs on every push.

const (
	// The property: a line that puts a DHCPv6 identity on a client's
	// options, or the mode it runs in.
	//
	// BOTH SPELLINGS, AND THE LITERAL ONE MATCHES NOTHING IN THE TREE
	// TODAY. Every site goes through the helper, which assigns; a
	// fourth site could just as well build a DHCPClientOptions literal,
	// which is the shape the sweep would otherwise walk straight past.
	// A matcher with no live subject is an inert control, so
	// TestV6WiringMatchers_SeeBothSpellings below drives all four
	// against lines written out by hand.
	v6WiringIdentityAssign  = ".Identity6 ="
	v6WiringIdentityLiteral = "Identity6:"
	v6WiringModeAssign      = ".Mode6 ="
	v6WiringModeLiteral     = "Mode6:"
	v6WiringCall            = "v6Wiring("
	// The file that is allowed to do it.
	v6WiringOwner = "ipv6_mode.go"
)

// v6WiringSiteFiles is the population, named rather than globbed, on
// v6AbsenceSiteFiles' rule: a universal over a set that can be emptied
// by deleting a file is satisfied by deleting the file.
var v6WiringSiteFiles = []string{"network.go", "parent_attached.go", "dhcp_manager.go"}

// pluginSourceLines returns a package file's lines with whole-line
// comments dropped, so a comment that names a field is not mistaken for
// a line that sets one. A trailing comment on a real assignment still
// counts, which is the direction that is safe.
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

// TestV6Wiring_EverySiteThatStartsAV6ClientGoesThroughTheHelper is the
// whole point of the helper: one place sets the identity, so one place
// sets the mode, so the two cannot come apart.
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

	// The list is a floor. A FOURTH site added later would be judged by
	// nothing, so the package is swept for the same property and
	// anything found outside the owner is a failure here rather than a
	// silent `dhcp` network in production.
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

	// Non-vacuity, in the direction the sweep cannot cover: if the
	// field spellings change, every loop above goes quiet and this test
	// passes while checking nothing. The owner is where both spellings
	// must still appear.
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

// The matchers, driven directly.
//
// THREE OF THE FOUR SPELLINGS MATCH NOTHING IN THE PACKAGE, so without
// this the sweep above could be asserting over a predicate that is
// simply false everywhere and would stay green through the very edit it
// exists to catch. The owner-file check at the end of the sweep covers
// the two assignment spellings; this covers the composite-literal ones,
// and the comment stripping that decides what a "line" is.
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
	// The other direction: ordinary lines are not findings, or the
	// sweep fires on the first unrelated edit and gets deleted.
	for _, l := range []string{
		"\tid6, err := p.resolveIdentity6(r.EndpointID)",
		"\tif opts.ipv6Enabled() {",
		"",
	} {
		if lineSetsV6Identity(l) || lineSetsV6Mode(l) {
			t.Errorf("an ordinary line is reported as a wiring site: %q", l)
		}
	}
	// A whole-line comment naming a field is not a line that sets one.
	// pluginSourceLines is what makes that true, and a comment about
	// Identity6 in any of these files would otherwise be a permanent
	// failure nobody could fix without deleting the sentence.
	for i, got := range pluginSourceLines(t, v6WiringOwner) {
		if strings.HasPrefix(strings.TrimSpace(got), "//") {
			t.Fatalf("%s:%d survived comment stripping: %q", v6WiringOwner, i+1, got)
		}
	}
}
