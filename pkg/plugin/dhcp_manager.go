// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	dNetwork "github.com/docker/docker/api/types/network"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// linkAwaitTimeout caps how long Start waits for the macvlan child to
// reappear in the container netns under its post-rename name. Bridge mode
// keys off the veth peer index, which is symmetric across netns and
// available immediately, so it doesn't need this.
//
// A var and not a const so a drive can shrink it, the way
// attachDaemonBusyGrace is shrunk: the behaviour that follows a link
// which never appears cannot be driven root-free in 30 seconds, and a
// drive that cannot run is a property nobody asserts. Production never
// writes it.
var linkAwaitTimeout = 30 * time.Second

const pollTime = 100 * time.Millisecond

// dhcpClientReapTimeout caps how long the event consumer waits for a
// self-stopped client to finish unwinding.
//
// The name is a fossil and the budget is not. It was the wait to reap a
// dhcpcd child process before letting it linger as a zombie; there is no
// child process now, and what the wait is for is stated at its call
// site: Wait is the only thing that says the library's Run has RETURNED
// and its AF_PACKET socket is closed. Give up too early and a Join for
// the next container can open a second client on the same interface
// while this one is still on it.
const dhcpClientReapTimeout = 5 * time.Second

// dhcpClientFinishTimeout caps how long Stop waits for the persistent
// client to unwind and return.
//
// Two things it no longer covers, in the order they went. Before #800 it
// covered a DHCPRELEASE round trip; a release now happens before this
// wait begins, on its own budget, and only on a `release_lease=on_stop`
// network (#962), so by the time Stop waits the client has nothing to
// send. And
// it once bounded a SIGTERM to a dhcpcd child and that child's own
// teardown — dropping the address, closing its lease file, reaping its
// own children. There is no child: the client is a goroutine and a
// socket in this process.
//
// The value is unchanged deliberately. What it bounds now is the
// library cancelling its own timers, closing its socket and returning
// from Run, and shortening it would start counting slow-but-clean exits
// as client_stop_failures. Short enough either way that plugin shutdown
// / Leave is not held hostage.
const dhcpClientFinishTimeout = 5 * time.Second

// dnsPropagateTimeout caps the docker-API round-trip cost of
// resolving the container PID for resolv.conf writes. Short because
// it runs on every DHCP bound/renew event; a slow daemon shouldn't
// stack up bound goroutines waiting on inspect calls. On timeout
// we log and skip — the next renewal will retry.
const dnsPropagateTimeout = 2 * time.Second

// NO OUTAGE WATCHDOG, AND WHAT COUNTS dhcp_timeouts NOW.
//
// A ticker used to ask an outageTracker, every 30 seconds, whether this
// client was still being served, because dhcpcd under `--noconfigure`
// announced nothing when a bound lease lapsed (#353): no EXPIRE, and a
// RELEASE indistinguishable from a graceful stop. The recurring signal
// had to be synthesised from a lease lifetime and a clock.
//
// The library reports it directly. Its state machine owns the
// retransmission schedule and the T1/T2/expiry timers, and it emits
// Failed{ReasonNoServer} when an attempt runs out of retries — which
// the chassis translates to "leasefail" and handleEvent counts as
// dhcp_timeouts, per attempt, for as long as the outage lasts. That is
// the same signal busybox udhcpc gave and dhcpcd took away, back from
// the client that actually knows.
//
// Three things went with the watchdog and are named here because each
// was load-bearing for something:
//
//   - outageTracker's lease deadline. The library holds the lease and
//     drives its own expiry; there is no second party guessing when a
//     lease lapsed from a lifetime it was told once.
//   - clampLeaseDeadline and lease_time_clamped. Option 51's 0xFFFFFFFF
//     is an INFINITE lease, and the library represents it as a zero
//     Expire (seam D-10) rather than as 4294967295 seconds. There is no
//     nanosecond multiplication to overflow into a negative duration,
//     so there is no clamp, so there is nothing to count.
//   - OUTAGE_TICK / OUTAGE_GRACE. They existed to make the failure
//     suite affordable by shortening a synthetic cadence. There is no
//     synthetic cadence.

// noteDNSPropagationPIDMismatch counts a DNS propagation refused because
// the PID it resolved turned out not to belong to the container it was
// resolved for (#317).
//
// # THIS METHOD USED TO COVER BOTH REFUSALS, AND MUST NOT AGAIN
//
// It was written against a tree where the netns refusal was counted at
// its call site in Start, and it took a `kind` so one predicate served
// both. #731 then moved the netns count INSIDE openSandboxNetNS, at the
// chokepoint a caller cannot bypass — the better placement, and for the
// same reason given below. Neither change conflicted textually and both
// were green on their own head; rebased together they counted one
// refusal TWICE, and TestCountingWrappers_AreTheOnlyCallers is what
// said so. If a second kind is ever wanted here, check first whether
// the operation it guards already has an opener that can own it.
//
// The pairing lives here rather than at the call site for the reason
// observeLease does. The site read:
//
//	if errors.Is(err, errPIDNotContainer) && m.plugin != nil {
//		m.plugin.<counter>.Add(1)
//	}
//
// three lines each, inside methods that need a live container and a
// real netns to reach — so nothing could drive them, and deleting BOTH
// Add(1) lines left `go test ./...` completely green. Meanwhile
// container_netns_test.go:37 and :95 assert that the error still
// carries errPIDNotContainer, with comments saying in writing that
// they do it "so the counter can fire" and "or the mismatch is never
// counted". The sentinel's survival was pinned deliberately, naming
// the counter as the reason; the counter itself was pinned by nothing.
//
// That is the precondition asserted in place of the effect: whether
// the plugin DECIDED a mismatch is not what an operator reads, and a
// test whose message names an effect it does not assert is how a
// counter ends up with no reader while looking guarded.
//
// The nil check is on plugin, not on the error: unit tests that do not
// stand up a Plugin leave it nil (see dhcpManager.plugin), and the
// refusal is still a refusal when there is no counter to bump.
func (m *dhcpManager) noteDNSPropagationPIDMismatch(err error) {
	if !errors.Is(err, errPIDNotContainer) || m.plugin == nil {
		return
	}
	m.plugin.dnsPropagationPIDMismatches.Add(1)
}

// closeNsHandle / closeNetHandle log close errors at Debug instead of
// silently dropping them. Cleanup paths can't act on a Close failure
// (we're already on an error path or shutting down), but a recurring
// EBADF / EIO here is the breadcrumb a future netns-leak debugging
// session will want.
func closeNsHandle(h netns.NsHandle) {
	if err := h.Close(); err != nil {
		log.WithError(err).Debug("netns handle close failed")
	}
}
func closeNetHandle(h *netlink.Handle) {
	if h == nil {
		return
	}
	// netlink.Handle.Close has no return value; the wrapper exists
	// for symmetry with closeNsHandle so call sites read uniformly.
	h.Close()
}

type dhcpManager struct {
	docker  dockerClient
	joinReq JoinRequest
	opts    DHCPNetworkOptions

	// plugin is a back-reference for bumping plugin-level counters
	// (lease_changed_total, etc.) and reaching the docker client when
	// an event handler needs to look up the container behind this
	// endpoint. Unit tests that don't drive lease events can pass nil;
	// every production path goes through Plugin.Join.
	plugin *Plugin

	// ipMu guards lastIP / lastIPv6, the two lastEvent fields, clientV4
	// and hostname. Writes happen from the lease-event
	// goroutine (renew); reads happen from Leave after Stop has
	// drained that goroutine. The drain establishes happens-before in
	// practice, but the race detector doesn't always see the channel
	// pairing through `select`, and a future change to stop priority
	// could turn this into a real race. Cheap to make explicit.
	ipMu     sync.Mutex
	lastIP   *netlink.Addr
	lastIPv6 *netlink.Addr
	// v6Installed is EVERY IPv6 address this manager has put on the
	// container link, keyed by the address's own string, and lastIPv6
	// above is the one of them Docker was told about.
	//
	// A SET AND NOT A LAST VALUE, because RFC 4862 section 5.5.3 forms
	// one address per autonomous prefix and a lease holds all of them.
	// The change path for a single value can only ever delete the one
	// address it remembers: on a link that advertised two prefixes and
	// then withdrew one, it would delete whichever address the lease
	// happened to name first and leave the withdrawn one on the link
	// for as long as the container ran.
	v6Installed map[string]*netlink.Addr
	// lastEvent / lastEventAt are the most recent lifecycle event this
	// manager saw and when it saw it, for the per-endpoint half of
	// /Plugin.Health. Under ipMu with the addresses beside them
	// because they are written from the same goroutine at the same
	// moments, and a reader that got the address from one instant and
	// the event from another would describe an endpoint that never
	// existed.
	lastEvent   string
	lastEventAt time.Time

	// recordID is the durable lease record this manager writes to.
	// Empty means there is none — a unit-test manager, or an endpoint
	// adopted from Docker's view with no record behind it — and every
	// record call is a no-op then.
	recordID string

	// recordID6 is the DHCPv6 record, which is a SECOND record under a
	// second scope: a lease.Record binds one family and one identity,
	// both write-once, so a dual-stack endpoint has two. See
	// dhcp.Scope6.
	recordID6 string

	// policyRestricted is whether this client was started against an
	// operator-named allow-list. Captured at setupClient rather than
	// re-resolved where it is read, so the counter cannot describe a
	// policy the client is not running under.
	policyRestricted bool

	// boundV4 records that the persistent v4 client actually took
	// ownership of the binding, i.e. that it reached a bound/renew.
	//
	// "Start succeeded" is NOT that proof, and the difference leaks a
	// lease. CreateEndpoint's one-shot runs `-1 -p` and deliberately
	// does not release, because handing the binding over is the
	// persistent client's job. Up to v1.8.x Stop had exactly two
	// branches: Start failed (reclaim the lease, #370) or Start
	// succeeded (signal dhcpcd and let its `release` directive do the
	// work). A client that starts and is SIGTERMed before it ever binds
	// falls between them, and the lease was left held upstream with
	// nobody responsible for it.
	//
	// #800 removed both halves of that machinery — the reclaim and the
	// `release` directive — so an outstanding lease is now the DESIGNED
	// outcome rather than a leak, and it expires on the server's clock.
	// The flag survives the change because the ledger still has to tell
	// "this client held the binding" from "it never did": one of those
	// is a stop worth recording, the other is not.
	//
	// Not hypothetical: run 31917924943 has DHCPOFFER/REQUEST/ACK for
	// 192.168.99.12 and no DHCPRELEASE anywhere in the run, with the
	// whole start-and-signal sequence inside one second. That surfaced
	// as an intermittent integration failure (#549) rather than as the
	// leak it is, because the counter the test watched belongs to the
	// path this case never reaches.
	//
	// Written from the v4 consumer goroutine, read in Stop after that
	// goroutine has been drained via errChan, which is the same
	// happens-before startErr already relies on.
	boundV4 atomic.Bool
	// boundV6 is the same proof for the persistent v6 client, written
	// from the v6 consumer goroutine and read in Stop after errChanV6
	// has been drained. It exists because the v6 shutdown path had none
	// of the above (#608): Stop audited v6 on the exit error alone, so a
	// v6 client signalled before it bound was written up as a clean
	// release of an address the server had never been asked to free,
	// and the reclaim — v4-only until then — left the IA_NA address the
	// one-shot took held upstream until it expired, which since #800 is
	// what happens to every lease on a `release_lease=never` network.
	// #962 added the one exception: a Leave on a `release_lease=on_stop`
	// network hands the address back, built from the record, whether or
	// not this flag was ever set. Not a race, the only behaviour. Both flags are now read the same way when the
	// ledger entry for each family is written.
	boundV6 atomic.Bool

	// lastAdvertRoutes is the set of more-specific IPv6 routes this
	// endpoint currently has because a Router Advertisement asked for
	// them, keyed destination -> next hop.
	//
	// IT IS A DIFF BASE AND NOTHING ELSE. An advertisement that stops
	// mentioning a prefix is asking for that route to go away (RFC 4191
	// section 2.3 spells the withdrawal as a Route Information option
	// with a lifetime of 0, and RFC 4861 section 6.3.4 keeps the router
	// list per-router rather than cumulative), and "which routes did we
	// put there" is not a question the kernel's table can answer: the
	// container also carries routes Join copied off the host bridge,
	// and deleting those would be this plugin taking away something an
	// operator configured.
	//
	// Written and read only from the v6 consumer goroutine, which is
	// the one goroutine that runs handleEvent with v6 true.
	lastAdvertRoutes map[string]string

	// mtuMu guards the two numbers below, which are the only state in
	// this manager written by BOTH family goroutines: each family's
	// most recently accepted MTU, zero when that family has supplied
	// none. See propagateMTU for why the link takes the smaller.
	mtuMu sync.Mutex
	mtuV4 int
	mtuV6 int
	// mtuBase is the link's own MTU before this manager wrote to it,
	// which is where the link goes back to when both families stop
	// supplying one.
	mtuBase int

	// MacAddress is set in macvlan mode so we can re-find the link inside
	// the container netns after Docker has moved and renamed it. Empty in
	// bridge mode.
	MacAddress net.HardwareAddr

	// hostname is the container's name as this endpoint puts it on the
	// wire, and it is EMPTY FOR TWO OPPOSITE REASONS: the container has
	// no name, or safeHostname refused the one it has. See the field
	// comment on unsafeHostnamesRejected.
	//
	// UNDER ipMu SINCE #961, and that is not tidiness. The attach used
	// to write it before any client existed; it now writes it after the
	// v4 client is already leasing, and audit() reads it from the
	// lease-event goroutine for every ledger row. Read it through
	// hostnameOnTheWire and write it through setHostname; audit is
	// never called with ipMu held.
	hostname  string
	nsHandle  netns.NsHandle
	netHandle *netlink.Handle
	ctrLink   netlink.Link

	stopChan  chan struct{}
	errChan   chan error
	errChanV6 chan error

	// ctrID caches the container ID behind this endpoint for ledger
	// entries; resolved at most once via ctrIDOnce (see containerID).
	ctrIDOnce sync.Once
	ctrID     string

	// startedCh is closed when Start has finished (success or failure);
	// startErr captures the result. This lets Stop be called against a
	// manager whose Start is still in flight (e.g. when Leave races
	// against the goroutine that Join spawned to call Start) — Stop
	// blocks until Start completes, then short-circuits if Start failed.
	startedCh chan struct{}
	startErr  error

	// startPhases / startTotal carry the per-phase timing of Start.
	// Written once in Start's deferred exit and read only after
	// startedCh closes, which is the same happens-before startErr
	// already relies on.
	//
	// WRITTEN ON SUCCESS TOO SINCE #403. They used to be empty on a
	// Start that worked, on the reasoning that timing a Join that
	// worked belongs in a benchmark. #403 asks what the distribution of
	// Join durations actually is on a loaded host, against a 10s
	// budget, and a record that exists only for the Joins that missed
	// the budget answers that question with the tail and calls it the
	// distribution. The failure line is unchanged; the success line is
	// at debug, where the per-attach key refusal already is.
	startPhases string
	startTotal  string

	// attachCancel aborts an in-flight Start. Stop calls it before
	// waiting on startedCh, which is what keeps the longer attach
	// budget (see attachDaemonBusyGrace) from turning into a longer
	// Leave: a container that goes away mid-attach cancels the attach
	// instead of making libnetwork wait out the whole grace for a
	// container nobody is waiting on any more.
	attachCancel context.CancelFunc

	// attachAborted records that the cancellation above was OUR doing,
	// i.e. Stop ran because the endpoint is leaving.
	//
	// Without it the resulting "context canceled" is indistinguishable
	// from any other attach failure and gets counted as a plugin fault
	// — a running container left with no renewal client — when the
	// truth is the opposite: there is no container left to renew for.
	// Measured on run 30700597210, where six attaches reported
	// join_start_failures with `context canceled`, all of them endpoints
	// that were being torn down (#406).
	//
	// Deliberately a flag rather than inferring it from
	// errors.Is(err, context.Canceled): a cancelled context could also
	// come from somewhere that is not a teardown, and excusing every
	// cancellation would be exactly the blanket amnesty #373 and #376
	// were careful not to grant.
	attachAborted atomic.Bool

	// clientV4 is the persistent v4 client, published for READING only:
	// the health document asks it for the lease it holds and the RFC
	// 5227 phase it is in, and nothing writes through it.
	//
	// Its type is the three-method endpointClient rather than
	// *dhcp.DHCPClient so that the health document can be driven
	// against a client in a state a unit test cannot reach otherwise --
	// a bound lease with T1, T2, an expiry and a server ID lives inside
	// the library's own client, which no test in this package can
	// construct. The narrow type is what makes the endpoints array
	// assertable on its FIELDS rather than on its length.
	//
	// Under ipMu, which is released before the client is asked
	// anything; see healthView.
	//
	// SINCE #961 IT IS ALSO THE ONE THING THE ATTACH WRITES THROUGH,
	// and that is why the type is joinClient and not endpointClient:
	// the container's name arrives from the daemon after this client is
	// already leasing, and SetHostname is what puts it on the wire. One
	// field and one publisher for both directions, so a future path
	// cannot publish the client the health document reads without also
	// publishing the one the name goes to.
	//
	// v6 has no counterpart BY CHOICE, not by absence. A dual-stack
	// endpoint runs two clients and the endpoints array has one entry
	// per endpoint, so one of them is the one it describes; it is this
	// one, because the array's RFC 5227 pair has no v6 meaning at all.
	// The name has the same answer for a different reason: this library
	// sends no name option for DHCPv6 at all, and lease.Manager
	// refuses the call on that family. TestHealthClient_IsPublishedOnlyForV4
	// holds the guard at the one call site and docs/reference.md states
	// the bound on the row.
	clientV4 joinClient

	// releasedV4 / releasedV6 record that this endpoint's lease was
	// actually handed back, so Leave can close the record instead of
	// leaving it re-bindable and DeleteEndpoint can decline to lay a
	// tombstone for an address that is no longer ours.
	//
	// WRITTEN FROM THE OUTCOME, NOT FROM THE OPTION. A network set to
	// release whose release did not leave the host still holds its
	// lease upstream, and a tombstone skipped on the option alone would
	// throw away restart stability for an endpoint that released
	// nothing.
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

// withPlugin attaches a Plugin back-reference. Used by Plugin.Join /
// recoverEndpoint to wire the manager to the live counters before
// Start. Test helpers omit it; production callers always set it.
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

// joinClient is the persistent v4 client as the ATTACH holds it: the
// health document's read-only view plus the one call the attach makes
// into a client that is already running.
//
// It is declared here and not beside endpointClient because it is the
// attach's demand and not the health document's. Widening
// endpointClient instead would tell every reader of the health surface
// that the document writes to the client, which it does not.
type joinClient interface {
	endpointClient

	// SetHostname gives the running client the container's name for
	// DHCP option 12 and makes it tell the server at once (#961). See
	// dhcp.DHCPClient.SetHostname for what the error is about.
	SetHostname(name string) error
}

// setHealthClient publishes the client the health document reads and
// the attach names.
func (m *dhcpManager) setHealthClient(c joinClient) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	m.clientV4 = c
}

// healthClient is the published client, or nil. The lock is dropped
// before the caller asks the client anything.
func (m *dhcpManager) healthClient() joinClient {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.clientV4
}

// setHostname records the name this endpoint puts on the wire.
func (m *dhcpManager) setHostname(h string) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	m.hostname = h
}

// hostnameOnTheWire is the name this endpoint is currently sending, or
// the empty string for a container with no name and for one whose name
// safeHostname refused.
func (m *dhcpManager) hostnameOnTheWire() string {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.hostname
}

// noteResumedACD reports an address picked up from a durable record
// whose RFC 5227 section 2.1 check had not completed (D23).
//
// THE CONDITION IS INSIDE AND THE CALL SITE IS UNCONDITIONAL. It used
// to be an `if` around the log line at the call site with nothing
// observing it, and the M6b review measured the consequence: the
// inverted-guard mutant -- warn on a clean resume, stay silent on a
// half-checked one -- survived the whole suite. There is no guard left
// at the call site to invert, and the counter beside the line puts the
// same fact in /Plugin.Health as the acd_resumed_unchecked warn check,
// so the operator half of D23 is reachable without reading logs.
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

// lastEventSeen returns the most recent lifecycle event and its time.
func (m *dhcpManager) lastEventSeen() (string, time.Time) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.lastEvent, m.lastEventAt
}

// noteEvent records one lifecycle event for the health document.
func (m *dhcpManager) noteEvent(kind string) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	m.lastEvent, m.lastEventAt = kind, time.Now()
}

// lastIPs returns the most recently observed v4/v6 leases under ipMu.
func (m *dhcpManager) lastIPs() (*netlink.Addr, *netlink.Addr) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.lastIP, m.lastIPv6
}

// setLastIP records a freshly-bound address under ipMu.
func (m *dhcpManager) setLastIP(v6 bool, addr *netlink.Addr) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	if v6 {
		m.lastIPv6 = addr
	} else {
		m.lastIP = addr
	}
}

// audit appends a lease-lifecycle event to the plugin's ledger when
// this network opted in via audit_log=true. Best-effort by design:
// ledger problems are counted and logged inside Append and must never
// affect lease handling. ip is the bare address ("192.168.0.10"),
// derived by the caller from whatever form it has at hand.
func (m *dhcpManager) audit(kind, ip string) {
	m.auditFrom(kind, ip, "")
}

// auditFrom is audit with the address's source, for the rows where
// "where did this address come from" is not answered by the kind.
//
// A FORMED ADDRESS AND A GRANTED ONE ARE THE SAME `bound` ROW OTHERWISE,
// and they are not the same event to anyone reading the ledger back: no
// DHCP server was involved in the first, so there is no lease on any
// server to correlate it with and no server log it appears in.
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

// auditSource is the ledger's `source` column for one lease.
func auditSource(info dhcp.Info) string {
	if info.SLAAC {
		return "slaac"
	}
	return ""
}

// containerID resolves (once, then caches) the ID of the container
// behind this manager's endpoint, for ledger entries. Resolution
// failure degrades to an empty field rather than blocking the event
// path — sync.Once keeps concurrent v4/v6 event goroutines from
// racing the lookup.
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

// endpointMAC returns the MAC this endpoint's DHCP identity is keyed
// to: the one CreateEndpoint ran its one-shot exchange under, carried
// on the join hint and restored from the tombstone on recovery. Falls
// back to the located container link so a manager that never saw a
// hint degrades to reading the live link rather than to no MAC.
func (m *dhcpManager) endpointMAC() net.HardwareAddr {
	if len(m.MacAddress) > 0 {
		return m.MacAddress
	}
	if m.ctrLink != nil {
		return m.ctrLink.Attrs().HardwareAddr
	}
	return nil
}

// clientID resolves this endpoint's DHCP option-61 identity.
//
// Every exchange the manager makes has to present the id the
// CreateEndpoint one-shot used. Present a different one and the server
// sees a different client: the lease it already holds is neither
// renewed nor handed back to the endpoint that owns it, silently. Since
// #371 the id is mode-dependent (MAC-derived, except ipvlan), so
// deriving it in one place from one input is what keeps them in step.
//
// This got MORE load-bearing in v1.9.0, not less (#800). The plugin no
// longer releases anything, so a restarting container gets its address
// back by asking for it again and being recognised — the identity here
// IS the mechanism. Before, a wrong id would have shown up as a lease
// that failed to be freed; now it shows up as a container that came
// back on a different address, which is the guarantee this project
// exists to provide.
func (m *dhcpManager) clientID() []byte {
	return resolveClientID(m.opts, m.joinReq.EndpointID, m.endpointMAC())
}

// macString returns the endpoint's MAC for ledger entries: the
// container-side link's address when Start has located it (set before
// the event goroutines exist, so reads here are race-free), falling
// back to the MacAddress recorded on the join hint.
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

// bareIP strips the prefix length off a CIDR-form address for ledger
// entries, passing through anything that doesn't parse as CIDR.
func bareIP(cidr string) string {
	if ip, _, err := net.ParseCIDR(cidr); err == nil {
		return ip.String()
	}
	return cidr
}

// findContainerPID resolves the host PID of the container that owns
// this manager's endpoint, together with the container ID it came
// from. Returns an error if the endpoint is not found in the
// network's container list (rare race during teardown) or if the
// container has no PID (not running). Mirrors
// Plugin.lookupEndpointMAC's lookup shape.
//
// The container ID is returned, not discarded, because the PID alone
// is not enough to act on: by the time anything opens /proc/<pid> the
// container may have exited and the kernel may have handed that PID
// to an unrelated host process. Callers pair the two and let
// openContainerProc decide (#688).
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

// renew applies one accepted lease to the container's netns. Each
// phase below is a separate method so it can be exercised on its own;
// the order they run in is load-bearing and is documented at each
// call site rather than inside the phases.
func (m *dhcpManager) renew(v6 bool, info dhcp.Info) error {
	ip, err := netlink.ParseAddr(info.IP)
	if err != nil {
		return fmt.Errorf("failed to parse IP address: %w", err)
	}
	if v6 {
		v6AddrAttrs(ip, info.LeaseSeconds, info.PreferredSeconds)
	}

	// Address first, routes after — the ordering the kernel itself
	// requires (see applyAddressChange).
	if err := m.applyAddressChange(v6, ip, info); err != nil {
		return err
	}

	m.logObservedOptions(v6, info)

	// Track the freshly-bound address so Leave can hand it to the
	// tombstone (and thus the next CreateEndpoint's option-50 hint).
	// Without this the manager keeps reporting whatever the very
	// first CreateEndpoint DISCOVER produced, even if the client has
	// moved to a different lease since. After applyAddressChange, which
	// needs the previous value.
	m.setLastIP(v6, ip)

	m.propagateDNS(v6, info)
	m.propagateMTU(v6, info)

	// THE DIFF BASE HAS TO BE SEEDED HERE, not left to the first live
	// advertisement. Docker installs the routes from the Join answer
	// (network.go's v6AdvertisedRoutes), so by the time this manager
	// starts the container ALREADY has them, and lastAdvertRoutes is
	// nil. A later advertisement that drops one of them would then diff
	// against an empty record, find nothing to withdraw, and leave the
	// container routing over a prefix the segment stopped offering --
	// which is the ordinary case, not an edge one, because the first
	// change to any route is always a change to a route installed at
	// Join.
	//
	// It is a reconcile and not an assignment because the two can
	// already disagree: Join ran before this manager existed, the
	// advertisement may have moved in between, and RouteReplace against
	// what Docker installed is a no-op when they agree.
	if v6 {
		if err := m.reconcileAdvertisedRoutes(info); err != nil {
			log.WithError(err).WithFields(m.logFields(v6)).
				Warn("Failed to reconcile the routes the Router Advertisement asked for")
		}
	}

	return m.reconcileDefaultRoute(v6, info)
}

// applyAddressChange re-applies the lease to the link when the server
// handed back a different address than the one currently recorded. A
// no-op on the steady-state renewal path, which is the common case.
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

	// libnetwork has no in-place endpoint-IP swap RPC, so Docker's
	// NetworkSettings.IPAddress still reports the previous address
	// — `docker inspect` lies until the container is recreated.
	// Bump the counter so operators can alert on the truthfulness
	// gap; design discussion for a deeper fix is deferred (issue #104).
	if m.plugin != nil {
		bumpFamily(&m.plugin.leaseChangedV4, &m.plugin.leaseChangedV6, v6)
	}
	log.
		WithFields(m.logFields(v6)).
		WithField("old_ip", lastIP).
		WithField("new_ip", ip).
		Warn("dhcp renew with changed IP — Docker's view is now stale")

	// Apply the re-acquired lease to the link. Found by
	// TestFailure_LeaseRefusedOnRenewal (#128): without this the
	// kernel keeps the ORIGINAL address forever — the container
	// answers on an address the server may already have handed to
	// someone else, and after a server-side renumbering the
	// default-route replacement in reconcileDefaultRoute failed with
	// "network is unreachable" (no address in the new subnet),
	// aborting the bind and black-holing the endpoint. Address first,
	// routes after — same ordering the kernel itself requires.
	//
	// Applies to both families now (#152): the plugin pins the same
	// DUID-LL/IAID for the one-shot and persistent clients, so the
	// persistent v6 client renews the SAME address Docker was told
	// — a "changed IP" is therefore a genuine renumber to re-apply,
	// not the old IA-split steady state. (Previously the v6 arm was
	// disabled because busybox's per-process random IAID made every
	// ipv6=true container look like it changed address seconds after
	// start.)
	//
	// netHandle/ctrLink are always live on the production path
	// (renew runs from the event loop, post-Start); the guard
	// keeps pre-Start unit tests of the counter semantics valid.
	if m.netHandle == nil || m.ctrLink == nil {
		return nil
	}
	if err := nlHandleAddrReplace(m.netHandle, m.ctrLink, ip); err != nil {
		return fmt.Errorf("failed to apply re-acquired address %v: %w", ip, err)
	}
	if err := m.netHandle.AddrDel(m.ctrLink, lastIP); err != nil {
		// Non-fatal: a lingering stale address is strictly
		// better than failing the bind on cleanup.
		log.
			WithError(err).
			WithFields(m.logFields(v6)).
			WithField("stale_ip", lastIP).
			Warn("Failed to remove stale address after lease change")
	}
	return nil
}

// v6AddrAttrs puts the two things a DHCPv6 address needs beyond its
// bytes onto the netlink address: IFA_F_NODAD, and RFC 9915 section
// 7.1's two lifetimes.
//
// # WHY NODAD (D30 Q1)
//
// THE DUPLICATE-ADDRESS CHECK HAS ALREADY BEEN RUN, BY THE LIBRARY, AND
// IT PASSED. RFC 9915 section 18.2.10.1: "The client performs duplicate
// address detection on each of the received addresses in any IAs it
// accepts before using that address for traffic"; the library does it
// and emits Acquired only after the check comes back clean. Installing
// the address without this flag makes the KERNEL run RFC 4862 section
// 5.4 a second time on an address that has just passed it, and the
// second run is not free:
//
//   - the address is `tentative` for the length of the check, during
//     which the container cannot use it and cannot answer a neighbor
//     solicitation for it. A proof that reads `ip -6 addr` right after
//     the bind sees a usable address on a fast box and a tentative one
//     on a loaded runner -- so the proofs assert the FLAG, not the
//     timing.
//   - RFC 7527 section 4.1's loopback case, or any node that answers
//     the second probe, marks the address `dadfailed` and the kernel
//     takes it out of service. That is an address the library cleared
//     seconds earlier being withdrawn by a check nobody asked for.
//
// RFC 4429 section 3.3 is the same argument from the other side: an
// address whose uniqueness has been established does not need the
// interface to hold it tentative again.
//
// # WHO OWNS EXPIRY
//
// THE LIBRARY DOES. Lost{ReasonExpired} is what removes the address;
// the kernel lifetimes here are a BELT, not the mechanism. They are set
// because a plugin that dies between the expiry and its own restart
// would otherwise leave a container holding an address whose lease ran
// out -- the kernel is then the only thing left that knows -- and
// because a deprecated address (preferred elapsed, valid remaining) is
// something only the kernel can express to the applications inside the
// container: RFC 4862 section 5.5.4 has a deprecated address still
// usable by an established connection and not chosen for a new one, and
// no plugin-side bookkeeping can deliver that to a socket.
//
// Both lifetimes zero means an infinite lease and sends no
// IFA_CACHEINFO at all, which is the kernel's "forever".
func v6AddrAttrs(addr *netlink.Addr, valid, preferred int) {
	addr.Flags |= unix.IFA_F_NODAD
	addr.ValidLft = valid
	addr.PreferedLft = preferred
	// AN INFINITE VALID LIFETIME BESIDE A FINITE PREFERRED ONE CANNOT
	// BE SENT AS A ZERO, and it is a shape a router may legally
	// advertise: RFC 4862 section 5.5.3 takes the two lifetimes from
	// the Prefix Information option independently and only requires
	// preferred <= valid. The netlink library attaches IFA_CACHEINFO
	// when EITHER lifetime is non-zero and puts both numbers in it, so
	// the pair (0, 1800) reaches the kernel as a valid lifetime of zero
	// seconds and the address is refused with EINVAL -- the container
	// then has no address at all, which is the opposite of what an
	// unbounded advertisement asked for. The kernel's own spelling of
	// "forever" in that structure is 0xFFFFFFFF, so that is what is
	// sent once the pair has to be sent at all. Both zero still sends
	// no IFA_CACHEINFO, which is the permanent address this plugin
	// installed before there was anything to choose.
	if addr.ValidLft == 0 && addr.PreferedLft > 0 {
		addr.ValidLft = infiniteLft
	}
}

// infiniteLft is the kernel's IFA_CACHEINFO spelling of "no expiry",
// which is not the same value as this plugin's own (Info's zero). See
// v6AddrAttrs for the one case where the two have to be translated.
const infiniteLft = 0xFFFFFFFF

// installV6Address applies the DHCPv6 lease to the container link.
//
// IT RUNS ON EVERY EVENT THAT CARRIES AN ADDRESS, not only on a change,
// and that is the difference from the v4 path above:
//
//   - THE FIRST BIND IS NOT A NO-OP HERE. libnetwork installed
//     AddressIPv6 itself when it built the sandbox, from the value
//     CreateEndpoint returned -- with no NODAD flag and no lifetimes,
//     because libnetwork knows nothing about either. So the address on
//     the link is the right address with the wrong attributes until
//     this re-applies it. The v4 path has nothing equivalent to fix.
//   - A RENEWAL MUST REFRESH THE LIFETIMES. The address is unchanged
//     and the DEADLINES are not; skipping the re-apply would leave the
//     kernel counting down the lifetimes of the previous Reply, and the
//     address would go away under a lease the server is happily
//     renewing.
//
// AddrReplace and not AddrAdd for both reasons: it is the one operation
// that is correct whether or not the address is already there.
func (m *dhcpManager) installV6Address(ip, lastIP *netlink.Addr, changed bool, info dhcp.Info) error {
	if changed {
		// Same counter and the same warning as the v4 path: Docker's
		// NetworkSettings still reports the previous address, because
		// libnetwork has no in-place endpoint-IP swap RPC (#104).
		if m.plugin != nil {
			bumpFamily(&m.plugin.leaseChangedV4, &m.plugin.leaseChangedV6, true)
		}
		log.
			WithFields(m.logFields(true)).
			WithField("old_ip", lastIP).
			WithField("new_ip", ip).
			Warn("dhcp renew with changed IP — Docker's view is now stale")
	}

	// netHandle/ctrLink are always live on the production path (renew
	// runs from the event loop, post-Start); the guard keeps pre-Start
	// unit tests of the counter semantics valid.
	if m.netHandle == nil || m.ctrLink == nil {
		return nil
	}
	want, err := v6WantedAddrs(ip, info)
	if err != nil {
		return err
	}
	// Read the installed set BEFORE the loop: withdrawV6AddrsNotIn
	// writes it, and a renewal re-applies every address it already
	// holds, so a counter derived from the set afterwards would count
	// each refresh as a new address.
	had := m.installedV6()
	for _, a := range want {
		if err := nlHandleAddrReplace(m.netHandle, m.ctrLink, a.addr); err != nil {
			return fmt.Errorf("failed to apply the IPv6 address %v: %w", a.addr, err)
		}
		if _, seen := had[a.key]; !seen && info.SLAAC && m.plugin != nil {
			m.plugin.ipv6SLAACAddresses.Add(1)
		}
	}
	m.withdrawV6AddrsNotIn(want, auditSource(info))
	return nil
}

// One address this lease wants on the link, keyed the same way the
// installed set is.
type wantedV6Addr struct {
	addr *netlink.Addr
	key  string
}

// v6WantedAddrs is every address this lease says the container should
// hold, each carrying its OWN pair of lifetimes.
//
// THE ORDER PUTS THE MAIN ADDRESS FIRST, which matters only for the
// error path: if one of several AddrReplace calls is going to fail, the
// one Docker already told the container about is the one worth applying
// before any of the others.
//
// A LEASE THAT CARRIES NO LIST STILL PRODUCES ONE ENTRY. Info.Addrs is
// empty for a DHCPv4 lease and for any v6 lease the library built
// before it held per-address lifetimes, so the single address and the
// lease-wide pair are the fallback rather than a case with no
// behaviour. That keeps this function total over every Info a caller
// can hand it, including the ones a unit test writes by hand.
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
		v6AddrAttrs(addr, a.ValidSeconds, a.PreferredSeconds)
		w := wantedV6Addr{addr: addr, key: addr.String()}
		if w.key == mainKey {
			out = append([]wantedV6Addr{w}, out...)
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// withdrawV6AddrsNotIn removes from the container link every address
// this manager installed that the current lease no longer holds, and
// records each one.
//
// WHY A SET DIFFERENCE AND NOT A COMPARISON WITH THE LAST ADDRESS. Two
// things take an address out of a lease and neither is a renewal onto a
// different address: a valid lifetime that ran out (RFC 4862 section
// 5.5.4's second phase), and a router that stopped advertising the
// prefix it was formed from. On a link with two autonomous prefixes
// either can happen to either address while the other is untouched, so
// "the address changed" is not a question with one answer, and the
// container keeping an address whose prefix is no longer routed is
// worse than a stale one: it is chosen as a source address for new
// connections that then go nowhere.
//
// EVERY FAILURE HERE IS NON-FATAL AND EVERY ONE IS COUNTED. The address
// is out of the lease whatever the kernel says, so a manager that
// returned an error here would fail a renewal over an address it was
// trying to clean up.
func (m *dhcpManager) withdrawV6AddrsNotIn(want []wantedV6Addr, source string) {
	keep := make(map[string]bool, len(want))
	for _, w := range want {
		keep[w.key] = true
	}
	for key, addr := range m.installedV6() {
		if keep[key] {
			continue
		}
		m.forgetV6Addr(key)
		if err := m.netHandle.AddrDel(m.ctrLink, addr); err != nil {
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
			Info("An IPv6 address left this endpoint's lease and was removed from the link")
	}
	for _, w := range want {
		m.rememberV6Addr(w.key, w.addr)
	}
}

// installedV6 is a copy of the installed set, taken under ipMu so the
// caller can walk it while the map is being written.
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

// logObservedOptions surfaces DHCP options the plugin captures but
// doesn't auto-apply (NTP servers, TFTP server, boot-file name, search
// list when not propagating DNS). Operators can grep plugin logs for
// these without flipping LOG_LEVEL=trace. Only emits when at least one
// is non-empty so plain LANs don't get a noisy line per renewal.
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
	// Observe-only informational extras (#262): WPAD URL (opt 252),
	// RFC 4833 timezone (opt 100/101), legacy time offset (opt 2).
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

// propagateDNS applies the resolvers the segment supplied when opt-in
// and there is something to apply: DHCP option 6 on the v4 path, and on
// the v6 one DHCPv6 option 23 and RFC 8106's RDNSS and DNSSL from the
// Router Advertisement, which the library has already merged into the
// same two slices with DHCPv6 taking precedence (RFC 8106 section
// 5.3.1). Never fails the renewal: name resolution is recoverable, the
// lease is not.
//
// AN EMPTY LIST IS A NO-OP AND NOT A CLOBBER, and on the v6 path that
// is not only the defensive choice, it is what the RFC asks for. RFC
// 8106 section 6.1: the DNS options "need not be dropped if the expiry
// of the RA router lifetime happens". A router that withdraws itself is
// telling the container not to route through it, not to stop resolving
// names -- so the default route goes (see withdrawV6DefaultRoute) and
// resolv.conf stays.
//
// THE BOUND, since an empty list is how a withdrawal would have to
// arrive: a segment that shrinks its RDNSS list to nothing cannot take
// the last resolver away through this path. writeContainerResolvConf
// refuses to write a file with no nameserver line in it -- an empty
// resolv.conf silently breaks every lookup in the container -- so the
// container keeps the resolvers it had. A list that shrinks to a
// SHORTER non-empty list is applied in full, which is the case that
// matters: RFC 4861 section 6.3.4 makes the answer per-advertisement,
// so dropping one of two resolvers does reach the container.
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

	// The container's own name for the link, read off the link this
	// manager located inside the sandbox. It is the scope zone a
	// link-local resolver needs; see zonedNameserver. Empty before the
	// link is located, which is the pre-Start unit-test case and not a
	// production one.
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

// propagateMTU applies the MTU the segment supplied: DHCP option 26 on
// the v4 path, and RFC 4861 section 4.6.4's MTU option on the v6 one.
// Skipping zero is mandatory: the library reports 0 when neither was
// supplied, and forcing MTU 0 on a kernel link is undefined.
//
// # THE v6 MTU IS NOT GATED ON propagate_mtu (#821)
//
// It is not an opt-in on that path because it was never an opt-in on
// that path. Until #821 the KERNEL applied the advertised MTU, on
// every v6 network, whatever propagate_mtu said, because
// accept_ra was on and RFC 4861 section 6.3.4 tells a host to use it.
// #821 turns accept_ra off so the plugin owns the IPv6 route, and an
// option that defaults to false would then have silently taken the
// advertised MTU away from every existing v6 network. Keeping the
// behaviour is the conservative choice; making it opt-in would be the
// change.
//
// WHAT IS DIFFERENT FROM THE KERNEL'S VERSION, and it is worth saying
// because it is visible: the kernel wrote the per-interface IPv6 MTU,
// which bounds IPv6 only. This writes the LINK MTU, which bounds both
// families, because the link MTU is the mechanism this plugin already
// has and adding a second one would put two writers on one link. On a
// dual-stack network whose advertised MTU is below the link's, IPv4
// packets are now bounded by it too. The refusal range below still
// applies, so the value cannot go below minPropagatedMTU whichever
// family supplied it.
func (m *dhcpManager) propagateMTU(v6 bool, info dhcp.Info) {
	// The option gate first, because a family the operator switched off
	// does not get a vote either way: not to raise the link and not to
	// withdraw a value the other family supplied.
	if !v6 && !m.opts.PropagateMTU {
		return
	}

	// SILENCE IS NOT A WITHDRAWAL, and on IPv6 the two look identical
	// in Info.MTU alone.
	//
	// RFC 9915 section 18.2.1's Solicit goes out WITHOUT waiting for
	// router discovery, so a DHCPv6 lease event can be stamped before
	// the first advertisement arrives on a link that does have a
	// router; the library documents its own field that way, "the zero
	// value means it had seen none WHEN THIS EVENT WAS STAMPED". The
	// first bound event, which is the one that brings the MTU, is
	// exactly the one that can be stamped that early.
	//
	// Without this the zero from such an event reaches rememberMTU as a
	// withdrawal: the v6 vote is dropped, wantedMTU falls back to the
	// v4 number, the link moves, the next event carries the
	// advertisement and the link moves back. That is the flip the
	// smaller-of-two rule below exists to prevent, arriving through the
	// withdrawal path instead of through last-writer-wins.
	//
	// A router that HAS spoken and carried no MTU option sets
	// RouterSeen with MTU 0, and that one is a withdrawal and is acted
	// on.
	if v6 && !info.RouterSeen {
		return
	}

	// A ZERO IS A WITHDRAWAL AND NOT "NOTHING TO DO". This used to
	// return here, which meant a family that STOPPED supplying an MTU
	// kept its last vote for the life of the endpoint: a router that
	// drops its MTU option left mtuV6 holding the old number and the
	// link clamped to a value nothing on the segment was asking for any
	// more. The change does reach this function -- advertisedDiffers
	// compares MTU, so the routeradvert event fires -- and it was
	// discarded at the door.
	//
	// It is recorded BELOW the refusal, not here, so the two stay
	// distinct: a refused value leaves the previous vote standing,
	// because "the server said something impossible" is not "the
	// server stopped asking".
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

	// ONE LINK, TWO FAMILIES, ONE NUMBER.
	//
	// Both families write the SAME link MTU, so last writer wins unless
	// something decides between them. On a dual-stack network with
	// propagate_mtu=true whose DHCPv4 option 26 says X and whose
	// advertisement says Y, last-writer-wins flips the link between X
	// and Y once per renewal of either family, forever, and neither
	// value is ever stable. netlink caches the value it set on
	// Attrs().MTU, so the "nothing to do" test above cannot catch it
	// either: each family sees the other's number and moves it back.
	//
	// The smaller of the two is the only answer that is correct for
	// both. An MTU is an upper bound on what the link will carry, so
	// the larger value is a promise the link cannot keep for the family
	// that asked for the smaller one, while the smaller one costs the
	// other family throughput and nothing else. A family that supplied
	// nothing does not vote.
	m.rememberMTU(v6, info.MTU, m.ctrLink.Attrs().MTU)
	want := m.wantedMTU()
	if want == 0 {
		// BOTH FAMILIES HAVE STOPPED ASKING. The link goes back to what
		// it had before this manager first touched it, which is what
		// Docker gave it. Leaving it clamped would keep a number no
		// server and no router is asking for any more, and there is no
		// later event that would clear it: the next thing that moves
		// this link is another supplied MTU.
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
		// Don't fail the renewal — IP/gateway are usable; MTU
		// is a perf-correctness knob. Log loudly so operators
		// notice; a surprise small MTU under a never-applied
		// large MTU is exactly the kind of latent
		// black-hole bug worth surfacing.
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

// rememberMTU records the value one family just supplied, where zero
// means it has stopped supplying one, and records the link's own MTU
// the first time this manager looks at it.
//
// base is read from the caller rather than taken here because it must
// be the value the link had BEFORE this manager wrote to it, and the
// first call is the only moment that is still true.
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

// baseMTU is what the link had before this manager first wrote to it.
func (m *dhcpManager) baseMTU() int {
	m.mtuMu.Lock()
	defer m.mtuMu.Unlock()
	return m.mtuBase
}

// wantedMTU is the smaller of the values the two families supplied,
// ignoring a family that supplied none.
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

// reconcileDefaultRoute points the container's default route at the
// gateway the segment just supplied. On the v4 path it is skipped when
// the operator pinned a gateway override on the network -- leave their
// override in place. The v6 path is a different function below,
// because the shape of the answer is different: it has a withdrawal.
func (m *dhcpManager) reconcileDefaultRoute(v6 bool, info dhcp.Info) error {
	if v6 {
		return m.reconcileV6DefaultRoute(info)
	}
	if info.Gateway == "" || m.opts.Gateway != "" {
		return nil
	}

	newGateway := net.ParseIP(info.Gateway)
	if newGateway == nil {
		// #728's second guard, and since 2.0 its only one. The first
		// lived in pkg/dhcp.BuildEvent, which parsed dhcpcd's hook
		// environment in a separate process; there is no hook and no
		// second process now, and the library hands over a parsed
		// netip.Addr rather than a string. What is left is this
		// function's own obligation, which it always had: it is
		// reached by callers that build an Info without a server
		// exchange at all -- the recovery and replay paths do.
		//
		// Nil is the dangerous value precisely because netlink accepts
		// it. `Gw: nil` is not "no change", it is `default dev ethX
		// scope link` -- an on-link default route. Returning here is
		// the same thing the guard above does for an empty Gateway:
		// leave the container's existing route as it is.
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

// isDefaultRoute reports whether a route in the kernel's table is a
// default route. The kernel spells it two ways -- a nil destination,
// and ::/0 or 0.0.0.0/0 written out -- and which one comes back
// depends on the family and the netlink library's parsing, so both are
// asked rather than one assumed.
func isDefaultRoute(r netlink.Route) bool {
	if r.Dst == nil {
		return true
	}
	ones, _ := r.Dst.Mask.Size()
	return ones == 0
}

// reconcileV6DefaultRoute makes the container's IPv6 default route
// match what the routers on its segment currently advertise.
//
// THE PLUGIN OWNS THIS ROUTE NOW (#821, design note Q1). Before it, the
// container's kernel installed it from the advertisement and expired it
// on the Router Lifetime; accept_ra=0 means nothing does that any more,
// so both halves have to be here: the install, and the withdrawal.
//
// THE WITHDRAWAL IS THE HALF THAT IS EASY TO LEAVE OUT. RFC 4861
// section 4.2 gives Router Lifetime as "the lifetime associated with
// the default router", and section 6.3.4: "a Lifetime of 0 indicates
// that the router is no longer to be used as a default router". A
// container left pointing at a router that said that has a default
// route to a black hole, and nothing in the DHCPv6 exchange would ever
// tell it so -- a DHCPv6 server and a router are not the same box and
// the lease keeps renewing. That is what ipv6_router_withdrawn counts.
//
// WHAT IS DELETED IS BOUNDED. Only default routes, only on this
// endpoint's link, and never one the kernel installed itself
// (RTPROT_KERNEL): a container can be on more than one network, and
// this manager answers for one of them.
func (m *dhcpManager) reconcileV6DefaultRoute(info dhcp.Info) error {
	// netHandle/ctrLink are always live on the production path (this
	// runs from the event loop, post-Start); the guard keeps pre-Start
	// unit tests of the surrounding semantics valid, the same way
	// applyAddressChange's does.
	if m.netHandle == nil || m.ctrLink == nil {
		return nil
	}
	idx := m.ctrLink.Attrs().Index

	// RT_FILTER_OIF alone, and the default-route test in Go. Asking
	// netlink to match a nil Dst is what the v4 sibling does and it is
	// right there, but the two families do not agree on how a default
	// route's destination comes back, and a filter that silently
	// matched nothing would read as "this container has no default
	// route" -- which is the answer that makes the withdrawal below a
	// no-op forever.
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
		// Same reasoning as the v4 sibling's nil check, and one more:
		// an IPv4 address here would install a default route for the
		// wrong family. `Gw: nil` is not "no change" to netlink, it is
		// an on-link default route.
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
	// The protocol comes off with it. A route this plugin installs is
	// not a route the kernel learned from an advertisement, and leaving
	// RTPROT_RA on it would make `ip -6 route` say "proto ra" about a
	// route the kernel had nothing to do with. That is the exact
	// reading the troubleshooting row for two default routes asks an
	// operator to make, and a wrong answer there sends them looking for
	// a guard failure that did not happen. Zero means the kernel stamps
	// it RTPROT_BOOT, which is what every other route this plugin adds
	// carries.
	existing[0].Protocol = 0
	if err := nlHandleRouteReplace(m.netHandle, &existing[0]); err != nil {
		return fmt.Errorf("failed to replace IPv6 default route: %w", err)
	}
	// A second default route on the same link is a leftover, not a
	// choice: two of them make the winner a metric comparison nobody
	// wrote down. RouteReplace fixed the first; the rest go.
	for i := 1; i < len(existing); i++ {
		if err := nlHandleRouteDel(m.netHandle, &existing[i]); err != nil {
			log.WithError(err).WithFields(m.logFields(true)).
				WithField("gateway", existing[i].Gw).
				Warn("Failed to remove a second IPv6 default route")
		}
	}
	return nil
}

// withdrawV6DefaultRoute takes the container's IPv6 default route away
// because no router on the segment claims to be one any more.
//
// THE COUNTER MOVES ONLY WHEN A ROUTE CAME OFF, which is the whole
// point of counting it: an advertisement with Router Lifetime 0 arrives
// repeatedly -- RFC 4861 section 6.2.5 has a router send several as it
// shuts down -- and a counter that moved on each of them would report
// a number of withdrawals rather than a number of containers that lost
// their route. It is also why the count is taken from the delete and
// not from the advertisement.
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

// reconcileAdvertisedRoutes makes the more-specific IPv6 routes on the
// container's link match the Route Information options (RFC 4191) the
// segment currently advertises.
//
// IT IS A DIFF AGAINST WHAT THIS MANAGER INSTALLED, not against the
// table: see lastAdvertRoutes. A prefix that stops being advertised has
// its route removed, which is the direction that has no other
// mechanism now that accept_ra is 0.
//
// ON-LINK PREFIXES ARE NOT REVISITED HERE. They are applied once, in
// the Join answer, out of the advertisement the acquisition saw, and
// pkg/dhcp deliberately reports none on this path: the library's router
// table carries the prefixes of the most recent frame, so following
// them live would take the segment's own prefix away from a container
// because one advertisement happened to omit it.
func (m *dhcpManager) reconcileAdvertisedRoutes(info dhcp.Info) error {
	if m.netHandle == nil || m.ctrLink == nil {
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

	// Recorded even when some write failed: the record is "what this
	// manager has asked the kernel for", and a retry on the next
	// advertisement is what the next diff gives anyway.
	m.lastAdvertRoutes = want
	return firstErr
}

// applyRouterAdvert re-applies everything a Router Advertisement can
// change, on a link whose lease has not moved.
//
// WHY IT EXISTS AT ALL: there is no lease event here. The library's v6
// state machine emits Acquired and Renewed for the ADDRESS; an
// advertisement that changes the router, the MTU, the routes or the DNS
// servers and nothing else produces no lease transition, so the chassis
// watches the router table and synthesises this one (see
// pkg/dhcp/chassis.go's raWatchInterval). Without it, a segment that
// renumbers its router keeps every running container pointed at the old
// one until that container is restarted.
//
// THE ADDRESS IS NOT TOUCHED, deliberately: nothing in an advertisement
// changes a DHCPv6 lease, and routing this through renew() would re-run
// the address state machine on every advertisement for no reason.
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

// markBound records that the persistent client of one family holds
// its binding. See boundV4 / boundV6 for what that proof is for.
func (m *dhcpManager) markBound(v6 bool) {
	if v6 {
		m.boundV6.Store(true)
		return
	}
	m.boundV4.Store(true)
}

// neverBound reports whether the persistent client of one family
// stopped without ever holding its binding. Only meaningful once that
// family's consumer goroutine has been drained (see stop) or was never
// started (see the Start-failed path in stop).
func (m *dhcpManager) neverBound(v6 bool) bool {
	if v6 {
		return !m.boundV6.Load()
	}
	return !m.boundV4.Load()
}

// bumpFamily increments EXACTLY ONE of a counter pair: the v4 half or
// the v6 half, never both and never a third aggregate (#212, #730).
//
// It used to bump an aggregate on every event and the _v6 sibling on v6
// ones, which made the v6 counter a subset of the aggregate rather than
// its peer, and left the v4 count to be recovered by subtracting them
// at render time. Two independently-updated atomics combined by
// subtraction can be read in an order that yields a value LOWER than
// the previous read — and a counter that goes down is a reset to
// Prometheus, which then attributes the whole accumulated value as an
// increase. One dropped unit became a rate spike of the entire count.
//
// Storing both halves and adding them where a total is wanted has the
// property subtraction does not: the sum of two monotonic counters is
// monotonic under EVERY interleaving, because neither operand can
// decrease. See healthSnapshot for the addition and #730 for the
// arithmetic.
func bumpFamily(v4Counter, v6Counter intCounter, v6 bool) {
	if v6 {
		v6Counter.Add(1)
		return
	}
	v4Counter.Add(1)
}

// clientServerLists returns the allow/deny lists the persistent client
// for this family is started under.
//
// Both dhcp_servers directives are v4-only, and this is the single
// place that is decided (#111). Split out of setupClient so the rule
// can be asserted directly: dhcp_server_policy_timeouts is deliberately
// not family-split, and the reason it can be is that a v6 client is
// never restricted. That premise lived in a comment beside a two-line
// `if`, which is exactly the shape of thing this project has now twice
// found to be wrong in prose while right in code, and once the other
// way round.
func clientServerLists(pol serverPolicy, v6 bool) (allow, deny []string) {
	if v6 {
		return nil, nil
	}
	return pol.allowList(), pol.denyList()
}

// countOutageTick records one watchdog outage tick.
//
// Split out of the goroutine in setupClient so the accounting can be
// exercised without a live client. The whole meaning of
// dhcp_server_policy_timeouts is a relationship to dhcp_timeouts --
// strict subset -- and a relationship between two counters is not a
// thing a comment can hold: it has to be written by one function that a
// test can call twice.
func (m *dhcpManager) countOutageTick(v6, policyRestricted bool) {
	bumpFamily(&m.plugin.dhcpTimeoutsV4, &m.plugin.dhcpTimeoutsV6, v6)
	if !policyRestricted {
		return
	}
	// The renewal half of #731. dhcp_server_policy_exhausted covers the
	// acquisition half only: acquireWithPolicy walks a ladder and can
	// run off the end of it, while this client holds one whitelist and
	// simply gets no answers. Without this, an allow-list that has gone
	// stale -- the named server renumbered, retired or firewalled -- is
	// indistinguishable from the DHCP server being down.
	//
	// NOT healthy-affecting, and the subset relationship is why: this
	// tick was already counted above, and counting one outage twice
	// would weight a policy-restricted endpoint worse than an
	// unrestricted one failing in exactly the same way.
	m.plugin.dhcpServerPolicyTimeouts.Add(1)
}

// handleEvent dispatches one lifecycle event from the
// persistent client: health counters, audit-ledger entries, and the
// kernel-facing renew work. Extracted from the consumer goroutine so
// the counter semantics are unit-testable — wire-level NAKs in
// particular can't be provoked deterministically (dnsmasq silently
// ignores refused renewals in several shapes instead of NAKing), so
// the naks_received contract is pinned here rather than in an
// integration test (#128).
func (m *dhcpManager) handleEvent(event dhcp.Event, v6 bool) {
	m.noteEvent(event.Type)
	// The hook process already dropped these; all that is left here is
	// to make the drop visible. Counted for every event type, including
	// the data-less ones, because the count describes the exchange and
	// not the lease (#703).
	if event.UnsafeValuesDropped > 0 && m.plugin != nil {
		m.plugin.unsafeOptionValuesDropped.Add(int32(event.UnsafeValuesDropped))
		log.
			WithFields(m.logFields(v6)).
			WithField("dropped", event.UnsafeValuesDropped).
			Warn("DHCP option values dropped before use: they carried control characters")
	}

	switch event.Type {
	// "deconfig" is intentionally not handled. Deleting the
	// container's IP from the kernel would also wipe the
	// static routes Join copied off the host bridge, and
	// there's no clean way to re-derive them without
	// re-running the bridge route copy. Better to keep
	// the stale address until the next bound/renew
	// overwrites it.
	case "bound":
		// The persistent client's first DHCPACK can land
		// on a different IP than CreateEndpoint's initial
		// DISCOVER (some servers, including Fritz.Box,
		// hand out a fresh address per DISCOVER even for
		// the same MAC). Reuse the renew path so LastIP
		// reflects what's actually in the kernel.
		// Ownership of the binding has transferred; Stop no longer has
		// an outstanding one-shot lease to answer for. See boundV4 / boundV6.
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

		// Same proof as "bound", and needed separately: a client that
		// comes up against a lease it already holds can report renew
		// without a preceding bound.
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
		// A DHCPv6 information reply: options, no address (#815). It
		// must NOT touch the address state machine -- no markBound, no
		// setLastIP, no renew. renew() begins with
		// netlink.ParseAddr(info.IP), and Info.IP is empty here by
		// definition, so routing this through the lease path would fail
		// on every stateless network rather than configure one.
		//
		// It also must not restart the outage deadline. nextAcquiring
		// leaves the acquiring state unchanged for any event that is not
		// bound/renew/leasefail, which is the behaviour this case wants
		// and relies on: an information reply is proof the server is
		// reachable, but it is NOT proof we hold a lease, and treating
		// it as one would silence the timeout counter on a network that
		// answers information requests and refuses addresses.
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
		// The routers on this segment changed what they advertise and
		// the lease did not move (#821). There is no lease event for
		// this -- the chassis watches the library's router table and
		// synthesises it -- and without it a container keeps pointing
		// at a router that has gone, keeps an MTU the segment no longer
		// uses, and keeps resolvers that have been replaced, until
		// somebody restarts it.
		//
		// NO markBound AND NO setLastIP, for the same reason the
		// "config" case above has neither: this carries no address and
		// is not proof of a lease.
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
		// EVERY FORMED ADDRESS COMES OFF THE LINK, and nothing here
		// touches the outage counters. The lease held addresses formed
		// from advertised prefixes and holds none now, so the set the
		// container should have is empty; withdrawV6AddrsNotIn removes
		// what is left, counts each one and writes its ledger row. The
		// kernel would eventually drop them on their own valid
		// lifetimes, which is the belt v6AddrAttrs installs, but "when
		// each address's own advertisement runs out" is not the same
		// moment as "the client no longer holds this prefix", and the
		// gap is time the container spends choosing a source address on
		// a prefix that is not routed any more.
		if m.netHandle != nil && m.ctrLink != nil {
			m.withdrawV6AddrsNotIn(nil, auditSource(event.Data))
		}
		log.
			WithFields(m.logFields(v6)).
			WithField("ip", event.Data.IP).
			Warn("This endpoint's IPv6 addresses were formed from a router advertisement and the client no longer holds them")
	case "leasefail":
		// dhcp_timeouts, from the library's Failed{ReasonNoServer}
		// rather than from a ticker. Through countOutageTick, because
		// dhcp_server_policy_timeouts is defined as a STRICT SUBSET of
		// this counter and a relationship between two counters is not
		// something a comment can hold — it has to be written by one
		// function a test can call twice.
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

// startDHCPClient opens the persistent client's socket. A seam, and
// the only one in this file that is not netlink's.
//
// THE ORDER THIS ATTACH DELIVERS IS UNOBSERVABLE WITHOUT IT (#961).
// "No daemon call between Join arriving and the persistent client
// starting" is a statement about an INSTANT, and a count taken when
// Start returns cannot see one: an attach that inspected first leaves
// the same totals. The instant is here. Opening the socket needs
// CAP_NET_ADMIN and a live namespace, which the unit lane has neither
// of, so without a seam the only drive available is the count at the
// end -- which is the measurement that cannot fail.
var startDHCPClient = func(c *dhcp.DHCPClient) (chan dhcp.Event, error) { return c.Start() }

func (m *dhcpManager) setupClient(v6 bool) (chan error, error) {
	v6Str := ""
	if v6 {
		v6Str = "v6"
	}

	log.
		WithFields(m.logFields(v6)).
		Info("Starting persistent DHCP client")

	// THE LINK'S NAME IS READ HERE, NOT WHERE THE LINK WAS FOUND. The
	// engine moves the link into the sandbox and then renames it, and
	// locateContainerLink takes the link the moment it appears, so the
	// snapshot it leaves can carry the pre-rename name. That was
	// harmless while this call followed it immediately. The reorder put
	// the hostname inspect in between, and that wait is the whole of
	// #406: on a busy daemon it is most of the attach budget, and the
	// client is then opened by a name the kernel no longer has. Hosted
	// run 34624582681 did exactly that -- phases
	// "locate_link=0.22s resolve_container_id=0.42s", then no such
	// network interface for dh-3b1d3b0061fd -- and the container it
	// belonged to kept no renewal client at all.
	//
	// The index survives a rename, so re-reading by it is what makes
	// the name current. A read that fails leaves the snapshot in place:
	// the link being gone is what the client open is about to report,
	// with the reason, and there is nothing better to say here (#417).
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

	// WHAT THIS MANAGER MAY ASK THE SERVER FOR, and the difference
	// between the two answers is a whole RFC section.
	//
	// The record holds the lease the CreateEndpoint one-shot won, or
	// the one a previous plugin process was renewing. If it is still
	// unexpired, the first packet on the wire is an INIT-REBOOT
	// DHCPREQUEST (RFC 2131 section 4.4.2): the server confirms the
	// address or NAKs, and the container keeps the IP it had across a
	// plugin restart instead of being handed a new one. If the record
	// only PREFERS an address — a tombstone's, or a lapsed lease's —
	// that goes out as option 50 in an ordinary DHCPDISCOVER, which a
	// server may ignore (section 4.4.1 makes it a MAY).
	//
	// The two are never both set: the library's Record.Prefer refuses
	// whatever Record.Resume answers.
	//
	// lastIPs() is the fallback for an endpoint with no record at all —
	// one adopted from Docker's own view during recovery. It is what
	// this function did for every endpoint before the record existed,
	// and it is strictly weaker: Docker knows the address and nothing
	// about the lease behind it, so there is no expiry to decide
	// whether an INIT-REBOOT is even legal.
	requestedIP := ""
	preferredV6 := ""
	var (
		resumption dhcp.Resumption
		identity6  dhcp.Identity6
		recordID   string
	)
	if !v6 {
		m.recordID, resumption = m.resumeFromRecord()
		recordID = m.recordID
		requestedIP = resumption.Prefer
		if resumption.Lease == nil && requestedIP == "" {
			if v4Addr, _ := m.lastIPs(); v4Addr != nil && v4Addr.IP != nil {
				requestedIP = v4Addr.IP.String()
			}
		}
	} else {
		// The v6 record answers BOTH questions a v6 manager has: what
		// it may ask the server for, and who it is while asking. RFC
		// 9915 section 18.2.12's Confirm is only worth sending under
		// the DUID the binding was made with.
		m.recordID6, resumption, identity6 = m.resumeFromRecord6()
		recordID = m.recordID6
		preferredV6 = resumption.Prefer
		if resumption.Lease == nil && preferredV6 == "" {
			if _, v6Addr := m.lastIPs(); v6Addr != nil && v6Addr.IP != nil {
				preferredV6 = v6Addr.IP.String()
			}
		}
		if identity6.IsZero() {
			// No record, or a record with no identity: this endpoint
			// was adopted from Docker's own view during recovery, or
			// its record was written by a build that had no DUID.
			// Minting one here is a NEW client to the server -- a new
			// binding and a new address -- and it is still better than
			// refusing to start the endpoint, so it is loud rather
			// than fatal.
			id6, err := resolveIdentity6(m.opts, m.joinReq.EndpointID, m.endpointMAC())
			if err != nil {
				return nil, fmt.Errorf("no DHCPv6 identity for this endpoint: %w", err)
			}
			identity6 = id6
			log.WithFields(m.logFields(true)).
				Warn("No stored DHCPv6 identity for this endpoint; minting one. The server sees a new client and will grant a new address")
		}
	}
	// The persistent client gets the WHOLE allowed set, not the single
	// tier that won acquisition: it must still be able to rebind after
	// the preferred server goes away, and a whitelist pinned to that one
	// server would strand the endpoint with no lease rather than fail
	// over. Preference is an acquisition-time concept (#111) — the lease
	// then stays put on its own, because renewal is unicast to whoever
	// granted it. v6 gets neither list; both directives are v4-only.
	//
	// The options were validated at CreateNetwork, so an error here means
	// the persisted state is corrupt. Refuse rather than silently start
	// an unrestricted client, which would ignore a deny-list.
	pol, err := resolveServerPolicy(m.opts)
	if err != nil {
		return nil, fmt.Errorf("invalid persisted DHCP server policy: %w", err)
	}
	allowServers, denyServers := clientServerLists(pol, v6)

	// Whether THIS client is restricted to an operator-named server
	// list. Captured on the manager rather than re-resolved where it is
	// read: a second resolveServerPolicy could disagree with what the
	// client was actually started with, and the counter would then
	// describe a policy that is not in force.
	m.policyRestricted = len(allowServers) > 0

	clientOpts := dhcp.DHCPClientOptions{
		Hostname:     m.hostnameOnTheWire(),
		AllowServers: allowServers,
		DenyServers:  denyServers,
		FQDN:         m.opts.fqdnMode(),
		V6:           v6,
		NetNS:        &m.nsHandle,
		// Same MAC the CreateEndpoint one-shot used — this is the same
		// link, moved into the netns — so the chaddr and the derived
		// client-id are identical and the server renews the very lease
		// Docker was told about (#152).
		MAC:         m.ctrLink.Attrs().HardwareAddr,
		RequestedIP: requestedIP,
		// The record's unexpired lease, which makes the first packet an
		// INIT-REBOOT rather than a DISCOVER. nil is the ordinary
		// CreateEndpoint -> Join path having found nothing to resume.
		Resume:   resumption.Lease,
		Records:  m.recordStore(),
		RecordID: recordID,
		// No Broadcast option: the library sets the BROADCAST flag of
		// RFC 2131 section 2 by default and the chassis no longer
		// overrides it. The ipvlan reason this used to name (#243 --
		// slaves share the parent MAC, so a unicast renewal cannot be
		// demuxed to the right slave) is real and is now covered as a
		// special case of the general one: every mode runs on a raw
		// AF_PACKET socket. See the note in pkg/dhcp/params.go.
		// Same client-id the initial DISCOVER used in CreateEndpoint, so
		// renewals are seen as the same client by the server. Derived
		// from the MAC the one-shot ran under rather than from the link
		// in hand (#371). Honours the operator's client_id override.
		ClientID:    m.clientID(),
		VendorClass: m.opts.VendorClass,
		// HonorRouterAdverts is REQUIRED on a persistent v6 client and
		// refused on every other shape, which is what makes "the v6
		// endpoint's kernel is processing Router Advertisements" a
		// precondition the client cannot start without (#875, D30 Q3).
		// The rest of the v6 halves -- identity, record, preferred
		// address and mode -- arrive together just below.
		HonorRouterAdverts: v6,
	}
	if v6 {
		// The third and last client this plugin starts. The manager
		// already holds the v6 record id in recordID, so this call
		// re-states it rather than changing it; what it adds is the
		// mode, which the persistent client needs for the same reason
		// the one-shots do -- a renewal or a rebind runs the machine
		// the mode selected, not the one its zero value names.
		if err := m.plugin.v6Wiring(&clientOpts, m.opts, identity6, recordID, preferredV6, m.joinReq.EndpointID); err != nil {
			return nil, err
		}
	}
	if err := m.plugin.conflictWiring(&clientOpts, m.opts, roleJoin, m.joinReq.NetworkID, m.joinReq.EndpointID, v6); err != nil {
		return nil, err
	}
	// HERE AND NOT IN conflictWiring, AND ON THIS PATH ONLY. This is
	// the persistent client, the only one that holds a lease long
	// enough to renew it; the two roleAcquire call sites are
	// CreateEndpoint one-shots (#940).
	m.plugin.renewalWiring(&clientOpts, m.joinReq.NetworkID, m.joinReq.EndpointID, v6)
	// THE PHASE IS NOT PASSED TO THE CLIENT, and there is nothing for it
	// to do there: proto.Machine runs RFC 5227 section 2.1's check on
	// the INIT-REBOOT DHCPACK whatever the record said, so the resumed
	// address is re-checked either way (D23; the library states it on
	// lease.Record.ACD). What the durable phase buys is the line below —
	// the operator's only evidence that this process picked up an
	// address a previous one never finished checking.
	m.noteResumedACD(resumption, clientOpts.ConflictMode, v6)

	client, err := dhcp.NewDHCPClient(m.ctrLink.Attrs().Name, &clientOpts)
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

	// Buffered: a partial-Start failure (v4 OK, v6 fails) bypasses Stop's
	// errChan reads; Stop short-circuits on m.startErr. Without a buffer
	// the goroutine here would block forever on the final write below.
	errChan := make(chan error, 1)
	go func() {
		for {
			select {
			case event, ok := <-events:
				if !ok {
					// The manager returned on its own: the link went
					// away, the sandbox was torn down under it, or Run
					// hit an error it could not continue from. The
					// chassis closes this channel when its translate
					// goroutine ends. Without this branch a receive on
					// a closed channel returns the zero Event every
					// iteration, the switch matches nothing, and this
					// goroutine spins a core forever.
					log.
						WithFields(m.logFields(v6)).
						Warn("dhcp event stream closed; the renewal client stopped")

					// Wait is not a reap any more — there is no child
					// process to leave a zombie — but it is still the
					// only thing that says Run has RETURNED, and the
					// AF_PACKET socket is closed there. Leave without
					// it and a Join for the next container can open a
					// second client on the same interface while this
					// one is still on it.
					reapCtx, reapCancel := context.WithTimeout(context.Background(), dhcpClientReapTimeout)
					if err := client.Wait(reapCtx); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							Debug("waiting for the renewal client returned an error")
					}
					reapCancel()

					// Unblock Stop() if it's waiting on errChan. The
					// channel is buffered=1 so this never blocks; if
					// nobody's reading yet, the value sits until Stop
					// calls close(stopChan) and reads it.
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

// locateContainerLink populates m.ctrLink with the post-Docker-move
// interface inside the container netns. The mechanism differs by mode:
//
//   - bridge: veth peer indexes are symmetric, so we read the host-side
//     veth's peer index and look that up in the sandbox netns. We also
//     wait for Docker's rename (the link must no longer carry the
//     pre-move name) so the persistent client doesn't race the move.
//   - macvlan / ipvlan: only one link is created and Docker moves it
//     wholesale, so we identify it by MAC after it reappears in the
//     sandbox. For ipvlan the child shares the parent's MAC, but the
//     parent is not in the container netns, so the MAC is still unique
//     within the search scope (loopback's MAC is all-zeros).
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
				// Not in the container netns yet — keep polling.
				return false, nil
			}
			m.ctrLink = link
			return true, nil
		}, pollTime)
	}

	hostName, oldCtrName := vethPairNames(m.joinReq.EndpointID)
	hostLink, err := netlink.LinkByName(hostName)
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

// joinPhases records how long each stage of Start took, so a Join that
// runs out of budget can say WHERE the budget went.
//
// Start is one deadline covering five quite different waits: resolving
// the endpoint to a real container ID, inspecting that container,
// opening its netns, locating its link, and starting the client. When it
// expires, every one of them reports the same "context deadline
// exceeded", and the two explanations that matter are indistinguishable
// (#406):
//
//   - the daemon was genuinely slow, and the container is still running
//     with no renewal client — a real fault;
//   - an earlier phase consumed the budget, so a later one inherited an
//     already-expired context and failed instantly against a perfectly
//     healthy daemon.
//
// Those want opposite fixes — a bigger budget, or a budget spent
// differently — so guessing between them is how #401's first attempt
// went wrong. This makes the next ordinary CI run answer it, rather
// than needing a reproduction nobody has managed to build locally.
//
// Deliberately not a health counter. This is diagnostic detail for a
// failure that is already being counted and logged; adding a counter
// per phase would put five numbers on an operator's health surface to
// answer a question only a developer asks.
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

// mark closes the phase that has just finished.
func (p *joinPhases) mark(name string) {
	if p == nil {
		return
	}
	now := time.Now()
	p.spans = append(p.spans, joinPhaseSpan{name: name, took: now.Sub(p.last)})
	p.last = now
}

// summary renders the phases as a log field: "resolve_id=8.9s inspect=1.1s".
// The phase that ate the budget is then the obvious one to read.
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

// total is the whole of Start, for reading against the budget.
func (p *joinPhases) total() time.Duration {
	if p == nil {
		return 0
	}
	return time.Since(p.start)
}

// openSandboxNetNS opens the container's network namespace, preferring
// the sandbox key Docker publishes and falling back to the container's
// PID, and counts what it did.
//
// TWO COUNTERS, NOT ONE, AND THAT IS THE MEASUREMENT. sandbox_key_entries
// says the key path carried an open; sandbox_pid_fallbacks says one left
// it. A single "no fallbacks" reading is satisfied by a plugin that never
// opened a namespace at all, so the pair is what makes "every entry went
// through the key" a statement with a domain. The integration cells assert
// both deltas, per cell, and the PID route is removed only on the strength
// of that -- not on the strength of nothing having failed.
//
// The count lives HERE, wrapped around the open, rather than at the call
// site in Start, and that placement is the point: a caller cannot get
// the namespace without going through this, so the counter cannot be
// lost by a future path that opens the namespace and forgets to look
// for the sentinel. It also makes the branch reachable from a unit test
// -- Start needs Docker, netlink and a live namespace, and until this
// existed nothing executed the increment at all. docs/reference.md says
// netns_pid_mismatches is the ONLY thing that distinguishes a PID-reuse
// refusal from a slow container start, so an operator reads its zero as
// "did not happen" (#731 review).
func (m *dhcpManager) openSandboxNetNS(ctx context.Context, sandboxKey string, pid int, ctrID string, interval time.Duration) (netns.NsHandle, error) {
	return m.openSandboxNetNSLazyPID(ctx, sandboxKey, interval, func() (int, string, error) {
		return pid, ctrID, nil
	})
}

// openSandboxNetNSLazyPID is the same opener with the container's PID
// obtained only if the key route is refused.
//
// THE POINT OF THE LAZINESS IS WHO IS ASKED, NOT WHEN. The PID and the
// container ID come from the daemon, and the attach runs while the
// daemon is inside ContainerStart for the very container being attached
// (#406), so resolving them up front put a call that can block for the
// length of a container start in front of a route that needs neither.
// Where the sandbox key resolves, this plugin now enters the namespace
// with no daemon call at all; where it does not, resolvePID runs and
// the cost is exactly what it always was (#417).
//
// resolvePID's error is reported with the key error beside it, for the
// same reason the fallback's is: the key refusal is what made the PID
// necessary, and reporting only the second is how the first became
// invisible.
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
	// DEBUG, NOT WARN, AND THE LEVEL IS DERIVED FROM WHAT AN OPERATOR
	// SHOULD DO ABOUT IT: nothing. On a host whose sandbox netns mount is
	// private (sandbox_netns_propagation=0) this fires once per attach,
	// for every container, forever -- the daemon's per-sandbox netns
	// mounts are made after the plugin's own /var/run/docker bind was
	// taken and a private mount does not deliver them, so the key
	// resolves to the placeholder file and the PID route carries the
	// attach exactly as it did before the key route existed. A warning is a request for attention, and a request for
	// attention that is correct on every attach of a healthy host trains
	// its reader to ignore the level.
	//
	// The signal is not lost by lowering it. sandbox_key_entries,
	// sandbox_key_entry_failures, sandbox_pid_fallbacks and the four
	// arm counters are on /Plugin.Health and /metrics at every level,
	// and they are what says which route this host takes. This line is
	// the detail behind them, and detail is what Debug is for.
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
		// Both routes failed. The key error is the one that explains
		// why the fallback was reached at all, and reporting only the
		// second is how the first became invisible.
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
		// Recorded on the manager rather than logged from here.
		// #411 put this on its own Debug line, and the line was
		// invisible where it mattered: the health floor's evidence
		// dump prints error and warning lines, so the run that
		// failed showed six "context deadline exceeded" errors with
		// no timing anywhere near them. A diagnostic that only
		// appears somewhere else is not a diagnostic — the caller
		// now folds these onto the failure line itself (#406).
		m.startPhases = phases.summary()
		m.startTotal = phases.total().Round(10 * time.Millisecond).String()
		close(m.startedCh)
	}()
	// WHAT THIS ORDER DELIVERS, AND WHAT IT DOES NOT (#417, #961).
	//
	// Entering the container's network namespace, finding its link and
	// STARTING THE PERSISTENT CLIENT no longer need the daemon. The
	// container inspect that supplies DHCP option 12 has moved to the
	// far side of the client start: the library takes a name on a
	// RUNNING client and renews early to carry it (RFC 2131 section
	// 4.4.5), so the name no longer has to be in hand before the socket
	// is opened.
	//
	// It matters because of what the daemon is doing at the time. The
	// attach runs in a goroutine Join does not wait for, and the daemon
	// is inside ContainerStart for this same container while it runs
	// (#406), so every call made here can block for the length of a
	// container start. Before this order, a host whose sandbox key
	// resolves still paid that wait before it opened anything, and a
	// daemon that never answered meant no namespace, no link and no
	// client -- on a host where the key alone would have carried all
	// three. Now it means a container that leases on time and is named
	// when the daemon gets round to answering.
	//
	// THE WAIT IS NOT GONE, IT IS AFTER THE CLIENT, so
	// attachDaemonBusyGrace stays load-bearing: a busy daemon still
	// stretches the attach, joinAttachSlow still counts it, and the
	// grace is still what keeps that from being read as a plugin
	// failure. What changed is what the container has while it waits.
	//
	// THE CLAIM IS ABOUT THIS FUNCTION, AND JOIN DOES NOT ALWAYS REACH
	// IT FIRST. A Join carrying no hint rebuilds the endpoint before
	// any attach begins: on `docker restart` libnetwork sends Leave
	// then Join on the same endpoint with no CreateEndpoint between
	// them, so reacquireEndpoint recovers the MAC from the daemon and
	// replays CreateEndpoint, whose own hostname lookup and one-shot
	// exchange run against the same busy daemon. That route still pays
	// the wait, in Join and not here, and
	// TestReacquireEndpoint_AsksTheDaemonBeforeTheAttachBegins pins it.
	// What this order delivers is a container START that does not wait.
	//
	// TWO SHAPES STILL TAKE THE NAME BEFORE THE START, and both are
	// below rather than here:
	//
	//   - the routes that have already inspected. Where the sandbox key
	//     is refused, the PID fallback asked the daemon on the way in,
	//     so the name is already in hand and deferring it would buy
	//     nothing and delay it for no reason.
	//   - register_dns. That option puts the name in RFC 4702's option
	//     81, which section 3.1 forbids the Host Name option beside,
	//     and the library takes option 81 at construction and offers no
	//     setter for it. Such a network pays the wait the default
	//     network no longer pays; docs/reference.md says so on the
	//     register_dns row.
	var (
		ctrID         string
		ctrPID        int
		ctrName       string
		ctrHostname   string
		ctrSandboxKey string
		inspected     bool
	)
	// inspect resolves the container behind this endpoint and reads the
	// three fields the attach wants from it. Called at most once: both
	// the PID fallback and the hostname want the same answer, and a
	// second call on a daemon this busy is a second wait.
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

			// Seems like Docker makes the container ID just the endpoint until it's ready
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

		// Field by field, each guarded: an inspect that answers with a
		// section missing is a daemon disagreeing with its own API, and
		// a nil dereference there would surface as a plugin crash
		// rather than as the attach failure it is.
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

	// The sandbox key is the primary route (sandbox_netns.go). Join
	// carries it in the request; recovery synthesises a request that
	// carries none and reads it from the inspect, which is why that
	// path still inspects first -- there is no key to try until the
	// daemon has answered, and always the daemon's current answer
	// rather than a value this plugin wrote down earlier and might be
	// wrong about.
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
			// A link that never appears has two causes, and they want
			// opposite answers from an operator: a container whose link
			// is late or already gone, which is a fault, and an
			// endpoint no container ever claimed, which is not one
			// (#566). The namespace and the link are found without the
			// daemon, but telling those two apart is precisely what the
			// daemon knows and this plugin cannot see.
			//
			// Before the reorder the container-ID poll ran first and
			// answered this by construction, so util.ErrNoContainer was
			// the only way out of an unclaimed endpoint's attach and
			// join_aborted_no_container was the counter that moved. The
			// reorder made the link lookup fail first, with a deadline
			// that is not that error, and charged an endpoint nobody
			// claimed to join_start_failures, which is Healthy-
			// affecting and pages about a container that does not
			// exist. Only a host that takes the key route reaches it,
			// which is why the pool lane stayed green and the hosted
			// one went red.
			//
			// So the question is asked here, once, on what is left of
			// the attach budget: the macvlan wait is capped at
			// linkAwaitTimeout inside a window of awaitTimeout plus
			// attachDaemonBusyGrace, so the answer is affordable. If
			// nothing is left, the link error stands on its own, which
			// is the behaviour without this branch.
			if ierr := inspect(); ierr != nil {
				return fmt.Errorf("%w (no link for this endpoint in the sandbox: %w)", ierr, err)
			}
			return err
		}

		phases.mark("locate_link")

		// THE NAME BEFORE THE START, IN THE TWO CASES THAT WANT IT
		// THERE (#961). Everything else takes it afterwards, from
		// nameTheRunningClient below.
		//
		// `inspected` is the route that already asked: the daemon has
		// answered, so the name costs nothing here and deferring it
		// would only put it on the wire later than it needs to be.
		// fqdnMode is register_dns, whose option 81 the library takes
		// at construction and has no setter for.
		//
		// Config-only either way: the name reaches the DHCP hostname
		// option and nothing that makes an identity decision, so a
		// refusal is just an omitted option here.
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
			// The engine disables IPv6 outright on a sandbox interface
			// whose endpoint carries no IPv6 address, which is now a
			// reachable state (#868). Clear that BEFORE the link-local
			// wait below, because on a disabled link the link-local
			// never appears at all and the wait would time out for a
			// reason DAD has nothing to do with. See v6_link.go.
			m.ensureIPv6Enabled()

			// THE LINK-LOCAL WAIT IS THE LIBRARY'S AND IS NOT REPEATED
			// HERE. DHCPv6 needs a usable link-local source address —
			// the link has just landed in this netns, so its LL is
			// typically still DAD-tentative, and a host must NOT answer
			// neighbor solicitations for a tentative address, so the
			// server's unicast ADVERTISE/REPLY can never be delivered
			// (#103, found by TestLeaseRenewIPv6_HonorsT1). setupClient
			// reaches runtime.InterfaceLinkLocal, which resolves the
			// interface on the calling thread, refuses a tentative or
			// dad-failed address and waits up to its own derived bound
			// for a usable one. A wait here as well is a SECOND
			// derivation of one fact: it was ten seconds against the
			// library's four, so a link whose LL never clears spent
			// fourteen seconds of the Join deadline reaching the same
			// refusal (#911 review round 1, finding 5). The property
			// that keeps it gone is
			// TestTheChassisDoesNotWaitForALinkLocalItself.
			if m.errChanV6, err = m.setupClient(true); err != nil {
				close(m.stopChan)
				// The v4 consumer goroutine is already live and may be
				// mid-renew on m.netHandle; stopChan only signals it.
				// Drain its exit ack so the outer cleanup can't close
				// the netlink/netns handles out from under it (and so
				// the v4 client is stopped, not orphaned).
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

	// AFTER THE ATTACH HAS SUCCEEDED, AND ITS FAILURE IS NOT THE
	// ATTACH'S (#961, #978). The container is leasing; what is missing
	// is a name, in the server's table and on the host-side link, which
	// is worth a counter and a log line and is not worth tearing a
	// working endpoint down for.
	m.afterAttach(phases, inspected, inspect, ctrName, &ctrHostname)

	return nil
}

// nameTheRunningClient obtains the container's name and gives it to the
// v4 client that is already leasing (#961).
//
// lookup is the attach's own inspect closure, so this makes no second
// daemon call: where the name was already in hand the caller does not
// come here at all. name points at the field that closure fills.
//
// It reports whether the daemon answered, which is a different
// question from whether the name was handed over: a refused or absent
// hostname still means the inspect succeeded and the rest of the
// attach has the container's fields. #978 renames the host-side link
// on that answer, so it is returned rather than read back out of the
// closure's captured variable, where a reordering would take it
// silently.
//
// NOTHING HERE FAILS THE ATTACH. Every arm is counted instead, because
// each one leaves a different thing true: the daemon never answered,
// the client would not take the name, or the name was refused before it
// got that far. docs/reference.md carries the three rows.
func (m *dhcpManager) nameTheRunningClient(phases *joinPhases, lookup func() error, name *string) bool {
	if err := lookup(); err != nil {
		m.plugin.hostnameLookupFailures.Add(1)
		log.WithError(err).
			WithFields(m.logFields(false)).
			Warn("The container's name could not be read from the daemon; this endpoint holds its lease but the DHCP server's table has no name for it")
		return false
	}
	phases.mark("hostname")

	// safeHostname REFUSES as well as reporting absent, and both
	// arrive here as an empty string. Neither is handed over: the
	// library treats an empty name as "stop sending option 12", which
	// is what this client is already doing, so the call would be a
	// no-op that made hostnames_applied_late rise on every container
	// started without --hostname.
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
	// AFTER THE HANDOVER, NOT BEFORE IT. audit() reads this field for
	// every ledger row on an audit_log network and the accessor is
	// named for what it means: the name this endpoint puts on the
	// wire. Recording it on the way past would make the ledger name an
	// endpoint the server was never told about, on both arms above,
	// while hostname_apply_failures said the opposite.
	m.setHostname(safe.name)
	m.plugin.hostnamesAppliedLate.Add(1)
	log.WithFields(m.logFields(false)).
		WithField("hostname", safe.name).
		Info("The container's name was given to the running DHCP client, which asks the server to record it at once")
	return true
}

// Stop shuts the persistent clients down WITHOUT assuming the endpoint
// is going away.
//
// This is the shutdown every caller but Leave wants: plugin Close stops
// every live manager so their persistent clients close their sockets and
// their goroutines return rather than being cut off mid-exchange by
// process exit, and the containers behind them keep running.
// Same for a manager displaced by a newer one for the same endpoint, and
// for managers cleaned up when a network is removed.
//
// Stopping is all it is. Since #800 neither this nor the leaving variant
// releases the lease, which is why the two produce identical ledger
// entries and identical counters — asserted as an equality in
// TestStop_LeavingAndNotLeavingAreTheSame.
func (m *dhcpManager) Stop() error {
	return m.stop(false)
}

// StopForLeave is Stop for an endpoint that is being torn down.
//
// The difference was the orphan reclaim, removed in #800. A lease whose persistent client
// never took ownership has to be handed back, and only here is that
// unambiguously safe: the endpoint is leaving, so nothing is going to
// use the address again.
//
// Getting this wrong is worse than the leak it fixes, which is why it
// is a separate entry point rather than a flag with a default. On
// plugin Close the containers are still running and still using their
// addresses; reclaiming there would tell the server an address is free
// while a live container holds it, and the server would be entitled to
// hand it to somebody else. That is the duplicate-assignment failure
// this release added conflict detection for (#524) — manufactured by
// the plugin itself.
func (m *dhcpManager) StopForLeave() error {
	return m.stop(true)
}

func (m *dhcpManager) stop(leaving bool) error {
	// Abort an attach that is still running before waiting for it.
	// Without this, the attach grace added for #406 would be charged to
	// every Leave that arrives during one — libnetwork would block for
	// the full grace waiting on an attach whose container is already
	// leaving.
	if m.attachCancel != nil {
		m.attachAborted.Store(true)
		m.attachCancel()
	}
	// Wait for Start to finish so we don't tear down half-initialised
	// state.
	<-m.startedCh

	// THE RELEASE, AND THE `leaving` ARM IS THE WHOLE GUARD ON IT
	// (#962). Plugin.Close, a manager displaced by a newer one for the
	// same endpoint, and the cleanup that follows `docker network rm`
	// all arrive here through Stop, with leaving false and their
	// containers still running: releasing there tells the server an
	// address is free while a live container holds it, which is the
	// duplicate assignment #524 added detection for, manufactured by
	// the plugin. TestReleaseLease_StopDoesNotRelease drives that arm
	// under a network that DOES release, which is the only shape where
	// the guard can be seen to do anything.
	//
	// ABOVE THE startErr RETURN, and that placement is the whole of
	// what a failed Join gets. The release is built from the durable
	// record, so a persistent client that never started takes nothing
	// away from it: the one-shot at CreateEndpoint acquired an address
	// and wrote it into the record, and that address is what goes back.
	// Below this return the endpoint would be silent instead -- neither
	// sent nor failed -- on the one population where an operator who
	// asked for releases most wants to see what happened.
	//
	// Before close(m.stopChan) and before the clients are drained, and
	// the reason is no longer the client. The release is built from the
	// durable record and sent from the host, so it does not need the
	// client at all; what it does need is the v6 address still on the
	// container link to be takeable off it (RFC 9915 section 18.2.7),
	// and the container's namespace still open for that. Both are gone
	// once the teardown below has run.
	if leaving && m.opts.releasesOnStop() {
		releasedV4, releasedV6 := m.releaseHeldLeases()
		m.releasedV4.Store(releasedV4)
		m.releasedV6.Store(releasedV6)
	}

	// `on_remove` releases NOTHING here, and that is the value working
	// rather than the value missing (#984). Its release is timed: the
	// record keeps its lease and its deadline, exactly as under
	// `never`, and the sweep hands the address back when the restart
	// window has run out. All that happens at the stop is the line
	// that says so.
	if leaving && m.opts.releasesOnRemove() {
		m.announceDeferredRelease()
	}

	if m.startErr != nil {
		// No persistent client ever ran, so there is nothing to stop,
		// and the CreateEndpoint one-shot's lease is left where
		// it is. It expires on its own (#800).
		//
		// This block used to reclaim that lease when the endpoint was
		// leaving, on the reasoning that an address nobody took over is
		// an address leaked. The reasoning was sound and the mechanism
		// was not: the reclaim could not tell "this endpoint is gone for
		// good" from "this endpoint is coming straight back", because at
		// the moment it runs those two look identical. A `docker
		// restart` is a Leave followed by a Join for the SAME MAC, and
		// the tombstone exists precisely to promise that restart the
		// same address — so a reclaim on the leaving half raced a
		// promise the joining half was about to collect, and was
		// observed handing back an address a live container then came
		// back and used.
		//
		// Waiting for it to expire is what a lease is for. A physical
		// host that loses power does not release anything either; the
		// server holds its address for the lease time and hands it back
		// when the host returns. A container is a host on this segment
		// and now costs the server exactly what one costs.
		//
		// A `release_lease=on_stop` network has already had its chance
		// by the time this runs: the release is attempted at the top of
		// stop(), above this return, precisely so that a Start failure
		// does not silently skip it (#962). It is built from the record,
		// so on that network the one-shot's address has usually gone
		// back and the paragraph above describes the `never` default.
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

	// Guard against zero handles: Stop can be called against a manager
	// whose Start failed before awaitContainerNetNS / NewHandleAt set these
	// (see C-2 fix), in which case the deferred Close on the zero
	// value emits a noisy EBADF.
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

	// Drain BOTH consumer goroutines before doing anything else — in
	// particular before this function can return and run the deferred
	// handle closes. A v4 release failure must not leave the v6
	// consumer live and mid-renew on m.netHandle while the deferred
	// closeNetHandle nils the netlink socket out from under it (the
	// netlink Handle's Close is unsynchronized against requests).
	lastIP, lastIPv6 := m.lastIPs()
	errV4 := <-m.errChan
	var errV6 error
	if m.opts.ipv6Enabled() {
		errV6 = <-m.errChanV6
	}

	// What the shutdown meant is decided by whether this client ever
	// held a binding, NOT by how its process ended. That ordering is
	// the whole of #607.
	//
	// The stop error says how the client ENDED, which is a different
	// question. In 1.x it was a process exit status: dhcpcd answered
	// SIGTERM by exiting 0, but only once it was far enough into startup
	// to have installed the handler, so a client signalled before that
	// died ON the signal and Finish reaped "signal: terminated". The
	// library returns its own cancellation error in the same position.
	// Testing errV4 first therefore routed the never-bound case into the
	// stop-failure branch below, counting a fault where none had
	// occurred. That is
	// #549's bug one branch to the left, and the comment this replaces
	// stated the assumption that hid it: "the client exited cleanly, so
	// errV4 is nil". Sometimes it is not, and it changes nothing — a
	// client that never bound cannot have failed to release a lease it
	// never had.
	//
	// Reading boundV4 / boundV6 is safe here and only here: each
	// family's consumer goroutine is the sole writer of its flag and
	// the receive from its error channel above is its last act.
	//
	// The v6 client is judged by exactly the same rule (#608). Until
	// then it was judged on its exit error alone: a v6 client signalled
	// before it bound exits cleanly, so the ledger recorded "stopped"
	// for the IA_NA address the CreateEndpoint one-shot had taken — the
	// ledger asserting the server saw a DHCPv6 RELEASE for a specific
	// address that no client ever held a binding to release — and the
	// address itself was left leased upstream, because the reclaim was
	// v4-only. Both families now go through settleFamily, and one
	// reclaim covers whichever of them is owed.
	neverBoundV4 := m.settleFamily(false, lastIP, errV4, leaving)
	neverBoundV6 := false
	if m.opts.ipv6Enabled() {
		neverBoundV6 = m.settleFamily(true, lastIPv6, errV6, leaving)
	}
	if neverBoundV4 || neverBoundV6 {
		// The one-shot's lease is still outstanding for at least one
		// family. It stays outstanding and expires on the server's
		// clock (#800) — the reclaim that used to run here was removed
		// because it raced the tombstone for the same address. Nothing is audited
		// either way: no RELEASE was sent on this path, and writing
		// "stopped" would be the ledger claiming something the server
		// never saw, which is the one thing this ledger exists not to
		// do. `release_lease=on_stop` does NOT reach the same answer: the
		// release runs before this point, is built from the record
		// rather than from the client that never bound, and hands the
		// one-shot's address back (#962). This block is the `never`
		// network's answer and the answer for a release that failed.
		log.WithFields(m.logFields(false)).
			WithField("v4_outstanding", neverBoundV4).
			WithField("v6_outstanding", neverBoundV6).
			Info("a client was signalled before it bound; the one-shot's lease " +
				"is left to expire on the server")
	}

	// A client that never bound reports no error, whatever its exit
	// status: we sent the SIGTERM, it died because we asked it to, and
	// the lease it never took has been dealt with above. Returning the
	// exit status here is what turned a correctly handled teardown into
	// a 500 from Leave, for a shutdown in which nothing actually
	// failed. The startErr path earlier in this function already
	// returns nil on the same reasoning (#607).
	if errV4 != nil && !neverBoundV4 {
		return fmt.Errorf("failed shut down DHCP client: %w", errV4)
	}
	if errV6 != nil && !neverBoundV6 {
		return fmt.Errorf("failed shut down DHCPv6 client: %w", errV6)
	}
	return nil
}

// settleFamily writes down what one family's shutdown meant — ledger
// entry, counter, log line — and reports whether that family's client
// never held its binding, i.e. whether the one-shot's lease for that
// family is still outstanding. Called from stop for v4 always and for
// v6 when the network is dual-stack, after both consumer goroutines
// have been drained. Nothing is done ABOUT an outstanding lease since
// #800: it expires on the server's clock. The caller reports it once,
// across both families, rather than twice.
func (m *dhcpManager) settleFamily(v6 bool, last *netlink.Addr, exitErr error, leaving bool) bool {
	neverBound := m.neverBound(v6)
	neverBoundLog := log.WithFields(m.logFields(v6)).WithField("ip", auditIP(last))
	if neverBound && exitErr != nil {
		// Expected rather than a fault, but recorded so a signalled
		// exit is not simply invisible. Debug, because there is nothing
		// here for an operator to act on: the one-shot's lease is left
		// to expire on the server's clock like any other (#800).
		neverBoundLog = neverBoundLog.WithField("client_exit", exitErr)
	}
	switch {
	case neverBound && leaving:
		// This line said "reclaiming it" until #800, and by then it was
		// naming an action the plugin no longer took — the reclaim it
		// referred to had been deleted. An operator reading it would
		// have been told the lease was handed back when it was not.
		// What is settled here is that nothing is audited as released,
		// which is the honest record: no RELEASE was sent on this path.
		// It is "this path" and no longer "any path" since #962, which
		// gave `release_lease=on_stop` networks a release at Leave. That
		// release is built from the record and does not care that this
		// client never bound, so on such a network the address may well
		// have gone back before this line runs; what stays true here is
		// that nothing the LEDGER writes claims it.
		// TestStop_NoStopPathClaimsAReclaimOrRelease keeps it honest,
		// because prose cannot — and this comment is the proof of that:
		// it named the test's pre-rename spelling long after the rename,
		// so a sentence asserting that prose decays had itself decayed.
		// Nothing observes a Go comment's test references, so this is
		// the one class of decay the suite cannot catch itself.
		neverBoundLog.Info("Persistent client stopped before it ever held the lease; " +
			"the one-shot's lease is left to expire on the server")
	case neverBound:
		// Not leaving, so the address may still be in use by a running
		// container — see StopForLeave. Nothing is released and nothing
		// is audited as released; the lease expires on its own either
		// way, so the two cases differ only in what is worth logging.
		neverBoundLog.Debug("Persistent client stopped before it held the lease; " +
			"the endpoint is not leaving")
	case exitErr != nil:
		// Held a binding and did not shut down cleanly, so
		// SIGTERM -> exit did not complete: the client was killed, timed
		// out, or exited non-zero. Bump so operators can alert on a
		// pattern of clients dying hard — typically points at a wedged
		// client or an over-tight dhcpClientFinishTimeout; split by
		// family like its neighbours so a dual-stack host can tell which
		// client failed (#608).
		//
		// This says NOTHING about the lease. Since #800 no path releases
		// one, so the address is held to expiry whether the client exits
		// cleanly or is killed — which is why the counter and the ledger
		// kind are both named for the client (client_stop_failures,
		// "stop_failed") and not for a release. Both families are
		// audited independently: a failed v4 stop does not hide the v6
		// outcome from the ledger.
		if m.plugin != nil {
			bumpFamily(&m.plugin.clientStopFailuresV4, &m.plugin.clientStopFailuresV6, v6)
		}
		m.audit("stop_failed", auditIP(last))
	default:
		m.audit("stopped", auditIP(last))
	}
	return neverBound
}

// auditIP renders a netlink address for a ledger entry, tolerating
// the nil case (endpoint never completed a bind). netlink.Addr
// embeds *net.IPNet, so the IPNet pointer must be checked before
// reaching the promoted IP field — otherwise an Addr with a nil
// embedded IPNet panics on the guard itself.
func auditIP(addr *netlink.Addr) string {
	if addr == nil || addr.IPNet == nil || addr.IP == nil {
		return ""
	}
	return addr.IP.String()
}
