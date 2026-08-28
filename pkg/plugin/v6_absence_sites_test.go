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
			t.Errorf("%s acquires a lease and is not in v6AbsenceSiteFiles, so nothing "+
				"checked whether it tolerates a segment that offers no DHCPv6. Add it to "+
				"the list — after making it consult %s.", name, v6AbsenceConsult)
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
