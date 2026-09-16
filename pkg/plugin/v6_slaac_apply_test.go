// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func mustParseAddr(t *testing.T, cidr string) *netlink.Addr {
	t.Helper()
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", cidr, err)
	}
	return a
}

// EVERY ADDRESS TAKES ITS LIFETIMES FROM ITS OWN ADVERTISEMENT.
// Defeat row 6 of the #818 list, and the reason it is a row: the lease
// carries an aggregate pair as well, and reading it here is both easy
// and wrong. The library's Deadlines() makes Lease.Expire the LONGEST
// valid lifetime across the addresses it holds and its preferred
// deadline the SHORTEST preferred, so on a link advertising a
// unique-local prefix for an hour and a global one for five minutes,
// an aggregate-fed install gives the five-minute address an hour to
// live: it stays on the link, and stays PREFERRED, long after the
// router stopped saying it exists. Nothing fails, and the container
// keeps choosing a source address from a prefix that is no longer
// advertised.
//
// Both addresses also carry IFA_F_NODAD, for the reason
// TestV6AddrAttrs_TurnsOffDuplicateAddressDetection gives: the library
// ran RFC 4862 section 5.4's check on each of them before it reported
// the lease.
func TestV6WantedAddrs_EachAddressCarriesItsOwnLifetimes(t *testing.T) {
	main := mustParseAddr(t, "2001:db8:1::a/64")
	info := dhcp.Info{
		IP:               "2001:db8:1::a/64",
		LeaseSeconds:     3600,
		PreferredSeconds: 1800,
		SLAAC:            true,
		Addrs: []dhcp.V6Addr{
			{IP: "2001:db8:1::a/64", ValidSeconds: 3600, PreferredSeconds: 1800},
			{IP: "fd00:db8:2::a/64", ValidSeconds: 300, PreferredSeconds: 120},
		},
	}

	want, err := v6WantedAddrs(main, info)
	if err != nil {
		t.Fatalf("v6WantedAddrs: %v", err)
	}
	if len(want) != 2 {
		t.Fatalf("a two-prefix lease produced %d addresses: %v", len(want), want)
	}
	byKey := map[string]*netlink.Addr{}
	for _, w := range want {
		byKey[w.key] = w.addr
	}
	for _, tc := range []struct {
		key       string
		valid     uint32
		preferred uint32
	}{
		{"2001:db8:1::a/64", 3600, 1800},
		{"fd00:db8:2::a/64", 300, 120},
	} {
		got, ok := byKey[tc.key]
		if !ok {
			t.Fatalf("%s is not in the install set: %v", tc.key, byKey)
		}
		if got.ValidLft != int(tc.valid) || got.PreferedLft != int(tc.preferred) {
			t.Errorf("%s got ValidLft=%d PreferedLft=%d, want %d/%d -- these are the "+
				"other address's numbers, or the lease's aggregate",
				tc.key, got.ValidLft, got.PreferedLft, tc.valid, tc.preferred)
		}
		if got.Flags&unix.IFA_F_NODAD == 0 {
			t.Errorf("%s reaches the kernel without IFA_F_NODAD", tc.key)
		}
	}
}

// The address Docker was told about is applied first.
//
// It matters on one path and only one: an AddrReplace that fails takes
// the whole renewal with it, and if one of several is going to fail,
// the address `docker inspect` already shows is the one worth having on
// the link. The order is asserted rather than left to the lease's,
// because the library's list order is the router's advertisement order
// and ipv6_main_prefix exists precisely because that order is not the
// operator's choice.
func TestV6WantedAddrs_AppliesTheReportedAddressFirst(t *testing.T) {
	main := mustParseAddr(t, "2001:db8:1::a/64")
	info := dhcp.Info{
		IP: "2001:db8:1::a/64",
		Addrs: []dhcp.V6Addr{
			{IP: "fd00:db8:2::a/64", ValidSeconds: 300, PreferredSeconds: 120},
			{IP: "fd00:db8:3::a/64", ValidSeconds: 300, PreferredSeconds: 120},
			{IP: "2001:db8:1::a/64", ValidSeconds: 3600, PreferredSeconds: 1800},
		},
	}
	want, err := v6WantedAddrs(main, info)
	if err != nil {
		t.Fatalf("v6WantedAddrs: %v", err)
	}
	if len(want) != 3 {
		t.Fatalf("got %d addresses, want 3", len(want))
	}
	if want[0].key != "2001:db8:1::a/64" {
		t.Errorf("the first address applied is %s; Docker reports %s for this endpoint",
			want[0].key, info.IP)
	}
}

// A lease that carries no list still installs its address.
//
// Info.Addrs is empty for every DHCPv4 lease, for a DHCPv6 lease from a
// server that granted one address, and for any Info a caller builds by
// hand. A function that returned nothing for those would silently stop
// installing addresses on the paths that have worked all along, and no
// v6 test naming SLAAC would notice.
func TestV6WantedAddrs_ALeaseWithNoListInstallsItsOwnAddress(t *testing.T) {
	main := mustParseAddr(t, "2001:db8:1::a/128")
	main.ValidLft, main.PreferedLft = 7200, 3600

	want, err := v6WantedAddrs(main, dhcp.Info{IP: "2001:db8:1::a/128", LeaseSeconds: 7200})
	if err != nil {
		t.Fatalf("v6WantedAddrs: %v", err)
	}
	if len(want) != 1 || want[0].key != "2001:db8:1::a/128" {
		t.Fatalf("got %v, want the lease's single address", want)
	}
	if want[0].addr.ValidLft != 7200 || want[0].addr.PreferedLft != 3600 {
		t.Errorf("the caller's lifetimes were overwritten: ValidLft=%d PreferedLft=%d",
			want[0].addr.ValidLft, want[0].addr.PreferedLft)
	}
}

// An address the lease reports and netlink cannot parse fails the
// renewal instead of being skipped.
//
// Skipping it would install a SUBSET of the lease and then hand that
// subset to the set difference below, which would withdraw the rest of
// the container's addresses as though the lease had dropped them.
func TestV6WantedAddrs_RefusesAnAddressItCannotParse(t *testing.T) {
	main := mustParseAddr(t, "2001:db8:1::a/64")
	_, err := v6WantedAddrs(main, dhcp.Info{
		IP:    "2001:db8:1::a/64",
		Addrs: []dhcp.V6Addr{{IP: "2001:db8:1::a/64"}, {IP: "not-an-address"}},
	})
	if err == nil {
		t.Fatal("an unparseable address was accepted; the install set is now a subset of " +
			"the lease and everything missing from it is about to be withdrawn")
	}
	if !strings.Contains(err.Error(), "not-an-address") {
		t.Errorf("the error does not name the address that failed: %v", err)
	}
}

// THE ADDRESSES THAT LEAVE THE LEASE COME OFF THE LINK.
// Defeat row 4. Two things take an address out of a v6 lease and
// neither is a renewal onto a different address: a valid lifetime that
// ran out, and a router that stopped advertising the prefix it was
// formed from. "The address changed" is not a question with one answer
// on a link with two of them, so the manager keeps the set it installed
// and diffs it.
//
// The kernel's own lifetimes are a backstop and not this: a router that
// keeps refreshing the valid lifetime of a prefix it no longer
// advertises to THIS client leaves the address on the link for as long
// as the lease lives.
func TestV6AddrsToWithdraw_RemovesWhatTheLeaseNoLongerHolds(t *testing.T) {
	installed := func(keys ...string) map[string]*netlink.Addr {
		out := map[string]*netlink.Addr{}
		for _, k := range keys {
			out[k] = mustParseAddr(t, k)
		}
		return out
	}
	wanted := func(t *testing.T, keys ...string) []wantedV6Addr {
		t.Helper()
		var out []wantedV6Addr
		for _, k := range keys {
			out = append(out, wantedV6Addr{addr: mustParseAddr(t, k), key: k})
		}
		return out
	}

	for _, tc := range []struct {
		name      string
		installed map[string]*netlink.Addr
		want      []wantedV6Addr
		gone      []string
	}{
		{
			name:      "a steady renewal withdraws nothing",
			installed: installed("2001:db8:1::a/64", "fd00:db8:2::a/64"),
			want:      wanted(t, "2001:db8:1::a/64", "fd00:db8:2::a/64"),
			gone:      nil,
		},
		{
			name:      "a renumbering withdraws the old prefix",
			installed: installed("2001:db8:1::a/64"),
			want:      wanted(t, "2001:db8:9::a/64"),
			gone:      []string{"2001:db8:1::a/64"},
		},
		{
			name:      "one of two expires and the other is untouched",
			installed: installed("2001:db8:1::a/64", "fd00:db8:2::a/64"),
			want:      wanted(t, "2001:db8:1::a/64"),
			gone:      []string{"fd00:db8:2::a/64"},
		},
		{
			name:      "the lease holds nothing and every address goes",
			installed: installed("2001:db8:1::a/64", "fd00:db8:2::a/64"),
			want:      nil,
			gone:      []string{"2001:db8:1::a/64", "fd00:db8:2::a/64"},
		},
		{
			name:      "a first install withdraws nothing",
			installed: installed(),
			want:      wanted(t, "2001:db8:1::a/64"),
			gone:      nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := v6AddrsToWithdraw(tc.installed, tc.want)
			var keys []string
			for _, g := range got {
				keys = append(keys, g.key)
				if g.addr == nil {
					t.Errorf("%s is withdrawn with no address to hand AddrDel", g.key)
				}
			}
			if strings.Join(keys, ",") != strings.Join(tc.gone, ",") {
				t.Errorf("withdrawing %v, want %v", keys, tc.gone)
			}
		})
	}
}

// A FORMED ADDRESS THAT EXPIRES IS NOT A DHCP OUTAGE.
// Defeat row 11. `Lost{ReasonExpired}` on a SLAAC lease arrives as its
// own event type, and the event type is what decides which counter
// moves: routed through "leasefail" it would feed countOutageTick, so
// dhcp_timeouts -- the counter an operator alerts on to mean "the DHCP
// server stopped answering" -- would climb for a router that withdrew a
// prefix, with no DHCP server involved at any point in the endpoint's
// life.
//
// The address removal itself is guarded by netHandle/ctrLink, which are
// nil here, so this drives the counter semantics alone; the removal is
// TestV6AddrsToWithdraw's and the integration suite's.
func TestHandleEvent_ASLAACExpiryIsNotADHCPOutage(t *testing.T) {
	p := &Plugin{}
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"}, DHCPNetworkOptions{}).withPlugin(p)

	m.handleEvent(dhcp.Event{
		Type: "slaac_lost",
		Data: dhcp.Info{IP: "2001:db8:1::a/64", SLAAC: true},
	}, true)

	if got := p.dhcpTimeoutsV6.Load(); got != 0 {
		t.Errorf("dhcp_timeouts_v6 = %d after a formed address expired; that counter means "+
			"a DHCP server went quiet and this endpoint never spoke to one", got)
	}
	if got := p.dhcpServerPolicyTimeouts.Load(); got != 0 {
		t.Errorf("dhcp_server_policy_timeouts = %d, want 0", got)
	}

	// The other direction, or the assertion above is satisfied by an
	// event type nothing handles at all.
	m.handleEvent(dhcp.Event{Type: "leasefail"}, true)
	if got := p.dhcpTimeoutsV6.Load(); got != 1 {
		t.Errorf("dhcp_timeouts_v6 = %d after a leasefail, want 1: the counter this test "+
			"says must not move for slaac_lost does not move for anything", got)
	}
}

// The ledger says where an address came from.
//
// `bound` is otherwise the same row for an address a DHCP server handed
// out and one this plugin formed from an advertisement, and they are
// not the same event to anyone reading the file back: there is no lease
// on any server behind the second, so there is nothing to correlate it
// with and no server log it appears in.
//
// Both directions, because a `source` written on every row would be
// just as useless as one written on none.
func TestAudit_AFormedAddressSaysWhereItCameFrom(t *testing.T) {
	var failures atomic.Int32
	p := &Plugin{}
	p.ledger = newLeaseLedger(filepath.Join(t.TempDir(), ledgerFileName), &failures)
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"},
		DHCPNetworkOptions{AuditLog: true}).withPlugin(p)

	m.auditFrom("bound", bareIP("2001:db8:1::a/64"), auditSource(dhcp.Info{SLAAC: true}))
	m.auditFrom("bound", bareIP("2001:db8:9::b/128"), auditSource(dhcp.Info{}))
	m.auditFrom("withdrawn", bareIP("fd00:db8:2::a/64"), auditSource(dhcp.Info{SLAAC: true}))

	entries := readLedgerLines(t, p.ledger.path)
	if len(entries) != 3 {
		t.Fatalf("wrote %d ledger rows, want 3", len(entries))
	}
	if entries[0].Source != "slaac" {
		t.Errorf("a formed address bound with source=%q, want \"slaac\"", entries[0].Source)
	}
	if entries[1].Source != "" {
		t.Errorf("a server-granted address bound with source=%q; every row carrying a source "+
			"is the same as no row carrying one", entries[1].Source)
	}
	if entries[2].Kind != "withdrawn" || entries[2].IP != "fd00:db8:2::a" {
		t.Errorf("the withdrawal row is %+v; it has to name the address that left", entries[2])
	}
}

// An ipv6_main_prefix that matched nothing is counted once and named.
//
// THE COUNTER'S POPULATION IS ENDPOINTS, which is why it is bumped from
// the acquisition and not from the lease seam that computed the flag:
// every renewal crosses that seam, and a counter bumped there would
// report how often a client renewed. Read as endpoints, a value of 1 on
// a fleet means one network's option names a prefix its router does not
// advertise; read as renewals it means nothing at all.
//
// It is a warning and not a failure. The addresses are formed either
// way and the container has them; what is wrong is that `docker
// inspect` shows one the operator did not ask for, and a typo in an
// option must not take containers down.
func TestNoteMainPrefixFallback_CountsEndpointsAndNamesBothPrefixes(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	main := netip.MustParsePrefix("2001:db8:ffff::/48")
	p := &Plugin{}
	p.noteMainPrefixFallback(true, dhcp.Info{IP: "fd00:9::42/64", MainAddrFallback: true}, main, "abcdef0123456789")

	if got := p.ipv6MainPrefixUnmatched.Load(); got != 1 {
		t.Fatalf("ipv6_main_prefix_unmatched = %d, want 1", got)
	}
	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("want one log entry, got %d", len(entries))
	}
	if entries[0].Level != log.WarnLevel {
		t.Errorf("logged at %v, want warn", entries[0].Level)
	}
	if fmt.Sprint(entries[0].Data["ipv6_main_prefix"]) != main.String() ||
		entries[0].Data["address"] != "fd00:9::42/64" {
		t.Errorf("the line names %v; an operator needs the prefix they asked for AND the "+
			"address Docker was given, or there is nothing to compare", entries[0].Data)
	}

	// Every direction that must NOT count, in one place: a lease that
	// matched, a v4 acquisition (Info.MainAddrFallback is a v6 field
	// and a v4 Info can only carry its zero), and a network that named
	// no prefix at all.
	q := &Plugin{}
	q.noteMainPrefixFallback(true, dhcp.Info{IP: "2001:db8:1::42/64"}, main, "e1")
	q.noteMainPrefixFallback(false, dhcp.Info{IP: "192.168.99.50/24", MainAddrFallback: true}, netip.Prefix{}, "e2")
	q.noteMainPrefixFallback(true, dhcp.Info{IP: "2001:db8:1::42/64"}, netip.Prefix{}, "e3")
	if got := q.ipv6MainPrefixUnmatched.Load(); got != 0 {
		t.Errorf("ipv6_main_prefix_unmatched = %d for three endpoints that matched or asked "+
			"for nothing; a counter that moves on every endpoint says nothing about any", got)
	}
}

// fakeV6LinkAddrs records the netlink calls the v6 apply path makes and
// makes none of them. It is the TRANSPORT and not the verdict: it
// returns whatever error a case asks for and decides nothing else.
type fakeV6LinkAddrs struct {
	replaced []string
	deleted  []string
	replErr  map[string]error
	delErr   map[string]error
}

func (f *fakeV6LinkAddrs) AddrReplace(_ netlink.Link, a *netlink.Addr) error {
	f.replaced = append(f.replaced, a.String())
	return f.replErr[a.String()]
}

func (f *fakeV6LinkAddrs) AddrDel(_ netlink.Link, a *netlink.Addr) error {
	f.deleted = append(f.deleted, a.String())
	return f.delErr[a.String()]
}

// applyManager is a manager wired for the apply path and nothing else:
// a link that is never dialled, a plugin for the counters, a ledger.
func applyManager(t *testing.T) (*dhcpManager, *Plugin) {
	t.Helper()
	var failures atomic.Int32
	p := &Plugin{}
	p.ledger = newLeaseLedger(filepath.Join(t.TempDir(), ledgerFileName), &failures)
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"},
		DHCPNetworkOptions{AuditLog: true}).withPlugin(p)
	m.ctrLink = &netlink.Device{}
	return m, p
}

// THE LOOP, NOT THE ARITHMETIC. Defeat rows 3 and 4.
//
// v6WantedAddrs and v6AddrsToWithdraw have their own tables above, and
// both of them stayed green against a manager that installed the first
// address of the set and never removed anything: the helpers were
// tested and the code that calls them was observed by nothing. That is
// this test's whole subject, which is why it asserts on the netlink
// calls that were made and in what order.
//
// The three phases are one sequence on one manager because that is what
// makes them a renumbering. Asserting each from a fresh manager would
// test three first binds.
func TestApplyV6Addrs_InstallsEveryAddressAndRemovesWhatLeftTheLease(t *testing.T) {
	m, p := applyManager(t)
	h := &fakeV6LinkAddrs{}

	// Phase 1: a lease holding two advertised prefixes.
	two := dhcp.Info{
		IP:    "2001:db8:1::a/64",
		SLAAC: true,
		Addrs: []dhcp.V6Addr{
			{IP: "2001:db8:1::a/64", ValidSeconds: 3600, PreferredSeconds: 1800},
			{IP: "fd00:9::a/64", ValidSeconds: 300, PreferredSeconds: 120},
		},
	}
	if err := m.applyV6Addrs(h, mustParseAddr(t, two.IP), two); err != nil {
		t.Fatalf("applying a two-address lease: %v", err)
	}
	if len(h.replaced) != 2 {
		t.Fatalf("AddrReplace was called %d time(s) for a lease holding two advertised "+
			"prefixes: %v.\nRFC 4862 section 5.5.3 forms one address per autonomous "+
			"prefix and the library holds and refreshes all of them, so a container "+
			"given only the first has an address its own client believes it has too.",
			len(h.replaced), h.replaced)
	}
	if h.replaced[0] != "2001:db8:1::a/64" {
		t.Errorf("the first AddrReplace was %q, want the address Docker was told about: "+
			"if one of several calls is going to fail, that is the one worth making "+
			"first", h.replaced[0])
	}
	if len(h.deleted) != 0 {
		t.Errorf("a first bind deleted %v; nothing had left a lease that had not existed",
			h.deleted)
	}
	if got := p.ipv6SLAACAddresses.Load(); got != 2 {
		t.Errorf("ipv6_slaac_addresses = %d after two formed addresses were installed, want 2", got)
	}

	// Phase 2: the same lease again, which is what a renewal is. No
	// address arrived and none left.
	if err := m.applyV6Addrs(h, mustParseAddr(t, two.IP), two); err != nil {
		t.Fatalf("re-applying the same lease: %v", err)
	}
	if len(h.replaced) != 4 {
		t.Errorf("a renewal made %d AddrReplace calls in total, want 4: every address has "+
			"to be re-applied or the kernel keeps counting down the previous "+
			"advertisement's lifetimes", len(h.replaced))
	}
	if len(h.deleted) != 0 {
		t.Errorf("a renewal onto the same addresses deleted %v", h.deleted)
	}
	if got := p.ipv6SLAACAddresses.Load(); got != 2 {
		t.Errorf("ipv6_slaac_addresses = %d after a renewal onto the same two addresses, "+
			"want 2: the counter's population is addresses formed, and one that counts "+
			"refreshes reports how often the router advertised", got)
	}

	// Phase 3: the router stops advertising the second prefix.
	one := dhcp.Info{
		IP:    "2001:db8:1::a/64",
		SLAAC: true,
		Addrs: []dhcp.V6Addr{{IP: "2001:db8:1::a/64", ValidSeconds: 3600, PreferredSeconds: 1800}},
	}
	if err := m.applyV6Addrs(h, mustParseAddr(t, one.IP), one); err != nil {
		t.Fatalf("applying the renumbered lease: %v", err)
	}
	if len(h.deleted) != 1 || h.deleted[0] != "fd00:9::a/64" {
		t.Fatalf("the renumbering deleted %v, want exactly the withdrawn prefix's address.\n"+
			"A valid lifetime that the router keeps refreshing never expires on its own, "+
			"so an address nothing removes is one the container holds for as long as it "+
			"runs -- and chooses as a source address for connections that go nowhere.",
			h.deleted)
	}
	if got := p.ipv6AddressesWithdrawn.Load(); got != 1 {
		t.Errorf("ipv6_addresses_withdrawn = %d, want 1", got)
	}

	rows := readLedgerLines(t, p.ledger.path)
	if len(rows) != 1 || rows[0].Kind != "withdrawn" || rows[0].IP != "fd00:9::a" {
		t.Fatalf("the ledger holds %+v; the one row this sequence writes is the withdrawal, "+
			"naming the address that left", rows)
	}
	if rows[0].Source != "slaac" {
		t.Errorf("the withdrawal row's source is %q, want \"slaac\": no DHCP server was "+
			"involved and there is no lease anywhere to correlate the row with",
			rows[0].Source)
	}
}

// A kernel that refuses one address does not silently drop the rest.
//
// The error direction, because the loop above returns on the first
// failure: what must not happen is a refusal being swallowed and the
// endpoint coming up holding an address set nobody checked.
func TestApplyV6Addrs_AKernelRefusalIsReturnedAndNamesTheAddress(t *testing.T) {
	m, _ := applyManager(t)
	h := &fakeV6LinkAddrs{replErr: map[string]error{"2001:db8:1::a/64": unix.EINVAL}}

	err := m.applyV6Addrs(h, mustParseAddr(t, "2001:db8:1::a/64"), dhcp.Info{
		IP:    "2001:db8:1::a/64",
		SLAAC: true,
		Addrs: []dhcp.V6Addr{{IP: "2001:db8:1::a/64", ValidSeconds: 3600, PreferredSeconds: 1800}},
	})
	if err == nil {
		t.Fatal("the kernel refused the address and applyV6Addrs returned nil; the endpoint " +
			"would come up reporting an address the container does not have")
	}
	if !strings.Contains(err.Error(), "2001:db8:1::a/64") {
		t.Errorf("the error is %q and does not name the address that was refused", err)
	}
}

// A withdrawal the kernel refuses is a warning and not a failed renewal,
// and the address is out of the manager's set either way.
//
// The opposite direction of the row above, and they are different on
// purpose: an address that is arriving is the lease, and an address that
// is leaving is already gone. A manager that returned an error here
// would fail a renewal over cleanup it no longer has any use for.
func TestApplyV6Addrs_AFailedWithdrawalDoesNotFailTheRenewal(t *testing.T) {
	m, p := applyManager(t)
	h := &fakeV6LinkAddrs{delErr: map[string]error{"fd00:9::a/64": unix.ENODEV}}

	two := dhcp.Info{
		IP:    "2001:db8:1::a/64",
		SLAAC: true,
		Addrs: []dhcp.V6Addr{
			{IP: "2001:db8:1::a/64", ValidSeconds: 3600, PreferredSeconds: 1800},
			{IP: "fd00:9::a/64", ValidSeconds: 300, PreferredSeconds: 120},
		},
	}
	if err := m.applyV6Addrs(h, mustParseAddr(t, two.IP), two); err != nil {
		t.Fatalf("applying a two-address lease: %v", err)
	}
	one := dhcp.Info{
		IP:    "2001:db8:1::a/64",
		SLAAC: true,
		Addrs: []dhcp.V6Addr{{IP: "2001:db8:1::a/64", ValidSeconds: 3600, PreferredSeconds: 1800}},
	}
	if err := m.applyV6Addrs(h, mustParseAddr(t, one.IP), one); err != nil {
		t.Fatalf("a renewal failed over an address that was being removed: %v", err)
	}
	if got := p.ipv6AddressesWithdrawn.Load(); got != 0 {
		t.Errorf("ipv6_addresses_withdrawn = %d for a removal the kernel refused, want 0: "+
			"the counter's population is addresses that came off the link", got)
	}
	if _, still := m.installedV6()["fd00:9::a/64"]; still {
		t.Error("the manager still holds an address that left the lease, so the next " +
			"renewal tries to remove it again and writes a second ledger row for one " +
			"withdrawal")
	}
}

// THE DEPRECATION CARRIED BY ONE MEMBER OF THE SET REACHES THE KERNEL.
//
// The pair of lifetimes cannot say it: an address deprecated on a
// prefix advertised forever renders as (0, 0), and so does an address
// advertised with no deadlines at all. This walks the set-building loop
// with one of each, so the flag has to travel per address and not per
// lease. A version that passed the same answer for every member would
// either install the deprecated address preferred, or deprecate the one
// the router is still telling the host to prefer -- and both are
// silent, because RFC 4862 section 5.5.4 leaves a deprecated address on
// the link and reachable.
func TestV6WantedAddrs_ADeprecatedMemberKeepsItsDeprecation(t *testing.T) {
	main := mustParseAddr(t, "2001:db8:1::a/64")
	info := dhcp.Info{
		IP:    "2001:db8:1::a/64",
		SLAAC: true,
		Addrs: []dhcp.V6Addr{
			// Advertised forever and still preferred.
			{IP: "2001:db8:1::a/64"},
			// Advertised forever, preferred lifetime spent.
			{IP: "fd00:db8:2::a/64", Deprecated: true},
		},
	}

	want, err := v6WantedAddrs(main, info)
	if err != nil {
		t.Fatalf("v6WantedAddrs: %v", err)
	}
	if len(want) != 2 {
		t.Fatalf("a two-prefix lease produced %d addresses: %v", len(want), want)
	}
	byKey := map[string]*netlink.Addr{}
	for _, w := range want {
		byKey[w.key] = w.addr
	}

	dep, ok := byKey["fd00:db8:2::a/64"]
	if !ok {
		t.Fatalf("the deprecated address is not in the set: %v", byKey)
	}
	if dep.ValidLft != infiniteLft || dep.PreferedLft != 0 {
		t.Errorf("the deprecated address goes to the kernel as ValidLft=%d PreferedLft=%d, "+
			"want %d and 0. Both lifetimes zero attaches no IFA_CACHEINFO at all and the "+
			"kernel installs it permanent and preferred, which is the opposite of what the "+
			"router advertised", dep.ValidLft, dep.PreferedLft, infiniteLft)
	}

	// The preservation control: the member that is NOT deprecated still
	// carries no lifetimes, which is this plugin's permanent address.
	keep, ok := byKey["2001:db8:1::a/64"]
	if !ok {
		t.Fatalf("the main address is not in the set: %v", byKey)
	}
	if keep.ValidLft != 0 || keep.PreferedLft != 0 {
		t.Errorf("the address advertised with no deadlines gave ValidLft=%d PreferedLft=%d, "+
			"want both zero: one member's deprecation must not reach the other",
			keep.ValidLft, keep.PreferedLft)
	}
}

// THE CHAIN FROM A LEASE EVENT TO NETLINK, which nothing below the
// integration lane could see.
//
// Every test above enters at applyV6Addrs with a transport handed in,
// so all of them stayed green against a manager whose dispatch never
// reached the apply path at all: `case "bound"` not calling renew,
// installV6Address returning early, the netHandle guard widened. The
// only observer of that stretch was an integration arm, and an
// integration arm is a poor one here -- the engine installs the address
// CreateEndpoint reported when it builds the sandbox, so the container's
// link holds the right address, with IFA_F_NODAD, before this plugin
// has applied anything (MEASURED, engine 29.8.0, run 35153680517:
// `flags 02 valid_lft forever preferred_lft forever`).
//
// So this drives the whole chain from the event the persistent client
// emits, and asserts the two things that install is NOT: the lease's
// own lifetimes on the wire to netlink, and the counter an operator
// reads moving for each address.
func TestHandleEvent_ABoundSLAACLeaseReachesTheLink(t *testing.T) {
	m, p := applyManager(t)
	h := &fakeV6LinkAddrs{}
	m.v6Addrs = h

	m.handleEvent(dhcp.Event{
		Type: "bound",
		Data: dhcp.Info{
			IP:               "2001:db8:1::a/64",
			SLAAC:            true,
			LeaseSeconds:     3600,
			PreferredSeconds: 1800,
			Addrs: []dhcp.V6Addr{
				{IP: "2001:db8:1::a/64", ValidSeconds: 3600, PreferredSeconds: 1800},
				{IP: "fd00:9::a/64", ValidSeconds: 300, PreferredSeconds: 120},
			},
		},
	}, true)

	if len(h.replaced) != 2 {
		t.Fatalf("a bound SLAAC lease holding two formed addresses made %d AddrReplace "+
			"call(s): %v.\nThe address the container ends up with is whatever the engine "+
			"installed when it built the sandbox unless this path runs, so a dispatch that "+
			"never reaches it leaves a container holding a permanent address on a prefix "+
			"the router can withdraw", len(h.replaced), h.replaced)
	}
	if got := p.ipv6SLAACAddresses.Load(); got != 2 {
		t.Errorf("ipv6_slaac_addresses = %d after a bound lease holding two formed "+
			"addresses, want 2", got)
	}
	if got := p.leasesObtainedV6.Load(); got != 1 {
		t.Errorf("leases_obtained_v6 = %d after one bound event, want 1", got)
	}

	// The lifetimes, because the address alone is the one thing the
	// engine's install already got right.
	_, last := m.lastIPs()
	if last == nil {
		t.Fatal("the bound event recorded no IPv6 address")
	}
	if last.ValidLft != 3600 || last.PreferedLft != 1800 {
		t.Errorf("the address was applied with ValidLft=%d PreferedLft=%d, want 3600 and "+
			"1800: the advertised lifetimes are the whole difference between this install "+
			"and the engine's, which carries none and is therefore permanent",
			last.ValidLft, last.PreferedLft)
	}

	// The other direction. Without it, a dispatch that applied the
	// address set on every event whatsoever would satisfy everything
	// above: `config` is a DHCPv6 information reply, it carries no
	// address, and it must not touch the link.
	other, _ := applyManager(t)
	oh := &fakeV6LinkAddrs{}
	other.v6Addrs = oh
	other.handleEvent(dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{"2001:db8::53"}}}, true)
	if len(oh.replaced) != 0 || len(oh.deleted) != 0 {
		t.Errorf("an information reply touched the link: replaced=%v deleted=%v",
			oh.replaced, oh.deleted)
	}
}
