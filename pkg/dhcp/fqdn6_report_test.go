// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/wire"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// fqdnOpts6 is a register_dns v6 endpoint named web1, the one reportFQDN6 speaks for (#1029).
func fqdnOpts6() *DHCPClientOptions {
	return &DHCPClientOptions{V6: true, FQDN: "both", Hostname: "web1"}
}

func boundWith(f *wire.ClientFQDN) lease.Event {
	ev := lease.Event{Kind: lease.Acquired, Lease: lease.Lease{Addr: netip.MustParsePrefix("fd00:6470:6865::61/128")}}
	if f != nil {
		ev.Lease.FQDN, ev.Lease.HasFQDN = *f, true
	}
	return ev
}

func captureLog(t *testing.T) *logtest.Hook {
	t.Helper()
	hook := logtest.NewLocal(log.StandardLogger())
	level := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetLevel(level); hook.Reset() })
	return hook
}

// fqdnEntries keeps the entries reportFQDN6 wrote, told apart by their hostname field.
func fqdnEntries(hook *logtest.Hook) []log.Entry {
	var out []log.Entry
	for _, e := range hook.AllEntries() {
		if e.Data["hostname"] == "web1" {
			out = append(out, *e)
		}
	}
	return out
}

func TestReportFQDN6_SaysWhatTheServerAnswered(t *testing.T) {
	cases := []struct {
		name    string
		opts    *DHCPClientOptions
		ev      lease.Event
		want    log.Level
		wantMsg string
		flags   string
		sON     [3]bool
	}{
		{"S=1: the server registers the AAAA", fqdnOpts6(),
			boundWith(&wire.ClientFQDN{Flags: wire.ClientFQDNFlagS, Name: "web1.dh6.test."}),
			log.InfoLevel, "registers the AAAA", "0x01", [3]bool{true, false, false}},
		{"S=1 O=1: registers, overriding", fqdnOpts6(),
			boundWith(&wire.ClientFQDN{Flags: wire.ClientFQDNFlagS | wire.ClientFQDNFlagO, Name: "web1."}),
			log.InfoLevel, "registers the AAAA", "0x03", [3]bool{true, true, false}},
		{"S=0: nobody registers", fqdnOpts6(),
			boundWith(&wire.ClientFQDN{Name: "web1"}), log.WarnLevel, "without the S flag", "0x00", [3]bool{}},
		{"N=1: nobody registers", fqdnOpts6(),
			boundWith(&wire.ClientFQDN{Flags: wire.ClientFQDNFlagN, Name: "web1"}),
			log.WarnLevel, "without the S flag", "0x04", [3]bool{false, false, true}},
		{"no option 39 in the Reply", fqdnOpts6(), boundWith(nil), log.InfoLevel, "carried no Client FQDN", "", [3]bool{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := captureLog(t)
			tc.opts.reportFQDN6(tc.ev)
			got := fqdnEntries(hook)
			if len(got) != 1 {
				t.Fatalf("%d log entries, want one: the bind is where the server's answer is reported (RFC 4704 "+
					"section 4.1)", len(got))
			}
			if got[0].Level != tc.want || !strings.Contains(got[0].Message, tc.wantMsg) {
				t.Errorf("logged %s %q, want %s containing %q", got[0].Level, got[0].Message, tc.want, tc.wantMsg)
			}
			if tc.flags == "" {
				return
			}
			if got[0].Data["fqdn_flags"] != tc.flags {
				t.Errorf("fqdn_flags = %v, want %s", got[0].Data["fqdn_flags"], tc.flags)
			}
			for i, k := range []string{"fqdn_s", "fqdn_o", "fqdn_n"} {
				if got[0].Data[k] != tc.sON[i] {
					t.Errorf("%s = %v, want %v for flags %s", k, got[0].Data[k], tc.sON[i], tc.flags)
				}
			}
		})
	}
}

func renewedWith(f *wire.ClientFQDN) lease.Event {
	ev := boundWith(f)
	ev.Kind = lease.Renewed
	return ev
}

// resumedOpts6 is fqdnOpts6 restarted from a remembered lease, so its Acquired answers a Confirm (#1029).
func resumedOpts6() *DHCPClientOptions {
	o := fqdnOpts6()
	o.Resume = &lease.Lease{Addr: netip.MustParsePrefix("fd00:6470:6865::61/128")}
	return o
}

func TestReportFQDN6_StaysQuietWhereNoNameWasSent(t *testing.T) {
	cases := []struct {
		name string
		opts *DHCPClientOptions
		ev   lease.Event
	}{
		{"no register_dns", &DHCPClientOptions{V6: true, Hostname: "web1"}, boundWith(nil)},
		{"no hostname", &DHCPClientOptions{V6: true, FQDN: "both"}, boundWith(nil)},
		{"a v4 endpoint", &DHCPClientOptions{FQDN: "both", Hostname: "web1"}, boundWith(nil)},
		{"a resumed binding's Confirm answer", resumedOpts6(), boundWith(nil)},
		{"a lease change", fqdnOpts6(), func() lease.Event { e := boundWith(nil); e.Kind = lease.Changed; return e }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := captureLog(t)
			tc.opts.reportFQDN6(tc.ev)
			if got := hook.AllEntries(); len(got) != 0 {
				t.Errorf("logged %q; no message that carried option 39 was answered here", got[0].Message)
			}
		})
	}
}

// The CI capture of #1105: a Confirm-resumed Acquired, then the early Renew whose Reply carries option 39 (#1029).
func TestReportFQDN6_ReportsTheFirstAnswerOnce(t *testing.T) {
	s := &wire.ClientFQDN{Flags: wire.ClientFQDNFlagS, Name: "web1.dh6.test."}
	cases := []struct {
		name    string
		opts    *DHCPClientOptions
		evs     []lease.Event
		wantMsg string
	}{
		{"Confirm, then the Renew's answer", resumedOpts6(), []lease.Event{boundWith(nil), renewedWith(s), renewedWith(s)},
			"registers the AAAA"},
		{"Confirm, then a Renew answered without it", resumedOpts6(),
			[]lease.Event{boundWith(nil), renewedWith(nil), renewedWith(s)}, "carried no Client FQDN"},
		{"a resumed binding re-solicited", resumedOpts6(), []lease.Event{boundWith(s), renewedWith(nil)},
			"registers the AAAA"},
		{"the Request's answer, then renewals", fqdnOpts6(),
			[]lease.Event{boundWith(nil), renewedWith(s), renewedWith(nil)}, "carried no Client FQDN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := captureLog(t)
			for _, ev := range tc.evs {
				tc.opts.reportFQDN6(ev)
			}
			got := fqdnEntries(hook)
			if len(got) != 1 || !strings.Contains(got[0].Message, tc.wantMsg) {
				t.Fatalf("%d entries (%v), want one containing %q: the first answer to option 39 is reported once",
					len(got), got, tc.wantMsg)
			}
		})
	}
}

// Both v6 loops report: the one-shot acquisition at CreateEndpoint and the resumed persistent client's early Renew
// (#1029).
func TestReportFQDN6_BothLoopsReportTheBind(t *testing.T) {
	answer := &wire.ClientFQDN{Flags: wire.ClientFQDNFlagS, Name: "web1.dh6.test."}

	t.Run("the persistent client", func(t *testing.T) {
		hook := captureLog(t)
		c, _ := advertWatch(t)
		c.opts = *fqdnOpts6()
		c.opts.Resume = resumedOpts6().Resume
		src := make(chan lease.Event, 2)
		c.src, c.events = src, newEventChan()
		src <- boundWith(nil)
		src <- renewedWith(answer)
		close(src)
		c.translate()
		if got := fqdnEntries(hook); len(got) != 1 || got[0].Data["fqdn_name"] != answer.Name {
			t.Errorf("the persistent client's early Renew logged %d option 39 entries, want one naming %q", len(got),
				answer.Name)
		}
	})

	t.Run("the one-shot acquisition", func(t *testing.T) {
		hook := captureLog(t)
		client := &fakeV6Client{events: make(chan lease.Event, 1)}
		client.events <- boundWith(answer)
		_, _, err := acquisition6Result(t, context.Background(), client, fqdnOpts6(), netip.Addr{},
			3*time.Second, 10*time.Second)
		if err != nil {
			t.Fatalf("runAcquisition6: %v", err)
		}
		if got := fqdnEntries(hook); len(got) != 1 || got[0].Data["fqdn_name"] != answer.Name {
			t.Errorf("the one-shot's bind logged %d option 39 entries, want one naming %q", len(got), answer.Name)
		}
	})
}
