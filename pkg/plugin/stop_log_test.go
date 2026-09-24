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

func TestStop_NoStopPathClaimsAReclaimOrRelease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		anchor   string
		releases bool
		leaving  bool
		wantErr  bool
		mk       func(t *testing.T, p *Plugin) *dhcpManager
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
			name:    "bound_clean_stop",
			leaving: true,
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				return stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
			},
		},
		{
			name:    "bound_hard_exit",
			leaving: true,
			wantErr: true,
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				return stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true},
					errors.New("signal: killed"), nil)
			},
		},
		{
			name:    "start_failed",
			anchor:  "outstanding",
			leaving: true,
			mk:      failedStartManager,
		},
		{
			name:     "release_lease_on_stop_leaving",
			anchor:   "handing this endpoint's lease back",
			releases: true,
			leaving:  true,
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				installSender(t, nil)
				return releasingManager(t, p, ReleaseOnStop, false)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			if tc.releases {
				return
			}

			for _, r := range rendered {
				lower := strings.ToLower(r)
				for _, claim := range []string{"reclaim", "releas"} {
					if strings.Contains(lower, claim) {
						t.Errorf("a stop logged %q, which tells an operator the lease was "+
							"%sed. On a network that does not set release_lease — the "+
							"default, and every network before #962 — nothing this plugin "+
							"runs reclaims or releases a lease on any path, and the address "+
							"is left to expire on the server's clock (#800)", r, claim)
					}
				}
			}
		})
	}
}
