// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	dContainer "github.com/moby/moby/api/types/container"
	dNetwork "github.com/moby/moby/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// dualStackRig is one manager driven through handleEvent with the netlink, resolv.conf and docker seams faked (#1279).
type dualStackRig struct {
	m      *dhcpManager
	p      *Plugin
	dns    *writtenResolvConf
	addrs  *fakeV6LinkAddrs
	routes *fakeRouteTable

	mu        sync.Mutex
	linkMTU   int
	mtuWrites []int
}

func newDualStackRig(t *testing.T) *dualStackRig {
	t.Helper()
	r := &dualStackRig{linkMTU: 1500, addrs: &fakeV6LinkAddrs{}, routes: &fakeRouteTable{}}
	r.dns = captureResolvConf(t)
	r.m = dualStackDNSManager(t)
	r.p = r.m.plugin
	r.m.opts.IPv6Mode, r.m.opts.PropagateMTU = "slaac", true
	r.routes.install(t, r.m)
	r.m.ctrLink = &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: 7, Name: "eth0", MTU: 1500}}
	r.m.v6Addrs = r.addrs

	prevIdx, prevMTU := nlLinkByIndex, nlHandleLinkSetMTU
	nlLinkByIndex = func(_ *netlink.Handle, index int) (netlink.Link, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: index, Name: "eth0", MTU: r.linkMTU}}, nil
	}
	nlHandleLinkSetMTU = func(_ *netlink.Handle, _ netlink.Link, mtu int) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.linkMTU = mtu
		r.mtuWrites = append(r.mtuWrites, mtu)
		return nil
	}
	t.Cleanup(func() { nlLinkByIndex, nlHandleLinkSetMTU = prevIdx, prevMTU })
	return r
}

func (r *dualStackRig) mtu() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.linkMTU
}

func v4Lease(dns string, mtu int) dhcp.Info {
	return dhcp.Info{IP: "192.0.2.10/24", DNSServers: []string{dns}, Domain: "lan.example", MTU: mtu}
}

func v6Lease(dns string, mtu int) dhcp.Info {
	return dhcp.Info{
		IP: "2001:db8:1::a/64", SLAAC: true, RouterSeen: true, LeaseSeconds: 3600, PreferredSeconds: 1800,
		DNSServers: []string{dns}, MTU: mtu,
	}
}

func (r *dualStackRig) bothBound(t *testing.T) {
	t.Helper()
	r.m.handleEvent(dhcp.Event{Type: "bound", Data: v4Lease("192.0.2.53", 0)}, false)
	r.m.handleEvent(dhcp.Event{Type: "bound", Data: v6Lease("2001:db8::53", 0)}, true)
	v4, v6 := r.m.lastIPs()
	if v4 == nil || v6 == nil {
		t.Fatalf("fixture: lastIPs after two bound events = %v, %v", v4, v6)
	}
	if got := strings.Join(nameservers(r.dns.last(t)), ","); got != "192.0.2.53,2001:db8::53" {
		t.Fatalf("fixture: resolv.conf after two bound events has nameservers %q", got)
	}
}

func TestDualStack_V4LeaseLossLeavesTheV6SideAlone(t *testing.T) {
	r := newDualStackRig(t)
	r.bothBound(t)
	_, v6Before := r.m.lastIPs()
	writes := len(r.dns.writes)
	replaced := len(r.addrs.replaced)

	r.m.handleEvent(dhcp.Event{Type: "leasefail"}, false)

	if len(r.addrs.deleted) != 0 || len(r.routes.deleted) != 0 || len(r.routes.replace) != 0 {
		t.Errorf("a v4 leasefail touched the link: addresses deleted %v, routes deleted %v replaced %v",
			r.addrs.deleted, r.routes.deleted, r.routes.replace)
	}
	if _, v6 := r.m.lastIPs(); v6 != v6Before {
		t.Errorf("a v4 leasefail changed the v6 lease record from %v to %v", v6Before, v6)
	}
	if !r.m.boundV6.Load() {
		t.Error("a v4 leasefail cleared the v6 bound mark")
	}
	if len(r.dns.writes) != writes || len(r.addrs.replaced) != replaced {
		t.Errorf("a v4 leasefail rewrote resolv.conf (%d to %d) or the v6 addresses (%d to %d)",
			writes, len(r.dns.writes), replaced, len(r.addrs.replaced))
	}

	r.m.handleEvent(dhcp.Event{Type: "renew", Data: v6Lease("2001:db8::53", 0)}, true)
	if got := strings.Join(nameservers(r.dns.last(t)), ","); got != "192.0.2.53,2001:db8::53" {
		t.Errorf("a v6 renew after a v4 leasefail wrote nameservers %q, want both families", got)
	}
	if h := r.p.healthSnapshot(); h.DHCPTimeoutsV4 != 1 || h.DHCPTimeoutsV6 != 0 {
		t.Errorf("dhcp_timeouts_v4=%d dhcp_timeouts_v6=%d, want 1 and 0", h.DHCPTimeoutsV4, h.DHCPTimeoutsV6)
	}
}

func TestDualStack_V6LeaseLossLeavesTheV4SideAlone(t *testing.T) {
	r := newDualStackRig(t)
	r.bothBound(t)
	v4Before, _ := r.m.lastIPs()
	writes := len(r.dns.writes)

	r.m.handleEvent(dhcp.Event{Type: "leasefail"}, true)

	if v4, _ := r.m.lastIPs(); v4 != v4Before {
		t.Errorf("a v6 leasefail changed the v4 lease record from %v to %v", v4Before, v4)
	}
	if !r.m.boundV4.Load() {
		t.Error("a v6 leasefail cleared the v4 bound mark")
	}
	if len(r.dns.writes) != writes {
		t.Errorf("a v6 leasefail rewrote resolv.conf %d time(s)", len(r.dns.writes)-writes)
	}

	r.m.handleEvent(dhcp.Event{Type: "renew", Data: v4Lease("192.0.2.53", 0)}, false)
	if got := strings.Join(nameservers(r.dns.last(t)), ","); got != "192.0.2.53,2001:db8::53" {
		t.Errorf("a v4 renew after a v6 leasefail wrote nameservers %q, want both families", got)
	}
	if h := r.p.healthSnapshot(); h.DHCPTimeoutsV6 != 1 || h.DHCPTimeoutsV4 != 0 {
		t.Errorf("dhcp_timeouts_v6=%d dhcp_timeouts_v4=%d, want 1 and 0", h.DHCPTimeoutsV6, h.DHCPTimeoutsV4)
	}
}

// The delegated-prefix aggregate belongs to the DHCPv6 lease, so only a v6 loss may take it (#214, #1279).
func TestDualStack_OnlyAV6LeaseLossTakesTheDelegatedPrefixRoute(t *testing.T) {
	r := newDualStackRig(t)
	r.m.opts.IPv6Mode, r.m.opts.IPv6PD = "dhcp", 64
	pd := v6Lease("2001:db8::53", 0)
	pd.DelegatedPrefixes = []dhcp.V6Addr{{IP: "fd00:98:0:1::/64", ValidSeconds: 3600, PreferredSeconds: 1800}}
	r.m.handleEvent(dhcp.Event{Type: "bound", Data: v4Lease("192.0.2.53", 0)}, false)
	r.m.handleEvent(dhcp.Event{Type: "bound", Data: pd}, true)
	if len(r.routes.replace) != 1 {
		t.Fatalf("fixture: the delegated prefix was not installed, routes replaced = %v", r.routes.replace)
	}
	r.routes.routes = append(r.routes.routes, r.routes.replace...)

	r.m.handleEvent(dhcp.Event{Type: "leasefail"}, false)
	if len(r.routes.deleted) != 0 {
		t.Fatalf("a v4 leasefail deleted the v6 delegated prefix route: %v", destinations(r.routes.deleted))
	}
	r.m.handleEvent(dhcp.Event{Type: "leasefail"}, true)
	if got := destinations(r.routes.deleted); len(got) != 1 || got[0] != "fd00:98:0:1::/64" {
		t.Errorf("after a v6 leasefail deleted routes = %v, want the one aggregate", got)
	}
}

func TestDualStack_V6AddressWithdrawalLeavesV4Alone(t *testing.T) {
	r := newDualStackRig(t)
	r.bothBound(t)
	v4Before, _ := r.m.lastIPs()
	nlReplaces := 0
	prev := nlHandleAddrReplace
	nlHandleAddrReplace = func(*netlink.Handle, netlink.Link, *netlink.Addr) error { nlReplaces++; return nil }
	t.Cleanup(func() { nlHandleAddrReplace = prev })

	r.m.handleEvent(dhcp.Event{Type: "slaac_lost"}, true)

	if len(r.addrs.deleted) != 1 || r.addrs.deleted[0] != "2001:db8:1::a/64" {
		t.Errorf("slaac_lost deleted addresses %v, want the one formed v6 address", r.addrs.deleted)
	}
	if got := r.p.ipv6AddressesWithdrawn.Load(); got != 1 {
		t.Errorf("ipv6_addresses_withdrawn = %d, want 1", got)
	}
	if v4, _ := r.m.lastIPs(); v4 != v4Before || nlReplaces != 0 {
		t.Errorf("slaac_lost touched the v4 side: record %v to %v, v4 address writes %d", v4Before, v4, nlReplaces)
	}
	if h := r.p.healthSnapshot(); h.DHCPTimeoutsV4+h.DHCPTimeoutsV6 != 0 {
		t.Errorf("slaac_lost counted an outage tick (v4=%d v6=%d)", h.DHCPTimeoutsV4, h.DHCPTimeoutsV6)
	}
}

func TestDualStack_EachEventCountsOnItsOwnFamily(t *testing.T) {
	r := newDualStackRig(t)
	for _, tc := range []struct {
		event string
		v6    bool
		info  dhcp.Info
	}{
		{"bound", false, v4Lease("192.0.2.53", 0)},
		{"renew", false, v4Lease("192.0.2.53", 0)},
		{"nak", false, dhcp.Info{}},
		{"leasefail", false, dhcp.Info{}},
		{"bound", true, v6Lease("2001:db8::53", 0)},
		{"renew", true, v6Lease("2001:db8::53", 0)},
		{"renew", true, v6Lease("2001:db8::53", 0)},
		{"nak", true, dhcp.Info{}},
		{"nak", true, dhcp.Info{}},
		{"nak", true, dhcp.Info{}},
		{"leasefail", true, dhcp.Info{}},
		{"leasefail", true, dhcp.Info{}},
	} {
		r.m.handleEvent(dhcp.Event{Type: tc.event, Data: tc.info}, tc.v6)
	}
	p := r.p
	for _, c := range []struct {
		name     string
		got, wnt int32
	}{
		{"leases_obtained_v4", p.leasesObtainedV4.Load(), 1},
		{"leases_obtained_v6", p.leasesObtainedV6.Load(), 1},
		{"leases_renewed_v4", p.leasesRenewedV4.Load(), 1},
		{"leases_renewed_v6", p.leasesRenewedV6.Load(), 2},
		{"naks_received_v4", p.naksReceivedV4.Load(), 1},
		{"naks_received_v6", p.naksReceivedV6.Load(), 3},
		{"dhcp_timeouts_v4", p.dhcpTimeoutsV4.Load(), 1},
		{"dhcp_timeouts_v6", p.dhcpTimeoutsV6.Load(), 2},
	} {
		if c.got != c.wnt {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.wnt)
		}
	}
}

// Option 6 on bound and renew, the RFC 8106 option on a router advertisement, option 23 on a configuration reply:
// every route into propagateDNS must name its own family or one erases the other (#1250).
func TestDualStack_DNSFromEveryEventKindMerges(t *testing.T) {
	r := newDualStackRig(t)

	r.m.handleEvent(dhcp.Event{Type: "bound", Data: v4Lease("192.0.2.53", 0)}, false)
	r.m.handleEvent(dhcp.Event{Type: "routeradvert", Data: dhcp.Info{DNSServers: []string{"2001:db8::53"}, RouterSeen: true}}, true)
	if got := strings.Join(nameservers(r.dns.last(t)), ","); got != "192.0.2.53,2001:db8::53" {
		t.Errorf("v4 bound then a router advertisement wrote nameservers %q", got)
	}

	r.m.handleEvent(dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{"2001:db8::54"}}}, true)
	if got := strings.Join(nameservers(r.dns.last(t)), ","); got != "192.0.2.53,2001:db8::54" {
		t.Errorf("a v6 configuration reply wrote nameservers %q, want the v4 server and only the new v6 one", got)
	}

	r.m.handleEvent(dhcp.Event{Type: "renew", Data: v4Lease("192.0.2.54", 0)}, false)
	if got := strings.Join(nameservers(r.dns.last(t)), ","); got != "192.0.2.54,2001:db8::54" {
		t.Errorf("a v4 renew wrote nameservers %q, want the new v4 server and the kept v6 one", got)
	}
	if got := searchLine(r.dns.last(t)); got != "search lan.example" {
		t.Errorf("search line after the v6 events = %q, want the v4 domain kept", got)
	}
}

func TestDualStack_AFailedWriteIsCaughtUpByTheNextFamily(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(r *dualStackRig) (restore func())
	}{
		{"the daemon cannot find the container", func(r *dualStackRig) func() {
			d := r.m.docker.(*fakeDocker)
			d.containerErr = errors.New("daemon busy")
			return func() { d.containerErr = nil }
		}},
		{"the file write fails", func(r *dualStackRig) func() {
			prev := resolvConfWriter
			resolvConfWriter = func(int, string, []string, []string, string, string) error {
				return errors.New("read-only file system")
			}
			return func() { resolvConfWriter = prev }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDualStackRig(t)
			restore := tc.break_(r)
			r.m.propagateDNS(false, v4Lease("192.0.2.53", 0))
			restore()
			if len(r.dns.writes) != 0 {
				t.Fatalf("fixture: the broken v4 write still reached the file: %v", r.dns.writes)
			}

			r.m.propagateDNS(true, v6Lease("2001:db8::53", 0))
			if got := strings.Join(nameservers(r.dns.last(t)), ","); got != "192.0.2.53,2001:db8::53" {
				t.Errorf("the v6 write after a failed v4 write has nameservers %q, want both", got)
			}
		})
	}
}

// A write that carries the other family's resolvers must be exclusive of the other family's update, so the lock is
// held while the file is written, not only while the two lists are combined (#1250).
func TestDualStack_TheResolvConfWriteHoldsTheDNSLock(t *testing.T) {
	r := newDualStackRig(t)
	var lockedDuringWrite []bool
	resolvConfWriter = func(int, string, []string, []string, string, string) error {
		free := r.m.dnsMu.TryLock()
		if free {
			r.m.dnsMu.Unlock()
		}
		lockedDuringWrite = append(lockedDuringWrite, !free)
		return nil
	}

	r.m.propagateDNS(false, v4Lease("192.0.2.53", 0))
	r.m.propagateDNS(true, v6Lease("2001:db8::53", 0))

	if len(lockedDuringWrite) != 2 {
		t.Fatalf("the writer ran %d times, want 2", len(lockedDuringWrite))
	}
	for i, held := range lockedDuringWrite {
		if !held {
			t.Errorf("write %d ran with dnsMu free", i+1)
		}
	}
}

// Renews of both families at once: nothing either goroutine writes may erase the other's state, and the link takes
// the smaller of the two MTUs whichever family spoke last (#821, #1250).
func TestDualStack_ConcurrentEventsOfBothFamiliesKeepBothFamiliesState(t *testing.T) {
	const rounds = 100
	r := newDualStackRig(t)
	r.bothBound(t)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			r.m.handleEvent(dhcp.Event{Type: "renew", Data: v4Lease("192.0.2.53", 1400)}, false)
			r.m.handleEvent(dhcp.Event{Type: "leasefail"}, false)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			r.m.handleEvent(dhcp.Event{Type: "renew", Data: v6Lease("2001:db8::53", 1280)}, true)
			r.m.handleEvent(dhcp.Event{Type: "routeradvert", Data: v6Lease("2001:db8::53", 1280)}, true)
		}
	}()
	close(start)
	wg.Wait()

	if got := r.mtu(); got != 1280 {
		t.Errorf("link MTU = %d after both families supplied one, want the smaller, 1280", got)
	}
	if got := strings.Join(nameservers(r.dns.last(t)), ","); got != "192.0.2.53,2001:db8::53" {
		t.Errorf("last resolv.conf nameservers = %q, want both families", got)
	}
	if v4, v6 := r.m.lastIPs(); v4 == nil || v6 == nil {
		t.Errorf("lease records after the run: v4 %v v6 %v", v4, v6)
	}
	if h := r.p.healthSnapshot(); h.DHCPTimeoutsV4 != rounds || h.DHCPTimeoutsV6 != 0 {
		t.Errorf("dhcp_timeouts_v4=%d dhcp_timeouts_v6=%d, want %d and 0", h.DHCPTimeoutsV4, h.DHCPTimeoutsV6, rounds)
	}
	if got := r.p.leasesRenewedV4.Load(); got != rounds {
		t.Errorf("leases_renewed_v4 = %d, want %d", got, rounds)
	}
}

// startRig is a whole Start with both families, the clients faked at their constructor and start seams (#1279).
type startRig struct {
	m      *dhcpManager
	events map[bool]chan dhcp.Event
	order  []string

	mu       sync.Mutex
	failV6   error
	onV6     func()
	entered  chan bool
	gate     chan struct{}
	writes   int
	clientOf map[*dhcp.DHCPClient]bool
}

func newStartRig(t *testing.T) *startRig {
	t.Helper()
	pid := os.Getpid()
	ctrID := selfCgroupLeaf(t, pid)
	m, _ := daemonFreeManager(t, &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{ctrID: {EndpointID: "ep-abcdef"}}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			ctrID: {State: &dContainer.State{Pid: pid}, Config: &dContainer.Config{Hostname: "ctr-1"}},
		},
	})
	m.opts.IPv6Mode, m.opts.PropagateDNS = "slaac", true
	r := &startRig{
		m:        m,
		events:   map[bool]chan dhcp.Event{false: make(chan dhcp.Event), true: make(chan dhcp.Event)},
		entered:  make(chan bool, 16),
		clientOf: map[*dhcp.DHCPClient]bool{},
	}

	prevNew, prevStart, prevEnter, prevWrite := newDHCPClient, startDHCPClient, v6EnterSandbox, resolvConfWriter
	newDHCPClient = func(_ string, o *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if o.V6 {
			r.order = append(r.order, "v6 client")
			if r.onV6 != nil {
				r.mu.Unlock()
				r.onV6()
				r.mu.Lock()
			}
			if r.failV6 != nil {
				return nil, r.failV6
			}
		} else {
			r.order = append(r.order, "v4 client")
		}
		c := &dhcp.DHCPClient{}
		r.clientOf[c] = o.V6
		return c, nil
	}
	startDHCPClient = func(c *dhcp.DHCPClient) (chan dhcp.Event, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.events[r.clientOf[c]], nil
	}
	v6EnterSandbox = func(*dhcpManager, func(string)) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.order = append(r.order, "sandbox prep")
		return errors.New("no sandbox in this test")
	}
	resolvConfWriter = func(int, string, []string, []string, string, string) error {
		r.mu.Lock()
		r.writes++
		gate := r.gate
		r.mu.Unlock()
		if gate != nil {
			r.entered <- true
			<-gate
		}
		return nil
	}
	t.Cleanup(func() {
		newDHCPClient, startDHCPClient, v6EnterSandbox, resolvConfWriter = prevNew, prevStart, prevEnter, prevWrite
	})
	return r
}

func (r *startRig) start(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.m.Start(ctx)
}

// within fails the test rather than hang when a teardown that must return does not.
func within(t *testing.T, what string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

func (r *startRig) stopAsync() <-chan error {
	done := make(chan error, 1)
	go func() { done <- r.m.Stop() }()
	return done
}

func (r *startRig) writeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writes
}

func TestDualStack_StartOpensTheV4ClientThenPreparesTheLinkThenOpensV6(t *testing.T) {
	r := newStartRig(t)
	if err := r.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = within(t, "Stop", r.stopAsync()) })

	if got := strings.Join(r.order, ", "); got != "v4 client, sandbox prep, v6 client" {
		t.Errorf("set-up order = %q, want the v4 client first, the link prepared for v6, then the v6 client", got)
	}
	if r.m.errChan == nil || r.m.errChanV6 == nil || r.m.errChan == r.m.errChanV6 {
		t.Error("each family needs its own exit channel for Stop to drain")
	}
}

func TestDualStack_AV6SetupFailureStopsTheRunningV4ClientBeforeStartReturns(t *testing.T) {
	r := newStartRig(t)
	r.failV6 = errors.New("no v6 socket")
	r.gate = make(chan struct{})
	r.onV6 = func() {
		// The v4 goroutine is mid-event, as it is when a lease lands during the v6 set-up.
		r.events[false] <- dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{"192.0.2.53"}}}
		<-r.entered
	}

	done := make(chan error, 1)
	go func() { done <- r.start(t) }()

	select {
	case err := <-done:
		t.Fatalf("Start returned (%v) while the v4 goroutine was still inside its event", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(r.gate)
	err := within(t, "Start", done)
	if err == nil || !strings.Contains(err.Error(), "DHCPv6") {
		t.Errorf("Start error = %v, want the v6 client failure", err)
	}
	select {
	case <-r.m.stopChan:
	default:
		t.Error("stopChan is open after a failed Start: the v4 client keeps running with no owner")
	}
	if len(r.m.errChan) != 0 {
		t.Error("the v4 exit value was left in errChan for a Stop that will not read it")
	}
	if r.m.ctrLink == nil {
		t.Error("the v4 side lost its link in the failed v6 set-up")
	}
	if err := within(t, "Stop", r.stopAsync()); err != nil {
		t.Errorf("Stop after the failed Start: %v", err)
	}
}

// Stop while one family is mid-event: its goroutine must be drained before the handles close under it (#608).
func TestDualStack_StopWaitsForTheFamilyThatIsStillRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		v6   bool
	}{{"v4 mid-event", false}, {"v6 mid-event", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStartRig(t)
			if err := r.start(t); err != nil {
				t.Fatalf("Start: %v", err)
			}
			r.mu.Lock()
			r.gate = make(chan struct{})
			r.mu.Unlock()
			dns := "192.0.2.53"
			if tc.v6 {
				dns = "2001:db8::53"
			}
			r.events[tc.v6] <- dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{dns}}}
			<-r.entered

			done := r.stopAsync()
			select {
			case err := <-done:
				t.Fatalf("Stop returned (%v) with the %s family inside its event", err, tc.name)
			case <-time.After(150 * time.Millisecond):
			}
			r.mu.Lock()
			close(r.gate)
			r.mu.Unlock()
			if err := within(t, "Stop", done); err != nil {
				t.Errorf("Stop: %v", err)
			}
			after := r.writeCount()
			for _, v6 := range []bool{false, true} {
				select {
				case r.events[v6] <- dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{"192.0.2.99"}}}:
					t.Errorf("an event for v6=%v was received after Stop returned", v6)
				case <-time.After(30 * time.Millisecond):
				}
			}
			if got := r.writeCount(); got != after {
				t.Errorf("resolv.conf written %d time(s) after Stop", got-after)
			}
		})
	}
}
