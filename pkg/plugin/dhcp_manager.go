// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dNetwork "github.com/docker/docker/api/types/network"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// linkAwaitTimeout caps Start's wait for the renamed macvlan child in the container netns; a var so a test shrinks it.
var linkAwaitTimeout = 30 * time.Second

const pollTime = 100 * time.Millisecond

// dhcpClientReapTimeout caps the wait for a self-stopped client's Wait, the only sign that Run returned and its
// AF_PACKET socket closed, so a next Join cannot open a second client on the interface (#901).
const dhcpClientReapTimeout = 5 * time.Second

// dhcpClientFinishTimeout caps Stop's wait for the client's Run to return; a release happens earlier on its own
// budget (#800, #962), and a shorter value would count slow clean exits as client_stop_failures.
const dhcpClientFinishTimeout = 5 * time.Second

// dnsPropagateTimeout is short, since the PID lookup runs on every bound and renew; a timeout is retried next renewal.
const dnsPropagateTimeout = 2 * time.Second

// The library emits Failed{ReasonNoServer} when an attempt runs out of retries, translated to "leasefail" and
// counted as dhcp_timeouts per attempt. It represents option 51's 0xFFFFFFFF infinite lease as a zero Expire, so
// no lease-time clamp exists (#899).

// noteDNSPropagationPIDMismatch counts the DNS refusal only; the netns refusal is counted inside openSandboxNetNS
// (#731), and TestCountingWrappers_AreTheOnlyCallers catches a double count (#317, #707).
func (m *dhcpManager) noteDNSPropagationPIDMismatch(err error) {
	if !errors.Is(err, errPIDNotContainer) || m.plugin == nil {
		return
	}
	m.plugin.dnsPropagationPIDMismatches.Add(1)
}

// closeNsHandle and closeNetHandle log close errors at Debug, a breadcrumb for netns leaks.
func closeNsHandle(h netns.NsHandle) {
	if err := h.Close(); err != nil {
		log.WithError(err).Debug("netns handle close failed")
	}
}
func closeNetHandle(h *netlink.Handle) {
	if h == nil {
		return
	}
	h.Close()
}

type dhcpManager struct {
	docker  dockerClient
	joinReq JoinRequest
	opts    DHCPNetworkOptions

	// plugin is nil in unit tests that drive no lease events; production sets it in Plugin.Join.
	plugin *Plugin

	// ipMu guards lastIP, lastIPv6, seenV4, clientV4 and hostname, written by the event goroutine.
	ipMu     sync.Mutex
	lastIP   *netlink.Addr
	lastIPv6 *netlink.Addr
	// v6Installed is every IPv6 address put on the container link, a set since RFC 4862 section 5.5.3 forms one per
	// autonomous prefix and a withdrawn prefix's address must go (#818).
	v6Installed map[string]*netlink.Addr
	// seenV4 is the v4 client as its event goroutine last recorded it; the health entry renders its lease from here
	// because the library marks a lease held before it emits the event (#1044).
	seenV4 v4Record

	// recordID is the durable lease record (#899); empty in unit tests and adopted endpoints, where record calls no-op.
	recordID string

	// recordID6 is the DHCPv6 record, a second one because a lease.Record binds one family and one identity (#911).
	recordID6 string

	// policyRestricted is captured at setupClient, so the counter describes the policy the client runs under.
	policyRestricted bool

	// boundV4 is set once the v4 client reaches bound or renew; a client stopped before that never held the binding,
	// and Stop must not record a release for it (#549, #800). Written by the v4 consumer goroutine, read in Stop after
	// errChan drains.
	boundV4 atomic.Bool
	// boundV6 is the same proof for the v6 client, read in Stop after errChanV6 drains (#608, #962).
	boundV6 atomic.Bool

	// lastAdvertRoutes holds the RA-installed more-specific routes, destination to next hop, as the diff base: an
	// advertisement that drops a prefix withdraws it (RFC 4191 section 2.3), and the kernel table also holds routes
	// copied from the host bridge that must stay (#821). Used by the v6 consumer goroutine only.
	lastAdvertRoutes map[string]string

	// mtuMu guards each family's last accepted MTU, the only state both family goroutines write.
	mtuMu sync.Mutex
	mtuV4 int
	mtuV6 int
	// mtuBase is the link's MTU before this manager wrote one, restored when neither family supplies an MTU.
	mtuBase int

	// MacAddress is set in macvlan mode to re-find the link after Docker moves and renames it; empty in bridge mode.
	MacAddress net.HardwareAddr

	// hostname is the name put on the wire, empty for no name or a refused one; it sits under ipMu because the attach
	// writes it while the v4 client already leases (#961). Use hostnameOnTheWire and setHostname.
	hostname  string
	nsHandle  netns.NsHandle
	netHandle *netlink.Handle
	ctrLink   netlink.Link

	// v6Addrs is a test seam for applying the IPv6 address set; nil in production, where netHandle is used.
	v6Addrs v6LinkAddrs

	stopChan  chan struct{}
	errChan   chan error
	errChanV6 chan error

	// ctrID caches the container ID for ledger entries, resolved once via ctrIDOnce.
	ctrIDOnce sync.Once
	ctrID     string

	// startedCh closes when Start finishes, so Stop can wait out a Start that Join's goroutine is still running.
	startedCh chan struct{}
	startErr  error

	// startPhases and startTotal hold Start's per-phase timing, recorded on success too so the logged distribution
	// includes fast Joins (#403). Written in Start's deferred exit, read after startedCh closes.
	startPhases string
	startTotal  string

	// attachCancel aborts an in-flight Start, so a Leave mid-attach does not wait out attachDaemonBusyGrace.
	attachCancel context.CancelFunc

	// attachAborted marks a Start cancelled by Stop, so the teardown's "context canceled" is not counted as a
	// join_start_failure (run 30700597210, #406); a flag and not errors.Is, since other cancellations stay faults
	// (#373).
	attachAborted atomic.Bool

	// clientV4 is the persistent v4 client: the health document reads its lease and RFC 5227 phase, and the attach
	// sets its hostname through it (#961). Under ipMu, released before the client is called. v6 has no counterpart:
	// the RFC 5227 pair has no v6 meaning and the library sends no DHCPv6 name option
	// (TestHealthClient_IsPublishedOnlyForV4).
	clientV4 joinClient

	// releasedV4 and releasedV6 record that the lease was actually handed back, set from the outcome and not the
	// option, so Leave closes the record and DeleteEndpoint skips the tombstone only when nothing is held upstream
	// (#962).
	releasedV4 atomic.Bool
	releasedV6 atomic.Bool
}

func newDHCPManager(docker dockerClient, r JoinRequest, opts DHCPNetworkOptions) *dhcpManager {
	return &dhcpManager{
		docker:  docker,
		joinReq: r,
		opts:    opts,

		stopChan:  make(chan struct{}),
		startedCh: make(chan struct{}),
	}
}

// withPlugin wires the manager to the live counters; unit-test helpers omit it.
func (m *dhcpManager) withPlugin(p *Plugin) *dhcpManager {
	m.plugin = p
	return m
}

func (m *dhcpManager) logFields(v6 bool) log.Fields {
	return log.Fields{
		"network":  shortID(m.joinReq.NetworkID),
		"endpoint": shortID(m.joinReq.EndpointID),
		"sandbox":  m.joinReq.SandboxKey,
		"is_ipv6":  v6,
	}
}

// joinClient is the persistent v4 client as the attach holds it: endpointClient plus SetHostname (#961).
type joinClient interface {
	endpointClient

	// SetHostname gives the running client the container name for option 12 and tells the server at once (#961).
	SetHostname(name string) error
}

func (m *dhcpManager) setHealthClient(c joinClient) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	m.clientV4 = c
}

// healthClient returns the published client, or nil; the lock is dropped before the caller uses it.
func (m *dhcpManager) healthClient() joinClient {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.clientV4
}

func (m *dhcpManager) setHostname(h string) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	m.hostname = h
}

// hostnameOnTheWire returns the name being sent, empty for an unnamed container or a refused name.
func (m *dhcpManager) hostnameOnTheWire() string {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.hostname
}

// noteResumedACD reports a record-resumed address whose RFC 5227 section 2.1 check had not completed (D23). The
// condition lives inside so the call site has no guard to invert; the counter feeds acd_resumed_unchecked.
func (m *dhcpManager) noteResumedACD(r dhcp.Resumption, mode proto.ConflictMode, v6 bool) {
	if r.Lease == nil || !r.ACDUnfinished() {
		return
	}
	if m.plugin != nil {
		m.plugin.acdResumedUnchecked.Add(1)
	}
	log.
		WithFields(m.logFields(v6)).
		WithField("address", r.Lease.Addr.Addr()).
		WithField("acd_phase", r.ACD).
		WithField("conflict_check", mode).
		Warn("Resuming an address whose RFC 5227 check had not completed when the plugin last stopped; " +
			"it is re-checked on the INIT-REBOOT acknowledgement")
}

type v4Record struct {
	event string
	at    time.Time
	lease lease.Lease
}

// noteEvent records a v4 event with the client's lease when that lease is the address the event carries (only bound
// and renew carry one); v6 events are not recorded, since every field of the entry describes the v4 client (#1044).
func (m *dhcpManager) noteEvent(event dhcp.Event, v6 bool) {
	if v6 {
		return
	}
	rec := v4Record{event: event.Type, at: time.Now()}
	if c := m.healthClient(); c != nil {
		if l, ok := c.Lease(); ok && l.Addr.String() == event.Data.IP {
			rec.lease = l
		}
	}
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	m.seenV4 = rec
}

func (m *dhcpManager) healthSnapshot() (v4Record, joinClient) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.seenV4, m.clientV4
}

func (m *dhcpManager) lastIPs() (*netlink.Addr, *netlink.Addr) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.lastIP, m.lastIPv6
}

func (m *dhcpManager) setLastIP(v6 bool, addr *netlink.Addr) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	if v6 {
		m.lastIPv6 = addr
	} else {
		m.lastIP = addr
	}
}

// audit appends a lease event to the ledger when the network set audit_log=true; ledger errors never affect leasing.
func (m *dhcpManager) audit(kind, ip string) {
	m.auditFrom(kind, ip, "")
}

// auditFrom is audit with the address source, since a SLAAC-formed and a DHCP-granted address are both `bound` rows
// and only the granted one has a server to correlate with (#818).
func (m *dhcpManager) auditFrom(kind, ip, source string) {
	if m.plugin == nil || m.plugin.ledger == nil || !m.opts.AuditLog {
		return
	}
	m.plugin.ledger.Append(ledgerEntry{
		Kind:      kind,
		Network:   m.joinReq.NetworkID,
		Endpoint:  m.joinReq.EndpointID,
		Container: m.containerID(),
		Hostname:  m.hostnameOnTheWire(),
		IP:        ip,
		Source:    source,
		MAC:       m.macString(),
	})
}

func auditSource(info dhcp.Info) string {
	if info.SLAAC {
		return "slaac"
	}
	return ""
}

// containerID resolves the endpoint's container ID once for ledger entries; a failure leaves the field empty.
func (m *dhcpManager) containerID() string {
	m.ctrIDOnce.Do(func() {
		if m.docker == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		dockerNet, err := m.docker.NetworkInspect(ctx, m.joinReq.NetworkID, dNetwork.InspectOptions{})
		if err != nil {
			log.WithError(err).WithFields(m.logFields(false)).Debug("ledger container lookup failed")
			return
		}
		for ctrID, info := range dockerNet.Containers {
			if info.EndpointID == m.joinReq.EndpointID {
				m.ctrID = ctrID
				return
			}
		}
	})
	return m.ctrID
}

// endpointMAC returns the MAC the endpoint's DHCP identity is keyed to, from the join hint or the tombstone, falling
// back to the live link (#46).
func (m *dhcpManager) endpointMAC() net.HardwareAddr {
	if len(m.MacAddress) > 0 {
		return m.MacAddress
	}
	if m.ctrLink != nil {
		return m.ctrLink.Attrs().HardwareAddr
	}
	return nil
}

// clientID resolves the endpoint's option-61 identity, which must match the CreateEndpoint one-shot's or the server
// sees a different client; since #800 nothing is released, so a restarting container keeps its address only by
// presenting the same id (#371). recordIdentity, the record's option 61, wins over anything derived including
// client_id, via ipamExchangeClientID: in IPAM mode Docker mints a fresh MAC on restart, and a derived id got the
// reservation .10 while the client was NAKed onto .11 (#1047).
func (m *dhcpManager) clientID(recordIdentity []byte) []byte {
	return ipamExchangeClientID(resolveClientID(m.opts, m.joinReq.EndpointID, m.endpointMAC()), recordIdentity)
}

// macString returns the endpoint MAC for ledger entries: the located link's, else the join hint's.
func (m *dhcpManager) macString() string {
	if m.ctrLink != nil {
		if hw := m.ctrLink.Attrs().HardwareAddr; len(hw) > 0 {
			return hw.String()
		}
	}
	if len(m.MacAddress) > 0 {
		return m.MacAddress.String()
	}
	return ""
}

func bareIP(cidr string) string {
	if ip, _, err := net.ParseCIDR(cidr); err == nil {
		return ip.String()
	}
	return cidr
}

// findContainerPID returns the host PID and container ID behind this endpoint; the ID travels with the PID because
// the PID may be recycled before use, and openContainerProc checks the pair (#688).
func (m *dhcpManager) findContainerPID(ctx context.Context) (int, string, error) {
	dockerNet, err := m.docker.NetworkInspect(ctx, m.joinReq.NetworkID, dNetwork.InspectOptions{})
	if err != nil {
		return 0, "", fmt.Errorf("NetworkInspect: %w", err)
	}
	for ctrID, info := range dockerNet.Containers {
		if info.EndpointID != m.joinReq.EndpointID {
			continue
		}
		ins, err := m.docker.ContainerInspect(ctx, ctrID)
		if err != nil {
			return 0, "", fmt.Errorf("ContainerInspect(%s): %w", shortID(ctrID), err)
		}
		if ins.State == nil || ins.State.Pid == 0 {
			return 0, "", fmt.Errorf("container %s has no PID (state=%+v)", shortID(ctrID), ins.State)
		}
		return ins.State.Pid, ctrID, nil
	}
	return 0, "", fmt.Errorf("endpoint %s not found in network %s container list", shortID(m.joinReq.EndpointID), shortID(m.joinReq.NetworkID))
}

// renew applies one accepted lease to the container's netns; the phase order is documented at each call site.
func (m *dhcpManager) renew(v6 bool, info dhcp.Info) error {
	ip, err := netlink.ParseAddr(info.IP)
	if err != nil {
		return fmt.Errorf("failed to parse IP address: %w", err)
	}
	if v6 {
		v6AddrAttrs(ip, info.LeaseSeconds, info.PreferredSeconds, info.IPDeprecated)
	}

	// Address first, routes after: the kernel rejects a route with no address in its subnet.
	if err := m.applyAddressChange(v6, ip, info); err != nil {
		return err
	}

	m.logObservedOptions(v6, info)

	// Tracked after applyAddressChange, which needs the previous value, so Leave tombstones the current lease (#46).
	m.setLastIP(v6, ip)

	m.propagateDNS(v6, info)
	m.propagateMTU(v6, info)

	// The diff base is seeded from the Join answer's routes, which Docker installed before this manager started; left
	// nil, a first advertisement dropping one would withdraw nothing (RFC 4191 section 2.3, #821). RouteReplace makes
	// it a reconcile, a no-op when Join and the advertisement agree.
	if v6 {
		if err := m.reconcileAdvertisedRoutes(info); err != nil {
			log.WithError(err).WithFields(m.logFields(v6)).
				Warn("Failed to reconcile the routes the Router Advertisement asked for")
		}
	}

	return m.reconcileDefaultRoute(v6, info)
}

// applyAddressChange re-applies the lease when the server returned a different address; a no-op on steady renewals.
func (m *dhcpManager) applyAddressChange(v6 bool, ip *netlink.Addr, info dhcp.Info) error {
	v4, v6Last := m.lastIPs()
	lastIP := v4
	if v6 {
		lastIP = v6Last
	}
	changed := lastIP != nil && !ip.Equal(*lastIP)
	if v6 {
		return m.installV6Address(ip, lastIP, changed, info)
	}
	if !changed {
		return nil
	}

	// libnetwork has no endpoint-IP swap, so `docker inspect` reports the previous address until the container is
	// recreated; the counter lets operators alert on the gap (#104).
	if m.plugin != nil {
		bumpFamily(&m.plugin.leaseChangedV4, &m.plugin.leaseChangedV6, v6)
	}
	log.
		WithFields(m.logFields(v6)).
		WithField("old_ip", lastIP).
		WithField("new_ip", ip).
		Warn("dhcp renew with changed IP — Docker's view is now stale")

	// Without this the kernel keeps the original address after a renumber and the default-route replace fails with
	// "network is unreachable" (#128). Both families since #152: the one-shot and persistent clients share the DUID and
	// IAID, so a changed v6 address is a genuine renumber. The netHandle guard serves pre-Start unit tests.
	if m.netHandle == nil || m.ctrLink == nil {
		return nil
	}
	if err := nlHandleAddrReplace(m.netHandle, m.ctrLink, ip); err != nil {
		return fmt.Errorf("failed to apply re-acquired address %v: %w", ip, err)
	}
	routes, routesErr := m.linkRoutesV4()
	if err := m.netHandle.AddrDel(m.ctrLink, lastIP); err != nil {
		// Non-fatal: a stale address left behind is better than failing the bind.
		log.
			WithError(err).
			WithFields(m.logFields(v6)).
			WithField("stale_ip", lastIP).
			Warn("Failed to remove stale address after lease change")
		return nil
	}
	return m.recoverFlushedSecondary(ip, lastIP, routes, routesErr)
}

// recoverFlushedSecondary puts back what the kernel removed with the old address. With promote_secondaries=0 (the
// kernel default; the effective value is conf.all OR the link's) deleting a primary address deletes every secondary
// in its subnet, so a same-subnet new address goes too, and with it the default route and the option 121 routes
// Docker installed at Join, which renew does not re-install (#1081). A promote_secondaries=1 link keeps the new
// address and every route, and nothing is written.
func (m *dhcpManager) recoverFlushedSecondary(ip, lastIP *netlink.Addr, routes []netlink.Route, routesErr error) error {
	held, err := util.DumpResult(m.netHandle.AddrList(m.ctrLink, unix.AF_INET))
	if err != nil {
		return fmt.Errorf("failed to list addresses after removing %v: %w", lastIP, err)
	}
	for _, a := range held {
		if a.Equal(*ip) {
			return nil
		}
	}
	if routesErr != nil {
		log.WithError(routesErr).WithFields(m.logFields(false)).
			Warn("Could not list the link's routes before the renumber; the routes the kernel removed are not restored")
	}
	if err := nlHandleAddrReplace(m.netHandle, m.ctrLink, ip); err != nil {
		// The new address is gone too; the old one is better than none, as when the first write fails.
		if rerr := nlHandleAddrReplace(m.netHandle, m.ctrLink, lastIP); rerr == nil {
			m.restoreRoutes(routes)
		}
		return fmt.Errorf("failed to re-apply address %v after removing %v: %w", ip, lastIP, err)
	}
	m.restoreRoutes(routes)
	return nil
}

// linkRoutesV4 lists the link's IPv4 routes in every table, less the kernel's own, which come back with the address.
func (m *dhcpManager) linkRoutesV4() ([]netlink.Route, error) {
	all, err := util.DumpResult(m.netHandle.RouteListFiltered(unix.AF_INET, &netlink.Route{
		LinkIndex: m.ctrLink.Attrs().Index,
		Table:     unix.RT_TABLE_UNSPEC,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE))
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, r := range all {
		if r.Protocol != unix.RTPROT_KERNEL {
			out = append(out, r)
		}
	}
	return out, nil
}

// restoreRoutes re-installs routes listed before the address swap.
func (m *dhcpManager) restoreRoutes(routes []netlink.Route) {
	for i := range routes {
		if err := nlHandleRouteReplace(m.netHandle, &routes[i]); err != nil {
			log.WithError(err).WithFields(m.logFields(false)).WithField("route", routes[i].String()).
				Warn("Failed to restore a route the kernel removed with the old address")
		}
	}
}

// v6AddrAttrs sets IFA_F_NODAD and the two lifetimes on a DHCPv6 address (#911). NODAD because the library already
// ran duplicate address detection (RFC 9915 section 18.2.10.1); a second kernel run leaves the address tentative
// and can mark it dadfailed (RFC 7527 section 4.1). The library owns expiry; the kernel lifetimes cover a plugin
// down at expiry and express deprecation to sockets (RFC 4862 section 5.5.4). Both zero sends no IFA_CACHEINFO.
func v6AddrAttrs(addr *netlink.Addr, valid, preferred int, deprecated bool) {
	addr.Flags |= unix.IFA_F_NODAD
	addr.ValidLft = valid
	addr.PreferedLft = preferred
	// Once IFA_CACHEINFO is sent an infinite valid lifetime must be 0xFFFFFFFF: (valid 0, preferred 1800) is EINVAL,
	// and (valid 0, deprecated) would otherwise send nothing and install a preferred permanent address. RFC 4862
	// section 5.5.3 allows both shapes. Measured 2026-09-16 in a user namespace: (forever, 0) installs deprecated and
	// (forever, 3) is deprecated by the kernel 3 s later (#818, #819).
	if addr.ValidLft == 0 && (addr.PreferedLft > 0 || deprecated) {
		addr.ValidLft = infiniteLft
	}
}

// infiniteLft is the kernel's IFA_CACHEINFO "no expiry", unlike Info's zero; see v6AddrAttrs.
const infiniteLft = 0xFFFFFFFF

// installV6Address applies the DHCPv6 lease on every event carrying an address (#911). The first bind is not a
// no-op: libnetwork installed AddressIPv6 with forever lifetimes (engine 29.8.0 measured `flags 02`, NODAD too),
// and a renewal must refresh the lifetimes. AddrReplace is correct whether or not the address is present.
func (m *dhcpManager) installV6Address(ip, lastIP *netlink.Addr, changed bool, info dhcp.Info) error {
	if changed {
		// Docker still reports the previous address; libnetwork has no endpoint-IP swap (#104).
		if m.plugin != nil {
			bumpFamily(&m.plugin.leaseChangedV4, &m.plugin.leaseChangedV6, true)
		}
		log.
			WithFields(m.logFields(true)).
			WithField("old_ip", lastIP).
			WithField("new_ip", ip).
			Warn("dhcp renew with changed IP — Docker's view is now stale")
	}

	// The guard serves pre-Start unit tests; production renew always has a live transport and link.
	h := m.v6AddrTransport()
	if h == nil || m.ctrLink == nil {
		return nil
	}
	return m.applyV6Addrs(h, ip, info)
}

// v6AddrTransport returns the test seam, else netHandle wrapped in handleV6Addrs, else an untyped nil so the
// caller's `h == nil` guard holds (#818).
func (m *dhcpManager) v6AddrTransport() v6LinkAddrs {
	if m.v6Addrs != nil {
		return m.v6Addrs
	}
	if m.netHandle == nil {
		return nil
	}
	return handleV6Addrs{m.netHandle}
}

// v6LinkAddrs is the netlink transport for the v6 address set, a seam so tests drive the set logic without a link.
type v6LinkAddrs interface {
	AddrReplace(link netlink.Link, addr *netlink.Addr) error
	AddrDel(link netlink.Link, addr *netlink.Addr) error
}

// handleV6Addrs keeps address writes on nlHandleAddrReplace, the seam that injects netlink failures without
// CAP_NET_ADMIN (#818, #819).
type handleV6Addrs struct{ h *netlink.Handle }

func (a handleV6Addrs) AddrReplace(link netlink.Link, addr *netlink.Addr) error {
	return nlHandleAddrReplace(a.h, link, addr)
}

func (a handleV6Addrs) AddrDel(link netlink.Link, addr *netlink.Addr) error {
	return a.h.AddrDel(link, addr)
}

// applyV6Addrs installs every address the lease holds and removes every one it no longer holds.
func (m *dhcpManager) applyV6Addrs(h v6LinkAddrs, ip *netlink.Addr, info dhcp.Info) error {
	want, err := v6WantedAddrs(ip, info)
	if err != nil {
		return err
	}
	// Read before the loop: a renewal re-applies held addresses, so a set read afterwards counts refreshes as new.
	had := m.installedV6()
	for _, a := range want {
		if err := h.AddrReplace(m.ctrLink, a.addr); err != nil {
			return fmt.Errorf("failed to apply the IPv6 address %v: %w", a.addr, err)
		}
		if _, seen := had[a.key]; !seen && info.SLAAC && m.plugin != nil {
			m.plugin.ipv6SLAACAddresses.Add(1)
		}
	}
	m.withdrawV6AddrsNotIn(h, want, auditSource(info))
	return nil
}

type wantedV6Addr struct {
	addr *netlink.Addr
	key  string
}

// v6WantedAddrs lists the lease's addresses with their own lifetimes, the main address first so it is applied
// before any other can fail; a lease with no Addrs list yields its single address (#818).
func v6WantedAddrs(main *netlink.Addr, info dhcp.Info) ([]wantedV6Addr, error) {
	if len(info.Addrs) == 0 {
		return []wantedV6Addr{{addr: main, key: main.String()}}, nil
	}
	out := make([]wantedV6Addr, 0, len(info.Addrs))
	mainKey := main.String()
	for _, a := range info.Addrs {
		addr := main
		if a.IP != info.IP {
			parsed, err := netlink.ParseAddr(a.IP)
			if err != nil {
				return nil, fmt.Errorf("failed to parse the IPv6 address %q: %w", a.IP, err)
			}
			addr = parsed
		}
		v6AddrAttrs(addr, a.ValidSeconds, a.PreferredSeconds, a.Deprecated)
		w := wantedV6Addr{addr: addr, key: addr.String()}
		if w.key == mainKey {
			out = append([]wantedV6Addr{w}, out...)
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// withdrawV6AddrsNotIn removes every installed address the lease no longer holds, as a set difference: an expired
// valid lifetime (RFC 4862 section 5.5.4) or a withdrawn prefix can take one address while another stays, and an
// unrouted address would still be picked as a source (#818, #819). Failures are logged, never fatal.
//
// EADDRNOTAVAIL counts as withdrawn: the installed valid_lft is floored to whole seconds, so the kernel drops the
// address about a second before the library's deadline and this delete finds it gone (measured 2026-09-24, #1016).
func (m *dhcpManager) withdrawV6AddrsNotIn(h v6LinkAddrs, want []wantedV6Addr, source string) {
	for _, gone := range v6AddrsToWithdraw(m.installedV6(), want) {
		key, addr := gone.key, gone.addr
		m.forgetV6Addr(key)
		err := h.AddrDel(m.ctrLink, addr)
		kernelExpired := errors.Is(err, unix.EADDRNOTAVAIL)
		if err != nil && !kernelExpired {
			log.
				WithError(err).
				WithFields(m.logFields(true)).
				WithField("withdrawn_ip", key).
				Warn("Failed to remove an IPv6 address the lease no longer holds")
			continue
		}
		if m.plugin != nil {
			m.plugin.ipv6AddressesWithdrawn.Add(1)
		}
		m.auditFrom("withdrawn", bareIP(key), source)
		log.
			WithFields(m.logFields(true)).
			WithField("withdrawn_ip", key).
			WithField("source", source).
			WithField("kernel_expired", kernelExpired).
			Info("An IPv6 address left this endpoint's lease and was removed from the link")
	}
	for _, w := range want {
		m.rememberV6Addr(w.key, w.addr)
	}
}

// v6AddrsToWithdraw is installed minus held, sorted so ledger rows and logs come out in a stable order.
func v6AddrsToWithdraw(installed map[string]*netlink.Addr, want []wantedV6Addr) []wantedV6Addr {
	keep := make(map[string]bool, len(want))
	for _, w := range want {
		keep[w.key] = true
	}
	out := make([]wantedV6Addr, 0, len(installed))
	for key, addr := range installed {
		if keep[key] {
			continue
		}
		out = append(out, wantedV6Addr{addr: addr, key: key})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// installedV6 copies the installed set under ipMu for the caller to walk.
func (m *dhcpManager) installedV6() map[string]*netlink.Addr {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	out := make(map[string]*netlink.Addr, len(m.v6Installed))
	for k, v := range m.v6Installed {
		out[k] = v
	}
	return out
}

func (m *dhcpManager) rememberV6Addr(key string, addr *netlink.Addr) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	if m.v6Installed == nil {
		m.v6Installed = make(map[string]*netlink.Addr, 2)
	}
	m.v6Installed[key] = addr
}

func (m *dhcpManager) forgetV6Addr(key string) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	delete(m.v6Installed, key)
}

// logObservedOptions logs captured options the plugin does not apply, only when at least one is set.
func (m *dhcpManager) logObservedOptions(v6 bool, info dhcp.Info) {
	if len(info.NTPServers) == 0 && info.TFTPServer == "" && info.BootFile == "" && len(info.SearchList) == 0 &&
		info.WPAD == "" && info.PosixTimezone == "" && info.TZDBTimezone == "" && info.TimeOffset == "" {
		return
	}

	fields := m.logFields(v6)
	if len(info.NTPServers) > 0 {
		fields["ntp"] = info.NTPServers
	}
	if info.TFTPServer != "" {
		fields["tftp"] = info.TFTPServer
	}
	if info.BootFile != "" {
		fields["bootfile"] = info.BootFile
	}
	if len(info.SearchList) > 0 {
		fields["search"] = info.SearchList
	}
	// Observe-only extras (#262): WPAD URL (opt 252), RFC 4833 timezone (opt 100/101), time offset (opt 2).
	if info.WPAD != "" {
		fields["wpad"] = info.WPAD
	}
	if info.PosixTimezone != "" {
		fields["posix_tz"] = info.PosixTimezone
	}
	if info.TZDBTimezone != "" {
		fields["tzdb_tz"] = info.TZDBTimezone
	}
	if info.TimeOffset != "" {
		fields["time_offset"] = info.TimeOffset
	}
	log.WithFields(fields).Info("DHCP options received")
}

// propagateDNS applies option 6, or on v6 option 23 merged with RFC 8106 RDNSS and DNSSL (section 5.3.1), when
// opted in; it never fails the renewal. An empty list is a no-op, as RFC 8106 section 6.1 keeps resolvers past the
// router lifetime, and writeContainerResolvConf refuses a file with no nameserver. A shorter non-empty list is
// applied in full (RFC 4861 section 6.3.4, #821).
func (m *dhcpManager) propagateDNS(v6 bool, info dhcp.Info) {
	if !m.opts.PropagateDNS || len(info.DNSServers) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), dnsPropagateTimeout)
	pid, ctrID, err := m.findContainerPID(ctx)
	cancel()
	if err != nil {
		log.
			WithError(err).
			WithFields(m.logFields(v6)).
			Warn("Skipping DNS propagation — could not resolve container PID")
		return
	}

	// The container's link name, the zone a link-local resolver needs (see zonedNameserver); empty before Start.
	iface := ""
	if m.ctrLink != nil {
		iface = m.ctrLink.Attrs().Name
	}

	if err := writeContainerResolvConf(pid, ctrID, info.DNSServers, info.SearchList, info.Domain, iface); err != nil {
		m.noteDNSPropagationPIDMismatch(err)
		log.
			WithError(err).
			WithFields(m.logFields(v6)).
			WithField("dns", info.DNSServers).
			Error("Failed to write container resolv.conf")
		return
	}

	log.
		WithFields(m.logFields(v6)).
		WithField("dns", info.DNSServers).
		Debug("Propagated DHCP DNS servers to container resolv.conf")
}

// propagateMTU applies option 26, or on v6 the RFC 4861 section 4.6.4 MTU option; zero is skipped. The v6 MTU is
// not gated on propagate_mtu because the kernel applied it on every v6 network until #821 turned accept_ra off. It
// writes the link MTU, which bounds IPv4 too, and minPropagatedMTU still applies.
func (m *dhcpManager) propagateMTU(v6 bool, info dhcp.Info) {
	// A family whose option is off gets no vote, neither to raise the link nor to withdraw the other family's value.
	if !v6 && !m.opts.PropagateMTU {
		return
	}

	// Zero MTU without RouterSeen is silence, not a withdrawal: an RFC 9915 section 18.2.1 Solicit does not wait for
	// router discovery, so the first bound event can precede the first advertisement, and treating it as a withdrawal
	// flips the link between families (#821). RouterSeen with MTU 0 is a withdrawal.
	if v6 && !info.RouterSeen {
		return
	}

	// A zero is a withdrawal, recorded below the refusal: a family that stops supplying an MTU must drop its vote,
	// while a refused value leaves the previous vote standing (#821).
	if info.MTU > 0 && !mtuAcceptable(info.MTU) {
		if m.plugin != nil {
			m.plugin.mtuRefused.Add(1)
		}
		log.
			WithFields(m.logFields(v6)).
			WithField("mtu", info.MTU).
			WithField("min", minPropagatedMTU).
			WithField("max", maxPropagatedMTU).
			Warn("Refusing DHCP-supplied MTU outside the acceptable range; container link MTU unchanged")
		return
	}
	if m.netHandle == nil || m.ctrLink == nil {
		return
	}

	// Both families write one link MTU, so last-writer-wins flips it on every renewal when option 26 and the
	// advertisement differ. The smaller is correct for both: the larger is a promise the link cannot keep (#821).
	m.rememberMTU(v6, info.MTU, m.ctrLink.Attrs().MTU)
	want := m.wantedMTU()
	if want == 0 {
		// Neither family supplies an MTU any more: restore the link's MTU from before this manager wrote one (#821).
		want = m.baseMTU()
	}
	if want <= 0 {
		return
	}

	current := m.ctrLink.Attrs().MTU
	if current == want {
		return
	}

	if err := nlHandleLinkSetMTU(m.netHandle, m.ctrLink, want); err != nil {
		// Not fatal: the address and gateway work; the loud log surfaces a latent MTU black hole.
		log.
			WithError(err).
			WithFields(m.logFields(v6)).
			WithField("mtu", want).
			Error("Failed to apply DHCP-supplied MTU; container link MTU unchanged")
		return
	}

	log.
		WithFields(m.logFields(v6)).
		WithField("old_mtu", current).
		WithField("new_mtu", want).
		WithField("supplied_mtu", info.MTU).
		Info("Applied DHCP-supplied MTU")
}

// rememberMTU records one family's value, zero meaning withdrawn, and the link's own MTU on the first call, the
// only moment it is still the pre-write value (#821).
func (m *dhcpManager) rememberMTU(v6 bool, mtu, base int) {
	m.mtuMu.Lock()
	defer m.mtuMu.Unlock()
	if m.mtuBase == 0 {
		m.mtuBase = base
	}
	if v6 {
		m.mtuV6 = mtu
		return
	}
	m.mtuV4 = mtu
}

func (m *dhcpManager) baseMTU() int {
	m.mtuMu.Lock()
	defer m.mtuMu.Unlock()
	return m.mtuBase
}

func (m *dhcpManager) wantedMTU() int {
	m.mtuMu.Lock()
	defer m.mtuMu.Unlock()
	switch {
	case m.mtuV4 == 0:
		return m.mtuV6
	case m.mtuV6 == 0:
		return m.mtuV4
	case m.mtuV4 < m.mtuV6:
		return m.mtuV4
	default:
		return m.mtuV6
	}
}

// reconcileDefaultRoute points the v4 default route at the supplied gateway, unless the network pins a gateway.
func (m *dhcpManager) reconcileDefaultRoute(v6 bool, info dhcp.Info) error {
	if v6 {
		return m.reconcileV6DefaultRoute(info)
	}
	if info.Gateway == "" || m.opts.Gateway != "" {
		return nil
	}

	newGateway := net.ParseIP(info.Gateway)
	if newGateway == nil {
		// Recovery and replay build an Info with no exchange, and netlink reads `Gw: nil` as an on-link default route
		// (`default dev ethX scope link`), so a nil gateway leaves the existing route alone (#728).
		log.WithFields(m.logFields(v6)).
			WithField("gateway", info.Gateway).
			Warn("DHCP gateway is not an IP address; leaving the existing default route alone")
		return nil
	}

	routes, err := util.DumpResult(m.netHandle.RouteListFiltered(unix.AF_INET, &netlink.Route{
		LinkIndex: m.ctrLink.Attrs().Index,
		Dst:       nil,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST))
	if err != nil {
		return fmt.Errorf("failed to list routes: %w", err)
	}

	if len(routes) == 0 {
		log.
			WithFields(m.logFields(v6)).
			WithField("gateway", newGateway).
			Info("dhcp renew adding default route")

		if err := m.netHandle.RouteAdd(&netlink.Route{
			LinkIndex: m.ctrLink.Attrs().Index,
			Gw:        newGateway,
		}); err != nil {
			return fmt.Errorf("failed to add default route: %w", err)
		}
		return nil
	}

	if !newGateway.Equal(routes[0].Gw) {
		log.
			WithFields(m.logFields(v6)).
			WithField("old_gateway", routes[0].Gw).
			WithField("new_gateway", newGateway).
			Info("dhcp renew replacing default route")

		routes[0].Gw = newGateway
		if err := m.netHandle.RouteReplace(&routes[0]); err != nil {
			return fmt.Errorf("failed to replace default route: %w", err)
		}
	}

	return nil
}

// isDefaultRoute accepts both kernel spellings of a default route, a nil destination and an all-zero prefix.
func isDefaultRoute(r netlink.Route) bool {
	if r.Dst == nil {
		return true
	}
	ones, _ := r.Dst.Mask.Size()
	return ones == 0
}

// reconcileV6DefaultRoute installs and withdraws the v6 default route, which the plugin owns since accept_ra=0
// (#821). Router Lifetime 0 means "no longer a default router" (RFC 4861 sections 4.2 and 6.3.4); only default
// routes on this link that the kernel did not install (RTPROT_KERNEL) are deleted.
func (m *dhcpManager) reconcileV6DefaultRoute(info dhcp.Info) error {
	// The guard serves pre-Start unit tests, as in applyAddressChange.
	if m.netHandle == nil || m.ctrLink == nil {
		return nil
	}
	idx := m.ctrLink.Attrs().Index

	// Filtered on RT_FILTER_OIF only: the families return a default route's Dst differently, and a nil-Dst filter that
	// matched nothing would make the withdrawal a silent no-op (#821).
	routes, err := nlHandleRouteListFiltered(m.netHandle, unix.AF_INET6, &netlink.Route{
		LinkIndex: idx,
	}, netlink.RT_FILTER_OIF)
	if err != nil {
		return fmt.Errorf("failed to list IPv6 routes: %w", err)
	}

	var existing []netlink.Route
	for _, r := range routes {
		if isDefaultRoute(r) && r.Protocol != unix.RTPROT_KERNEL {
			existing = append(existing, r)
		}
	}

	if info.Gateway == "" {
		return m.withdrawV6DefaultRoute(existing)
	}

	gw := net.ParseIP(info.Gateway)
	if gw == nil || gw.To4() != nil {
		// netlink reads `Gw: nil` as an on-link default route, and a v4 address would install a route for the wrong family.
		log.WithFields(m.logFields(true)).
			WithField("gateway", info.Gateway).
			Warn("Advertised IPv6 gateway is not an IPv6 address; leaving the existing default route alone")
		return nil
	}

	if len(existing) == 0 {
		log.WithFields(m.logFields(true)).
			WithField("gateway", gw).
			Info("Adding the IPv6 default route the Router Advertisement asked for")
		if err := nlHandleRouteAdd(m.netHandle, &netlink.Route{LinkIndex: idx, Gw: gw}); err != nil {
			return fmt.Errorf("failed to add IPv6 default route: %w", err)
		}
		return nil
	}

	if gw.Equal(existing[0].Gw) && len(existing) == 1 {
		return nil
	}

	log.WithFields(m.logFields(true)).
		WithField("old_gateway", existing[0].Gw).
		WithField("new_gateway", gw).
		Info("Replacing the IPv6 default route: the Router Advertisement names a different router")
	existing[0].Gw = gw
	// Protocol zero, stamped RTPROT_BOOT like every plugin route: RTPROT_RA would make `ip -6 route` show "proto ra"
	// for a route the kernel did not learn, misleading the troubleshooting reading for two default routes (#821).
	existing[0].Protocol = 0
	if err := nlHandleRouteReplace(m.netHandle, &existing[0]); err != nil {
		return fmt.Errorf("failed to replace IPv6 default route: %w", err)
	}
	// A second default route on the link is a leftover: RouteReplace fixed the first, the rest go.
	for i := 1; i < len(existing); i++ {
		if err := nlHandleRouteDel(m.netHandle, &existing[i]); err != nil {
			log.WithError(err).WithFields(m.logFields(true)).
				WithField("gateway", existing[i].Gw).
				Warn("Failed to remove a second IPv6 default route")
		}
	}
	return nil
}

// withdrawV6DefaultRoute deletes the v6 default route and counts only a route that came off, since a shutting-down
// router sends several Router Lifetime 0 advertisements (RFC 4861 section 6.2.5, #821).
func (m *dhcpManager) withdrawV6DefaultRoute(existing []netlink.Route) error {
	if len(existing) == 0 {
		return nil
	}
	removed := 0
	var firstErr error
	for i := range existing {
		if err := nlHandleRouteDel(m.netHandle, &existing[i]); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to remove the withdrawn IPv6 default route: %w", err)
			}
			continue
		}
		removed++
		log.WithFields(m.logFields(true)).
			WithField("gateway", existing[i].Gw).
			Warn("The IPv6 router withdrew itself (Router Lifetime 0); removed the container's default route")
	}
	if removed > 0 && m.plugin != nil {
		m.plugin.ipv6RouterWithdrawn.Add(int32(removed))
	}
	return firstErr
}

// reconcileAdvertisedRoutes diffs the RFC 4191 Route Information routes against what this manager installed (see
// lastAdvertRoutes), removing a route no longer advertised (#821). On-link prefixes come from the Join answer only:
// the library's router table holds the latest frame, and one advertisement omitting a prefix must not remove it.
// skip_routes opts out here as it does at Join, for both the lease and the advertisement path (#1016).
func (m *dhcpManager) reconcileAdvertisedRoutes(info dhcp.Info) error {
	if m.netHandle == nil || m.ctrLink == nil || m.opts.SkipRoutes {
		return nil
	}
	idx := m.ctrLink.Attrs().Index

	want := make(map[string]string, len(info.Routes))
	for _, r := range info.Routes {
		want[r.Destination] = r.Gateway
	}

	var firstErr error
	for dest, gw := range m.lastAdvertRoutes {
		if _, still := want[dest]; still {
			continue
		}
		_, dst, err := net.ParseCIDR(dest)
		if err != nil {
			continue
		}
		route := &netlink.Route{LinkIndex: idx, Dst: dst}
		if gw != "" {
			route.Gw = net.ParseIP(gw)
		}
		if err := nlHandleRouteDel(m.netHandle, route); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to remove withdrawn route %v: %w", dest, err)
			}
			continue
		}
		log.WithFields(m.logFields(true)).WithField("route", dest).
			Info("The Router Advertisement stopped offering this route; removed it from the container")
	}

	for dest, gw := range want {
		if m.lastAdvertRoutes[dest] == gw {
			continue
		}
		_, dst, err := net.ParseCIDR(dest)
		if err != nil {
			log.WithFields(m.logFields(true)).WithField("route", dest).
				Warn("Advertised route destination is not a prefix; skipping it")
			delete(want, dest)
			continue
		}
		route := &netlink.Route{LinkIndex: idx, Dst: dst}
		if gw != "" {
			route.Gw = net.ParseIP(gw)
		}
		if err := nlHandleRouteReplace(m.netHandle, route); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to apply advertised route %v: %w", dest, err)
			}
			continue
		}
		log.WithFields(m.logFields(true)).WithField("route", dest).WithField("gateway", gw).
			Info("Applied a route the Router Advertisement asked for")
	}

	// Recorded even when a write failed: the record is what was asked of the kernel, and the next diff retries.
	m.lastAdvertRoutes = want
	return firstErr
}

// applyRouterAdvert re-applies what an advertisement can change without a lease event (router, MTU, routes, DNS);
// the chassis synthesises the event from the router table (raWatchInterval, #821). The address is not touched.
func (m *dhcpManager) applyRouterAdvert(info dhcp.Info) {
	m.propagateDNS(true, info)
	m.propagateMTU(true, info)
	if err := m.reconcileV6DefaultRoute(info); err != nil {
		log.WithError(err).WithFields(m.logFields(true)).
			WithField("gateway", info.Gateway).
			Error("Failed to apply the IPv6 default route from the Router Advertisement")
	}
	if err := m.reconcileAdvertisedRoutes(info); err != nil {
		log.WithError(err).WithFields(m.logFields(true)).
			Error("Failed to apply the routes from the Router Advertisement")
	}
}

func (m *dhcpManager) markBound(v6 bool) {
	if v6 {
		m.boundV6.Store(true)
		return
	}
	m.boundV4.Store(true)
}

// neverBound reports a family's client stopped without holding its binding; valid once its goroutine drained.
func (m *dhcpManager) neverBound(v6 bool) bool {
	if v6 {
		return !m.boundV6.Load()
	}
	return !m.boundV4.Load()
}

// bumpFamily increments exactly one of a v4/v6 counter pair: a total derived by subtracting two atomics can read
// lower than before, which Prometheus takes as a reset, while a sum of two monotonic counters stays monotonic
// (#212, #730).
func bumpFamily(v4Counter, v6Counter intCounter, v6 bool) {
	if v6 {
		v6Counter.Add(1)
		return
	}
	v4Counter.Add(1)
}

// clientServerLists returns the family's allow and deny lists; both dhcp_servers directives are v4-only, so a v6
// client is never restricted and dhcp_server_policy_timeouts needs no family split (#111).
func clientServerLists(pol serverPolicy, v6 bool) (allow, deny []string) {
	if v6 {
		return nil, nil
	}
	return pol.allowList(), pol.denyList()
}

// countOutageTick counts one outage tick; dhcp_server_policy_timeouts is a strict subset of dhcp_timeouts (#731).
func (m *dhcpManager) countOutageTick(v6, policyRestricted bool) {
	bumpFamily(&m.plugin.dhcpTimeoutsV4, &m.plugin.dhcpTimeoutsV6, v6)
	if !policyRestricted {
		return
	}
	// The renewal half of #731: a stale allow-list reads as a server outage without it. Not healthy-affecting, since
	// the tick was already counted above.
	m.plugin.dhcpServerPolicyTimeouts.Add(1)
}

// handleEvent dispatches one client event to counters, ledger and renew; unit-testable because dnsmasq ignores
// refused renewals instead of NAKing, so naks_received is pinned here (#128).
func (m *dhcpManager) handleEvent(event dhcp.Event, v6 bool) {
	m.noteEvent(event, v6)
	// pkg/dhcp already dropped these; counted for every event type, since the count describes the exchange (#703).
	if event.UnsafeValuesDropped > 0 && m.plugin != nil {
		m.plugin.unsafeOptionValuesDropped.Add(int32(event.UnsafeValuesDropped))
		log.
			WithFields(m.logFields(v6)).
			WithField("dropped", event.UnsafeValuesDropped).
			Warn("DHCP option values dropped before use: they carried control characters")
	}

	switch event.Type {
	// "deconfig" is ignored: deleting the address would also wipe the routes Join copied off the host bridge (#102).
	case "bound":
		// The first ACK can differ from the one-shot's (a Fritz.Box offers a fresh address per DISCOVER); see boundV4.
		m.markBound(v6)
		if m.plugin != nil {
			bumpFamily(&m.plugin.leasesObtainedV4, &m.plugin.leasesObtainedV6, v6)
		}
		m.auditFrom("bound", bareIP(event.Data.IP), auditSource(event.Data))
		if err := m.renew(v6, event.Data); err != nil {
			log.
				WithError(err).
				WithFields(m.logFields(v6)).
				WithField("ip", event.Data.IP).
				Error("Failed to record initial bind")
		}
	case "renew":
		log.
			WithFields(m.logFields(v6)).
			Debug("dhcp renew")

		// A client resuming a held lease can report renew without a bound, so renew marks bound too.
		m.markBound(v6)
		if m.plugin != nil {
			bumpFamily(&m.plugin.leasesRenewedV4, &m.plugin.leasesRenewedV6, v6)
		}
		m.auditFrom("renew", bareIP(event.Data.IP), auditSource(event.Data))
		if err := m.renew(v6, event.Data); err != nil {
			log.
				WithError(err).
				WithFields(m.logFields(v6)).
				WithField("gateway", event.Data.Gateway).
				WithField("new_ip", event.Data.IP).
				Error("Failed to execute IP renewal")
		}
	case "config":
		// A DHCPv6 information reply carries options and no address (#815): renew would fail on the empty Info.IP, and
		// it is not proof of a lease, so it neither marks bound nor resets the outage deadline.
		if m.plugin != nil {
			m.plugin.dhcpv6ConfigOnly.Add(1)
		}
		m.audit("config", "")
		m.logObservedOptions(v6, event.Data)
		m.propagateDNS(v6, event.Data)
		log.
			WithFields(m.logFields(v6)).
			WithField("dns", event.Data.DNSServers).
			WithField("search", event.Data.SearchList).
			Info("DHCPv6 configuration received without an address")
	case "routeradvert":
		// Advertised router state changed with no lease move (#821); it carries no address, so no markBound or
		// setLastIP.
		m.audit("routeradvert", "")
		m.logObservedOptions(v6, event.Data)
		m.applyRouterAdvert(event.Data)
		log.
			WithFields(m.logFields(v6)).
			WithField("gateway", event.Data.Gateway).
			WithField("mtu", event.Data.MTU).
			WithField("dns", event.Data.DNSServers).
			WithField("routes", event.Data.Routes).
			Info("Router Advertisement changed; re-applying the container's IPv6 configuration")
	case "slaac_lost":
		// Every formed address comes off: the kernel's own valid lifetimes would drop them later, but a prefix no
		// longer held should stop being chosen as a source now (#818, #819). The outage counters are not touched.
		if h := m.v6AddrTransport(); h != nil && m.ctrLink != nil {
			m.withdrawV6AddrsNotIn(h, nil, auditSource(event.Data))
		}
		log.
			WithFields(m.logFields(v6)).
			WithField("ip", event.Data.IP).
			Warn("This endpoint's IPv6 addresses were formed from a router advertisement and the client no longer holds them")
	case "leasefail":
		// dhcp_timeouts from the library's Failed{ReasonNoServer}, through countOutageTick to keep the policy subset.
		if m.plugin != nil {
			m.countOutageTick(v6, m.policyRestricted)
		}
		log.WithFields(m.logFields(v6)).Warn("dhcp failed to get a lease")
	case "nak":
		if m.plugin != nil {
			bumpFamily(&m.plugin.naksReceivedV4, &m.plugin.naksReceivedV6, v6)
		}
		log.WithFields(m.logFields(v6)).Warn("dhcp client received NAK")
	}
}

// startDHCPClient opens the persistent client's socket; the seam lets a test observe that no daemon call precedes
// the client start, an instant a count at Start's return cannot see (#961).
var startDHCPClient = func(c *dhcp.DHCPClient) (chan dhcp.Event, error) { return c.Start() }

// newDHCPClient prepares the persistent client without opening anything, so a test reads what it was given (#1050).
var newDHCPClient = dhcp.NewDHCPClient

func (m *dhcpManager) setupClient(v6 bool) (chan error, error) {
	v6Str := ""
	if v6 {
		v6Str = "v6"
	}

	log.
		WithFields(m.logFields(v6)).
		Info("Starting persistent DHCP client")

	// The link name is re-read by index here: the engine renames the link after moving it, and the hostname inspect
	// before this can span the rename on a busy daemon (#406). Hosted run 34624582681 opened a stale name
	// dh-3b1d3b0061fd and left the container with no renewal client. A failed read keeps the snapshot (#417).
	if m.netHandle != nil && m.ctrLink != nil {
		if link, err := nlLinkByIndex(m.netHandle, m.ctrLink.Attrs().Index); err != nil {
			log.
				WithError(err).
				WithFields(m.logFields(v6)).
				Debug("re-reading the endpoint's link before opening the client failed")
		} else {
			m.ctrLink = link
		}
	}

	// An unexpired record lease makes the first packet an INIT-REBOOT DHCPREQUEST (RFC 2131 section 4.4.2), keeping
	// the IP across a plugin restart; a preferred-only address goes as option 50 in a DISCOVER, which the server may
	// ignore (section 4.4.1). Record.Prefer refuses what Record.Resume answers (#899). lastIPs() is the weaker fallback
	// for an endpoint adopted from Docker's view, with no expiry to judge an INIT-REBOOT by.
	requestedIP := ""
	preferredV6 := ""
	var (
		resumption dhcp.Resumption
		identity6  dhcp.Identity6
		recordID   string
		// The v4 record's option-61 identity only: a v6 identity is a DUID with an IAID, read through identity6 (#911).
		v4Identity []byte
	)
	if !v6 {
		m.recordID, resumption = m.resumeFromRecord()
		recordID = m.recordID
		requestedIP = resumption.Prefer
		v4Identity = resumption.Identity
		if resumption.Lease == nil && requestedIP == "" {
			if v4Addr, _ := m.lastIPs(); v4Addr != nil && v4Addr.IP != nil {
				requestedIP = v4Addr.IP.String()
			}
		}
	} else {
		// The v6 record gives both the preferred address and the DUID; RFC 9915 section 18.2.12's Confirm needs the
		// DUID the binding was made with (#911).
		m.recordID6, resumption, identity6 = m.resumeFromRecord6()
		recordID = m.recordID6
		preferredV6 = resumption.Prefer
		if resumption.Lease == nil && preferredV6 == "" {
			if _, v6Addr := m.lastIPs(); v6Addr != nil && v6Addr.IP != nil {
				preferredV6 = v6Addr.IP.String()
			}
		}
		if identity6.IsZero() {
			// No record or no identity: minting a DUID makes a new client and address, still better than refusing, so it warns.
			id6, err := resolveIdentity6(m.opts, m.joinReq.EndpointID, m.endpointMAC())
			if err != nil {
				return nil, fmt.Errorf("no DHCPv6 identity for this endpoint: %w", err)
			}
			identity6 = id6
			log.WithFields(m.logFields(true)).
				Warn("No stored DHCPv6 identity for this endpoint; minting one. The server sees a new client and will grant a new address")
		}
	}
	// The persistent client gets the whole allowed set so it can rebind after the preferred server goes; preference is
	// an acquisition concept (#111). v6 gets no lists. An error means corrupt persisted state, so refuse rather than
	// start unrestricted and ignore a deny-list.
	pol, err := resolveServerPolicy(m.opts)
	if err != nil {
		return nil, fmt.Errorf("invalid persisted DHCP server policy: %w", err)
	}
	allowServers, denyServers := clientServerLists(pol, v6)

	// Captured once, so the counter describes the policy the client was actually started with.
	m.policyRestricted = len(allowServers) > 0

	clientOpts := dhcp.DHCPClientOptions{
		Hostname:     m.hostnameOnTheWire(),
		AllowServers: allowServers,
		DenyServers:  denyServers,
		FQDN:         m.opts.fqdnMode(),
		V6:           v6,
		NetNS:        &m.nsHandle,
		// The link by index, which the engine does not change; the name is resolved again inside the namespace (#1050).
		LinkIndex: m.ctrLink.Attrs().Index,
		// The one-shot's MAC, so chaddr and client-id match and the server renews the lease Docker was told about
		// (#152).
		MAC:         m.ctrLink.Attrs().HardwareAddr,
		RequestedIP: requestedIP,
		// The record's unexpired lease, making the first packet an INIT-REBOOT; nil when there is nothing to resume.
		Resume:   resumption.Lease,
		Records:  m.recordStore(),
		RecordID: recordID,
		// No Broadcast option: the library sets RFC 2131 section 2's BROADCAST flag by default on its AF_PACKET socket,
		// which also covers ipvlan slaves sharing the parent MAC (#243). The client-id is the record's identity, else
		// derived from the one-shot's MAC honouring client_id (#371).
		ClientID:    m.clientID(v4Identity),
		VendorClass: m.opts.VendorClass,
		// HonorRouterAdverts is required on a persistent v6 client and refused elsewhere (#875, D30 Q3).
		HonorRouterAdverts: v6,
	}
	if v6 {
		// The v6 record id is restated; what this adds is the mode, which renewals and rebinds run under (#817).
		if err := m.plugin.v6Wiring(&clientOpts, m.opts, identity6, recordID, preferredV6, m.joinReq.EndpointID); err != nil {
			return nil, err
		}
	}
	if err := m.plugin.conflictWiring(&clientOpts, m.opts, roleJoin, m.joinReq.NetworkID, m.joinReq.EndpointID, v6); err != nil {
		return nil, err
	}
	// Only the persistent client holds a lease long enough to renew; both roleAcquire sites are one-shots (#940).
	m.plugin.renewalWiring(&clientOpts, m.joinReq.NetworkID, m.joinReq.EndpointID, v6)
	// The phase is not passed on: proto.Machine re-runs RFC 5227 section 2.1's check on the INIT-REBOOT ACK anyway
	// (D23); the durable phase only feeds the warning below.
	m.noteResumedACD(resumption, clientOpts.ConflictMode, v6)

	client, err := newDHCPClient(m.ctrLink.Attrs().Name, &clientOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP%v client: %w", v6Str, err)
	}
	if !v6 {
		m.setHealthClient(client)
	}
	events, err := startDHCPClient(client)
	if err != nil {
		return nil, fmt.Errorf("failed to start DHCP%v client: %w", v6Str, err)
	}

	// Buffered: a partial Start failure skips Stop's errChan read, and the final write must not block (#330).
	errChan := make(chan error, 1)
	go func() {
		for {
			select {
			case event, ok := <-events:
				if !ok {
					// The chassis closed the channel; without this return the loop spins on zero Events.
					log.
						WithFields(m.logFields(v6)).
						Warn("dhcp event stream closed; the renewal client stopped")

					// Wait is the only sign Run returned and its AF_PACKET socket closed; without it the next Join
					// can open a second client on the interface (#901).
					reapCtx, reapCancel := context.WithTimeout(context.Background(), dhcpClientReapTimeout)
					if err := client.Wait(reapCtx); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							Debug("waiting for the renewal client returned an error")
					}
					reapCancel()

					// errChan is buffered, so this never blocks; Stop reads the value later.
					errChan <- nil
					return
				}
				m.handleEvent(event, v6)

			case <-m.stopChan:
				log.
					WithFields(m.logFields(v6)).
					Info("Shutting down persistent DHCP client")

				ctx, cancel := context.WithTimeout(context.Background(), dhcpClientFinishTimeout)
				defer cancel()

				errChan <- client.Finish(ctx)
				return
			}
		}
	}()

	return errChan, nil
}

// locateContainerLink finds the moved link in the sandbox: bridge by the host veth's peer index after Docker's
// rename, macvlan and ipvlan by MAC (#125); an ipvlan child's parent is outside the netns, so its MAC is unique.
func (m *dhcpManager) locateContainerLink(ctx context.Context) error {
	if mode := m.opts.effectiveMode(); mode == ModeMacvlan || mode == ModeIPvlan {
		if len(m.MacAddress) == 0 {
			return fmt.Errorf("%v mode but no MAC address recorded for endpoint", mode)
		}

		awaitCtx, cancel := context.WithTimeout(ctx, linkAwaitTimeout)
		defer cancel()
		return util.AwaitCondition(awaitCtx, func() (bool, error) {
			link, err := findLinkByMAC(m.netHandle, m.MacAddress)
			if err != nil {
				return false, nil
			}
			m.ctrLink = link
			return true, nil
		}, pollTime)
	}

	hostName, oldCtrName := vethPairNames(m.joinReq.EndpointID)
	// Through the guarded seam: a displaced manager's rename can land between this lookup's kernel calls (#1051).
	hostLink, err := hostLinkByGeneratedName(hostName)
	if err != nil {
		return fmt.Errorf("failed to find host side of veth pair: %w", err)
	}
	hostVeth, ok := hostLink.(*netlink.Veth)
	if !ok {
		return util.ErrNotVEth
	}

	ctrIndex, err := netlink.VethPeerIndex(hostVeth)
	if err != nil {
		return fmt.Errorf("failed to get container side of veth's index: %w", err)
	}

	return util.AwaitCondition(ctx, func() (bool, error) {
		m.ctrLink, err = util.AwaitLinkByIndex(ctx, m.netHandle, ctrIndex, pollTime)
		if err != nil {
			return false, fmt.Errorf("failed to get link for container side of veth pair: %w", err)
		}
		return m.ctrLink.Attrs().Name != oldCtrName, nil
	}, pollTime)
}

// joinPhases times each stage of Start so an expired budget shows where it went: a slow daemon and an earlier
// phase consuming the budget both read "context deadline exceeded" and want opposite fixes (#401, #406). A log
// field, not a health counter.
type joinPhases struct {
	start time.Time
	last  time.Time
	spans []joinPhaseSpan
}

type joinPhaseSpan struct {
	name string
	took time.Duration
}

func newJoinPhases() *joinPhases {
	now := time.Now()
	return &joinPhases{start: now, last: now}
}

func (p *joinPhases) mark(name string) {
	if p == nil {
		return
	}
	now := time.Now()
	p.spans = append(p.spans, joinPhaseSpan{name: name, took: now.Sub(p.last)})
	p.last = now
}

// summary renders the phases as a log field, e.g. "resolve_id=8.9s inspect=1.1s".
func (p *joinPhases) summary() string {
	if p == nil || len(p.spans) == 0 {
		return "(no phase completed)"
	}
	parts := make([]string, 0, len(p.spans))
	for _, s := range p.spans {
		parts = append(parts, fmt.Sprintf("%s=%.2fs", s.name, s.took.Seconds()))
	}
	return strings.Join(parts, " ")
}

func (p *joinPhases) total() time.Duration {
	if p == nil {
		return 0
	}
	return time.Since(p.start)
}

// openSandboxNetNS opens the netns via the sandbox key, falling back to the PID, and counts which (#725, #691):
// sandbox_key_entries and sandbox_pid_fallbacks together show every open took the key, a single counter cannot.
// Counted here so no caller can skip it; netns_pid_mismatches is the only PID-reuse signal (#731).
func (m *dhcpManager) openSandboxNetNS(ctx context.Context, sandboxKey string, pid int, ctrID string, interval time.Duration) (netns.NsHandle, error) {
	return m.openSandboxNetNSLazyPID(ctx, sandboxKey, interval, func() (int, string, error) {
		return pid, ctrID, nil
	})
}

// openSandboxNetNSLazyPID resolves the PID only if the key route is refused: the daemon may be inside ContainerStart
// for this container (#406), so the key route makes no daemon call (#417). A PID error is reported with the key's.
func (m *dhcpManager) openSandboxNetNSLazyPID(ctx context.Context, sandboxKey string, interval time.Duration, resolvePID func() (int, string, error)) (netns.NsHandle, error) {
	ns, keyErr := awaitSandboxNetNSByKey(ctx, sandboxKey, interval)
	if keyErr == nil {
		if m.plugin != nil {
			m.plugin.sandboxKeyEntries.Add(1)
		}
		return ns, nil
	}
	if m.plugin != nil {
		m.plugin.sandboxKeyEntryFailures.Add(1)
		m.plugin.countSandboxKeyRefusal(keyErr)
	}
	// Debug, not Warn: with a private sandbox netns mount (sandbox_netns_propagation=0) this fires on every attach of
	// a healthy host, since the daemon's per-sandbox mounts do not propagate into the plugin's bind (#417). The route
	// counters on /Plugin.Health and /metrics carry the signal (#725).
	pid, ctrID, pidErr := resolvePID()
	if pidErr != nil {
		return netns.None(), fmt.Errorf("%w (sandbox key route: %w)", pidErr, keyErr)
	}

	log.WithError(keyErr).WithFields(log.Fields{
		"sandbox": sandboxKey,
		"pid":     pid,
	}).Debug("Entering the sandbox through its netns key was refused; the container PID route carries this attach")

	ns, err := awaitContainerNetNS(ctx, pid, ctrID, interval)
	if errors.Is(err, errPIDNotContainer) && m.plugin != nil {
		m.plugin.netnsPIDMismatches.Add(1)
	}
	if err != nil {
		// Both routes failed; the key error explains why the fallback ran, so it is reported too.
		return ns, fmt.Errorf("%w (sandbox key route: %w)", err, keyErr)
	}
	if m.plugin != nil {
		m.plugin.sandboxPIDFallbacks.Add(1)
	}
	return ns, nil
}

func (m *dhcpManager) Start(ctx context.Context) (err error) {
	phases := newJoinPhases()
	defer func() {
		m.startErr = err
		// Recorded on the manager and folded onto the failure line, since the health floor's evidence dump shows only
		// error and warning lines (#406, #411).
		m.startPhases = phases.summary()
		m.startTotal = phases.total().Round(10 * time.Millisecond).String()
		close(m.startedCh)
	}()
	// Netns, link and persistent client start without the daemon; the option-12 inspect runs after the client starts,
	// which then renews early to send the name (RFC 2131 section 4.4.5, #417, #961). The daemon is inside
	// ContainerStart for this container (#406), so attachDaemonBusyGrace and joinAttachSlow still apply after the
	// start. A hint-less Join on `docker restart` replays CreateEndpoint and pays the wait in Join, pinned by
	// TestReacquireEndpoint_AsksTheDaemonBeforeTheAttachBegins. The PID fallback and register_dns take the name before
	// the start: option 81 excludes option 12 (RFC 4702 section 3.1) and the library has no setter for it.
	var (
		ctrID         string
		ctrPID        int
		ctrName       string
		ctrHostname   string
		ctrSandboxKey string
		inspected     bool
	)
	// inspect reads the container's attach fields once; the PID fallback and the hostname share the answer.
	inspect := func() error {
		if inspected {
			return nil
		}
		if err := util.AwaitCondition(ctx, func() (bool, error) {
			dockerNet, err := m.docker.NetworkInspect(ctx, m.joinReq.NetworkID, dNetwork.InspectOptions{})
			if err != nil {
				return false, fmt.Errorf("failed to get Docker network info: %w", err)
			}

			for id, info := range dockerNet.Containers {
				if info.EndpointID == m.joinReq.EndpointID {
					ctrID = id
					break
				}
			}
			if ctrID == "" {
				return false, util.ErrNoContainer
			}

			return !strings.HasPrefix(ctrID, "ep-"), nil
		}, pollTime); err != nil {
			return err
		}
		phases.mark("resolve_container_id")

		ctr, err := util.AwaitContainerInspect(ctx, m.docker, ctrID, pollTime)
		if err != nil {
			return fmt.Errorf("failed to get Docker container info: %w", err)
		}

		phases.mark("inspect_container")

		// Each field is guarded: an inspect missing a section must fail the attach, not panic the plugin.
		if ctr.State != nil {
			ctrPID = ctr.State.Pid
		}
		ctrName = ctr.Name
		if ctr.Config != nil {
			ctrHostname = ctr.Config.Hostname
		}
		if ctr.NetworkSettings != nil {
			ctrSandboxKey = ctr.NetworkSettings.SandboxKey
		}
		inspected = true
		return nil
	}

	// Recovery's synthesised request has no sandbox key, so that path inspects first and reads the daemon's current
	// key (#725).
	sandboxKey := m.joinReq.SandboxKey
	if sandboxKey != "" {
		m.nsHandle, err = m.openSandboxNetNSLazyPID(ctx, sandboxKey, pollTime, func() (int, string, error) {
			if err := inspect(); err != nil {
				return 0, "", err
			}
			return ctrPID, ctrID, nil
		})
	} else {
		if err = inspect(); err != nil {
			return err
		}
		m.nsHandle, err = m.openSandboxNetNS(ctx, ctrSandboxKey, ctrPID, ctrID, pollTime)
	}
	if err != nil {
		return fmt.Errorf("failed to get sandbox network namespace: %w", err)
	}

	phases.mark("open_netns")

	m.netHandle, err = nlNewHandleAt(m.nsHandle)
	if err != nil {
		closeNsHandle(m.nsHandle)
		return fmt.Errorf("failed to open netlink handle in sandbox namespace: %w", err)
	}

	if err := func() error {
		if err := m.locateContainerLink(ctx); err != nil {
			// A missing link is either a late or gone container (a fault) or an endpoint no container claimed (not
			// one, #566). Only the daemon tells them apart, so it is asked once here within the remaining budget;
			// otherwise an unclaimed endpoint would count as a Healthy-affecting join_start_failure.
			if ierr := inspect(); ierr != nil {
				return fmt.Errorf("%w (no link for this endpoint in the sandbox: %w)", ierr, err)
			}
			return err
		}

		phases.mark("locate_link")

		// The name before the start only when already inspected, or for register_dns, whose option 81 has no setter
		// (#961). A refused name just omits the option.
		if inspected || m.opts.fqdnMode() != "" {
			if err := inspect(); err != nil {
				return err
			}
			m.setHostname(m.plugin.safeHostname(ctrHostname).name)
		}

		if m.errChan, err = m.setupClient(false); err != nil {
			close(m.stopChan)
			return err
		}

		if m.opts.ipv6Enabled() {
			// The engine disables IPv6 on a sandbox link whose endpoint has no v6 address (#868); clear it before the
			// link-local wait, which would otherwise time out. See v6_link.go.
			m.ensureIPv6Enabled()

			// The link-local wait is the library's: runtime.InterfaceLinkLocal refuses a tentative or dad-failed
			// address and waits its own bound, since a tentative LL cannot receive the server's unicast reply (#103).
			// A second wait here cost 14 s of the Join deadline for the same refusal (#911);
			// TestTheChassisDoesNotWaitForALinkLocalItself holds it.
			if m.errChanV6, err = m.setupClient(true); err != nil {
				close(m.stopChan)
				// The v4 goroutine may be mid-renew on m.netHandle; drain its exit so the handles are not closed under it.
				<-m.errChan
				return err
			}
		}

		phases.mark("start_clients")
		return nil
	}(); err != nil {
		closeNetHandle(m.netHandle)
		closeNsHandle(m.nsHandle)
		return err
	}

	// After a successful attach a missing name is counted and logged, not a reason to tear the endpoint down (#961,
	// #978).
	m.afterAttach(phases, inspected, inspect, &ctrName, &ctrHostname)

	return nil
}

// nameTheRunningClient gives the leasing v4 client the container's name through the attach's own inspect (#961).
// It returns whether the daemon answered, which #978's host link rename needs; every arm is counted, none fails.
func (m *dhcpManager) nameTheRunningClient(phases *joinPhases, lookup func() error, name *string) bool {
	if err := lookup(); err != nil {
		m.plugin.hostnameLookupFailures.Add(1)
		log.WithError(err).
			WithFields(m.logFields(false)).
			Warn("The container's name could not be read from the daemon; this endpoint holds its lease but the DHCP server's table has no name for it")
		return false
	}
	phases.mark("hostname")

	// An empty name (absent or refused) is not handed over: the client already sends no option 12, and handing it
	// over would raise hostnames_applied_late for every container without --hostname (#961).
	safe := m.plugin.safeHostname(*name)
	if safe.name == "" {
		return true
	}

	client := m.healthClient()
	if client == nil {
		m.plugin.hostnameApplyFailures.Add(1)
		log.WithFields(m.logFields(false)).
			Warn("No running DHCP client to give the container's name to; the DHCP server's table has no name for this endpoint")
		return true
	}
	if err := client.SetHostname(safe.name); err != nil {
		m.plugin.hostnameApplyFailures.Add(1)
		log.WithError(err).
			WithFields(m.logFields(false)).
			Warn("The running DHCP client would not take the container's name; the DHCP server's table has no name for this endpoint")
		return true
	}
	// Recorded after the handover, so the ledger never names an endpoint the server was not told about (#961).
	m.setHostname(safe.name)
	m.plugin.hostnamesAppliedLate.Add(1)
	log.WithFields(m.logFields(false)).
		WithField("hostname", safe.name).
		Info("The container's name was given to the running DHCP client, which asks the server to record it at once")
	return true
}

// Stop shuts the persistent clients down for an endpoint whose container keeps running, and never releases (#962).
func (m *dhcpManager) Stop() error {
	return m.stop(false)
}

// StopForLeave is Stop for an endpoint being torn down, the only path that may release the lease (#962).
func (m *dhcpManager) StopForLeave() error {
	return m.stop(true)
}

func (m *dhcpManager) stop(leaving bool) error {
	// Abort a running attach first, or every Leave during one waits out the attach grace (#406).
	if m.attachCancel != nil {
		m.attachAborted.Store(true)
		m.attachCancel()
	}
	<-m.startedCh

	// The release happens only when leaving (#962): Close, a displaced manager and network cleanup arrive with the
	// container still running, and releasing then invites the duplicate assignment #524 detects. It sits above the
	// startErr return, since the record holds the one-shot's address even if Start failed, and before the teardown,
	// since removing the v6 address needs the netns still open (RFC 9915 section 18.2.7).
	if leaving && m.opts.releasesOnStop() {
		releasedV4, releasedV6 := m.releaseHeldLeases()
		m.releasedV4.Store(releasedV4)
		m.releasedV6.Store(releasedV6)
	}

	// `on_remove` releases nothing at stop: the sweep releases once the restart window has run out (#984).
	if leaving && m.opts.releasesOnRemove() {
		m.announceDeferredRelease()
	}

	if m.startErr != nil {
		// No persistent client ran; the one-shot's lease expires on its own (#800). Reclaiming it raced `docker
		// restart`, a Leave then Join for the same MAC whose tombstone promises the same address (#46). An on_stop
		// network already released from the record above this return (#962).
		if v4, v6 := m.lastIPs(); v4 != nil || v6 != nil {
			log.WithFields(m.logFields(false)).
				WithField("ip", auditIP(v4)).
				WithField("ipv6", auditIP(v6)).
				Info("Start failed with the one-shot's lease outstanding; " +
					"leaving it to expire on the server, as it would for any " +
					"other host on the segment")
		}
		return nil
	}

	// Start may have failed before the handles were set, and closing a zero handle logs EBADF.
	defer func() {
		if m.nsHandle.IsOpen() {
			closeNsHandle(m.nsHandle)
		}
	}()
	defer func() {
		if m.netHandle != nil {
			closeNetHandle(m.netHandle)
		}
	}()

	close(m.stopChan)

	// Drain both consumer goroutines before the deferred closes: netlink Handle.Close is unsynchronised with requests.
	lastIP, lastIPv6 := m.lastIPs()
	errV4 := <-m.errChan
	var errV6 error
	if m.opts.ipv6Enabled() {
		errV6 = <-m.errChanV6
	}

	// Whether the client ever bound decides what the stop meant, not its exit error: a client stopped before binding
	// returns the library's cancellation error, and testing errV4 first counted that as a fault (#607, #549). The
	// flags are safe to read after each goroutine's final send; v6 follows the same rule (#608).
	neverBoundV4 := m.settleFamily(false, lastIP, errV4, leaving)
	neverBoundV6 := false
	if m.opts.ipv6Enabled() {
		neverBoundV6 = m.settleFamily(true, lastIPv6, errV6, leaving)
	}
	if neverBoundV4 || neverBoundV6 {
		// A one-shot lease is still outstanding for a family and expires on the server's clock (#800); nothing is
		// audited, since no RELEASE was sent. An on_stop network already released it from the record (#962).
		log.WithFields(m.logFields(false)).
			WithField("v4_outstanding", neverBoundV4).
			WithField("v6_outstanding", neverBoundV6).
			Info("a client was signalled before it bound; the one-shot's lease " +
				"is left to expire on the server")
	}

	// A client that never bound reports no error; returning its exit status made Leave answer 500 (#607).
	if errV4 != nil && !neverBoundV4 {
		return fmt.Errorf("failed shut down DHCP client: %w", errV4)
	}
	if errV6 != nil && !neverBoundV6 {
		return fmt.Errorf("failed shut down DHCPv6 client: %w", errV6)
	}
	return nil
}

// settleFamily records one family's stop in the ledger, counters and log, and reports whether the one-shot's lease
// is still outstanding (#607, #608).
func (m *dhcpManager) settleFamily(v6 bool, last *netlink.Addr, exitErr error, leaving bool) bool {
	neverBound := m.neverBound(v6)
	neverBoundLog := log.WithFields(m.logFields(v6)).WithField("ip", auditIP(last))
	if neverBound && exitErr != nil {
		// Expected, logged at Debug: the lease expires on the server's clock (#800).
		neverBoundLog = neverBoundLog.WithField("client_exit", exitErr)
	}
	switch {
	case neverBound && leaving:
		// Nothing is audited as released on this path; an on_stop network's release at Leave is written by the release
		// path (#962). TestStop_NoStopPathClaimsAReclaimOrRelease holds it.
		neverBoundLog.Info("Persistent client stopped before it ever held the lease; " +
			"the one-shot's lease is left to expire on the server")
	case neverBound:
		// Not leaving, so a running container may still hold the address: nothing is released or audited as released.
		neverBoundLog.Debug("Persistent client stopped before it held the lease; " +
			"the endpoint is not leaving")
	case exitErr != nil:
		// A client that held a binding and did not stop cleanly: killed, timed out or failed. The counter and the
		// "stop_failed" kind name the client, not the lease, which is held to expiry either way (#800); split by family
		// and audited per family (#608).
		if m.plugin != nil {
			bumpFamily(&m.plugin.clientStopFailuresV4, &m.plugin.clientStopFailuresV6, v6)
		}
		m.audit("stop_failed", auditIP(last))
	default:
		m.audit("stopped", auditIP(last))
	}
	return neverBound
}

// auditIP renders a netlink address for the ledger; a nil embedded IPNet is checked first, since the promoted IP
// field would panic (#109).
func auditIP(addr *netlink.Addr) string {
	if addr == nil || addr.IPNet == nil || addr.IP == nil {
		return ""
	}
	return addr.IP.String()
}
