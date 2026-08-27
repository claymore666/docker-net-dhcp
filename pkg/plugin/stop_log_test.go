// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// TestStop_NoStopPathClaimsAReclaimOrRelease pins the operator's view of
// a stop, which is the one thing about #800 that no other test in this
// package can see.
//
// The ledger and the counters were both corrected when the release
// paths went. The LOG LINE was not: it went on telling an operator
// "Persistent client stopped before it ever held the lease; reclaiming
// it" after the reclaim it named had been deleted. Nothing failed —
// the ledger was right, the counters were right, and the sentence a
// human reads was false. That is the shape prose decays in, and the
// only reason it was found is that someone grepped for the word rather
// than for a renamed symbol.
//
// # What is scanned, and why it is not just the message
//
// The haystack is the message PLUS every field key and value. Operators
// do not read logrus's Message; they read the rendered line, and in a
// JSON pipeline the fields are the only thing anyone greps. A first
// version of this test scanned e.Message alone and passed against a
// mutant that added WithField("lease_action", "reclaimed") to the very
// line under test, on the very path under test.
//
// # What is driven, and why more than one path
//
// Every arm of settleFamily plus the startErr early return — five rows.
// Two of the three places that ever reclaimed a lease sit OUTSIDE the
// never-bound path (the startErr block says so in its own comment), so
// a test that drove only that path left them unguarded: mutants that
// added a reclaim claim to the bound-clean, hard-exit and start-failed
// paths all survived it, and survived `go test ./...` entire.
//
// # Non-vacuity, per row
//
// A row that expects a report names the substring that identifies it
// and requires exactly one — "the capture is non-empty" is not enough,
// because a stop emits other lines and the first version of this test
// PASSED with the line under test deleted. Rows whose path logs nothing
// name no anchor: requiring one there would fail them for the wrong
// reason, and the claim scan is still not vacuous against these
// mutants, because a mutant ADDS a line rather than removing one.
func TestStop_NoStopPathClaimsAReclaimOrRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		// anchor identifies the report this path is expected to emit,
		// by the CONDITION it describes rather than by the clause under
		// test. Empty means the path logs nothing and none is required.
		anchor  string
		leaving bool
		wantErr bool
		mk      func(t *testing.T, p *Plugin) *dhcpManager
	}{
		{
			name:    "never_bound_leaving",
			anchor:  "held the lease",
			leaving: true,
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				m := stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
				m.boundV4.Store(false)
				return m
			},
		},
		{
			name:    "never_bound_not_leaving",
			anchor:  "held the lease",
			leaving: false,
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				m := stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
				m.boundV4.Store(false)
				return m
			},
		},
		{
			// settleFamily's default arm: audits "stopped" and logs
			// nothing. A mutant that starts claiming a release here is
			// exactly the regression #800 must not grow back.
			name:    "bound_clean_stop",
			leaving: true,
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				return stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
			},
		},
		{
			// case exitErr != nil: bumps client_stop_failures and audits
			// "stop_failed". Says nothing about the lease, and must not
			// start to.
			name:    "bound_hard_exit",
			leaving: true,
			wantErr: true,
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				return stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true},
					errors.New("signal: killed"), nil)
			},
		},
		{
			// The other historical reclaim site. Its own comment records
			// that it used to hand the one-shot's lease back.
			name:    "start_failed",
			anchor:  "outstanding",
			leaving: true,
			mk:      failedStartManager,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The not-leaving report is Debug; without this it is not
			// captured and the anchor check fires instead of the claim
			// under test.
			prev := log.GetLevel()
			log.SetLevel(log.DebugLevel)
			t.Cleanup(func() { log.SetLevel(prev) })

			hook := logtest.NewLocal(log.StandardLogger())
			defer hook.Reset()

			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)
			m := tc.mk(t, p)

			var err error
			if tc.leaving {
				err = m.StopForLeave()
			} else {
				err = m.Stop()
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("stop(leaving=%v) = %v, wantErr=%v", tc.leaving, err, tc.wantErr)
			}

			entries := hook.AllEntries()

			// The rendered line, not logrus's Message: fields are what a
			// JSON log pipeline greps, and a claim can hide in one.
			rendered := make([]string, 0, len(entries))
			for _, e := range entries {
				var b strings.Builder
				b.WriteString(e.Message)
				for k, v := range e.Data {
					fmt.Fprintf(&b, " %s=%v", k, v)
				}
				rendered = append(rendered, b.String())
			}

			if tc.anchor != "" {
				var found []string
				for _, r := range rendered {
					if strings.Contains(strings.ToLower(r), tc.anchor) {
						found = append(found, r)
					}
				}
				if len(found) != 1 {
					t.Fatalf("found %d line(s) matching the %q report in %d log entries, want exactly 1: %q.\n"+
						"  An operator gets no record of this stop, and nothing below this line "+
						"is being checked.", len(found), tc.anchor, len(entries), found)
				}
			}

			for _, r := range rendered {
				lower := strings.ToLower(r)
				for _, claim := range []string{"reclaim", "releas"} {
					if strings.Contains(lower, claim) {
						t.Errorf("a stop logged %q, which tells an operator the lease was "+
							"%sed. Since #800 nothing this plugin runs reclaims or releases "+
							"a lease, on any path — the address is left to expire on the "+
							"server's clock", r, claim)
					}
				}
			}
		})
	}
}
