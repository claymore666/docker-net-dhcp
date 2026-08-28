// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dispatcher is the one function allowed to launch the conflict probe.
const dispatcher = "dispatchConflictProbe"

// TestConflictProbe_EveryDispatchGoesThroughTheDispatcher holds the line
// for #881 at the source.
//
// The census the health floor runs is only as good as its DISPATCHED
// count, and that count is taken in exactly one place. A new call site
// that writes the obvious thing —
//
//	go p.checkAddressConflict(opts.Parent, addr, mac, epID, netID)
//
// — compiles, runs, and probes perfectly correctly. It just never tells
// anyone it did. The floor then sees zero dispatches, reads that as the
// honest empty case, and goes quiet: the exact shape of the defect this
// whole issue is about, reintroduced by an edit that looks right.
//
// Static rather than behavioural, for the reason the counter-window
// guard already gives: the property is textual, and reproducing the
// fault behaviourally would need a shard whose partition happens to
// isolate the new call site.
//
// The guard bounds itself rather than claiming completeness. It sees
// `go p.checkAddressConflict(` written literally in this package. It
// does NOT see a dispatch through a function value, a method expression,
// a reflective call, or a call added in another package — and cannot;
// the finding for that lives in the floor's census, which compares
// settlements against dispatches in the log and fires when a probe
// settles that nothing dispatched.
func TestConflictProbe_EveryDispatchGoesThroughTheDispatcher(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no *.go found; this guard would pass vacuously")
	}

	// Drive the absence: the literal this guard hunts for must exist
	// somewhere it is ALLOWED, or the search itself proves nothing.
	sawDispatcher := false

	const banned = "go p.checkAddressConflict("
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		checked++
		src := string(b)
		if strings.Contains(src, "func (p *Plugin) "+dispatcher+"(") {
			sawDispatcher = true
		}
		if !strings.Contains(src, banned) {
			continue
		}
		// The dispatcher itself is the one legitimate launcher.
		if strings.Contains(src, "func (p *Plugin) "+dispatcher+"(") {
			continue
		}
		t.Errorf("%s launches the conflict probe directly with %q.\n"+
			"Use p.%s(...) instead: it takes the dispatched count synchronously, "+
			"and without it the health floor's census sees no probe was ever "+
			"dispatched and passes vacuously (#881).", f, banned, dispatcher)
	}

	if checked == 0 {
		t.Fatal("no non-test sources checked; this guard would pass vacuously")
	}
	if !sawDispatcher {
		t.Fatalf("did not find %q in any source file. Either it was renamed — in which "+
			"case this guard is now watching a name nothing uses and would pass over "+
			"every real bypass — or the search is broken.", dispatcher)
	}
}
