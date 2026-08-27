// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"strings"
	"sync/atomic"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// TestStop_TheNeverBoundLogClaimsNoReclaimOrRelease pins the operator's
// view of a stop, which is the one thing about #800 that no other test
// in this package can see.
//
// The ledger and the counters were both corrected when the release
// paths went. The LOG LINE was not: it went on telling an operator
// "Persistent client stopped before it ever held the lease; reclaiming
// it" after the reclaim it named had been deleted. Nothing failed —
// the ledger was right, the counters were right, and the sentence a
// human reads was false. That is the shape prose decays in, and the
// only reason it was found here is that someone grepped for the word
// rather than for a renamed symbol.
//
// So this asserts on the words. A stop must not tell anyone that a
// lease was reclaimed or released, because since #800 neither ever
// happens, on any path. The check refuses an empty capture: a stop that
// logs nothing at all would otherwise satisfy "no line claims a
// release" without any line existing.
func TestStop_TheNeverBoundLogClaimsNoReclaimOrRelease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		leaving bool
	}{
		{name: "leaving", leaving: true},
		{name: "not_leaving", leaving: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The not-leaving line is Debug; without this the capture
			// is empty and the refusal below fires rather than the
			// claim under test.
			prev := log.GetLevel()
			log.SetLevel(log.DebugLevel)
			t.Cleanup(func() { log.SetLevel(prev) })

			hook := logtest.NewLocal(log.StandardLogger())
			defer hook.Reset()

			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)
			m := stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
			m.boundV4.Store(false)

			var err error
			if tc.leaving {
				err = m.StopForLeave()
			} else {
				err = m.Stop()
			}
			if err != nil {
				t.Fatalf("stop(leaving=%v) = %v, want nil", tc.leaving, err)
			}

			entries := hook.AllEntries()

			// Locate the never-bound report itself, and refuse to
			// proceed without it. "The stop logged SOMETHING" is not
			// enough: a stop emits other lines, so deleting the line
			// under test leaves the capture non-empty and every claim
			// check below vacuously true. Measured — with the line
			// removed and this block written as len(entries) == 0, the
			// test still passed.
			//
			// The report is found by the condition it describes, not by
			// the clause under test. If a deliberate rewording breaks
			// this, that is the right outcome: the sentence an operator
			// reads about a client signalled before it bound is exactly
			// what this test exists to hold still.
			var report []string
			for _, e := range entries {
				if strings.Contains(strings.ToLower(e.Message), "held the lease") {
					report = append(report, e.Message)
				}
			}
			if len(report) != 1 {
				t.Fatalf("found %d never-bound report(s) in %d log entries, want exactly 1: %q.\n"+
					"  An operator gets no record of a client that was signalled before it "+
					"ever bound, and nothing below this line is being checked.", len(report), len(entries), report)
			}
			for _, e := range entries {
				lower := strings.ToLower(e.Message)
				for _, claim := range []string{"reclaim", "releas"} {
					if strings.Contains(lower, claim) {
						t.Errorf("a stop logged %q, which tells an operator the lease was "+
							"%sed. Since #800 nothing this plugin runs reclaims or releases "+
							"a lease, on any path — the one-shot's address is left to expire "+
							"on the server's clock", e.Message, claim)
					}
				}
			}
		})
	}
}
