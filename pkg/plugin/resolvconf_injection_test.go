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
			got := buildResolvConf(tc.dns, tc.search, tc.domain)
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
	err := writeContainerResolvConf(1, "0123456789abcdef", []string{"bad\nnameserver 203.0.113.9"}, nil, "")
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
	))
	for _, want := range []string{
		"nameserver 192.0.2.53", "nameserver 2001:db8::53", "search corp.example example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
