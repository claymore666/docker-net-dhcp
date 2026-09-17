// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"strings"
	"testing"
)

// /etc/resolv.conf is line-oriented and unquoted, so a DHCP option
// carrying a newline appends a line the SERVER chose — and `nameserver`
// is a legal one, which turns a hostile or compromised DHCP server into
// the container's resolver. #689.
//
// The asymmetry this pins is real and was the actual defect: the DNS list
// and the search list arrive via strings.Fields, so whitespace is
// structurally impossible in them, while option 15 (the single domain) is
// taken whole. The tests cover all three anyway, because the safety of the
// first two is a property of a helper somewhere else that nothing here
// would notice losing.

func resolvLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestBuildResolvConf_ServerSuppliedDomainCannotAddALine(t *testing.T) {
	got := buildResolvConf(
		[]string{"192.0.2.53"},
		nil,
		"example.com\nnameserver 203.0.113.9",
		"",
	)
	for _, line := range resolvLines(got) {
		if strings.Contains(line, "203.0.113.9") {
			t.Fatalf("option 15 introduced a resolver:\n%s", got)
		}
	}
	if !strings.Contains(string(got), "nameserver 192.0.2.53") {
		t.Errorf("the legitimate nameserver was lost:\n%s", got)
	}
}

func TestBuildResolvConf_EveryFieldIsFiltered(t *testing.T) {
	cases := []struct {
		name        string
		dns, search []string
		domain      string
	}{
		{"domain", []string{"192.0.2.53"}, nil, "a\nnameserver 203.0.113.9"},
		{"search list", []string{"192.0.2.53"}, []string{"a\nnameserver 203.0.113.9"}, ""},
		{"dns list", []string{"192.0.2.53", "b\nnameserver 203.0.113.9"}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildResolvConf(tc.dns, tc.search, tc.domain, "")
			if strings.Contains(string(got), "203.0.113.9") {
				t.Errorf("%s reached the file:\n%s", tc.name, got)
			}
		})
	}
}

// A resolv.conf whose every nameserver was dropped must not be written at
// all: an empty one silently removes name resolution, which is worse than
// leaving the container with what it had. The renderer cannot make that
// call, so this pins that the WRITER's emptiness guard sees the filtered
// list rather than the raw one.
func TestWriteContainerResolvConf_RefusesWhenFilteringEmptiesTheList(t *testing.T) {
	// The container ID is irrelevant here: the emptiness guard fires
	// before the PID is ever looked at, which is itself part of the
	// contract -- filtering must not be reachable only via /proc.
	err := writeContainerResolvConf(1, "0123456789abcdef", []string{"bad\nnameserver 203.0.113.9"}, nil, "", "")
	if err == nil {
		t.Fatal("expected a refusal, got nil — an empty resolv.conf would have been written")
	}
	if !strings.Contains(err.Error(), "empty resolv.conf") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// The opposite failure: over-filtering would break ordinary deployments.
func TestBuildResolvConf_KeepsOrdinaryValues(t *testing.T) {
	got := string(buildResolvConf(
		[]string{"192.0.2.53", "2001:db8::53"},
		[]string{"corp.example", "example.com"},
		"fallback.example",
		"",
	))
	for _, want := range []string{
		"nameserver 192.0.2.53", "nameserver 2001:db8::53", "search corp.example example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestBuildResolvConf_DomainYieldsOneSearchDomain closes the gap #689
// recorded one character short: SafeValue rejects r < 0x20, and
// 0x20 -- the space -- is the field separator of the file it protects.
// So a space passed the filter and one search domain became three, with
// the server's choices ahead of the operator's.
//
// Removing the FirstSearchDomain call in buildResolvConf turns this red.
func TestBuildResolvConf_DomainYieldsOneSearchDomain(t *testing.T) {
	out := string(buildResolvConf([]string{"10.99.0.53"}, nil, "a.attacker.test b.attacker.test corp.example", ""))

	var search string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "search ") {
			search = line
		}
	}
	if search != "search a.attacker.test" {
		t.Errorf("search line = %q, want exactly one domain", search)
	}
	if strings.Contains(out, "b.attacker.test") || strings.Contains(out, "corp.example") {
		t.Errorf("a second domain reached the file:\n%s", out)
	}
}

// TestBuildResolvConf_SearchListEntryYieldsOneSearchDomain is #704
// through the OTHER channel, found by FuzzBuildResolvConf (#1010).
//
// TestBuildResolvConf_DomainYieldsOneSearchDomain above closed it for
// option 15, the single domain, because option 15 is taken whole. The
// search LIST was called structurally safe on the grounds that it
// reaches the plugin through strings.Fields — which is a property of
// whoever fills it, not of this renderer, and resolvSafe's own comment
// says the filter is here so the renderer cannot emit a line it was not
// asked for whoever calls it. It could: one entry carrying a space
// rendered as two search domains, with the server's first.
//
// Whether the library can put a space inside an RFC 1035 label is not
// measured here and is not what makes this a defect: the backstop
// claimed to hold for any caller and did not.
func TestBuildResolvConf_SearchListEntryYieldsOneSearchDomain(t *testing.T) {
	out := string(buildResolvConf([]string{"10.99.0.53"}, []string{"a.attacker.test b.attacker.test", "corp.example"}, "", ""))

	var search string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "search ") {
			search = line
		}
	}
	if search != "search corp.example" {
		t.Errorf("search line = %q, want only the entry that is one field", search)
	}
	if strings.Contains(out, "attacker.test") {
		t.Errorf("a multi-field search entry reached the file:\n%s", out)
	}
}

// TestBuildResolvConf_ZoneCannotRestructureTheLine is the one argument
// that reached the file unfiltered (#1010).
//
// RFC 4007 section 11 spells a scope zone after a '%', and
// zonedNameserver appends the container's interface name there for a
// link-local resolver (RFC 8106 section 5.1, #821). The name went in
// whole, so the nameserver line was only as well-formed as its caller.
//
// UNREACHABLE TODAY, and pinned anyway: the one caller reads the name
// off a netlink link (dhcp_manager.go), and a kernel interface name
// cannot hold whitespace. The filter costs one line and removes the
// dependence on that fact being true forever.
func TestBuildResolvConf_ZoneCannotRestructureTheLine(t *testing.T) {
	out := string(buildResolvConf([]string{"fe80::1"}, nil, "", "eth0\nnameserver 6.6.6.6"))

	if strings.Contains(out, "6.6.6.6") {
		t.Errorf("an interface name appended a line:\n%s", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("got %d lines, want the marker and one nameserver:\n%s", len(lines), out)
	}
	if lines[1] != "nameserver fe80::1" {
		t.Errorf("nameserver line = %q, want the address with no zone", lines[1])
	}
}
