// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bridge and parent-attached attach paths each need #868's tolerance and the integration
// fixture covers only the bridge one, so this test reads source. It keys on the call, since a
// site that named its context reqCtx once passed unchecked (#868).
const (
	v6AbsenceAcquireCall = "p.acquireWithPolicy("
	v6AbsenceConsult     = "p.noteV6Absence("
	v6AbsenceWindow      = 12
)

var v6AbsenceSiteFiles = []string{"network.go", "parent_attached.go"}

// The IPAM driver's reserve and link_local.go's acquireV4 are exempt because each call passes the literal false
// for v6, which the test checks; the reserve moves to the list above when the driver gains IPv6 (#110, #904).
var v6AbsenceV4OnlySites = []string{"ipam_reserve.go", "link_local.go"}

const v6AbsenceV4OnlyCall = ", false,"

func TestV6Absence_V4OnlySitesCannotAcquireV6(t *testing.T) {
	total := 0
	for _, name := range v6AbsenceV4OnlySites {
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("reading %s: %v\nIf it was renamed, rename it in "+
				"v6AbsenceV4OnlySites too — do not drop it.", name, err)
		}
		found := 0
		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, v6AbsenceAcquireCall) {
				continue
			}
			found++
			total++
			if !strings.Contains(line, v6AbsenceV4OnlyCall) {
				t.Errorf("%s:%d is exempt from the DHCPv6-absence classifier because it "+
					"only ever acquires IPv4, and this line does not pass the literal "+
					"false for the v6 argument:\n  %s\nEither keep it v4-only, or move "+
					"the file to v6AbsenceSiteFiles and make it consult %s.",
					name, i+1, strings.TrimSpace(line), v6AbsenceConsult)
			}
		}
		if found == 0 {
			t.Errorf("%s is listed as a v4-only acquisition site and contains no %s call. "+
				"An exemption for a site that does not exist is an exemption nothing checks.",
				name, v6AbsenceAcquireCall)
		}
	}
	if total == 0 {
		t.Errorf("found no acquisition site across %v — a universal over an empty set "+
			"is not a check", v6AbsenceV4OnlySites)
	}
}

func TestV6Absence_EveryAcquisitionSiteConsultsTheClassifier(t *testing.T) {
	total := 0
	for _, name := range v6AbsenceSiteFiles {
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("reading %s: %v\nThis file is one of the two attach paths #868 had "+
				"to change. If it was renamed, rename it in v6AbsenceSiteFiles too — "+
				"do not drop it.", name, err)
		}
		lines := strings.Split(string(body), "\n")

		found := 0
		for i, line := range lines {
			if !strings.Contains(line, v6AbsenceAcquireCall) {
				continue
			}
			found++
			total++

			end := min(i+1+v6AbsenceWindow, len(lines))
			window := strings.Join(lines[i+1:end], "\n")
			if !strings.Contains(window, v6AbsenceConsult) {
				t.Errorf("%s:%d acquires a lease and does not consult %s within %d lines.\n"+
					"On a stateless or SLAAC IPv6 segment this site fails the endpoint and "+
					"no container starts on it — the defect #868 is about, still present on "+
					"whichever attach mode this file serves.\n%s",
					name, i+1, v6AbsenceConsult, v6AbsenceWindow, window)
			}
		}
		if found == 0 {
			t.Errorf("%s contains no %q at all. Either the acquisition moved — in which "+
				"case this test is now checking nothing and must follow it — or the "+
				"attach path lost its DHCP acquisition entirely.", name, v6AbsenceAcquireCall)
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	named := map[string]bool{}
	for _, n := range v6AbsenceSiteFiles {
		named[n] = true
	}
	for _, n := range v6AbsenceV4OnlySites {
		named[n] = true
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if named[name] {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if strings.Contains(string(body), v6AbsenceAcquireCall) {
			t.Errorf("%s acquires a lease and is in neither v6AbsenceSiteFiles nor "+
				"v6AbsenceV4OnlySites, so nothing checked whether it tolerates a segment "+
				"that offers no DHCPv6. Add it to the first list — after making it consult "+
				"%s — or, if it can only ever acquire IPv4, to the second.", name, v6AbsenceConsult)
		}
	}

	if total < len(v6AbsenceSiteFiles) {
		t.Errorf("found %d acquisition site(s) across %v, want at least one per file — "+
			"a universal over an empty set is not a check",
			total, v6AbsenceSiteFiles)
	}
}
