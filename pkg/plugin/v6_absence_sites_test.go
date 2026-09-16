// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The attach path exists twice — once for bridge networks in
// network.go, once for parent-attached (macvlan / ipvlan) networks in
// parent_attached.go — and #868's tolerance had to be added to both.
//
// Nothing but this test says so. The two sites are structurally
// identical and neither compiles against the other, so a later change
// that touches one and not the other produces a plugin where the same
// stateless network works on a bridge and refuses every container on a
// macvlan. The integration fixture is a bridge, so that asymmetry ships
// green: this is the copy the fix does not reach.
//
// It reads source rather than behaviour because the behaviour needs a
// netns, a parent NIC and a DHCP server. A source check is the weaker
// instrument, and it is the one available at this level; the strong
// evidence is the integration lane, which covers exactly one of the two
// files.
// KEYED ON THE PROPERTY, NOT ON A SPELLING. The subject is "a site
// that acquires a lease", and the only thing that makes a line one is
// the call itself -- not what the caller happened to name its context.
//
// MEASURED: this const used to read "p.acquireWithPolicy(ctx," and a
// third site spelled `p.acquireWithPolicy(reqCtx,` with NO classifier
// consult passed the whole test, in the per-file loop and in the sweep
// alike. That is the defect this file exists to catch, reproduced by
// the gate meant to catch it: a site the fix does not reach, invisible
// because it renamed one variable.
const (
	v6AbsenceAcquireCall = "p.acquireWithPolicy("
	v6AbsenceConsult     = "p.noteV6Absence("
	// Lines to read after the call before concluding the site does not
	// consult the classifier. The two real sites answer within six; the
	// margin is for a reworded error, not for a different structure.
	v6AbsenceWindow = 12
)

// v6AbsenceSiteFiles is the population, named rather than discovered.
//
// A glob would make this test satisfiable by deleting a file: the
// universal "every site consults the classifier" is true of no sites at
// all. Naming them means removing one is a change to this list, made on
// purpose, by someone who read this comment.
var v6AbsenceSiteFiles = []string{"network.go", "parent_attached.go"}

// v6AbsenceV4OnlySites is the second population: files that acquire a
// lease and can never acquire a v6 one, so the classifier has nothing to
// classify for them.
//
// AN EXEMPTION WITH A MECHANICAL PROOF, not a waiver. A site is only in
// here if every one of its acquisitions passes the LITERAL false for
// acquireWithPolicy's v6 argument, which is checked below: a v6-capable
// call cannot hide in this list, because the moment the argument stops
// being that literal the site fails here instead of being excused. The
// IPAM driver's reserve is the one member -- the driver is IPv4-only in
// v2.1.0, and RequestPool refuses an IPv6 pool naming v2.2.0 -- and when
// v6 arrives it moves to the list above, which is the change this
// structure forces someone to make on purpose.
var v6AbsenceV4OnlySites = []string{"ipam_reserve.go"}

// v6AbsenceV4OnlyCall is what a v4-only acquisition looks like: the v6
// argument spelled as the literal false. It is the fourth argument, and
// the three before it are matched loosely because their spelling is the
// caller's business -- the same lesson v6AbsenceAcquireCall records.
const v6AbsenceV4OnlyCall = ", false,"

// TestV6Absence_V4OnlySitesCannotAcquireV6 is the proof behind the
// exemption list. Without it, adding a file to v6AbsenceV4OnlySites
// would be a way to opt out of the classifier -- the exact thing the
// named populations above exist to prevent.
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

	// The named list is a floor, not the whole judgement: a THIRD
	// attach path added later would be judged by nothing at all if the
	// list were the only source. So the tree is swept for the same call
	// and anything it finds outside the list is a failure here rather
	// than a discovery in production.
	//
	// The two directions do different work and both are needed. The
	// list stops the domain being emptied by deleting a file; the sweep
	// stops it being outgrown by adding one.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	named := map[string]bool{}
	for _, n := range v6AbsenceSiteFiles {
		named[n] = true
	}
	// The v4-only sites are named too, and they are not a hole: their
	// own test above proves each one cannot acquire a v6 lease at all.
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

	// The non-vacuity guard. Both files existing and both containing a
	// call is already asserted above; this catches the case where the
	// call spelling changes everywhere at once, which would leave every
	// loop above with nothing to judge and this test green.
	if total < len(v6AbsenceSiteFiles) {
		t.Errorf("found %d acquisition site(s) across %v, want at least one per file — "+
			"a universal over an empty set is not a check",
			total, v6AbsenceSiteFiles)
	}
}
