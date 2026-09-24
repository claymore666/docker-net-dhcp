// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"strings"
	"testing"
)

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

func TestWriteContainerResolvConf_RefusesWhenFilteringEmptiesTheList(t *testing.T) {
	err := writeContainerResolvConf(1, "0123456789abcdef", []string{"bad\nnameserver 203.0.113.9"}, nil, "", "")
	if err == nil {
		t.Fatal("expected a refusal, got nil — an empty resolv.conf would have been written")
	}
	if !strings.Contains(err.Error(), "empty resolv.conf") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

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
