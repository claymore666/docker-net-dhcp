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

// dhcpClientReapTimeout caps how long the dhcpcd consumer waits to
// reap a self-exited child process before giving up and letting it
// linger as a zombie. The kernel's eventual reaping by init handles
// the worst case; this just bounds wall time on the cleanup path.
const dhcpClientReapTimeout = 5 * time.Second

// dhcpClientFinishTimeout caps how long Stop waits for SIGTERM ->
// DHCPRELEASE -> exit on the persistent dhcpcd child. Long enough
// for a DHCPRELEASE round-trip on a healthy LAN; short enough that
// plugin shutdown / Leave isn't held hostage by an unresponsive
// upstream DHCP server.
const dhcpClientFinishTimeout = 5 * time.Second

// dnsPropagateTimeout caps the docker-API round-trip cost of
// resolving the container PID for resolv.conf writes. Short because
// it runs on every DHCP bound/renew event; a slow daemon shouldn't
// stack up bound goroutines waiting on inspect calls. On timeout
// we log and skip — the next renewal will retry.
const dnsPropagateTimeout = 2 * time.Second

// dhcpOutageTick / dhcpOutageGrace drive the DHCP-outage watchdog.
//
// busybox udhcpc ran the handler with "leasefail" on every failed
// acquisition/renewal cycle, so dhcp_timeouts climbed steadily while a
// DHCP server was unreachable. dhcpcd gives us nothing equivalent, and
// it gives us less than this comment used to claim (#353): under
// `--noconfigure` it does not even fire EXPIRE when a bound lease
// lapses. So we synthesise the recurring signal ourselves — a ticker
// asks outageTracker, on each tick, whether this client is currently
// being served. The grace is the settling time before the first tick
// counts, so a single slow exchange doesn't register as an outage.
//
// The tick is also the resolution of the signal: dhcp_timeouts climbs
// about once per tick for as long as the outage lasts.
//
// Both are overridable per-plugin (OUTAGE_TICK / OUTAGE_GRACE, see
// Options) because these two numbers are the only part of outage
// detection that is ours: the rest of the wait is the DHCP lease, and
// the integration fixture's lease has a hard 2-minute floor imposed by
// dnsmasq (#356). Lowering them is what makes the failure suite
// affordable. The defaults are the production values and are what any
// deployment that doesn't set the variables gets.
const defaultOutageTick = 30 * time.Second
const defaultOutageGrace = 25 * time.Second

// minOutageTick floors the ticker period. time.NewTicker panics on a
// non-positive duration, so a misconfigured OUTAGE_TICK must never
// reach it; this also stops a near-zero value from spinning the
// watchdog goroutine.
const minOutageTick = 100 * time.Millisecond

// outageCadence returns the tick and grace this manager's watchdog
// should use. m.plugin is nil in unit tests that drive a manager
// directly, and a zero field means "not configured", so both fall back
// to the production defaults.
func (m *dhcpManager) outageCadence() (tick, grace time.Duration) {
	tick, grace = defaultOutageTick, defaultOutageGrace
	if m.plugin != nil {
		if m.plugin.outageTick > 0 {
			tick = m.plugin.outageTick
		}
		if m.plugin.outageGrace > 0 {
			grace = m.plugin.outageGrace
		}
	}
	if tick < minOutageTick {
		tick = minOutageTick
	}
	return tick, grace
}

// outageTracker decides when a persistent client counts as "no longer
// getting DHCP service". It is a plain value with no clock of its own —
// the caller supplies `now` — so the whole state machine is unit
// testable without waiting out real DHCP timers.
//
// It has two independent triggers, and the second one is why this type
// exists (#353):
//
//   - the client is ACQUIRING (never bound, or bound then told the lease
//     was lost) and has stayed that way past the grace;
//   - the client believes it is bound, but the deadline the server
//     itself handed us has passed with no bound/renew in between.
//
// The first trigger was the original design. It assumed dhcpcd would
// announce a lapsed lease via an EXPIRE hook, flipping the client into
// the acquiring state. It does not: under `--noconfigure` a lapsed
// lease is reported as RELEASE (see pkg/dhcp.mapReason), which is
// indistinguishable from a graceful stop and so cannot be counted. With
// only the first trigger the watchdog was inert in exactly the scenario
// it was written for — a bound endpoint whose server disappears — and
// dhcp_timeouts stayed at zero through a total outage.
//
// The second trigger needs nothing from dhcpcd but the lease it already
// reported at bind time. It cannot false-positive on a healthy client:
// a client that is being served gets a fresh lease (as a REBIND at T2,
// see leaseDeadline) well before the previous one runs out, and that
// restarts the deadline.
type outageTracker struct {
	acquiring      bool
	acquiringSince time.Time

	// lastAffirmed is when the server last proved it was answering
	// (bound/renew); lapseAfter is how long after that the lease runs
	// out. Zero lapseAfter = the server told us no lifetime, so no
	// deadline is enforced and only the acquiring trigger applies.
	lastAffirmed time.Time
	lapseAfter   time.Duration
}

// newOutageTracker starts in the acquiring state: a freshly started
// persistent client has not confirmed its own lease yet.
func newOutageTracker(now time.Time) outageTracker {
	return outageTracker{acquiring: true, acquiringSince: now}
}

// leaseDeadline is how long after a confirmed lease the client must have
// been served again before we call it an outage: the full lease, which
// is the last instant the address is even valid.
//
// Not T1, and not any fraction of the lease. Under `--noconfigure` the
// interface carries no address, so dhcpcd's T1 unicast renewal cannot
// succeed and every renewal lands at T2 as a broadcast rebind — a
// T1-derived deadline would fire on healthy clients. The lease is the
// one instant that needs no assumption about which retry succeeded.
// Zero when the server supplied no lifetime, in which case no deadline
// is enforced at all.
//
// Bounded above by maxLeaseDeadline, and reports whether it had to be.
// The bound is on the DEADLINE only: data.LeaseSeconds still reaches the
// log and the ledger unchanged, because the anomaly is the thing worth
// seeing and rewriting it would hide it. See maxLeaseDeadline for why an
// unbounded lifetime disables this watchdog outright.
func leaseDeadline(data dhcp.Info) (time.Duration, bool) {
	if data.LeaseSeconds > 0 {
		return clampLeaseDeadline(time.Duration(data.LeaseSeconds) * time.Second)
	}
	return 0, false
}

// observe folds one client event into the tracker, reporting whether the
// lease lifetime it carried had to be clamped to stay usable as a
// deadline (see leaseDeadline).
func (o *outageTracker) observe(eventType string, data dhcp.Info, now time.Time) (clamped bool) {
	prev := o.acquiring
	o.acquiring = nextAcquiring(prev, eventType)
	if o.acquiring && !prev {
		// Just lost the lease: restart the grace so the first post-loss
		// timeout isn't counted until a full interval of continued failure.
		o.acquiringSince = now
	}
	// A bound/renew is the ONLY proof the server answered, so it is the
	// only thing that restarts the deadline. A NAK must not: it leaves
	// the acquiring state alone and is a refusal, not service.
	if eventType == "bound" || eventType == "renew" {
		o.lastAffirmed = now
		o.lapseAfter, clamped = leaseDeadline(data)
	}
	return clamped
}

// due reports whether this tick counts a DHCP timeout, and whether it is
// the tick that first noticed a silently-lapsed lease — worth saying
// differently in the log, because nothing failed audibly: the renewal
// simply never happened.
func (o *outageTracker) due(now time.Time, grace time.Duration) (count, silentLapse bool) {
	if o.acquiring {
		return now.Sub(o.acquiringSince) >= grace, false
	}
	if o.lapseAfter <= 0 || now.Sub(o.lastAffirmed) < o.lapseAfter+grace {
		return false, false
	}
	// Deadline blown with no bound/renew in between. dhcpcd may never say
	// so out loud, so say it here and drop into the recurring acquiring
	// state from now on.
	o.acquiring = true
	o.acquiringSince = now
	return true, true
}

// nextAcquiring returns the post-event acquisition state. A bound/renew
// means we hold a lease (not acquiring); a leasefail (dhcpcd EXPIRE /
// TIMEOUT) drops us back to acquiring. Other event types (nak) leave the
// state unchanged — a NAK is usually followed immediately by a fresh
// bound, and EXPIRE is the authoritative "lease lost" signal.
func nextAcquiring(prev bool, eventType string) bool {
	switch eventType {
	case "bound", "renew":
		return false
	case "leasefail":
		return true
	default:
		return prev
	}
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

	// orphanReleaseOnce guards the #370 reclaim. Both abandon paths —
	// Join's Start goroutine finding the container gone, and a Leave
	// that got there first — can reach the same manager, and they do
	// not serialise against each other.
	orphanReleaseOnce sync.Once

	// boundV4 records that the persistent v4 client actually took
	// ownership of the binding, i.e. that it reached a bound/renew.
	//
	// "Start succeeded" is NOT that proof, and the difference leaks a
	// lease. CreateEndpoint's one-shot runs `-1 -p` and deliberately
	// does not release, because handing the binding over is the
	// persistent client's job. Stop therefore had exactly two branches:
	// Start failed (reclaim the lease, #370) or Start succeeded (signal
	// dhcpcd and let its `release` directive do the work). A client that
	// starts and is SIGTERMed before it ever binds falls between them —
	// dhcpcd only releases a lease it holds, so it sends nothing, and
	// startErr is nil so the reclaim never runs. The address is left
	// held upstream with nobody responsible for it.
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
	// one-shot took held upstream until it expired. Not a race, the
	// only behaviour. releaseOrphanedLease now reads both flags to
	// decide which families it owes.
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
// Every exchange the manager makes — the persistent client's renewals,
// and the synthesised release of a lease an early-exiting container
// orphaned (#370) — has to present the id the CreateEndpoint one-shot
// used. Present a different one and the server sees a different client:
// the lease it already holds is neither renewed nor freed, silently.
// Since #371 the id is mode-dependent (MAC-derived, except ipvlan), so
// deriving it in one place from one input is what keeps those in step.
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
		bumpFamily(&m.plugin.leaseChanged, &m.plugin.leaseChangedV6, v6)
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
		if errors.Is(err, errPIDNotContainer) && m.plugin != nil {
			m.plugin.dnsPropagationPIDMismatches.Add(1)
		}
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

// handleEvent dispatches one dhcpcd lifecycle event from the
// persistent client: health counters, audit-ledger entries, and the
// kernel-facing renew work. Extracted from the consumer goroutine so
// the counter semantics are unit-testable — wire-level NAKs in
// particular can't be provoked deterministically (dnsmasq silently
// ignores refused renewals in several shapes instead of NAKing), so
// the naks_received contract is pinned here rather than in an
// integration test (#128).
// bumpFamily increments the aggregate counter (always) and its IPv6
// sibling (only for v6 events), so /Plugin.Health exposes a per-family
// breakdown while the aggregate stays a true v4+v6 total (#212).
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
// started (see releaseOrphanedLease on the Start-failed path).
func (m *dhcpManager) neverBound(v6 bool) bool {
	if v6 {
		return !m.boundV6.Load()
	}
	return !m.boundV4.Load()
}

func bumpFamily(total, v6Counter *atomic.Int32, v6 bool) {
	total.Add(1)
	if v6 {
		v6Counter.Add(1)
	}
}

func (m *dhcpManager) handleEvent(event dhcp.Event, v6 bool) {
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
			bumpFamily(&m.plugin.leasesObtained, &m.plugin.leasesObtainedV6, v6)
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
			bumpFamily(&m.plugin.leasesRenewed, &m.plugin.leasesRenewedV6, v6)
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
	case "leasefail":
		if m.plugin != nil {
			bumpFamily(&m.plugin.dhcpTimeouts, &m.plugin.dhcpTimeoutsV6, v6)
		}
		log.WithFields(m.logFields(v6)).Warn("dhcp failed to get a lease")
	case "nak":
		if m.plugin != nil {
			bumpFamily(&m.plugin.naksReceived, &m.plugin.naksReceivedV6, v6)
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

	// On plugin-restart recovery the persistent client should ask the
	// DHCP server for the IP the container is already using, instead
	// of doing a fresh DISCOVER that might return something different.
	// In the normal CreateEndpoint -> Join path lastIP / lastIPv6
	// already point at the IP we just acquired; passing it via the
	// dhcpcd `request` directive (DHCP option 50) is a no-op (server
	// still ACKs the same address). On recovery it's
	// what makes the lease "sticky".
	requestedIP := ""
	preferredV6 := ""
	if !v6 {
		if v4Addr, _ := m.lastIPs(); v4Addr != nil && v4Addr.IP != nil {
			requestedIP = v4Addr.IP.String()
		}
	} else {
		// Same stickiness for v6: on recovery ask for the IA_NA address
		// the container already holds (lastIPv6 is seeded from the
		// recovered state) rather than risk a fresh one. In the normal
		// create->Join path it's a no-op — dhcpcd's pinned IA already
		// returns the same address (#213).
		if _, v6Addr := m.lastIPs(); v6Addr != nil && v6Addr.IP != nil {
			preferredV6 = v6Addr.IP.String()
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
	var allowServers, denyServers []string
	if !v6 {
		allowServers, denyServers = pol.allowList(), pol.denyList()
	}

	client, err := dhcp.NewDHCPClient(m.ctrLink.Attrs().Name, &dhcp.DHCPClientOptions{
		Hostname:     m.hostname,
		AllowServers: allowServers,
		DenyServers:  denyServers,
		FQDN:         m.opts.fqdnMode(),
		V6:           v6,
		NetNS:        &m.nsHandle,
		// Same MAC the CreateEndpoint one-shot used (this is the same
		// link, moved into the netns), so dhcpcd derives the identical
		// DUID-LL/IAID and the persistent client renews the very lease
		// Docker was told about (#152).
		MAC:         m.ctrLink.Attrs().HardwareAddr,
		RequestedIP: requestedIP,
		PreferredV6: preferredV6,
		// ipvlan slaves share the parent's MAC; without a broadcast
		// reply the server may unicast renewals to the parent and the
		// kernel has no way to demux to the right slave. Requesting the
		// broadcast flag in ipvlan mode keeps lease lifecycle stable.
		// NOTE: dhcpcd broadcast handling is not yet wired in the client
		// (DHCPClientOptions.Broadcast, #243) — this flag is set for the
		// ipvlan path but currently has no effect.
		Broadcast: m.opts.effectiveMode() == ModeIPvlan,
		// Same client-id the initial DISCOVER used in CreateEndpoint, so
		// renewals are seen as the same client by the server. Derived
		// from the MAC the one-shot ran under rather than from the link
		// in hand, so this and the orphan-release path cannot drift
		// apart (#371). Honours the operator's client_id override.
		ClientID:    m.clientID(),
		VendorClass: m.opts.VendorClass,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP%v client: %w", v6Str, err)
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
		// DHCP-outage watchdog: dhcpcd emits no per-attempt failure hook,
		// so synthesise the recurring dhcp_timeouts signal busybox gave us
		// (see dhcpOutageTick). "acquiring" starts true — the persistent
		// client has not confirmed its own lease yet — and flips with each
		// bound/renew/leasefail event.
		tracker := newOutageTracker(time.Now())
		outageTick, outageGrace := m.outageCadence()
		ticker := time.NewTicker(outageTick)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if count, silentLapse := tracker.due(time.Now(), outageGrace); count {
					if m.plugin != nil {
						bumpFamily(&m.plugin.dhcpTimeouts, &m.plugin.dhcpTimeoutsV6, v6)
					}
					msg := "DHCP server still unreachable; lease not (re)acquired"
					if silentLapse {
						// The distinction matters when reading a log after
						// the fact: this one means dhcpcd never reported a
						// failure at all — the lease's own deadline is what
						// exposed the outage (#353).
						msg = "DHCP lease passed its renewal deadline with no server response; treating the server as unreachable"
					}
					log.
						WithFields(m.logFields(v6)).
						Warn(msg)
				}

			case event, ok := <-events:
				if !ok {
					// dhcpcd exited on its own (NAK, parent NIC vanished,
					// container netns torn down out from under us, etc.).
					// The scanner goroutine in dhcp.Start closes events
					// when its read pipe hits EOF. Without this branch,
					// `<-events` on a closed channel returns the zero
					// Event{} every iteration, the switch matches nothing,
					// and we burn a CPU thread forever.
					log.
						WithFields(m.logFields(v6)).
						Warn("dhcp event stream closed; client process exited")

					// Reap the child so it doesn't linger as a zombie:
					// cmd.Wait must be called exactly once per process,
					// and Stop's Finish path won't run if the consumer
					// returned first.
					reapCtx, reapCancel := context.WithTimeout(context.Background(), dhcpClientReapTimeout)
					if err := client.Wait(reapCtx); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							Debug("dhcp reap returned error")
					}
					reapCancel()

					// Unblock Stop() if it's waiting on errChan. The
					// channel is buffered=1 so this never blocks; if
					// nobody's reading yet, the value sits until Stop
					// calls close(stopChan) and reads it.
					errChan <- nil
					return
				}
				if tracker.observe(event.Type, event.Data, time.Now()) && m.plugin != nil {
					m.plugin.leaseTimeClamped.Add(1)
					log.
						WithFields(m.logFields(v6)).
						WithField("lease_seconds", event.Data.LeaseSeconds).
						WithField("deadline", maxLeaseDeadline).
						Warn("DHCP lease lifetime too long to use as an outage deadline; clamped for the watchdog only")
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
	m.hostname, _ = m.plugin.safeHostname(ctr.Config.Hostname)

	// Using the "sandbox key" directly causes issues on some platforms,
	// so the namespace is reached through the container's PID -- but
	// never through a /proc path rebuilt as a string, and never as a
	// path handed onward to be resolved a second time. See
	// openContainerNetNS.
	m.nsHandle, err = awaitContainerNetNS(ctx, ctr.State.Pid, ctrID, pollTime)
	if err != nil {
		if errors.Is(err, errPIDNotContainer) && m.plugin != nil {
			m.plugin.netnsPIDMismatches.Add(1)
		}
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
// every live manager so their dhcpcds get to send a DHCPRELEASE before
// the process exits, and the containers behind them keep running. Same
// for a manager displaced by a newer one for the same endpoint, and for
// managers cleaned up when a network is removed.
func (m *dhcpManager) Stop() error {
	return m.stop(false)
}

// StopForLeave is Stop for an endpoint that is being torn down.
//
// The difference is the orphan reclaim. A lease whose persistent client
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
		// signal — but that does NOT mean there is nothing to clean up.
		// The CreateEndpoint one-shot acquired an address and kept it
		// (`-1 -p`) precisely so this manager could take it over; with
		// Start failed, nobody ever did, and the server holds it until
		// it expires. Hand it back (#370).
		//
		// This used to read "If Start failed there's nothing to clean
		// up" and return. That was true of the manager's own state and
		// false of the lease, which is how 17 of 32 containers in one
		// integration run leaked an address.
		m.plugin.spawnOrphanRelease(m)
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
	// signalled. dhcpcd answers SIGTERM by releasing and exiting 0 —
	// but only once it is far enough into startup to have installed the
	// handler. Signal it before that and it dies ON the signal, so
	// Finish reaps "signal: terminated" and errV4 is non-nil. Testing
	// errV4 first therefore routed the never-bound case into the
	// release-failure branch below, skipping the reclaim and leaving
	// the lease held upstream with nobody responsible for it. That is
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
	// before it bound exits cleanly, so the ledger recorded "release"
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
	if leaving && (neverBoundV4 || neverBoundV6) {
		// The one-shot's lease is still outstanding for at least one
		// family and the endpoint is going away, so hand it to the same
		// reclaim that covers a Start that never happened at all;
		// releaseOrphanedLease reads boundV4/boundV6 to decide which
		// families it owes and writes the ledger entry for whichever
		// way that goes. Nothing is audited here: no RELEASE was sent,
		// and writing "release" would be the ledger claiming something
		// the server never saw, which is the one thing this ledger
		// exists not to do.
		m.plugin.spawnOrphanRelease(m)
	}

	// A client that never bound reports no error, whatever its exit
	// status: we sent the SIGTERM, it died because we asked it to, and
	// the lease it never took has been dealt with above. Returning the
	// exit status here is what turned a correctly handled teardown into
	// a 500 from Leave, for a shutdown in which nothing actually
	// failed. The startErr path earlier in this function already
	// reclaims and returns nil on the same reasoning (#607).
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
// never held its binding, i.e. whether its one-shot lease is still
// outstanding and owed to the reclaim. Called from stop for v4 always
// and for v6 when the network is dual-stack, after both consumer
// goroutines have been drained; the reclaim decision itself is taken by
// the caller, once, across both families.
func (m *dhcpManager) settleFamily(v6 bool, last *netlink.Addr, exitErr error, leaving bool) bool {
	neverBound := m.neverBound(v6)
	neverBoundLog := log.WithFields(m.logFields(v6)).WithField("ip", auditIP(last))
	if neverBound && exitErr != nil {
		// Expected rather than a fault, but recorded so a signalled
		// exit is not simply invisible. Debug, because there is nothing
		// here for an operator to act on: the lease is handled by the
		// reclaim the caller spawns.
		neverBoundLog = neverBoundLog.WithField("client_exit", exitErr)
	}
	switch {
	case neverBound && leaving:
		// The reclaim is the caller's to spawn, once for both families;
		// what is settled here is that nothing is audited as released,
		// which is the honest record: no RELEASE was sent.
		neverBoundLog.Info("Persistent client stopped before it ever held the lease; reclaiming it")
	case neverBound:
		// Not leaving, so the address may still be in use by a running
		// container — see StopForLeave. Nothing is released and nothing
		// is audited as released.
		neverBoundLog.Debug("Persistent client stopped before it held the lease; not reclaiming, the endpoint is not leaving")
	case exitErr != nil:
		// Held a binding and did not shut down cleanly, so
		// SIGTERM -> RELEASE -> exit did not complete and the upstream
		// server may now be holding a phantom lease against this
		// identity until its own expiry. Bump so operators can alert on
		// a pattern of releases failing — typically points at upstream
		// reachability problems mid-teardown; split by family like its
		// neighbours so a dual-stack host can tell which client failed
		// (#608). The ledger records release_failed rather than release
		// so the audit trail never claims a release the server may not
		// have seen. Both families are audited independently: a failed
		// v4 release does not hide the v6 outcome from the ledger.
		if m.plugin != nil {
			bumpFamily(&m.plugin.leaseReleaseFailures, &m.plugin.leaseReleaseFailuresV6, v6)
		}
		m.audit("release_failed", auditIP(last))
	default:
		m.audit("release", auditIP(last))
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
