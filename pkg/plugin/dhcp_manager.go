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

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// linkAwaitTimeout caps how long Start waits for the macvlan child to
// reappear in the container netns under its post-rename name. Bridge mode
// keys off the veth peer index, which is symmetric across netns and
// available immediately, so it doesn't need this.
const linkAwaitTimeout = 30 * time.Second

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
// covered a DHCPRELEASE round trip; the client has nothing to send. And
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

	// ipMu guards lastIP / lastIPv6. Writes happen from the dhcpcd
	// event goroutine (renew); reads happen from Leave after Stop has
	// drained that goroutine. The drain establishes happens-before in
	// practice, but the race detector doesn't always see the channel
	// pairing through `select`, and a future change to stop priority
	// could turn this into a real race. Cheap to make explicit.
	ipMu     sync.Mutex
	lastIP   *netlink.Addr
	lastIPv6 *netlink.Addr
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
	// what happens to every lease. Not a race, the
	// only behaviour. Both flags are now read the same way when the
	// ledger entry for each family is written.
	boundV6 atomic.Bool
	// MacAddress is set in macvlan mode so we can re-find the link inside
	// the container netns after Docker has moved and renamed it. Empty in
	// bridge mode.
	MacAddress net.HardwareAddr

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

	// startPhases / startTotal carry the per-phase timing of a FAILED
	// Start so the caller can log it on the same line as the error.
	// Written once in Start's deferred exit and read only after
	// startedCh closes, which is the same happens-before startErr
	// already relies on. Empty on success — timing a Join that worked
	// belongs in a benchmark, not on an operator's log.
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
	// v6 has no counterpart: 2.0 refuses IPv6 before a client is
	// constructed (see NewDHCPClient), so a v6 field would be nil by
	// construction rather than by observation.
	clientV4 endpointClient
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

// setHealthClient publishes the client the health document reads.
func (m *dhcpManager) setHealthClient(c endpointClient) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	m.clientV4 = c
}

// healthClient is the published client, or nil. The lock is dropped
// before the caller asks the client anything.
func (m *dhcpManager) healthClient() endpointClient {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.clientV4
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
	if m.plugin == nil || m.plugin.ledger == nil || !m.opts.AuditLog {
		return
	}
	m.plugin.ledger.Append(ledgerEntry{
		Kind:      kind,
		Network:   m.joinReq.NetworkID,
		Endpoint:  m.joinReq.EndpointID,
		Container: m.containerID(),
		Hostname:  m.hostname,
		IP:        ip,
		MAC:       m.macString(),
	})
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

	// Address first, routes after — the ordering the kernel itself
	// requires (see applyAddressChange).
	if err := m.applyAddressChange(v6, ip); err != nil {
		return err
	}

	m.logObservedOptions(v6, info)

	// Track the freshly-bound address so Leave can hand it to the
	// tombstone (and thus the next CreateEndpoint's `request`-directive
	// hint). Without this the manager keeps reporting whatever the very
	// first CreateEndpoint DISCOVER produced, even if dhcpcd has
	// moved to a different lease since. After applyAddressChange, which
	// needs the previous value.
	m.setLastIP(v6, ip)

	m.propagateDNS(v6, info)
	m.propagateMTU(v6, info)

	return m.reconcileDefaultRoute(v6, info)
}

// applyAddressChange re-applies the lease to the link when the server
// handed back a different address than the one currently recorded. A
// no-op on the steady-state renewal path, which is the common case.
func (m *dhcpManager) applyAddressChange(v6 bool, ip *netlink.Addr) error {
	v4, v6Last := m.lastIPs()
	lastIP := v4
	if v6 {
		lastIP = v6Last
	}
	if lastIP == nil || ip.Equal(*lastIP) {
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
	// Applies to both families now (#152): dhcpcd pins the same
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
	if err := m.netHandle.AddrReplace(m.ctrLink, ip); err != nil {
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

// propagateDNS applies DHCP option 6 / 23 (DNS server list) when opt-in
// and the server actually supplied servers. Empty list is a no-op rather
// than a clobber — see resolvconf.go for the rationale. v6 path
// uses DHCPv6 option 23, populated by the dhcpcd handler into the
// same DNSServers slice. Never fails the renewal: name resolution is
// recoverable, the lease is not.
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

	if err := writeContainerResolvConf(pid, ctrID, info.DNSServers, info.SearchList, info.Domain); err != nil {
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

// propagateMTU applies DHCP option 26 (Interface MTU) when both opt-in
// and non-zero. Skipping zero is mandatory: dhcp-handler emits 0
// when the server didn't supply the option, and forcing MTU 0
// on a kernel link is undefined / disallowed.
func (m *dhcpManager) propagateMTU(v6 bool, info dhcp.Info) {
	if !m.opts.PropagateMTU || info.MTU <= 0 {
		return
	}

	// Neither dhcpcd nor the kernel holds the bottom of this range: a
	// server-supplied 68 was exported verbatim and accepted by the
	// kernel, which destroys throughput and black-holes path MTU
	// discovery for the container, re-applied on every renewal. Refuse
	// and keep the MTU the link has (#702).
	if !mtuAcceptable(info.MTU) {
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

	current := m.ctrLink.Attrs().MTU
	if current == info.MTU {
		return
	}

	if err := m.netHandle.LinkSetMTU(m.ctrLink, info.MTU); err != nil {
		// Don't fail the renewal — IP/gateway are usable; MTU
		// is a perf-correctness knob. Log loudly so operators
		// notice; a surprise small MTU under a never-applied
		// large MTU is exactly the kind of latent
		// black-hole bug worth surfacing.
		log.
			WithError(err).
			WithFields(m.logFields(v6)).
			WithField("mtu", info.MTU).
			Error("Failed to apply DHCP-supplied MTU; container link MTU unchanged")
		return
	}

	log.
		WithFields(m.logFields(v6)).
		WithField("old_mtu", current).
		WithField("new_mtu", info.MTU).
		Info("Applied DHCP-supplied MTU")
}

// reconcileDefaultRoute points the container's default route at the
// gateway the server just supplied. Skipped when the operator pinned a
// gateway override on the network — leave their override in place — and
// on the v6 path, where the router advertises itself.
func (m *dhcpManager) reconcileDefaultRoute(v6 bool, info dhcp.Info) error {
	if v6 || info.Gateway == "" || m.opts.Gateway != "" {
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

	routes, err := m.netHandle.RouteListFiltered(unix.AF_INET, &netlink.Route{
		LinkIndex: m.ctrLink.Attrs().Index,
		Dst:       nil,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST)
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
// exercised without a live dhcpcd. The whole meaning of
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

// handleEvent dispatches one dhcpcd lifecycle event from the
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
		// Ownership of the binding has transferred; Stop can now rely
		// on dhcpcd's own release. See boundV4 / boundV6.
		m.markBound(v6)
		if m.plugin != nil {
			bumpFamily(&m.plugin.leasesObtainedV4, &m.plugin.leasesObtainedV6, v6)
		}
		m.audit("bound", bareIP(event.Data.IP))
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
		m.audit("renew", bareIP(event.Data.IP))
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

func (m *dhcpManager) setupClient(v6 bool) (chan error, error) {
	v6Str := ""
	if v6 {
		v6Str = "v6"
	}

	log.
		WithFields(m.logFields(v6)).
		Info("Starting persistent DHCP client")

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
	var resumption dhcp.Resumption
	if !v6 {
		m.recordID, resumption = m.resumeFromRecord()
		requestedIP = resumption.Prefer
		if resumption.Lease == nil && requestedIP == "" {
			if v4Addr, _ := m.lastIPs(); v4Addr != nil && v4Addr.IP != nil {
				requestedIP = v4Addr.IP.String()
			}
		}
	} else if _, v6Addr := m.lastIPs(); v6Addr != nil && v6Addr.IP != nil {
		// IPv6 is refused at CreateNetwork in 2.0; this branch is
		// reachable only for a network created by an earlier build,
		// and pkg/dhcp refuses it loudly a few lines below.
		preferredV6 = v6Addr.IP.String()
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
		Hostname:     m.hostname,
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
		PreferredV6: preferredV6,
		// The record's unexpired lease, which makes the first packet an
		// INIT-REBOOT rather than a DISCOVER. nil is the ordinary
		// CreateEndpoint -> Join path having found nothing to resume.
		Resume:   resumption.Lease,
		Records:  m.recordStore(),
		RecordID: m.recordID,
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
	}
	if err := m.plugin.conflictWiring(&clientOpts, m.opts, roleJoin, m.joinReq.NetworkID, m.joinReq.EndpointID); err != nil {
		return nil, err
	}
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

	events, err := client.Start()
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

// linkLocalDADTimeout caps the wait for the container link's IPv6
// link-local address to clear duplicate address detection. DAD with
// kernel defaults is one solicit + 1s; the budget is generous because
// the only cost of waiting is delaying the first SOLICIT.
const linkLocalDADTimeout = 10 * time.Second

// awaitLinkLocal blocks until the container-side link has a usable
// (non-tentative, non-failed) IPv6 link-local address — the
// precondition for any DHCPv6 exchange in the netns.
func (m *dhcpManager) awaitLinkLocal(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, linkLocalDADTimeout)
	defer cancel()
	return util.AwaitCondition(ctx, func() (bool, error) {
		addrs, err := m.netHandle.AddrList(m.ctrLink, unix.AF_INET6)
		if err != nil {
			return false, fmt.Errorf("failed to list IPv6 addresses: %w", err)
		}
		for _, a := range addrs {
			if a.Scope == unix.RT_SCOPE_LINK &&
				a.Flags&unix.IFA_F_TENTATIVE == 0 &&
				a.Flags&unix.IFA_F_DADFAILED == 0 {
				return true, nil
			}
		}
		return false, nil
	}, pollTime)
}

// joinPhases records how long each stage of Start took, so a Join that
// runs out of budget can say WHERE the budget went.
//
// Start is one deadline covering five quite different waits: resolving
// the endpoint to a real container ID, inspecting that container,
// opening its netns, locating its link, and spawning dhcpcd. When it
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
	// SHOULD DO ABOUT IT: nothing. On a stock engine this fires once per
	// attach, for every container, forever -- the daemon's per-sandbox
	// netns mounts are made after the plugin's own /var/run/docker bind
	// was taken, so the key resolves to the placeholder file and the PID
	// route carries the attach exactly as it did before the key route
	// existed. A warning is a request for attention, and a request for
	// attention that is correct on every attach of a healthy host trains
	// its reader to ignore the level.
	//
	// The signal is not lost by lowering it. sandbox_key_entries,
	// sandbox_key_entry_failures, sandbox_pid_fallbacks and the four
	// arm counters are on /Plugin.Health and /metrics at every level,
	// and they are what says which route this host takes. This line is
	// the detail behind them, and detail is what Debug is for.
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
		if err != nil {
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
		}
		close(m.startedCh)
	}()
	var ctrID string
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

	// Config-only: m.hostname reaches the generated dhcpcd.conf and
	// nothing that makes an identity decision, so a refusal is just an
	// omitted directive here.
	m.hostname = m.plugin.safeHostname(ctr.Config.Hostname).name

	// The sandbox key is the primary route (sandbox_netns.go). Join
	// carries it; recovery does not, and reads it from the inspect it
	// has already made -- one source for both paths, and always the
	// daemon's current answer rather than a value this plugin wrote
	// down earlier and might be wrong about.
	sandboxKey := m.joinReq.SandboxKey
	if sandboxKey == "" && ctr.NetworkSettings != nil {
		sandboxKey = ctr.NetworkSettings.SandboxKey
	}
	m.nsHandle, err = m.openSandboxNetNS(ctx, sandboxKey, ctr.State.Pid, ctrID, pollTime)
	if err != nil {
		return fmt.Errorf("failed to get sandbox network namespace: %w", err)
	}

	phases.mark("open_netns")

	m.netHandle, err = netlink.NewHandleAt(m.nsHandle)
	if err != nil {
		closeNsHandle(m.nsHandle)
		return fmt.Errorf("failed to open netlink handle in sandbox namespace: %w", err)
	}

	if err := func() error {
		if err := m.locateContainerLink(ctx); err != nil {
			return err
		}

		phases.mark("locate_link")

		if m.errChan, err = m.setupClient(false); err != nil {
			close(m.stopChan)
			return err
		}

		if m.opts.IPv6 {
			// The engine disables IPv6 outright on a sandbox interface
			// whose endpoint carries no IPv6 address, which is now a
			// reachable state (#868). Clear that BEFORE the link-local
			// wait below, because on a disabled link the link-local
			// never appears at all and the wait would time out for a
			// reason DAD has nothing to do with. See v6_link.go.
			m.ensureIPv6Enabled()

			// DHCPv6 needs a usable link-local source address. The
			// link just landed in this netns, so its LL is typically
			// still DAD-tentative — and a host must NOT answer
			// neighbor solicitations for a tentative address, so the
			// server's unicast ADVERTISE/REPLY can never be
			// delivered: dhcpcd SOLICITs forever while the server's
			// neighbor cache records an unreachable client (#103,
			// found by TestLeaseRenewIPv6_HonorsT1). Wait for DAD to
			// finish before starting the client. Timeout degrades to
			// a warn-and-try — DAD normally completes in ~1s.
			if err := m.awaitLinkLocal(ctx); err != nil {
				log.WithError(err).WithFields(m.logFields(true)).
					Warn("No usable link-local address; starting DHCPv6 client anyway")
			}
			if m.errChanV6, err = m.setupClient(true); err != nil {
				close(m.stopChan)
				// The v4 consumer goroutine is already live and may be
				// mid-renew on m.netHandle; stopChan only signals it.
				// Drain its exit ack so the outer cleanup can't close
				// the netlink/netns handles out from under it (and so
				// the v4 dhcpcd is reaped, not orphaned).
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

	return nil
}

// Stop shuts the persistent clients down WITHOUT assuming the endpoint
// is going away.
//
// This is the shutdown every caller but Leave wants: plugin Close stops
// every live manager so their dhcpcds exit cleanly rather than being
// orphaned by process exit, and the containers behind them keep running.
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
	if m.startErr != nil {
		// No persistent client ever ran, so there is no dhcpcd to
		// signal, and the CreateEndpoint one-shot's lease is left where
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
	if m.opts.IPv6 {
		errV6 = <-m.errChanV6
	}

	// What the shutdown meant is decided by whether this client ever
	// held a binding, NOT by how its process ended. That ordering is
	// the whole of #607.
	//
	// The exit status is a property of a process we deliberately
	// signalled. dhcpcd answers SIGTERM by exiting 0 — but only once it
	// is far enough into startup to have installed the handler. Signal
	// it before that and it dies ON the signal, so Finish reaps
	// "signal: terminated" and errV4 is non-nil. Testing errV4 first
	// therefore routed the never-bound case into the stop-failure
	// branch below, counting a fault where none had occurred. That is
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
	if m.opts.IPv6 {
		neverBoundV6 = m.settleFamily(true, lastIPv6, errV6, leaving)
	}
	if neverBoundV4 || neverBoundV6 {
		// The one-shot's lease is still outstanding for at least one
		// family. It stays outstanding and expires on the server's
		// clock (#800) — the reclaim that used to run here was removed
		// because it raced the tombstone for the same address. Nothing is audited
		// either way: no RELEASE was sent, and writing "stopped" would
		// be the ledger claiming something the server never saw, which
		// is the one thing this ledger exists not to do.
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
		// which is the honest record: no RELEASE was sent, on any path.
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
