// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dNetwork "github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/gorilla/handlers"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// DriverName is the name of the Docker Network Driver
const DriverName string = "net-dhcp"

// newInstanceID returns a value unique to this plugin process. It lets
// a caller holding two health reads tell "the counters did not move"
// apart from "the counters were reset under you" (#405).
//
// It must never return an empty string. A consumer comparing two empty
// ids sees them as equal, concludes no restart happened, and trusts a
// delta that spans a reset — precisely the failure the id exists to
// prevent. crypto/rand failing is not a real expectation, so the
// fallback is a formality; it still varies per process, which is the
// only property that matters here.
func newInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// shortID truncates a Docker network/endpoint ID to 12 chars for
// log fields, without panicking on short or empty IDs (which can
// happen on malformed daemon responses during recovery).
func shortID(id string) string {
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}

// Network attachment modes selected by the `mode` driver option.
const (
	ModeBridge  = "bridge"
	ModeMacvlan = "macvlan"
	ModeIPvlan  = "ipvlan"
)

// initialDHCPHostnameLookupTimeout caps how long CreateEndpoint waits
// for Docker to associate the container with the network so we can
// look up its hostname for the initial DISCOVER. Short on purpose: if
// the lookup misses, the persistent client will fill in the hostname
// on first renewal, so the worst case is "first lease appears in the
// upstream DHCP server's table without a hostname for a few minutes".
// defaultAwaitTimeout is the fallback for Options.AwaitTimeout and is
// the single source of truth for the value config.json ships as
// AWAIT_TIMEOUT's default.
const defaultAwaitTimeout = 10 * time.Second

// attachDaemonBusyGrace is added to AwaitTimeout for the Join attach,
// and only there.
//
// The attach has to ask the daemon about the container it is attaching
// to, and the daemon is inside ContainerStart for that same container
// while it does — so it does not answer until it is finished (#406).
// AwaitTimeout is a statement about how long the plugin's own work may
// take; this is the separate question of how long our caller may keep
// us waiting, and folding the second into the first meant a busy
// daemon read as a plugin failure and a running container was left
// without a renewal client.
//
// 60s because the wait ends when ContainerStart does, and the useful
// bound is "longer than a container can plausibly take to start", not
// a number tuned to a measurement — a wait that ends early is exactly
// the bug being fixed. Nothing waits on it: Stop cancels the attach, so
// a container that leaves during the grace does not pay for it, and the
// only cost of the ceiling being generous is a goroutine that outlives
// its usefulness on a daemon that never recovers.
// A var, not a const, so a test can shrink it: proving the grace changes
// the outcome otherwise costs 70s of real waiting per run, and a unit
// test nobody wants to run is a unit test that stops being run. Same
// seam as recoveryDaemonRetryInterval.
var attachDaemonBusyGrace = 60 * time.Second

const initialDHCPHostnameLookupTimeout = 2 * time.Second

// recoveryBudget caps the wall-time the plugin spends rebuilding its
// in-memory state for already-attached endpoints on startup. Each
// endpoint's recovery does its own DHCP DISCOVER through dhcpcd with
// network-IO timeouts; this is the umbrella above all of them. Beyond
// it, recovery is abandoned and the affected endpoints surface as
// recovery_failed on /Plugin.Health.
const recoveryBudget = 30 * time.Second

// recoveryPerNetworkTimeout caps each individual NetworkInspect /
// netOptions Docker round-trip during recovery. Without it (W-7 in
// the 2026-05-05 review) one stuck Docker call could consume the
// entire recoveryBudget and starve later networks of their chance
// to recover. Tight on purpose — these are local-socket calls that
// either return promptly or are wedged.
const recoveryPerNetworkTimeout = 3 * time.Second

// recoverySyncDaemonWait caps how long recovery will wait for the daemon
// to answer *before* the plugin socket is listening (#383). Docker
// respawns us during its own startup and calls into us while it comes
// up, so this window is added directly to plugin-enable latency and to
// any deadlock risk — keep it short. When it expires, recovery is
// deferred to the post-Listen retry rather than abandoned.
const recoverySyncDaemonWait = 3 * time.Second

// recoveryDeferredDaemonWait caps the post-Listen retry. Generous
// because it costs nothing: the socket is already serving, so a plugin
// waiting here is fully responsive. When *this* expires the daemon is
// genuinely unreachable, which is a real recovery_failed.
const recoveryDeferredDaemonWait = 60 * time.Second

// recoveryDaemonRetryInterval spaces the retries. The Docker client's
// own 2s timeout dominates each failed attempt, so this only controls
// the gap between them.
//
// A var, not a const, solely so tests can shrink it — the same reason
// as pluginShutdownTimeout below. Exercising the retry loop at the real
// interval would cost seconds per case for no added confidence. Never
// reassigned outside tests.
var recoveryDaemonRetryInterval = 500 * time.Millisecond

// clientIDFromEndpoint derives a stable DHCP option-61 client identifier
// from a Docker endpoint ID. Docker's endpoint IDs are 64 hex chars
// (32 bytes). We take the first 8 bytes — long enough to be unique
// in any realistic deployment, short enough to keep the option payload
// well below the 255-byte wire limit. The same endpoint ID is used
// across container restarts on the same network, so this client-id
// also stays stable, which is what makes Fritz.Box-style hostname
// reservations actually work for our containers.
//
// Returns nil if the endpoint ID isn't valid hex (which would only
// happen on a fundamentally broken libnetwork request).
func clientIDFromEndpoint(endpointID string) []byte {
	if len(endpointID) < 16 {
		return nil
	}
	b, err := hex.DecodeString(endpointID[:16])
	if err != nil {
		return nil
	}
	return b
}

// clientIDFromMAC derives the option-61 payload from the endpoint's
// MAC. Returns nil for an empty/unset MAC so callers fall back.
//
// The payload is the raw address bytes; formatClientID prepends the
// same type-byte 0x00 ("opaque") wrapper it always has. RFC 2132's
// type-0x01 ("ethernet") form would be more literal, but several
// servers treat a type-1 client-id as an alias for the chaddr, which
// would silently change matching semantics for operators. Opaque keeps
// the id an id.
func clientIDFromMAC(mac net.HardwareAddr) []byte {
	if len(mac) == 0 {
		return nil
	}
	return append([]byte(nil), mac...)
}

// resolveClientID picks the option-61 payload for a fresh DHCP
// exchange. Operator-supplied opts.ClientID wins when non-empty
// (treated as opaque ASCII bytes; the dhcpcd client adds the
// type-byte 0x00 wrapper on the wire).
//
// Otherwise the id comes from the MAC. This is what makes an IPv4
// address survive `docker restart`: the tombstone preserves the MAC, so
// the returning container presents the same identity and the server
// renews the same lease. It is the identity IPv6 has always used (its
// DUID/IAID is MAC-derived), which is why v6 survived restarts that v4
// did not (#371).
//
// It also removes the dependency on the shutdown DHCPRELEASE. That
// release is what previously freed the address for a container coming
// back under a new endpoint-derived id — and it is not always sent
// (#370: a ~2s window at startup where the persistent client is not yet
// bound), nor can it ever be sent on SIGKILL, OOM, or power loss.
//
// ipvlan is the exception. Its L2 slaves inherit the parent's MAC by
// kernel design, so a MAC-derived id would be *identical* for every
// container on the network and they would all claim one lease. Those
// keep the endpoint-derived id, and with it today's restart fragility —
// #219 owns that case.
//
// The endpoint-derived fallback also covers a missing MAC, so a caller
// that cannot supply one degrades to the previous behaviour rather than
// to no client-id at all.
func resolveClientID(opts DHCPNetworkOptions, endpointID string, mac net.HardwareAddr) []byte {
	if opts.ClientID != "" {
		return []byte(opts.ClientID)
	}
	if opts.effectiveMode() != ModeIPvlan {
		if id := clientIDFromMAC(mac); id != nil {
			return id
		}
	}
	return clientIDFromEndpoint(endpointID)
}

const defaultLeaseTimeout = 10 * time.Second

// driverRegexp matches plugin references that this driver should treat
// as "another instance of itself" when scanning for bridge conflicts.
// Pinned to known maintained namespaces (devplayer0 = upstream,
// claymore666 = this fork) — broader matching would treat an attacker-
// controlled image like `evil.example/docker-net-dhcp:bad` as ours and
// surface spurious "Bridge already in use" errors. New forks that need
// cross-detection should add their namespace here.
var driverRegexp = regexp.MustCompile(`(^|/)(devplayer0|claymore666)/docker-net-dhcp:.+$`)

// IsDHCPPlugin checks if a Docker network driver is an instance of this plugin
func IsDHCPPlugin(driver string) bool {
	return driverRegexp.MatchString(driver)
}

// DHCPNetworkOptions contains options for the DHCP network driver
type DHCPNetworkOptions struct {
	// Mode selects the attachment strategy: "bridge" (default, requires
	// `bridge`) or "macvlan" (requires `parent`).
	Mode   string `mapstructure:"mode"`
	Bridge string
	Parent string `mapstructure:"parent"`
	// Gateway, if set, overrides the default gateway returned by the
	// upstream DHCP server. Useful for split-horizon LANs where
	// containers should egress via a different router than the one
	// the DHCP server advertises (e.g. VPN gateway).
	Gateway         string
	IPv6            bool
	LeaseTimeout    time.Duration `mapstructure:"lease_timeout"`
	IgnoreConflicts bool          `mapstructure:"ignore_conflicts"`
	SkipRoutes      bool          `mapstructure:"skip_routes"`
	// PropagateDNS, when true, makes the plugin write DHCP option 6
	// (v4 DNS server list) or option 23 (v6) into the container's
	// /etc/resolv.conf on every bind/renew with a non-empty list.
	// Default false to preserve historical behaviour where Docker's
	// embedded resolver handled DNS — flipping this on means LAN-DNS
	// names suddenly resolve from inside containers.
	PropagateDNS bool `mapstructure:"propagate_dns"`
	// PropagateMTU, when true, makes the plugin set the container link's
	// MTU to DHCP option 26 on every bind/renew with a non-zero value.
	// Default false because some networks advertise non-standard MTUs
	// for reasons unrelated to host capability (e.g. hand-rolled tunnel
	// fragments) and silently re-MTU'ing a container could surprise an
	// operator. Opt-in keeps the behaviour change visible.
	PropagateMTU bool `mapstructure:"propagate_mtu"`
	// ClientID, when non-empty, overrides the derived DHCP option 61
	// (Client Identifier) for every endpoint on this network. Bytes go
	// on the wire prefixed with type byte 0x00 (RFC 2132 opaque).
	//
	// Default empty = derive per endpoint: from the MAC in bridge and
	// macvlan (unique, and preserved across a restart, so the lease
	// survives), from the Docker endpoint ID in ipvlan (whose slaves
	// share the parent MAC). See resolveClientID.
	//
	// Operator caveat: a static ClientID across containers means the
	// upstream DHCP server can't differentiate them — each new
	// container will appear to be the same logical client and may
	// receive the same lease. Typically only useful when paired with
	// VendorClass to drive class-based policy that doesn't depend on
	// per-client identity.
	ClientID string `mapstructure:"client_id"`
	// VendorClass, when non-empty, overrides the default DHCP option
	// 60 (Vendor Class Identifier) value of "docker-net-dhcp" for
	// every endpoint on this network. Lets DHCP servers using
	// class-based policy (Cisco / Aruba / etc.) differentiate
	// net-dhcp containers from other clients on the same LAN —
	// for example to issue a different gateway or option set to
	// containers tagged with a known vendor string.
	VendorClass string `mapstructure:"vendor_class"`
	// ValidateDHCP, when true, makes CreateNetwork run a one-shot
	// DHCP probe on the parent NIC before the network is created,
	// failing fast with a clear error if no DHCP server answers
	// within the budget (see preflightProbeBudget). Catches
	// misconfigurations (parent isolated from any DHCP server,
	// firewall blocking UDP/67-68, broken VLAN tag) at create time
	// rather than the first `docker run` attempt.
	//
	// macvlan / ipvlan modes only — bridge mode's "parent" is an
	// existing Linux bridge, where the probe semantics are different
	// and not yet implemented.
	//
	// The probe runs a full DHCPDISCOVER → REQUEST → ACK cycle
	// (dhcpcd has no DISCOVER-only mode), so the upstream
	// pool briefly sees one extra lease per `docker network create`
	// with this opt-in. The probe MAC is random (locally-administered
	// bit set) so it doesn't collide with anything stable upstream;
	// the lease times out naturally rather than dragging CreateNetwork
	// on a slow release path.
	ValidateDHCP bool `mapstructure:"validate_dhcp"`
	// RegisterDNS, when true, makes every endpoint on this network send
	// the DHCP FQDN option (81 v4 / 39 v6, dhcpcd `fqdn both`) built from
	// its resolved hostname, asking the DHCP server to register that name
	// in DNS (forward + reverse). Default false: dynamic-DNS registration
	// is a network-policy decision, never silent. Best-effort and advisory
	// — many consumer routers ignore option 81, so this requests
	// registration, it does not guarantee resolution. Reuses the same
	// hostname already sent as the option-12 hint (#261).
	RegisterDNS bool `mapstructure:"register_dns"`
	// AuditLog, when true, appends every lease-lifecycle event on
	// this network (bound / renew / release, plus release_failed when
	// the DHCPRELEASE didn't complete) to STATE_DIR/leases.jsonl —
	// an append-only JSONL audit trail answering "which IP did this
	// container hold last Tuesday?" without dnsmasq-log archaeology
	// (#109). Rotated at 16 MB or 30 days, whichever first; one
	// rotated generation is kept. Default false: the ledger costs a
	// disk write per lease event, and container-ID/IP correlation on
	// disk is privacy-relevant in some environments — operators opt
	// in deliberately. Append failures bump ledger_write_failures on
	// /Plugin.Health and never affect lease handling.
	AuditLog bool `mapstructure:"audit_log"`

	// DHCPServers is an ordered preference list of DHCPv4 server
	// addresses, e.g. "1.1.1.1,2.2.2.2": the first that answers within
	// its slice of the acquisition budget wins, and the list is
	// exhaustive — if none answers, acquisition fails rather than
	// falling back to whichever server happened to reply. Naming your
	// servers is what makes the list complete (#111).
	//
	// Empty (the default) accepts whichever OFFER arrives first, the
	// historical behaviour.
	DHCPServers string `mapstructure:"dhcp_servers"`
	// DenyServers is an unordered list of DHCPv4 server addresses this
	// network must never take a lease from, e.g. "3.3.3.3" — a rogue
	// appliance or a second router on the segment (#669).
	//
	// This is a permission, not a preference: it composes with
	// DHCPServers rather than competing with it. See serverPolicy for
	// why the two cannot both be handed to dhcpcd as directives.
	DenyServers string `mapstructure:"dhcp_deny_servers"`
}

// effectiveMode returns Mode with the empty default normalized to ModeBridge.
func (o DHCPNetworkOptions) effectiveMode() string {
	if o.Mode == "" {
		return ModeBridge
	}
	return o.Mode
}

// fqdnMode maps the register_dns opt-in to the dhcpcd `fqdn` directive
// mode passed to the client. "both" asks the server to update forward
// (A/AAAA) and reverse (PTR); "" omits the directive (the default). See
// DHCPNetworkOptions.RegisterDNS (#261).
func (o DHCPNetworkOptions) fqdnMode() string {
	if o.RegisterDNS {
		return "both"
	}
	return ""
}

func decodeOpts(input interface{}) (DHCPNetworkOptions, error) {
	var opts DHCPNetworkOptions
	optsDecoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           &opts,
		ErrorUnused:      true,
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
		),
	})
	if err != nil {
		return opts, fmt.Errorf("failed to create options decoder: %w", err)
	}

	if err := optsDecoder.Decode(input); err != nil {
		return opts, err
	}

	return opts, nil
}

type joinHint struct {
	IPv4    *netlink.Addr
	IPv6    *netlink.Addr
	Gateway string
	// Routes are DHCP option-121 classless static routes (RFC 3442)
	// captured from the initial v4 DHCP exchange in CreateEndpoint. Like
	// Gateway, they only arrive in CreateEndpoint, so they ride the hint
	// to be appended to the Join response's StaticRoutes.
	Routes []*StaticRoute
	// MacAddress is the MAC CreateEndpoint ran its one-shot DHCP
	// exchange under, and so the one this endpoint's DHCP identity is
	// keyed to (dhcpManager.clientID, #371). Set in every mode.
	//
	// In macvlan mode it additionally locates the link: Docker moves the
	// interface wholesale and renames it, so MAC is the only stable
	// handle left inside the container netns. Bridge mode finds its link
	// through the veth peer index instead and never consults this.
	MacAddress net.HardwareAddr
	// Ifname is the validated custom container-side interface name from
	// the ifnameOption endpoint option (#125). The option only arrives
	// in CreateEndpoint — libnetwork's remote proxy passes sandbox
	// labels, not endpoint options, to Join — so it rides the hint to
	// become the Join response's DstName.
	Ifname string
}

// Options carries the plugin's runtime knobs. Every field is sourced
// from an environment variable declared in config.json and parsed in
// cmd/net-dhcp; a zero field means "unset", and NewPlugin substitutes
// the documented default. Grouping them beats growing NewPlugin's
// parameter list one knob at a time.
type Options struct {
	// AwaitTimeout caps the polling helpers (sandbox readiness, link
	// rename, netns appearance). AWAIT_TIMEOUT, default 10s.
	AwaitTimeout time.Duration

	// OutageTick is how often the DHCP-outage watchdog re-checks, and
	// so the resolution of dhcp_timeouts. OUTAGE_TICK, default 30s.
	OutageTick time.Duration

	// OutageGrace is the settling time before the watchdog will call an
	// outage. It must stay comfortably above how long a healthy client
	// takes to acquire its first lease — below that, ordinary start-up
	// registers as an outage. OUTAGE_GRACE, default 25s.
	OutageGrace time.Duration

	// RequestCaptureDir, when non-empty, tees every libnetwork request
	// body into that directory so an integration run can be turned into
	// the replay fixtures under pkg/plugin/testdata/requests (#644).
	// REQUEST_CAPTURE_DIR, default empty (disabled).
	//
	// Test instrumentation: it is declared in config-cover.json only,
	// alongside GOCOVERDIR, and empty here costs the shipped plugin
	// nothing — captureHandler returns the mux unwrapped.
	RequestCaptureDir string
}

// Plugin is the DHCP network plugin
type Plugin struct {
	awaitTimeout time.Duration
	outageTick   time.Duration
	outageGrace  time.Duration
	startTime    time.Time
	// instanceID identifies this plugin *process*. Every counter on
	// HealthResponse lives in memory and returns to zero when the
	// process does, so a before/after pair of health reads is only
	// comparable when this value is unchanged between them (#405).
	// Written once at construction, never mutated.
	instanceID string

	docker dockerClient
	server http.Server

	// metricsServer is the OPTIONAL TCP listener for /metrics, nil
	// unless METRICS_ADDR was set. It is a second server rather than a
	// second listener on p.server for one reason, and it is a security
	// boundary rather than a style choice: p.server routes every
	// libnetwork RPC — CreateNetwork, Join, DeleteEndpoint — and this
	// plugin runs with CAP_NET_ADMIN, CAP_SYS_ADMIN and CAP_SYS_PTRACE
	// in the host network namespace. Serving p.server on a TCP port
	// would expose all of that to anyone who can reach the port.
	// ListenMetrics builds a mux carrying /metrics and nothing else.
	metricsServer *http.Server
	// metricsListener is kept so a test can learn the address the
	// kernel actually assigned when METRICS_ADDR named port 0.
	metricsListener net.Listener

	// mu guards joinHints, persistentDHCP, and endpointFingerprints.
	// libnetwork dispatches CreateEndpoint / Join / Leave from
	// concurrent HTTP handlers, each of which touches one or more
	// of these maps; without the mutex the race detector reproduces
	// a concurrent map read+write.
	mu             sync.Mutex
	joinHints      map[string]joinHint
	persistentDHCP map[string]*dhcpManager
	// endpointFingerprints records the MAC and last-known IPv4 of
	// each live endpoint so DeleteEndpoint can stash both as a
	// tombstone for the next CreateEndpoint on the same network to
	// inherit. By DeleteEndpoint time the dhcpManager (which also
	// holds these) has already been taken by Leave, so we keep our
	// own copy.
	endpointFingerprints map[string]endpointFingerprint

	// tombstones owns the tombstones.json read-modify-write path and
	// the lock that serialises it (tombstone_store.go). Held only
	// across that small operation; never combined with mu so the two
	// locks cannot deadlock against each other — a rule that
	// scripts/check-lock-discipline.sh enforces, because this comment
	// alone did not.
	tombstones tombstoneStore

	// recoveredOK and recoveryFailed are bumped by recoverOneEndpoint's
	// background Start goroutine and reported via /Plugin.Health, so
	// operators can see whether plugin-restart recovery succeeded for
	// every previously-attached container or whether some containers
	// are now running without renewal.
	recoveredOK    atomic.Int32
	recoveryFailed atomic.Int32

	// recoveryAlreadyManaged counts endpoints a recovery walk found
	// someone else already managing and therefore left alone. Not a
	// failure and not healthy-affecting: the endpoint has a renewal
	// client, just not one this walk built. It is here because the
	// event was previously invisible except as an inflated "recovered"
	// in one log line, and it is the only outward sign of recovery
	// racing a Join — the window that made a compare-and-set necessary
	// in the first place (#480).
	recoveryAlreadyManaged atomic.Int32

	// recoveryDeferred counts the times recovery could not start because
	// the daemon was not answering yet and had to be retried after the
	// socket came up (#383). Docker respawns us during its own startup,
	// so meeting a not-yet-ready daemon is the expected state at that
	// moment — not a fault. NOT healthy-affecting, same reasoning as
	// join_aborted_container_gone: only an exhausted retry budget is a
	// real failure, and that still lands on recovery_failed.
	recoveryDeferred atomic.Int32

	// recoveryPending is set by NewPlugin when the synchronous attempt
	// met a daemon that was not serving yet, and consumed by Listen.
	// Written before Listen and read there; never concurrent.
	recoveryPending bool

	// recoveryCancel stops the deferred-recovery goroutine at Close.
	// nil when recovery completed synchronously, which is the norm.
	recoveryCancel context.CancelFunc

	// recoveryAbortedContainerGone counts post-restart recoveries
	// abandoned because the container had already exited (or been
	// removed) by the time recovery reached it (#376). Deliberately
	// NOT healthy-affecting, for exactly the reason
	// joinAbortedContainerGone is not: there is no running container
	// left without a renewal client, so nothing is wrong.
	//
	// This is the recovery-side twin of joinAbortedContainerGone.
	// Before #376 both outcomes landed in recoveryFailed, so a routine
	// daemon restart with any since-exited container flipped healthy
	// to false and paged an operator over a normal exit. The
	// integration suite knew the counter conflated the two and
	// declined to assert on it at all.
	recoveryAbortedContainerGone atomic.Int32

	// recoveryNetworkGone counts networks skipped during post-restart
	// recovery because they no longer existed by the time we asked for
	// their detail (#648). recoverEndpoints lists networks and then
	// re-inspects each one for container detail; a `docker network rm`
	// landing between those two calls answers the second with a 404.
	//
	// Deliberately NOT healthy-affecting, for the same reason as
	// recoveryAbortedContainerGone: a network that is gone has no
	// running container left without a renewal client, so nothing is
	// wrong. It was counted as recoveryFailed until #648 — fatal, and
	// enough to flip healthy — which made an ordinary network removal
	// racing a daemon restart look like the plugin's most serious
	// fault. Found by an integration run that went red with every test
	// passing; only the health floor saw it.
	//
	// This is the third benign path carved out of recoveryFailed, after
	// recoveryDeferred (#383) and recoveryAbortedContainerGone (#376).
	// The counter is kept rather than dropping to a log line so the
	// rate stays visible: a host where this climbs steadily is churning
	// networks under a restarting daemon, which is worth knowing even
	// though no single occurrence is a fault.
	recoveryNetworkGone atomic.Int32

	// recoveryFingerprintsSkipped counts endpoints that recovery
	// adopted but could not describe: the ContainerInspect that would
	// have given the hostname did not answer, or answered with no
	// hostname at all (#721).
	//
	// It exists because the fix for #721 would otherwise have inherited
	// the exact invisibility of the bug it closes. Recovery not
	// recording a fingerprint means DeleteEndpoint lays no tombstone,
	// which means that endpoint loses its address on its next
	// `docker restart` — and the only outward sign was
	// tombstonesConsumed staying flat, which is indistinguishable from
	// a quiet host. An operator could not tell "recovery worked" from
	// "recovery silently skipped half my endpoints".
	//
	// Deliberately NOT healthy-affecting. Losing address stability for
	// one endpoint is a real regression for that container, but it is
	// not a running container without a renewal client — the line
	// recoveryFailed draws, and the one that decides what flips
	// healthy.
	//
	// A hostname REFUSED by safeHostname is deliberately not counted
	// here: it already moves unsafeHostnamesRejected, and keeping the
	// two disjoint is what lets an operator tell "the daemon would not
	// answer me" from "a container sent a hostname nobody should send".
	// Summing them is then a choice the reader makes, rather than one
	// this counter makes for them.
	recoveryFingerprintsSkipped atomic.Int32

	// joinStartFailures counts persistent-DHCP-client Start failures
	// at Join time (#317). Each bump is a running container that got
	// its initial lease but has NO renewal client: the lease silently
	// ages toward expiry and is never released on disconnect. The
	// canonical cause was the missing CAP_SYS_PTRACE (netns open on a
	// non-root container's /proc/<pid>/ns/net); the counter exists so
	// the next cause is visible on /Plugin.Health instead of only in
	// the plugin log. Healthy-affecting, same operator semantics as
	// recovery_failed: restart the affected container once the cause
	// is fixed.
	joinStartFailures atomic.Int32

	// joinAbortedContainerGone counts attaches abandoned because the
	// container exited before the persistent client could be started
	// (#373). Deliberately NOT healthy-affecting and deliberately not
	// silent: nothing is wrong — there is no running container missing
	// a renewal client — but a sudden rise still says something real
	// about the workload (containers dying seconds after start, a
	// crash-loop), so it stays visible on /Plugin.Health.
	//
	// This is the benign twin of joinStartFailures. The two are
	// distinguished by whether the Join's sandbox key still exists;
	// before #373 both landed in joinStartFailures and a normal fast
	// exit could flip healthy to false.
	joinAbortedContainerGone atomic.Int32

	// joinAbortedNoContainer counts attaches abandoned because no
	// container ever claimed the endpoint on the network (#566).
	//
	// The distinction from joinAbortedContainerGone is where the evidence
	// comes from, and it matters because the sandbox-key evidence is not
	// available in a shipped plugin at all: the netns directory is not
	// mounted into the plugin container, so the filesystem check can only
	// answer "no usable evidence" and every vanished container that does
	// not produce a Docker API 404 used to fall through to
	// joinStartFailures (#567). This counter is that fall-through,
	// recognised.
	//
	// Not healthy-affecting, for the same reason as its two siblings:
	// there is no running container missing a renewal client, because
	// there is no container. A sustained rise still says something real
	// about the workload, so it stays visible.
	joinAbortedNoContainer atomic.Int32

	// joinAttachSlow counts attaches that finished, but only after
	// outlasting AwaitTimeout — i.e. ones that would have been
	// abandoned before attachDaemonBusyGrace existed, and were counted
	// as join_start_failures (#406).
	//
	// Not healthy-affecting: nothing is wrong, the container has its
	// renewal client. It is here because it is the only way to see from
	// outside that the daemon is holding containers long enough to
	// matter, and because if this ever reads zero across a run while
	// join_start_failures moves, the grace is not the mechanism doing
	// the work and the fix needs re-examining.
	joinAttachSlow atomic.Int32

	// dhcpServerTierFallbacks counts initial acquisitions where a
	// preferred DHCP server did not answer inside its slice of the
	// budget and the next entry in dhcp_servers was tried (#111).
	//
	// Not healthy-affecting: falling back is the feature working, not
	// failing — the endpoint still gets an address. It is here because
	// it is the only signal from outside that a preferred server is
	// not answering. A steady rise means the primary is effectively
	// down while every container still comes up fine, which is exactly
	// the condition that otherwise goes unnoticed until the standby
	// fails too.
	dhcpServerTierFallbacks atomic.Int32

	// dhcpServerPolicyExhausted counts initial acquisitions abandoned
	// because no server in dhcp_servers answered (#111).
	//
	// Not healthy-affecting on its own: the acquisition failure it
	// accompanies already fails the operation visibly and is counted.
	// It is separate because the operator action differs — this one
	// says the address was refused by policy rather than that DHCP is
	// broken, and the two look identical in a timeout log.
	dhcpServerPolicyExhausted atomic.Int32

	// restartLinkUpWaited counts child links that came up only after
	// waiting out the departing link's hold on the address — the #408
	// window actually arising and the fix carrying the restart.
	// NOT healthy-affecting: a successful wait is the fix working.
	//
	// restartLinkUpTimeouts counts the same window outlasting the
	// budget, which is a real failure: the restart fails and the user
	// sees `address already in use`. Also not healthy-affecting, and
	// deliberately so — the error is already loud, surfacing through
	// CreateEndpoint to the operator's terminal, whereas `healthy`
	// exists for faults that are otherwise silent (#422).
	restartLinkUpWaited   atomic.Int32
	restartLinkUpTimeouts atomic.Int32

	// joinAbortedEndpointLeft counts attaches cancelled because Leave
	// arrived while they were still running. Not healthy-affecting and
	// not silent, on the same reasoning as
	// joinAbortedContainerGone: nothing is missing a renewal client,
	// but a sustained rise says containers are being torn down inside
	// the attach window (#406).
	joinAbortedEndpointLeft atomic.Int32

	// unsafeHostnamesRejected counts container hostnames dropped before
	// they could reach the generated dhcpcd config because they carried a
	// control character (#692).
	//
	// A counter rather than only a log line, because this is the one
	// finding from the #457 review with a deliberate actor behind it: a
	// legitimate hostname does not contain a newline, so a non-zero value
	// here is not noise, it is somebody trying. Dropping the directive is
	// the safe outcome — the hostname is cosmetic and the lease proceeds —
	// which is exactly why it would otherwise be invisible.
	unsafeHostnamesRejected atomic.Int32

	// dnsPropagationPIDMismatches counts DNS propagations refused
	// because the PID resolved through Docker no longer belonged to the
	// container it came from (#688).
	//
	// The plugin runs in the host PID namespace, so a recycled PID here
	// means the write would have landed in an unrelated host process.
	// Refusing is the safe outcome and leaves the container's
	// resolv.conf as it was, so without a counter the near-miss would
	// be invisible; a sustained rise says containers are exiting inside
	// the propagation window.
	dnsPropagationPIDMismatches atomic.Int32

	// netnsPIDMismatches counts sandbox netns opens refused because the
	// PID resolved through Docker no longer belonged to the container
	// it came from -- the same hazard as the counter above, on the path
	// with the larger blast radius.
	//
	// What the refusal prevents is not one file: the netlink handle
	// built from that namespace carries every address, MTU and route
	// the manager applies, with CAP_NET_ADMIN, and dhcpcd is spawned
	// into it as root. Refusing fails the attach, so unlike the DNS
	// case this one is at least visible as an error -- but the error
	// reads like a slow container start, and only the counter says the
	// PID belonged to something else.
	netnsPIDMismatches atomic.Int32

	// dhcpRoutesApplied counts DHCP option-121 classless static routes
	// handed to Docker, and dhcpDefaultRouteSuperseded counts the
	// Joins where those routes, taken together, cover 0.0.0.0/0.
	//
	// Applying them is correct client behaviour and is not the problem.
	// The problem was that a server could take every destination
	// without ever sending a default route -- `0.0.0.0/1 g
	// 128.0.0.0/1 g` -- and nothing in the plugin's output changed:
	// res.Gateway, `docker inspect` and the log all still named the
	// legitimate router, and the routes themselves were logged as a
	// count with no destinations and no next hops. Neither counter is
	// healthy-affecting; a superseded default is legitimate in
	// split-tunnel setups. They exist so "where did this container's
	// traffic go" has an answer after the fact (#700).
	dhcpRoutesApplied          atomic.Int32
	dhcpDefaultRouteSuperseded atomic.Int32

	// leaseTimeClamped counts leases whose option-51 lifetime was too
	// long to use as the outage watchdog's deadline and was cut to
	// maxLeaseDeadline.
	//
	// Not healthy-affecting -- the clamp is the safe outcome, and the
	// reported lease time is untouched. Read it anyway: a legitimate
	// server does not grant a container a lease measured in years, and
	// before the clamp one such ACK followed by silence left
	// dhcp_timeouts at zero through a total outage (#701).
	leaseTimeClamped atomic.Int32

	// mtuRefused counts DHCP option-26 MTUs outside the range
	// propagateMTU will apply, which leave the link's MTU alone.
	//
	// Not healthy-affecting: refusing is the safe outcome. It is
	// reported because the failure it prevents is silent -- a link
	// clamped to 68 bytes has its throughput destroyed and its path MTU
	// discovery black-holed, re-applied on every renewal, and the only
	// previous evidence was one Info line saying the MTU had been
	// applied (#702).
	mtuRefused atomic.Int32

	// unsafeOptionValuesDropped counts server-chosen DHCP string values
	// refused because they carried a control character -- option 66,
	// 67, 100, 101 and the plugin's own 252, plus the option-15 domain
	// truncated at its first space.
	//
	// Not healthy-affecting: dropping is the safe outcome and the lease
	// proceeds. Read it for the same reason as
	// unsafe_hostnames_rejected: no legitimate server sends one, so any
	// rise is deliberate. The count is produced in the dhcpcd hook
	// process and rides the event across the FIFO (#703, #704).
	unsafeOptionValuesDropped atomic.Int32

	// tombstoneWriteFailures counts saveTombstones failures (disk full,
	// EROFS) from addTombstone. Reported on /Plugin.Health so operators
	// can detect a degraded restart-stability window — every failure
	// here means one container that won't get its previous MAC/IP back
	// on restart until the disk recovers.
	tombstoneWriteFailures atomic.Int32

	// tombstonesConsumed counts the other side of that story: a
	// CreateEndpoint that found a fresh tombstone and reused its MAC/IP,
	// i.e. a container that got its address back across a recreate.
	//
	// Not healthy-affecting — it is the mechanism working, not failing.
	// It exists because it is the only way to tell, from outside, WHICH
	// path preserved an address after a restart: recovery re-adopting a
	// live endpoint (recovered_ok) or the tombstone being replayed. The
	// daemon-restart test could previously observe only the first, so
	// "neither happened and the address survived anyway" was
	// indistinguishable from success (#386).
	tombstonesConsumed atomic.Int32

	// leaseChanged counts renewals where dhcpcd returned a different
	// IP than the manager last recorded. Container's
	// NetworkSettings.IPAddress in `docker inspect` does NOT update
	// — libnetwork has no in-place endpoint-IP swap RPC. This counter
	// lets operators alert on the truthfulness gap until a deeper fix
	// (forced container restart on lease change, or an out-of-band
	// docker-socket update) lands. See issue #104 for the design
	// discussion deferred from v0.9.0.
	leaseChanged atomic.Int32

	// addressConflicts counts leases whose address was found to be
	// already held by another device on the segment (#524).
	// Healthy-affecting: the container is up, Docker reports an
	// address, and traffic is broken or intermittently wrong for two
	// hosts — an operator has to look, and nothing else will tell them.
	//
	// conflictProbeFailures counts probes that could not run at all
	// (unroutable parent, unparseable lease or MAC). NOT
	// Healthy-affecting: an unanswered question is not a known-broken
	// address. It is counted so the detector cannot quietly stop
	// working — a check that silently does not happen is exactly how
	// #524 stayed invisible through a production incident.
	addressConflicts      atomic.Int32
	conflictProbeFailures atomic.Int32

	// conflictProbeStaleRoutes counts leftover probe routes reclaimed
	// from a previous probe that was cut short before it could clean up
	// (#572). The probe goroutine is detached, so a plugin stop inside
	// its window leaves its /32 behind and every later probe for that
	// address fails with EEXIST until something removes it.
	//
	// Not healthy-affecting: the probe it appears in went on to run.
	// Counted because the recovery hides a real event — the plugin being
	// stopped mid-probe — and a detector that silently repairs itself is
	// how the last one stopped being trustworthy.
	conflictProbeStaleRoutes atomic.Int32
	// addressConflictProbes counts probes that ran to a verdict —
	// conflict or clean. Not Healthy-affecting, and the reason it
	// exists at all: without it, "the segment is clean" and "the
	// detector never ran" are the same reading (all counters zero),
	// which is precisely the ambiguity #524 hid behind. A run is only
	// evidence of a clean segment if this advanced.
	addressConflictProbes atomic.Int32

	// leasesObtained / leasesRenewed / dhcpTimeouts / leaseReleaseFailures
	// expose DHCP-wire-level counters via /Plugin.Health (T2-4). They
	// complement the lease_changed signal and let operators alert on
	// regressions in the DHCP exchange itself without scraping dnsmasq
	// logs server-side or running the plugin at trace level. Bumped
	// from dhcpManager:
	//   - leasesObtained: "bound" event — first successful
	//     DHCPACK on either initial bind or after a NAK / lease loss
	//   - leasesRenewed: "renew" event — a renewal DHCPACK
	//   - dhcpTimeouts: "leasefail" event — a bound lease lapsed
	//     (dhcpcd EXPIRE) or the outage watchdog fired without an
	//     OFFER or ACK
	//   - leaseReleaseFailures: client.Finish returned an error in
	//     Stop, meaning the SIGTERM-driven DHCPRELEASE didn't complete
	//     cleanly (timeout, exit code, or pipe closure)
	leasesObtained       atomic.Int32
	leasesRenewed        atomic.Int32
	dhcpTimeouts         atomic.Int32
	leaseReleaseFailures atomic.Int32

	// orphanedLeasesReleased / orphanedLeaseReleaseFailures cover the
	// lease the CreateEndpoint one-shot acquired when no persistent
	// client ever took ownership of it — a container that exited before
	// Join's async Start could attach (#370). See releaseOrphanedLease.
	//
	// Deliberately separate from leaseReleaseFailures: that counter
	// means "a client we were running failed to hand its lease back",
	// which points at upstream reachability. These mean "no client was
	// running at all", which points at container churn. Merging them
	// would make the pattern each one exists to reveal unreadable.
	//
	// Neither participates in Healthy. A failed synthesised release
	// leaves one lease held until it expires — worth alerting on as a
	// rate, not worth latching a plugin unhealthy over, in the same
	// spirit as #373/#376/#383: an ordinary container lifecycle must
	// not read as a plugin fault.
	orphanedLeasesReleased       atomic.Int32
	orphanedLeaseReleaseFailures atomic.Int32

	// parentGate serialises child-link creation per parent NIC, so an
	// asynchronous orphan-lease reclaim cannot hold a parent in one
	// attachment mode while an endpoint asks for the other. See
	// parent_gate.go for why this is per-parent and for the lock
	// ordering.
	//
	// parentLinkWaits / parentLinkWaitTimeouts are the observability
	// half. Read them together, like the orphan counters above: waits
	// climbing with timeouts flat is the gate absorbing contention,
	// which is it working. Timeouts climbing means something held a
	// parent longer than parentGateBudget — a wedged or unusually slow
	// reclaim — and the operations that gave up will have fallen back
	// to asking the kernel directly, so expect matching EBUSY failures
	// on the container-start path.
	//
	// Neither participates in Healthy. Contention on a shared parent is
	// a normal consequence of running macvlan and ipvlan networks on one
	// NIC, and even a timeout only restores the pre-gate behaviour.
	parentGate             parentGate
	parentLinkWaits        atomic.Int32
	parentLinkWaitTimeouts atomic.Int32

	// naksReceived counts "nak" events — the server refused a
	// REQUEST (pool reconfigured, address reassigned, lease revoked).
	// Until v1.0.0 a NAK was only a warn-level log line, invisible to
	// operators (#128). A NAK is followed by dhcpcd re-DISCOVERing, so
	// pair this with lease_changed: naks_received climbing while
	// lease_changed follows means containers are being re-addressed
	// mid-life — Docker's inspect view goes stale (see leaseChanged
	// above / #104) and DNS or firewall rules keyed on the old IP need
	// attention.
	naksReceived atomic.Int32

	// Per-family (IPv6) breakdown of the wire counters above (#212).
	// handleEvent/renew already receive a `v6 bool`; these atoms count
	// only the v6 client's events. The fields above stay aggregates
	// (v4+v6) so existing operator alerts keep their meaning — the v4
	// share is the aggregate minus the matching *V6 atom. On a dual-
	// stack host this is the only way to tell a v6-specific NAK or
	// timeout (the signal #152 is landing against) from a v4 one on
	// /Plugin.Health without scraping logs.
	leaseChangedV6   atomic.Int32
	leasesObtainedV6 atomic.Int32
	leasesRenewedV6  atomic.Int32
	dhcpTimeoutsV6   atomic.Int32
	naksReceivedV6   atomic.Int32
	// leaseReleaseFailuresV6 joined the split late (#608): until then a
	// dual-stack operator alerting on lease_release_failures could not
	// tell which family's client had failed to hand its lease back.
	leaseReleaseFailuresV6 atomic.Int32

	// displacedStops tracks the goroutines Join spawns to Stop a
	// manager it displaced (#338). Join must not block on the dhcpcd
	// release cycle, but Close must not exit while one is mid-release
	// either — an interrupted Stop means no DHCPRELEASE, and the
	// upstream server holds the lease until it expires on its own.
	// Tracked rather than bounded on purpose: a semaphore here would
	// put head-of-line blocking back into Join, which is the exact
	// thing the goroutine exists to avoid.
	//
	// displacedStopsTotal is the /Plugin.Health view of the same
	// event. A restart loop that repeatedly displaces managers is
	// otherwise visible only as scattered log lines.
	displacedStops      sync.WaitGroup
	displacedStopsTotal atomic.Int32

	// orphanReleases tracks the goroutines that hand back a lease no
	// persistent client ever owned (#370). Same reasoning as
	// displacedStops, and the same reason it must not be bounded: the
	// work exists to keep a release off the teardown path, so putting a
	// semaphore in front of it would reintroduce the blocking. Close
	// waits on it because an interrupted synthesised release leaks the
	// very lease it was spawned to reclaim.
	orphanReleases sync.WaitGroup

	// ledger is the append-only lease audit log (#109), written by
	// dhcpManager.audit for networks created with audit_log=true.
	// ledgerWriteFailures counts failed appends, surfaced on
	// /Plugin.Health. Unlike tombstone_write_failures it does NOT
	// flip Healthy: a lost audit line degrades forensics, not
	// networking or restart stability — operators who enable
	// audit_log should alert on the counter instead.
	ledger              *leaseLedger
	ledgerWriteFailures atomic.Int32
}

// storeJoinHint records the state collected during CreateEndpoint so
// Join can pick it up.
func (p *Plugin) storeJoinHint(endpointID string, h joinHint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.joinHints[endpointID] = h
}

// updateJoinHint applies fn to the (possibly-zero) hint for endpointID
// and stores the result. Allows the read-modify-write pattern used in
// CreateEndpoint without exposing the map directly. fn runs under the
// lock — keep it short; do not call back into Plugin from inside fn.
func (p *Plugin) updateJoinHint(endpointID string, fn func(*joinHint)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.joinHints[endpointID]
	fn(&h)
	p.joinHints[endpointID] = h
}

// takeJoinHint atomically retrieves and deletes the join hint for an
// endpoint. Returns ok=false if no hint was registered.
func (p *Plugin) takeJoinHint(endpointID string) (joinHint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.joinHints[endpointID]
	if ok {
		delete(p.joinHints, endpointID)
	}
	return h, ok
}

// registerDHCPManager stores a running per-endpoint DHCP client so Leave
// can find it. Caller registers the manager *before* spawning the
// goroutine that runs dhcpManager.Start; dhcpManager.Stop is safe to
// call against a manager whose Start is still in flight.
//
// Returns the manager this registration displaced, or nil. A displaced
// manager happens when Join lands on an endpoint the recovery path
// already registered (plugin restart while the container restarts:
// Docker sends Join with no preceding Leave to this plugin instance).
// Silently dropping it from the map would leak its running dhcpcd —
// unstoppable forever, and colliding with the new client on the same
// interface — so the caller must Stop it.
func (p *Plugin) registerDHCPManager(endpointID string, m *dhcpManager) *dhcpManager {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.persistentDHCP[endpointID]
	p.persistentDHCP[endpointID] = m
	return old
}

// dhcpManagerExists reports whether endpointID already has a registered
// manager. Advisory only — the answer can be stale the instant it is
// read, which is why the recovery path still registers through a
// compare-and-set rather than acting on this alone.
func (p *Plugin) dhcpManagerExists(endpointID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.persistentDHCP[endpointID]
	return exists
}

// registerDHCPManagerIfAbsent registers m only if no manager is already
// registered for endpointID, and reports whether it did. It is the
// recovery path's counterpart to registerDHCPManager: recovery adopts an
// endpoint precisely because nobody is managing it, so "register" and
// "only if unmanaged" have to be one operation.
//
// They used to be two. recoverOneEndpoint read the map, released the
// lock, built a manager, and registered it — and dropped the manager
// that registration displaced, which is exactly what registerDHCPManager
// says a caller must never do. A Join landing in that window had its
// live manager evicted from the registry while its dhcpcd kept running:
// untracked, unstoppable, and competing with recovery's fresh client on
// the same interface. Join guards the mirror-image case (network.go)
// because a Join is newer truth than a recovery and may displace it;
// recovery is older truth and must yield instead, which a
// compare-and-set expresses and a stop-what-I-displaced does not.
//
// The window is small — microseconds per endpoint — but the case that
// widens it is a real one: a plugin restart whose deferred recovery
// (#383) runs while a host full of restart-policy containers is
// rejoining, which is what an abrupt daemon death produces (#480).
func (p *Plugin) registerDHCPManagerIfAbsent(endpointID string, m *dhcpManager) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.persistentDHCP[endpointID]; exists {
		return false
	}
	p.persistentDHCP[endpointID] = m
	return true
}

// removeDHCPManagerIfSame deletes the registry entry for endpointID only
// if it still holds m. The failed-Start goroutines use this instead of
// takeDHCPManager: between Start failing (which unblocks a pending
// Leave) and the goroutine reaching its deregistration, a fast
// Leave+Join cycle can install a NEW healthy manager under the same
// key — deleting by key alone would evict that successor, leaking its
// running dhcpcd.
func (p *Plugin) removeDHCPManagerIfSame(endpointID string, m *dhcpManager) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.persistentDHCP[endpointID] == m {
		delete(p.persistentDHCP, endpointID)
	}
}

// takeDHCPManager atomically retrieves and deletes the DHCP manager for
// an endpoint, suitable for Leave's Stop-then-discard pattern.
func (p *Plugin) takeDHCPManager(endpointID string) (*dhcpManager, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.persistentDHCP[endpointID]
	if ok {
		delete(p.persistentDHCP, endpointID)
	}
	return m, ok
}

// takeDHCPManagersForNetwork atomically retrieves and removes every
// DHCP manager whose JoinRequest belongs to networkID. Used by
// DeleteNetwork to evict managers that libnetwork didn't issue a Leave
// for — typically the recovery-then-network-removed path: the plugin
// recovered an endpoint into its registry, the network was deleted
// while the container's netns was already gone, and no Leave RPC ever
// arrived. Without this prune, /Plugin.Health's active_endpoints
// count drifts upward across network upgrade cycles.
func (p *Plugin) takeDHCPManagersForNetwork(networkID string) []*dhcpManager {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*dhcpManager
	for id, m := range p.persistentDHCP {
		if m.joinReq.NetworkID == networkID {
			out = append(out, m)
			delete(p.persistentDHCP, id)
		}
	}
	return out
}

// endpointFingerprint is the stable identity of a live endpoint we
// remember between CreateEndpoint and DeleteEndpoint. When the
// endpoint is deleted these fields become a tombstone for the next
// CreateEndpoint on the same network to inherit.
type endpointFingerprint struct {
	MAC      string
	IPv4     string // bare IPv4, e.g. "192.168.0.166" (no /mask). May be empty.
	IPv6     string // bare IPv6, e.g. "2001:db8::1" (no /prefix). May be empty.
	Hostname string // container hostname; used to narrow tombstone match.
	// Ifname preserves the custom interface name (#125) across the
	// Leave -> Join cycle of a container restart, where the join hint
	// is gone and libnetwork does not re-send endpoint options.
	Ifname string
}

// rememberEndpoint stashes the fingerprint of an endpoint we just
// created so DeleteEndpoint can resurrect it as a tombstone later.
// No-op when the MAC is empty (avoids polluting the map for failed
// CreateEndpoints).
func (p *Plugin) rememberEndpoint(endpointID string, fp endpointFingerprint) {
	if fp.MAC == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endpointFingerprints[endpointID] = fp
}

// updateEndpointIPs overwrites the recorded IPv4/IPv6 of an existing
// fingerprint. Empty arguments leave the corresponding field
// untouched, so callers that only know one family don't accidentally
// erase the other. No-op if we're not tracking this endpoint. Used
// by Leave to capture the latest persistent-client lease before
// DeleteEndpoint freezes the value into a tombstone.
func (p *Plugin) updateEndpointIPs(endpointID, ipv4, ipv6 string) {
	if ipv4 == "" && ipv6 == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fp, ok := p.endpointFingerprints[endpointID]
	if !ok {
		return
	}
	if ipv4 != "" {
		fp.IPv4 = ipv4
	}
	if ipv6 != "" {
		fp.IPv6 = ipv6
	}
	p.endpointFingerprints[endpointID] = fp
}

// hintIfname returns the custom interface name recorded in the join
// hint for endpointID, or "" — used to copy it into the endpoint
// fingerprint without widening function signatures.
func (p *Plugin) hintIfname(endpointID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.joinHints[endpointID].Ifname
}

// fingerprintIfname returns the custom interface name remembered for
// a live endpoint, or "" — the restart-path fallback for Join when
// the hint has already been consumed.
func (p *Plugin) fingerprintIfname(endpointID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endpointFingerprints[endpointID].Ifname
}

// takeEndpoint atomically retrieves and deletes the remembered
// fingerprint for an endpoint. Returns ok=false if no fingerprint
// was recorded (e.g. an endpoint created before this build, or a
// CreateEndpoint that failed before reaching the remember call).
func (p *Plugin) takeEndpoint(endpointID string) (endpointFingerprint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fp, ok := p.endpointFingerprints[endpointID]
	if ok {
		delete(p.endpointFingerprints, endpointID)
	}
	return fp, ok
}

// addTombstone appends a tombstone for a deleted endpoint so the
// next CreateEndpoint on the same network within tombstoneTTL can
// inherit its MAC and last IP/IPv6. hostname narrows the match in
// consumeTombstone to the same container. Best-effort: a disk
// failure here just means restart-stability for this particular
// event is lost; it's logged and the flow continues.
func (p *Plugin) addTombstone(networkID, hostname, mac, ipv4, ipv6 string) {
	if mac == "" {
		return
	}
	if err := p.tombstones.add(networkID, hostname, mac, ipv4, ipv6); err != nil {
		p.tombstoneWriteFailures.Add(1)
		log.WithError(err).Warn("Failed to persist tombstone; container restart may pick a new MAC/IP")
	}
}

// consumeTombstone returns and removes a tombstone for networkID iff
// EXACTLY one fresh entry matches. When hostname is non-empty we
// narrow the match to NetworkID+Hostname so a sequential `compose
// restart` of multiple containers on the same network can't swap
// identities between containers. When hostname is empty we fall back
// to NetworkID-only matching (preserves the v0.5.0 contract for
// hostname-less containers and races where the lookup didn't return
// in time). The "exactly one" rule still applies after filtering.
func (p *Plugin) consumeTombstone(networkID, hostname string, hostnameTrusted bool) (mac, ipv4, ipv6 string, ok bool) {
	// hostnameTrusted is a parameter rather than a check at the two call
	// sites for the same reason tombstonesConsumed is counted here: a
	// third caller cannot forget what it is forced to pass.
	//
	// consume() reads an empty hostname as "match any tombstone on this
	// network" — deliberate, for v0.5.0 tombstones and for the
	// CreateEndpoint/container-registration race, both honest absences.
	// safeHostname also yields an empty string when it REFUSES a
	// hostname, and routing that into the same wildcard turned the
	// sanitiser into a wildcard generator: one \x01 in --hostname and the
	// container inherited another endpoint's MAC and asked the DHCP
	// server for its address. An untrusted hostname therefore consumes
	// nothing: the container still attaches, with a fresh identity, which
	// is the right answer for a value nobody should have sent.
	if !hostnameTrusted {
		return "", "", "", false
	}
	mac, ipv4, ipv6, ok = p.tombstones.consume(networkID, hostname)
	if !ok {
		return "", "", "", false
	}
	// Counted here rather than at the two call sites (network.go,
	// parent_attached.go) so a third caller cannot forget it and quietly
	// under-report.
	p.tombstonesConsumed.Add(1)
	return mac, ipv4, ipv6, true
}

// recoverEndpoints walks Docker's networks, finds the ones served by
// this plugin, and rebuilds an in-memory dhcpManager for each attached
// endpoint. This restores the lease-renewal goroutines after a plugin
// process restart (e.g. `docker plugin disable` + `enable`, or after
// the plugin container has crashed and been restarted by Docker).
//
// Recovery sources state from Docker rather than persisting our own
// per-endpoint files: NetworkInspect gives us the MAC and IP of each
// attached endpoint, ContainerInspect gives the hostname and the
// container's PID for netns access. dhcpcd is invoked with that IP set
// as its `request` directive (DHCP option 50) so the upstream DHCP
// server can ACK the lease the container is already using rather than
// handing out a fresh one.
// listNetworksWhenReady is recovery's entry gate. It retries NetworkList
// until the daemon answers or ctx expires.
//
// Retrying rather than pinging first is deliberate: NetworkList is the
// capability recovery actually needs, and a daemon can answer /_ping
// before its network store is ready. Retrying the real call closes that
// gap instead of trading one race for another.
func (p *Plugin) listNetworksWhenReady(ctx context.Context) ([]dNetwork.Summary, error) {
	var lastErr error
	for {
		nets, err := p.docker.NetworkList(ctx, dNetwork.ListOptions{})
		if err == nil {
			return nets, nil
		}
		lastErr = err
		// ctx is the wait budget; the client's own timeout is what makes
		// each individual attempt return promptly.
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(recoveryDaemonRetryInterval):
		}
	}
}

// ctx bounds the whole of recovery; daemonWait is the slice of it the
// entry gate may spend waiting for the daemon to answer. They are
// separate on purpose — time spent waiting must not come out of the
// budget the endpoints themselves need to re-DISCOVER.
//
// recoverEndpoints returns daemonNotReady=true when it could not even
// reach the daemon within daemonWait. That is a "try again later", not a
// failure: the caller decides whether a retry is still possible (see
// NewPlugin / Listen) and only the last attempt counts a real failure.
func (p *Plugin) recoverEndpoints(ctx context.Context, daemonWait time.Duration) (daemonNotReady bool) {
	// recordSyncFailure bumps both the local counter (used for the
	// summary log line) and the atomic surfaced on /Plugin.Health.
	// The async Start failure path bumps p.recoveryFailed directly;
	// without this, NetworkInspect / netOptions / recoverOneEndpoint
	// failures would only show up in the log line and not on the
	// health endpoint operators page on (W-2 in the 2026-05-05 review).
	var recovered, failed, gone, alreadyManaged int
	recordSyncFailure := func() {
		failed++
		p.recoveryFailed.Add(1)
	}
	// A network that has been removed is not a recovery failure (#648).
	// The list we are walking is a snapshot; anything in it can be gone
	// by the time we ask for its detail, and a network that is gone has
	// no container left to rebuild a renewal client for.
	recordNetworkGone := func(id string, err error) {
		gone++
		p.recoveryNetworkGone.Add(1)
		log.WithError(err).WithField("network", shortID(id)).
			Info("recovery: network removed before it could be read; skipping")
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, daemonWait)
	nets, err := p.listNetworksWhenReady(waitCtx)
	waitCancel()
	if err != nil {
		log.WithError(err).WithField("waited", daemonWait).
			Warn("recovery: daemon did not answer within the wait budget")
		return true
	}
	for _, n := range nets {
		if !IsDHCPPlugin(n.Driver) {
			continue
		}
		// Per-network bounded ctx so a single hung NetworkInspect /
		// netOptions doesn't consume the whole recoveryBudget.
		netCtx, netCancel := context.WithTimeout(ctx, recoveryPerNetworkTimeout)
		// Re-fetch with full container details (NetworkList is summary-only).
		netInfo, err := p.docker.NetworkInspect(netCtx, n.ID, dNetwork.InspectOptions{})
		if err != nil {
			netCancel()
			if cerrdefs.IsNotFound(err) {
				recordNetworkGone(n.ID, err)
				continue
			}
			log.WithError(err).WithField("network", shortID(n.ID)).
				Warn("recovery: NetworkInspect failed; skipping")
			recordSyncFailure()
			continue
		}
		opts, err := p.netOptions(netCtx, n.ID)
		netCancel()
		if err != nil {
			// netOptions prefers the on-disk cache and only reaches the
			// daemon when that misses, so a 404 here is the same race one
			// call later.
			if cerrdefs.IsNotFound(err) {
				recordNetworkGone(n.ID, err)
				continue
			}
			log.WithError(err).WithField("network", shortID(n.ID)).
				Warn("recovery: failed to load network options; skipping")
			recordSyncFailure()
			continue
		}
		for cid, info := range netInfo.Containers {
			// Skip libnetwork's "ep-<endpoint>" placeholder: it means
			// the container is mid-creation. Either CreateEndpoint /
			// Join will run for it shortly (and our normal flow will
			// take over), or it'll never come up.
			if strings.HasPrefix(cid, "ep-") {
				continue
			}
			adopted, err := p.recoverOneEndpoint(ctx, cid, n.ID, info.EndpointID, info.MacAddress, info.IPv4Address, info.IPv6Address, opts)
			if err != nil {
				log.WithError(err).WithFields(log.Fields{
					"network":  shortID(n.ID),
					"endpoint": shortID(info.EndpointID),
				}).Warn("recovery: endpoint recovery failed")
				recordSyncFailure()
				continue
			}
			if !adopted {
				alreadyManaged++
				continue
			}
			recovered++
		}
	}
	if recovered > 0 || failed > 0 || gone > 0 || alreadyManaged > 0 {
		log.WithFields(log.Fields{
			"recovered":       recovered,
			"failed":          failed,
			"network_gone":    gone,
			"already_managed": alreadyManaged,
		}).Info("Plugin recovery complete")
	}
	return false
}

// recoverEndpointsDeferred is the second half of #383. The synchronous
// attempt in NewPlugin met a daemon that was still starting; this runs
// once the socket is listening, so the plugin stays responsive to the
// very daemon it is waiting for.
//
// Safe to run late: recoverOneEndpoint bails when a manager already
// exists, so any endpoint a Join has meanwhile claimed is left alone
// (TestPlugin_RecoverOneEndpointIsIdempotent pins that).
// wait is a parameter rather than a constant read so tests can drive the
// exhausted-budget arm without a minute of wall clock.
func (p *Plugin) recoverEndpointsDeferred(ctx context.Context, wait time.Duration) {
	p.recoveryDeferred.Add(1)
	log.WithField("wait", wait).
		Info("recovery: daemon not ready yet; retrying after the socket comes up")

	// The overall budget has to cover the wait *and* the recovery work
	// that follows it, so it is the sum of the two.
	runCtx, cancel := context.WithTimeout(ctx, wait+recoveryBudget)
	defer cancel()

	if notReady := p.recoverEndpoints(runCtx, wait); notReady {
		// Budget exhausted with the daemon still unreachable. Now it is
		// a real failure: nothing else is going to retry, so every
		// previously-attached endpoint is running without renewal.
		log.Error("recovery: daemon never became reachable; endpoints are running without a renewal client")
		p.recoveryFailed.Add(1)
	}
}

// containerGone reports whether containerID names a container that is
// no longer running — either the daemon has never heard of it (it was
// removed) or it has stopped.
//
// This is the recovery-side counterpart to sandboxGone, deliberately
// built on a different mechanism. sandboxGone avoids the Docker API
// because the API round-trip is itself what times out when a container
// vanishes mid-Join, and a Join request carries a sandbox key it can
// look at instead. Recovery has neither constraint: it is not on any
// container's critical path, and it already holds the container ID
// straight from NetworkInspect, so a direct inspect is both available
// and more accurate than inferring. recoverOneEndpoint's synthesised
// JoinRequest has no SandboxKey, so sandboxGone is not reusable here
// even in principle.
//
// An inspect error that is not "no such container" returns false. No
// usable evidence is not evidence of absence — the same stance
// sandboxGone takes about an unreadable netns directory — so a daemon
// that is unreachable or erroring degrades to counting a real recovery
// failure rather than silently excusing one.
func (p *Plugin) containerGone(ctx context.Context, containerID string) bool {
	if containerID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, recoveryPerNetworkTimeout)
	defer cancel()

	ctr, err := p.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return cerrdefs.IsNotFound(err)
	}
	// Restarting, paused-then-stopped, exited, dead: none of them have
	// a live netns depending on us, and a container that comes back
	// arrives through Join, which builds its own manager. Only
	// State.Running means "something is relying on this recovery".
	return ctr.State == nil || !ctr.State.Running
}

// recoveredHostname returns the container hostname to record in a
// recovered endpoint's fingerprint, and whether it may be recorded at
// all.
//
// ok=false means "record no fingerprint for this endpoint", and it
// deliberately covers two different things:
//
//   - the inspect did not answer, so the hostname is simply unknown;
//   - safeHostname REFUSED the hostname because it carries a control
//     character (#693).
//
// Both end in the same place because of what an empty Hostname means
// downstream: tombstoneStore.consume reads a tombstone with no hostname
// as "matches any container on this network". Recording an empty one
// here would not write a weaker tombstone, it would write a wildcard
// one, and the next container to attach to this network would inherit a
// MAC and an address that were never its own. Recording nothing leaves
// this endpoint exactly the behaviour it has today, which is the only
// direction that cannot hurt a container that did nothing wrong.
func (p *Plugin) recoveredHostname(ctx context.Context, containerID string) (string, bool) {
	if containerID == "" {
		p.recoveryFingerprintsSkipped.Add(1)
		return "", false
	}
	// The SAME budget the CreateEndpoint path gives the same lookup, and
	// deliberately not a tighter one. The first draft of this used
	// 500ms, on the reasoning that the daemon had already answered
	// NetworkList and NetworkInspect so a slow ContainerInspect meant it
	// was degrading. #406 is the measured counterexample: dockerd
	// answered other calls normally while blocking on ContainerInspect
	// for a container it was inside ContainerStart for, and did not
	// answer until it was done. Those two earlier calls say nothing
	// about whether THIS container's inspect is blocked — and a
	// container mid-ContainerStart is the expected state of most of what
	// recovery walks, since recovery runs while the daemon is restarting
	// every container on the host. A tighter budget would therefore
	// expire in precisely the scenario #721 exists to fix, and no
	// fixture would ever show it: they all answer instantly.
	ctx, cancel := context.WithTimeout(ctx, initialDHCPHostnameLookupTimeout)
	defer cancel()

	ctr, err := p.docker.ContainerInspect(ctx, containerID)
	if err != nil || ctr.Config == nil || ctr.Config.Hostname == "" {
		// Counted, not logged: this is the arm that makes an endpoint
		// quietly lose its address on its next restart, and a log line
		// is not something an operator can alert on.
		p.recoveryFingerprintsSkipped.Add(1)
		return "", false
	}
	// A refusal is counted by safeHostname itself
	// (unsafeHostnamesRejected); see the field comment for why it is not
	// also counted here.
	return p.safeHostname(ctr.Config.Hostname)
}

// recoverOneEndpoint synthesises a JoinRequest and dhcpManager for a
// single existing endpoint, then spawns Start in a goroutine. Idempotent:
// if a manager already exists for the endpoint (e.g. because libnetwork
// raced with us and called Join concurrently), we skip.
//
// Returns adopted=false for that skip, so the caller can tell an endpoint
// this recovery took responsibility for from one it merely looked at. The
// completion log used to count both as recovered, which put "recovered=1"
// in the log of a run whose recovered_ok stayed 0 — the counter was right
// and the line an operator reads was not (#480).
//
// containerID is carried through solely so the async Start failure can
// tell a real failure from a container that has since exited (#376).
func (p *Plugin) recoverOneEndpoint(ctx context.Context, containerID, networkID, endpointID, macStr, ipv4Cidr, ipv6Cidr string, opts DHCPNetworkOptions) (adopted bool, err error) {
	// Cheap pre-check, and it has to come before the parse: an endpoint
	// somebody else is already managing is fine no matter what Docker
	// reports for its MAC, and reaching the parse would turn that into a
	// recovery_failed — a healthy-affecting counter — for an endpoint
	// with a working renewal client. The compare-and-set below is what
	// actually closes the race; this only spares the work.
	if p.dhcpManagerExists(endpointID) {
		p.recoveryAlreadyManaged.Add(1)
		return false, nil
	}

	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return false, fmt.Errorf("parse MAC %q: %w", macStr, err)
	}

	var ipv4, ipv6 *netlink.Addr
	if ipv4Cidr != "" {
		if a, err := netlink.ParseAddr(ipv4Cidr); err == nil {
			ipv4 = a
		}
	}
	if ipv6Cidr != "" {
		if a, err := netlink.ParseAddr(ipv6Cidr); err == nil {
			ipv6 = a
		}
	}

	fakeJoin := JoinRequest{
		NetworkID:  networkID,
		EndpointID: endpointID,
	}
	m := newDHCPManager(p.docker, fakeJoin, opts).withPlugin(p)
	m.setLastIP(false, ipv4)
	m.setLastIP(true, ipv6)
	m.MacAddress = mac
	// Checked and registered in one operation, so a Join that arrives
	// mid-recovery keeps its own manager instead of having it evicted
	// by ours. Building the manager first costs nothing when we lose:
	// it was never published, so nothing can reach it and it holds no
	// dhcpcd — Start is only spawned below, after we have won.
	if !p.registerDHCPManagerIfAbsent(endpointID, m) {
		p.recoveryAlreadyManaged.Add(1)
		return false, nil
	}

	// Recovery is the only path that takes ownership of a live endpoint
	// without a CreateEndpoint, and it used to leave the fingerprint map
	// untouched (#721). DeleteEndpoint lays a tombstone only for an
	// endpoint it holds a fingerprint for, so every endpoint that had
	// lived through a plugin restart lost address stability on its next
	// `docker restart` — silently, with tombstones_consumed simply
	// staying flat. Recorded after the compare-and-set above rather than
	// before it: an endpoint a concurrent Join won is that Join's to
	// describe, and overwriting its fingerprint with ours would hand its
	// tombstone our idea of the hostname.
	//
	// Ifname is deliberately left empty. Docker's record does not carry
	// the custom interface name (#125), so a recovered endpoint falls
	// back to the default on its next Join. Losing the name is visible
	// and the operator can restore it; inventing one is neither.
	if hostname, ok := p.recoveredHostname(ctx, containerID); ok {
		fpIPv4, fpIPv6 := "", ""
		if ipv4 != nil {
			fpIPv4 = ipv4.IP.String()
		}
		if ipv6 != nil {
			fpIPv6 = ipv6.IP.String()
		}
		p.rememberEndpoint(endpointID, endpointFingerprint{
			MAC:      mac.String(),
			IPv4:     fpIPv4,
			IPv6:     fpIPv6,
			Hostname: hostname,
		})
	}

	go func() {
		startCtx, cancel := context.WithTimeout(context.Background(), p.awaitTimeout)
		defer cancel()
		if err := m.Start(startCtx); err != nil {
			fields := log.Fields{
				"network":   shortID(networkID),
				"endpoint":  shortID(endpointID),
				"container": shortID(containerID),
			}
			// A container that exited before recovery reached it is not
			// a plugin failure. recovery_failed means "a RUNNING
			// container has no renewal client" and flips healthy;
			// firing it for a container that is simply gone would page
			// an operator over a normal exit (#376) — the same defect
			// #373 fixed on the Join side.
			//
			// Checked here rather than before Start: the container
			// being present when recovery began says nothing about
			// whether it survived the seconds Start takes, and an
			// inspect on the success path would be pure cost. A fresh
			// context because startCtx is already expired whenever
			// Start failed by timing out.
			if p.containerGone(context.Background(), containerID) {
				p.recoveryAbortedContainerGone.Add(1)
				log.WithError(err).WithFields(fields).
					Info("recovery: container went away before recovery completed; no persistent client needed")
				p.removeDHCPManagerIfSame(endpointID, m)
				return
			}
			p.recoveryFailed.Add(1)
			log.WithError(err).WithFields(fields).
				Error("recovery: persistent DHCP client Start failed; lease will not renew until container restart")
			// Identity-checked: a Join for this endpoint may already
			// have displaced us with a fresh manager we must not evict.
			p.removeDHCPManagerIfSame(endpointID, m)
			return
		}
		p.recoveredOK.Add(1)
	}()
	return true, nil
}

// lookupEndpointMAC reads the MAC address Docker has stored for an
// endpoint by inspecting the network it belongs to. We use this on the
// container-restart path so the rebuilt link can be given the same MAC
// libnetwork already returned to Docker — keeping `docker inspect`'s
// view consistent with the actual interface inside the container.
//
// Returns ErrNoHint-equivalent if the endpoint can't be found, which
// callers treat as "give up and let libnetwork error this Join".
func (p *Plugin) lookupEndpointMAC(ctx context.Context, networkID, endpointID string) (string, error) {
	dockerNet, err := p.docker.NetworkInspect(ctx, networkID, dNetwork.InspectOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to inspect network: %w", err)
	}
	for _, info := range dockerNet.Containers {
		if info.EndpointID == endpointID {
			return info.MacAddress, nil
		}
	}
	return "", fmt.Errorf("endpoint %v not found in network %v's container list", endpointID, networkID)
}

// reacquireEndpoint rebuilds the host-side link and re-runs the initial
// DHCP exchange for an endpoint whose state was lost. Invoked from
// Join when no joinHint is present, which happens when libnetwork
// drives Leave -> Join on the same EndpointID (Docker container restart).
//
// Implementation: synthesise the equivalent CreateEndpointRequest and
// reuse CreateEndpoint's logic. For ipvlan we deliberately leave the
// MAC blank — ipvlan children share the parent's MAC, so passing an
// explicit one would just trip the ipvlan-rejects-custom-MAC check;
// the rebuilt link will inherit the parent's MAC the same way the
// original did.
func (p *Plugin) reacquireEndpoint(ctx context.Context, r JoinRequest, opts DHCPNetworkOptions) error {
	macAddr := ""
	if opts.effectiveMode() != ModeIPvlan {
		mac, err := p.lookupEndpointMAC(ctx, r.NetworkID, r.EndpointID)
		if err != nil {
			return fmt.Errorf("failed to look up original endpoint MAC: %w", err)
		}
		macAddr = mac
	}
	fakeReq := CreateEndpointRequest{
		NetworkID:  r.NetworkID,
		EndpointID: r.EndpointID,
		Interface:  &EndpointInterface{MacAddress: macAddr},
	}
	if _, err := p.CreateEndpoint(ctx, fakeReq); err != nil {
		return fmt.Errorf("CreateEndpoint replay failed: %w", err)
	}
	return nil
}

// initialDHCPHostname makes a best-effort attempt to find the hostname
// of the container we're about to attach an endpoint to, so we can pass
// it in the initial DHCPDISCOVER. Polls the network's Containers map
// for up to initialDHCPHostnameLookupTimeout; if the container hasn't
// been registered yet (it's a race; sometimes Docker calls
// CreateEndpoint before the container appears in the network's
// container list), we fall through with an empty hostname. The
// persistent renewal client populates the hostname later regardless,
// so the worst case is "first lease appears in the upstream DHCP
// server's UI without a hostname for a few minutes".
//
// The second return value is false when a hostname was found and REFUSED
// (see safeHostname), as opposed to not found at all. Both produce an
// empty hostname and they must not be treated alike: an absent hostname
// is an honest unknown that tombstone matching deliberately treats as a
// wildcard, while a refused one is attacker-supplied and must not buy it.
func (p *Plugin) initialDHCPHostname(ctx context.Context, networkID, endpointID string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, initialDHCPHostnameLookupTimeout)
	defer cancel()

	// Each Docker call inside the poll body is bounded much tighter
	// than the outer 2s budget so a single hung NetworkInspect /
	// ContainerInspect doesn't burn the whole window. The Docker client
	// itself has its own 2s per-request timeout (NewPlugin), but that's
	// the same as our entire poll budget — without an inner cap, one
	// stuck call effectively turns the 100ms retry interval into a 2s
	// retry interval. Cap the inner ctx at the poll interval.
	const dockerCallTimeout = 200 * time.Millisecond

	var hostname string
	// Defaults to true: a lookup that never finds the container returns
	// an empty hostname that nobody chose, which is the honest-unknown
	// case. Only an actual refusal below flips it.
	trusted := true
	_ = util.AwaitCondition(ctx, func() (bool, error) {
		inner, innerCancel := context.WithTimeout(ctx, dockerCallTimeout)
		defer innerCancel()
		dockerNet, err := p.docker.NetworkInspect(inner, networkID, dNetwork.InspectOptions{})
		if err != nil {
			// Don't propagate the error — we want to keep retrying
			// while the timeout has time. The caller treats an empty
			// hostname as "not yet known" and lets renewal handle it.
			return false, nil
		}
		for ctrID, info := range dockerNet.Containers {
			if info.EndpointID != endpointID {
				continue
			}
			// Docker uses an "ep-<endpointID>" placeholder until the
			// real container ID is bound. Wait for the real one.
			if strings.HasPrefix(ctrID, "ep-") {
				return false, nil
			}
			ctr, err := p.docker.ContainerInspect(inner, ctrID)
			if err != nil {
				return false, nil
			}
			hostname, trusted = p.safeHostname(ctr.Config.Hostname)
			return true, nil
		}
		return false, nil
	}, 100*time.Millisecond)
	return hostname, trusted
}

// NewPlugin creates a new Plugin. Zero-valued Options fields take the
// documented defaults, so NewPlugin(Options{}) is a valid production
// configuration.
func NewPlugin(opts Options) (*Plugin, error) {
	warnIfStateDirIsNotThePersistentOne()
	if opts.AwaitTimeout <= 0 {
		opts.AwaitTimeout = defaultAwaitTimeout
	}
	if opts.OutageTick <= 0 {
		opts.OutageTick = defaultOutageTick
	}
	if opts.OutageGrace <= 0 {
		opts.OutageGrace = defaultOutageGrace
	}
	client, err := docker.NewClientWithOpts(
		docker.WithHost("unix:///run/docker.sock"),
		docker.WithAPIVersionNegotiation(),
		// Fail fast on hung API calls. Concretely defends against the
		// daemon-startup window where dockerd may be calling into us
		// before it can respond to our own NetworkInspect / etc.
		docker.WithTimeout(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	p := Plugin{
		awaitTimeout: opts.AwaitTimeout,
		outageTick:   opts.OutageTick,
		outageGrace:  opts.OutageGrace,
		startTime:    time.Now(),
		instanceID:   newInstanceID(),

		docker: client,

		joinHints:            make(map[string]joinHint),
		persistentDHCP:       make(map[string]*dhcpManager),
		endpointFingerprints: make(map[string]endpointFingerprint),
	}
	p.ledger = newLeaseLedger(filepath.Join(stateDir, ledgerFileName), &p.ledgerWriteFailures)

	// Routing table, and the RPCs deliberately left off it: routes.go.
	mux := p.newServeMux()

	// Capture sits INSIDE the access-logging handler so a captured
	// request is one that was actually served, and outside the mux so
	// it sees the raw body before any handler decodes it. With no
	// capture directory set — every shipped plugin — captureHandler
	// returns the mux itself and this line is a no-op (#644).
	// limitBody sits OUTSIDE the logging and capture handlers so the cap
	// applies to the body before either reads it. Timeouts and the cap
	// are explained in http_limits.go -- in particular why WriteTimeout
	// is zero here and not on the metrics server.
	p.server = http.Server{
		Handler:           limitBody(handlers.CustomLoggingHandler(nil, captureHandler(mux, opts.RequestCaptureDir, capturablePaths(p.routes())), util.WriteAccessLog)),
		ReadHeaderTimeout: socketReadHeaderTimeout,
		ReadTimeout:       socketReadTimeout,
		WriteTimeout:      socketWriteTimeout,
		IdleTimeout:       socketIdleTimeout,
	}

	// Run endpoint recovery synchronously before NewPlugin returns
	// (and thus before Listen accepts the first RPC). Doing it on a
	// background goroutine — the previous behaviour — opened a window
	// where a fresh CreateEndpoint could race recovery's Start for the
	// same endpoint: the map check is mutex-protected, but Start runs
	// outside the mutex. recoveryBudget bounds plugin-enable latency.
	//
	// The one case we do NOT finish here is a daemon that has not
	// started serving yet (#383). Docker respawns us during its own
	// startup, so blocking for it would add latency to plugin-enable
	// against the very daemon we are waiting on. Recovery is handed to
	// Listen instead, which runs it once the socket is up.
	{
		ctx, cancel := context.WithTimeout(context.Background(), recoveryBudget)
		p.recoveryPending = p.recoverEndpoints(ctx, recoverySyncDaemonWait)
		cancel()
	}

	return &p, nil
}

// Listen starts the plugin server
func (p *Plugin) Listen(bindSock string) error {
	// Best-effort: remove a stale socket file from a prior run so
	// net.Listen doesn't EADDRINUSE on it. Production plugin runtimes
	// recreate the workdir between starts, so this is a no-op there;
	// it matters for local / test runs where the file lingers.
	_ = os.Remove(bindSock)

	l, err := net.Listen("unix", bindSock)
	if err != nil {
		return err
	}

	// A UNIX socket is created with 0777 &^ umask, so without this the
	// access control on our entire RPC surface would be whatever umask
	// the plugin runtime happened to hand us -- 0755 today, 0775 under
	// a umask of 0002, 0777 under 0. SECURITY.md argues that serving
	// /metrics here is unchanged ground *because* the socket is
	// root-only; that property is now enforced rather than inherited
	// (#687). Only the daemon speaks this protocol and it connects as
	// root, so nothing legitimate needs group or other.
	if err := os.Chmod(bindSock, 0o600); err != nil {
		// Refuse to serve on a socket whose mode we could not pin:
		// an unknown mode is exactly the state this guards against.
		l.Close()
		return fmt.Errorf("restricting the plugin socket to the owner: %w", err)
	}

	// The socket exists now, so the daemon can reach us even while we
	// are still waiting on it. Start the deferred recovery here rather
	// than in NewPlugin (#383) — before this point, waiting would make
	// us unreachable to the daemon whose readiness we are waiting for.
	if p.recoveryPending {
		p.recoveryPending = false
		ctx, cancel := context.WithCancel(context.Background())
		p.recoveryCancel = cancel
		go p.recoverEndpointsDeferred(ctx, recoveryDeferredDaemonWait)
	}

	return p.server.Serve(l)
}

// pluginShutdownTimeout caps the WHOLE shutdown: the HTTP grace
// period, the persistent-client release fan-out, and the drain of
// in-flight displaced-manager stops all share this one budget (#338).
// Deliberately a total rather than a per-phase cap — phases have been
// added twice now, and a per-phase timeout silently multiplies the
// wall-clock an operator waits through on `docker plugin disable`.
// Short enough to keep a plugin upgrade snappy on hosts with many
// endpoints; long enough that a typical dhcpcd release-and-exit cycle
// completes well within it.
//
// A var, not a const, solely so tests can shrink it: the forced-path
// and timeout behaviours are only reachable by letting the budget
// expire, and a 5s wall-clock per case is not something to pay in the
// unit suite. Never reassigned outside tests.
var pluginShutdownTimeout = 5 * time.Second

// waitBounded waits for wg, giving up after d. Reports whether the
// wait completed.
//
// Known leak (W-8 in the 2026-05-05 review): on timeout the watcher
// goroutine and whatever the group was waiting on live until the OS
// reaps the process. Acceptable in Close, which runs at process exit
// — but DO NOT copy this into a long-lived caller (e.g. a future
// SIGHUP-driven re-attach). For long-lived use, pass a ctx into the
// work and have it abort cleanly on cancel.
func waitBounded(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// Close stops the plugin. The HTTP server is shut down FIRST so no new
// Join can register a manager while (or after) we stop the existing
// ones — with the old ordering a Join dispatched during the stop
// fan-out installed a manager into the fresh registry that nobody ever
// stopped, orphaning its lease (no DHCPRELEASE) and its dhcpcd.
// Persistent DHCP clients are then stopped before process exit so they
// get a chance to send DHCPRELEASE for their leases — otherwise plugin
// upgrade or `docker plugin disable` would orphan every active lease at
// the upstream DHCP server, defeating the release-on-stop contract
// Leave normally honors.
// ListenMetrics starts the optional TCP listener for /metrics.
//
// Off unless METRICS_ADDR is set, and that default is deliberate. The
// plugin holds CAP_NET_ADMIN, CAP_SYS_ADMIN and CAP_SYS_PTRACE with
// "network": {"type": "host"} in config.json, so any port it opens is on
// the host's own network namespace. Opening one has to be a decision an
// operator made, not something they inherited by upgrading (#651).
//
// The mux here carries /metrics ALONE. See the metricsServer field for
// why that is load-bearing rather than tidy.
//
// Returns once the listener is bound, so a bad METRICS_ADDR fails at
// startup where an operator will see it, rather than in a goroutine that
// logs and leaves the plugin running without the endpoint they asked for.
func (p *Plugin) ListenMetrics(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", p.apiMetrics)

	warnOnWildcardMetricsBind(addr)

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics listener on %q: %w", addr, err)
	}

	p.metricsListener = l
	// A scrape is a small GET whose handler renders one snapshot, so
	// unlike the plugin socket this server can carry a write timeout
	// safely. See http_limits.go.
	p.metricsServer = &http.Server{
		Handler:           limitBody(mux),
		ReadHeaderTimeout: metricsReadHeaderTimeout,
		ReadTimeout:       metricsReadTimeout,
		WriteTimeout:      metricsWriteTimeout,
		IdleTimeout:       metricsIdleTimeout,
	}
	go func() {
		if err := p.metricsServer.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithError(err).Error("Metrics listener stopped")
		}
	}()
	log.WithField("addr", addr).Info("Serving /metrics over TCP")
	return nil
}

func (p *Plugin) Close() error {
	// Stop the deferred-recovery retry first (#383). It can be sitting
	// in a 60s wait for a daemon that is going away with us, and a
	// recovery that registers a manager after the drain below would
	// orphan that manager's lease — exactly the ordering bug the
	// server-first shutdown is written to prevent.
	if p.recoveryCancel != nil {
		p.recoveryCancel()
	}

	// One deadline for every phase below; see pluginShutdownTimeout.
	deadline := time.Now().Add(pluginShutdownTimeout)
	remaining := func() time.Duration {
		if d := time.Until(deadline); d > 0 {
			return d
		}
		return 0
	}

	// Shutdown, unlike Close, waits for in-flight handlers to RETURN.
	// That is what makes a single drain below provably sufficient
	// rather than merely likely: registerDHCPManager runs synchronously
	// in the Join handler, before the goroutine that Starts the client
	// (see network.go), so once no handler is running the registry is
	// final and nothing further can register into it. The previous
	// two-pass sweep was approximating this guarantee by racing it.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), remaining())
	defer cancel()

	// forced records that a handler outlasted the grace period, so the
	// guarantee above does NOT hold and we fall back to the old
	// speculative behaviour. Rare, not impossible — keep it explicit
	// instead of assuming it away.
	// The metrics listener carries no plugin state and no in-flight
	// work worth draining, so it is closed outright rather than given a
	// slice of the shutdown budget. Doing it first stops a scrape
	// arriving mid-drain and reading a registry that is being emptied.
	if p.metricsServer != nil {
		_ = p.metricsServer.Close()
	}

	forced := false
	serverErr := p.server.Shutdown(shutdownCtx)
	if serverErr != nil {
		forced = true
		log.WithError(serverErr).Warn("HTTP server did not shut down gracefully; forcing connections closed")
		serverErr = p.server.Close()
	}

	// stopSnapshot drains the current registry once: snapshot under the
	// lock, then Stop each manager in parallel outside it (Stop blocks
	// on dhcpcd Wait and we don't want to hold p.mu across that).
	// Returns how many managers it stopped.
	stopSnapshot := func() int {
		p.mu.Lock()
		managers := make([]*dhcpManager, 0, len(p.persistentDHCP))
		for _, m := range p.persistentDHCP {
			managers = append(managers, m)
		}
		p.persistentDHCP = make(map[string]*dhcpManager)
		p.mu.Unlock()

		if len(managers) == 0 {
			return 0
		}
		log.WithField("count", len(managers)).Info("Stopping persistent DHCP clients before shutdown")
		// Stop in parallel — each dhcpcd release is independent and
		// we don't want N×timeout wall time.
		var wg sync.WaitGroup
		for _, m := range managers {
			wg.Add(1)
			go func(m *dhcpManager) {
				defer wg.Done()
				if err := m.Stop(); err != nil {
					log.WithError(err).Warn("Failed to stop persistent DHCP client at shutdown")
				}
			}(m)
		}
		// Bound wall time: we can't let one wedged dhcpcd hold up the
		// whole shutdown.
		if !waitBounded(&wg, remaining()) {
			log.Warn("Timeout waiting for persistent DHCP clients to stop; continuing shutdown")
		}
		return len(managers)
	}

	// One pass is enough on the graceful path: Shutdown returned, so no
	// handler is still running and the registry cannot grow behind us.
	stopSnapshot()
	if forced {
		// Degraded path only. A handler was still in flight when the
		// grace period expired, so it may have registered a manager
		// after the snapshot above. This is the pre-#338 behaviour,
		// kept for exactly this case.
		if n := stopSnapshot(); n > 0 {
			log.WithField("count", n).Info("Stopped late-registered DHCP clients in forced-shutdown sweep")
		}
	}

	// Drain displaced-manager stops spawned by Join (#338). Each is an
	// in-flight DHCPRELEASE for a client this plugin displaced; without
	// this, process exit cuts it short and orphans the lease at the
	// server — the same failure the fan-out above exists to prevent.
	if !waitBounded(&p.displacedStops, remaining()) {
		log.Warn("Timeout waiting for displaced DHCP manager stops; continuing shutdown")
	}

	// Same for orphan releases (#370). These reclaim a lease that no
	// client is holding open, so cutting one short is the one case where
	// shutdown itself causes the leak.
	if !waitBounded(&p.orphanReleases, remaining()) {
		log.Warn("Timeout waiting for orphaned-lease releases; continuing shutdown")
	}

	if err := p.docker.Close(); err != nil {
		return fmt.Errorf("failed to close docker client: %w", err)
	}

	if serverErr != nil {
		return fmt.Errorf("failed to close http server: %w", serverErr)
	}

	return nil
}

// safeHostname returns h when it can be carried into the generated dhcpcd
// config unchanged, and ("", false) when it cannot (#692).
//
// The hostname is the container's own and Docker does not validate it, so
// it is the one value on this path chosen by whoever started the
// container rather than by an operator or by us. dhcpcd.directive would
// drop it anyway — that is the structural guarantee — but doing it here
// means the event reaches a counter, and a counter is the only form an
// operator can alert on.
//
// Dropping rather than failing the endpoint is deliberate: the hostname
// only decorates the DHCP exchange (and the opt-in FQDN registration), so
// refusing the container over it would turn a cosmetic problem into an
// outage the attacker chose.
// WHY THERE IS A SECOND RETURN VALUE
//
// The first version of this returned a bare "" and that was a
// vulnerability, not a rough edge. The hostname is not only decoration:
// it is also the key that narrows tombstone matching to the container
// that wrote the tombstone, and in tombstoneStore.consume an EMPTY
// hostname means "match any tombstone on this network" — a deliberate
// carve-out for v0.5.0 tombstones and for the lookup race, both honest.
// Collapsing "I refused this value" into that same "" handed the caller a
// wildcard, so one control character in --hostname let a container
// inherit another endpoint's MAC and request its address.
//
// A refusal therefore has to be distinguishable from an absence. The
// caller that only writes the DHCP config can keep ignoring the
// difference; the caller that makes an identity decision must not.
func (p *Plugin) safeHostname(h string) (string, bool) {
	if dhcp.SafeDirectiveValue(h) {
		return h, true
	}
	p.unsafeHostnamesRejected.Add(1)
	log.WithField("hostname", fmt.Sprintf("%q", h)).
		Warn("Dropping container hostname: it carries a control character and cannot be written to the DHCP client config")
	return "", false
}
