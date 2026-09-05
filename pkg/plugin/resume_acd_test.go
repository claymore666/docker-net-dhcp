// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// D23's operator half, driven in BOTH directions.
//
// The M6b review measured what one direction is worth here: the warning
// had no observer at all, and the inverted-guard mutant — warn on a
// clean resume, stay silent on a half-checked one — survived the whole
// suite. A test that only drove the half-checked case would still pass
// under it, because that mutant's failure is a line that is NOT emitted
// on the other input.
//
// Two witnesses per row, and deliberately not one: the log line, which
// is what an operator greps, and the acd_resumed_unchecked check in
// /Plugin.Health, which is what a poller reads. They are written at one
// place and read at two, so a fix applied to either alone is visible.
func TestResumedACD_WarnsOnAHalfCheckedResumeAndIsSilentOnACleanOne(t *testing.T) {
	l := lease.Lease{Addr: netip.MustParsePrefix("192.0.2.44/24")}

	for _, tc := range []struct {
		name   string
		resume dhcp.Resumption
		mode   proto.ConflictMode
		want   bool
	}{
		{"probing", dhcp.Resumption{Lease: &l, ACD: proto.ACDProbing}, proto.ConflictAsync, true},
		{"settling", dhcp.Resumption{Lease: &l, ACD: proto.ACDSettling}, proto.ConflictAsync, true},
		{"announcing", dhcp.Resumption{Lease: &l, ACD: proto.ACDAnnouncing}, proto.ConflictAsync, false},
		{"defending", dhcp.Resumption{Lease: &l, ACD: proto.ACDDefending}, proto.ConflictWait, false},
		{"idle-because-off", dhcp.Resumption{Lease: &l, ACD: proto.ACDIdle}, proto.ConflictOff, false},
		{"nothing-to-resume", dhcp.Resumption{ACD: proto.ACDProbing}, proto.ConflictWait, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newHealthPlugin()
			m := newDHCPManager(nil, JoinRequest{EndpointID: "ep1", NetworkID: "net1"}, DHCPNetworkOptions{})
			m.plugin = p

			hook := logtest.NewLocal(log.StandardLogger())
			defer hook.Reset()

			m.noteResumedACD(tc.resume, tc.mode, false)

			warned := 0
			for _, e := range hook.AllEntries() {
				if strings.Contains(e.Message, "RFC 5227 check had not completed") {
					warned++
				}
			}
			if tc.want && warned != 1 {
				t.Errorf("%d resume warnings, want exactly 1", warned)
			}
			if !tc.want && warned != 0 {
				t.Errorf("%d resume warnings on a resume nothing was left unchecked on, want 0", warned)
			}

			h := p.healthSnapshot()
			wantN := int32(0)
			if tc.want {
				wantN = 1
			}
			if h.ACDResumedUnchecked != wantN {
				t.Errorf("acd_resumed_unchecked = %d, want %d", h.ACDResumedUnchecked, wantN)
			}
			c := onlyCheck(t, h, "acd_resumed_unchecked")
			wantStatus := statusPass
			if tc.want {
				wantStatus = statusWarn
			}
			if c.Status != wantStatus {
				t.Errorf("check acd_resumed_unchecked = %q, want %q", c.Status, wantStatus)
			}
			if h.Healthy != true {
				t.Error("a half-checked resume made the plugin unhealthy; the address is re-checked, so it is a warning")
			}
		})
	}
}

// The warning names the address, the phase it stopped in and the mode it
// ran under. Without the phase an operator cannot tell a probe that was
// still going from a record this build could not read, and without the
// mode `probing` and `idle` are not comparable at all.
func TestResumedACD_WarningCarriesTheFieldsThatMakeItActionable(t *testing.T) {
	p := newHealthPlugin()
	m := newDHCPManager(nil, JoinRequest{EndpointID: "ep1", NetworkID: "net1"}, DHCPNetworkOptions{})
	m.plugin = p

	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	l := lease.Lease{Addr: netip.MustParsePrefix("192.0.2.44/24")}
	m.noteResumedACD(dhcp.Resumption{Lease: &l, ACD: proto.ACDProbing}, proto.ConflictAsync, false)

	var found *log.Entry
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "RFC 5227 check had not completed") {
			found = e
		}
	}
	if found == nil {
		t.Fatal("no resume warning")
	}
	for _, c := range []struct {
		field string
		want  string
	}{
		{"address", "192.0.2.44"},
		{"acd_phase", "probing"},
		{"conflict_check", "async"},
	} {
		got, ok := found.Data[c.field]
		if !ok {
			t.Errorf("the warning carries no %s field", c.field)
			continue
		}
		if s := strings.TrimSpace(strings.Trim(fmtValue(got), `"`)); s != c.want {
			t.Errorf("%s = %q, want %q", c.field, s, c.want)
		}
	}
}

func fmtValue(v interface{}) string {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
