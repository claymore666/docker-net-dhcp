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
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	cerrdefs "github.com/containerd/errdefs"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/gorilla/handlers"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// DriverName is the name of the Docker Network Driver
const DriverName string = "net-dhcp"

// newInstanceID returns a per-process value so two health reads can tell unmoved counters from a reset (#405); it
// is never empty, since two empty ids compare equal and would hide a restart.
func newInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// shortID truncates an ID to 12 characters for log fields, tolerating short or empty IDs from recovery.
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

// defaultAwaitTimeout is the Options.AwaitTimeout fallback and the source of config.json's AWAIT_TIMEOUT default.
// initialDHCPHostnameLookupTimeout, below, caps CreateEndpoint's hostname lookup for the first DISCOVER; a miss
// is filled in by the persistent client on first renewal (#961).
const defaultAwaitTimeout = 10 * time.Second

// attachDaemonBusyGrace is added to AwaitTimeout for the Join attach only: the daemon is inside ContainerStart for
// this container and answers when it finishes (#406). 60 s exceeds a plausible container start; Stop cancels the
// attach, so a leaving container does not pay it. A var so tests can shrink it.
var attachDaemonBusyGrace = 60 * time.Second

const initialDHCPHostnameLookupTimeout = 2 * time.Second

// recoveryBudget caps the whole startup rebuild; past it, endpoints surface as recovery_failed.
const recoveryBudget = 30 * time.Second

// recoveryPerNetworkTimeout caps each recovery Docker call so one wedged call cannot consume recoveryBudget (#76).
const recoveryPerNetworkTimeout = 3 * time.Second

// recoverySyncDaemonWait caps the pre-Listen wait for the daemon, which respawns the plugin during its own startup
// and adds this window to plugin-enable latency (#383). On expiry recovery moves to the post-Listen retry.
const recoverySyncDaemonWait = 3 * time.Second

// recoveryDeferredDaemonWait caps the post-Listen retry, cheap because the socket already serves.
const recoveryDeferredDaemonWait = 60 * time.Second

// recoveryDaemonRetryInterval spaces retries; a var so tests can shrink it.
var recoveryDaemonRetryInterval = 500 * time.Millisecond

// clientIDFromEndpoint derives an option-61 id from the first 8 bytes of the endpoint ID; nil if the ID is not hex.
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

// clientIDFromMAC derives the option-61 payload from the MAC, nil when unset (#371). formatClientID adds type 0x00:
// several servers treat RFC 2132's type 0x01 as an alias for chaddr, which would change matching.
func clientIDFromMAC(mac net.HardwareAddr) []byte {
	if len(mac) == 0 {
		return nil
	}
	return append([]byte(nil), mac...)
}

// resolveClientID picks the option-61 payload: opts.ClientID when set, else the MAC, which the tombstone preserves
// so an IPv4 address survives `docker restart` without depending on a RELEASE that SIGKILL or power loss never
// sends (#370, #371). ipvlan slaves share the parent MAC, so they keep the endpoint-derived id (#219), which is
// also the fallback for a missing MAC.
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

// uuidBytes is the width of RFC 9915 section 11.5's DUID-UUID payload and of the ipvlan endpoint seed.
const uuidBytes = 16

// resolveIdentity6 picks the DHCPv6 DUID and IAID by mode (D30 Q4). Bridge and macvlan get RFC 9915 section 11.4's
// DUID-LL over the MAC with the MAC's low four bytes as IAID, 1.9.0's identity byte for byte, so an upgraded
// endpoint keeps its binding. ipvlan and the MAC-less fallback get section 11.5's DUID-UUID over the endpoint id,
// since slaves share the parent MAC (#895); an ipvlan endpoint upgraded from 1.x gets a new address once.
func resolveIdentity6(opts DHCPNetworkOptions, endpointID string, mac net.HardwareAddr) (dhcp.Identity6, error) {
	if opts.effectiveMode() != ModeIPvlan && len(mac) > 0 {
		duid, err := dhcp.DUIDLL(mac)
		if err != nil {
			return dhcp.Identity6{}, fmt.Errorf("failed to build the endpoint's DHCPv6 identity: %w", err)
		}
		iaid, err := dhcp.IAIDFromMAC(mac)
		if err != nil {
			return dhcp.Identity6{}, fmt.Errorf("failed to build the endpoint's DHCPv6 IAID: %w", err)
		}
		return dhcp.Identity6{DUID: duid, IAID: iaid}, nil
	}

	seed := endpointSeed(endpointID)
	if seed == nil {
		return dhcp.Identity6{}, fmt.Errorf("endpoint %q is too short to derive a DHCPv6 identity from and the mode supplies no usable MAC", shortID(endpointID))
	}
	duid, err := dhcp.DUIDUUID(seed)
	if err != nil {
		return dhcp.Identity6{}, fmt.Errorf("failed to build the endpoint's DHCPv6 identity: %w", err)
	}
	iaid, err := dhcp.IAIDFromBytes(seed)
	if err != nil {
		return dhcp.Identity6{}, fmt.Errorf("failed to build the endpoint's DHCPv6 IAID: %w", err)
	}
	return dhcp.Identity6{DUID: duid, IAID: iaid}, nil
}

// endpointRecordKey is the MAC half of the (scope, chaddr) record index, except on ipvlan, whose slaves share the
// parent MAC and would all resume the newest record (#895). There the endpoint id is folded to six bytes with the
// locally-administered bit set and the group bit clear, so no real link's address can collide. Pre-existing ipvlan
// records under the parent MAC are not found, and such an endpoint acquires afresh once.
func endpointRecordKey(mode, endpointID string, mac net.HardwareAddr) net.HardwareAddr {
	if mode != ModeIPvlan {
		return mac
	}
	seed := endpointSeed(endpointID)
	if len(seed) < 6 {
		return mac
	}
	key := append([]byte(nil), seed[:6]...)
	key[0] = (key[0] &^ 0x01) | 0x02
	return key
}

// endpointSeed is the first uuidBytes of the endpoint id, a prefix so a server log reads beside the endpoint.
func endpointSeed(endpointID string) []byte {
	if len(endpointID) < uuidBytes*2 {
		return nil
	}
	b, err := hex.DecodeString(endpointID[:uuidBytes*2])
	if err != nil {
		return nil
	}
	return b
}

// defaultLeaseTimeout is dhcp.ConflictRecoveryWindow, derived from the library constants: 34.0 s funds one
// RFC 5227 conflict, its DHCPDECLINE and RFC 2131 section 3.1(5)'s ten-second wait. 10 s and then 12 s both
// failed `docker run` against a working server, the 12 s case measured 0.8 s short on 2026-09-04 (#882).
// Asserted by TestLeaseTimeout_DefaultCoversTheWorstWaitAcquisition and
// TestLeaseTimeout_DefaultFundsOneConflictAndItsRestartDelay.
var defaultLeaseTimeout = dhcp.ConflictRecoveryWindow(proto.DefaultParams(nil))

// driverRegexp matches only the maintained namespaces (devplayer0, claymore666) as this driver, so an arbitrary
// image of the same name cannot trigger "Bridge already in use" (#74).
var driverRegexp = regexp.MustCompile(`(^|/)(devplayer0|claymore666)/docker-net-dhcp:.+$`)

// IsDHCPPlugin checks if a Docker network driver is an instance of this plugin
func IsDHCPPlugin(driver string) bool {
	return driverRegexp.MatchString(driver)
}

// DHCPNetworkOptions contains options for the DHCP network driver
type DHCPNetworkOptions struct {
	// Mode selects the attachment strategy: bridge (the default, needs `bridge`), macvlan or ipvlan (need `parent`).
	Mode   string `mapstructure:"mode"`
	Bridge string
	Parent string `mapstructure:"parent"`
	// Gateway, if set, overrides the DHCP-supplied default gateway, e.g. to egress via a VPN router.
	Gateway string
	// IPv6 switches DHCPv6 on and equals `ipv6_mode=dhcp`; read it only through ipv6Enabled (#817).
	IPv6 bool
	// IPv6Mode is off, dhcp, slaac or auto, proto.Mode6's spellings; a value disagreeing with IPv6 is refused (#817).
	IPv6Mode string `mapstructure:"ipv6_mode"`
	// IPv6AutoStrict makes `ipv6_mode=auto` fail an endpoint whose DHCPv6 server stays silent instead of falling back
	// to SLAAC and counting dhcpv6_auto_fallbacks (#817).
	IPv6AutoStrict bool `mapstructure:"ipv6_auto_strict"`
	// IPv6MainPrefix, a CIDR, picks which SLAAC address Docker is told about, since RFC 4862 section 5.5.3 forms one
	// per autonomous prefix; unset or unmatched uses the first advertised prefix (#818).
	IPv6MainPrefix string        `mapstructure:"ipv6_main_prefix"`
	LeaseTimeout   time.Duration `mapstructure:"lease_timeout"`
	// IgnoreConflicts skips CreateNetwork's check for another Docker network on this bridge or range; it is unrelated
	// to conflict_check's RFC 5227 detection on the wire.
	IgnoreConflicts bool `mapstructure:"ignore_conflicts"`
	// ConflictCheck is the RFC 5227 mode, wait, async or off, empty meaning dhcp.DefaultConflictCheck (#882, D23).
	ConflictCheck string `mapstructure:"conflict_check"`
	SkipRoutes    bool   `mapstructure:"skip_routes"`
	// PropagateDNS writes option 6 or 23 into the container's /etc/resolv.conf on every bind or renew.
	PropagateDNS bool `mapstructure:"propagate_dns"`
	// PropagateMTU sets the container link's MTU from option 26 on every bind or renew.
	PropagateMTU bool `mapstructure:"propagate_mtu"`
	// MTU, 68..65535, is set on every container link this network creates and is then its only source; 0 is unset (#1037).
	MTU int `mapstructure:"mtu"`
	// ClientID overrides the derived option 61 for every endpoint, sent with type byte 0x00; see resolveClientID
	// (#371).
	ClientID string `mapstructure:"client_id"`
	// VendorClass overrides option 60, default "docker-net-dhcp", for class-based server policy.
	VendorClass string `mapstructure:"vendor_class"`
	// ValidateDHCP runs a one-shot DHCP probe on a macvlan or ipvlan parent at CreateNetwork (#108).
	ValidateDHCP bool `mapstructure:"validate_dhcp"`
	// RegisterDNS sends the FQDN option (81 v4, 39 v6) from the hostname, asking the server to register it (#261).
	RegisterDNS bool `mapstructure:"register_dns"`
	// AuditLog appends every lease event on this network to STATE_DIR/leases.jsonl (#109).
	AuditLog bool `mapstructure:"audit_log"`

	// DHCPServers is an ordered, exhaustive preference list of DHCPv4 servers, e.g. "1.1.1.1,2.2.2.2" (#111).
	DHCPServers string `mapstructure:"dhcp_servers"`
	// DenyServers lists DHCPv4 servers never to take a lease from, composed with DHCPServers by serverPolicy (#669).
	DenyServers string `mapstructure:"dhcp_deny_servers"`
	// ReleaseLease is `never` (the default, leases expire) or `on_stop` (release when the endpoint leaves) (#962).
	ReleaseLease string `mapstructure:"release_lease"`
	// HostIfname names bridge mode's host-side link: empty for `dh-` plus 12 hex, or `container_name` or `hostname`
	// (#978).
	HostIfname string `mapstructure:"host_ifname"`
	// RequireMAC refuses an endpoint whose MAC the user did not set, so a MAC-keyed reservation always matches (#1036).
	RequireMAC bool `mapstructure:"require_mac"`
	// LinkLocalFallback gives an endpoint an RFC 3927 169.254/16 address when no DHCPv4 lease arrives in time (#904).
	LinkLocalFallback bool `mapstructure:"link_local_fallback"`
	// MacvlanMode is the macvlan child's kernel mode, bridge (the default), vepa, private or passthru (#905).
	MacvlanMode string `mapstructure:"macvlan_mode"`
	// IPvlanMode is the ipvlan child's kernel mode; l2 (the default) is the only accepted value, see parseIPvlanMode
	// (#905).
	IPvlanMode string `mapstructure:"ipvlan_mode"`
	// Vlan is an 802.1Q ID; the children attach to `<parent>.<id>`, created when missing (#902).
	Vlan string `mapstructure:"vlan"`
}

func (o DHCPNetworkOptions) effectiveMode() string {
	if o.Mode == "" {
		return ModeBridge
	}
	return o.Mode
}

// fqdnMode maps register_dns to the client's FQDN mode: "both" for forward and reverse, "" for none (#261).
func (o DHCPNetworkOptions) fqdnMode() string {
	if o.RegisterDNS {
		return "both"
	}
	return ""
}

func decodeOpts(input interface{}) (DHCPNetworkOptions, error) {
	opts, _, err := decodeOptsSet(input)
	return opts, err
}

// decodeOptsSet is decodeOpts plus the set of Go field names the input carried: #817 must refuse `ipv6=false` beside
// `ipv6_mode=slaac` while accepting `ipv6_mode=slaac` alone, and both decode to the same struct. mapstructure's
// Metadata.Keys mixes tags and field names, so normaliseOptionKeys maps them to field names, and
// dropEmptyOptionValues drops options written with no value.
func decodeOptsSet(input interface{}) (DHCPNetworkOptions, map[string]bool, error) {
	input = dropEmptyOptionValues(input)

	var opts DHCPNetworkOptions
	var md mapstructure.Metadata
	optsDecoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           &opts,
		ErrorUnused:      true,
		WeaklyTypedInput: true,
		Metadata:         &md,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			decimalIntHook,
		),
	})
	if err != nil {
		return opts, nil, fmt.Errorf("failed to create options decoder: %w", err)
	}

	if err := optsDecoder.Decode(input); err != nil {
		return opts, nil, err
	}

	return opts, normaliseOptionKeys(md.Keys), nil
}

// decimalIntHook reads an int option as base 10 only, since mapstructure's weak decode parses with base 0 and takes
// "0x5dc" as 1500 and "01400" as octal 768 (#1037).
func decimalIntHook(f reflect.Type, t reflect.Type, data interface{}) (interface{}, error) {
	if f.Kind() != reflect.String || t.Kind() != reflect.Int {
		return data, nil
	}
	n, err := strconv.Atoi(data.(string))
	if err != nil {
		return nil, fmt.Errorf("%q is not a decimal integer", data)
	}
	return n, nil
}

// normaliseOptionKeys maps mapstructure tags to Go field names, keeping an unmatched key as is.
func normaliseOptionKeys(keys []string) map[string]bool {
	byTag := map[string]string{}
	t := reflect.TypeOf(DHCPNetworkOptions{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if tag := f.Tag.Get("mapstructure"); tag != "" {
			byTag[tag] = f.Name
		}
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		if name, ok := byTag[k]; ok {
			set[name] = true
			continue
		}
		set[k] = true
	}
	return set
}

// dropEmptyOptionValues drops options written with no value, so `-o ipv6=` equals no `-o ipv6` and not
// `-o ipv6=false`: the pinned mapstructure decodes `{"ipv6": "", "ipv6_mode": "dhcp"}` to IPv6=false with the key
// set (#817). One rule for every option, applied before the decode; it makes `-o lease_timeout=` mean unset,
// stated in docs/reference.md and pinned by TestDecodeOptsSet_AnEmptyValueIsNotAValue. A non-map input passes.
func dropEmptyOptionValues(input interface{}) interface{} {
	m, ok := input.(map[string]interface{})
	if !ok {
		return input
	}
	var out map[string]interface{}
	for k, v := range m {
		if s, isStr := v.(string); isStr && s == "" {
			if out == nil {
				out = make(map[string]interface{}, len(m))
				for k2, v2 := range m {
					out[k2] = v2
				}
			}
			delete(out, k)
		}
	}
	if out == nil {
		return input
	}
	return out
}

type joinHint struct {
	IPv4    *netlink.Addr
	IPv6    *netlink.Addr
	Gateway string
	// Routes are the RFC 3442 option-121 routes from CreateEndpoint's exchange, for the Join answer (#700).
	Routes []*StaticRoute
	// GatewayIPv6 is the advertising router's link-local address from CreateEndpoint, since DHCPv6 has no gateway
	// (#821).
	GatewayIPv6 string
	// RoutesIPv6 are RFC 4191 next-hop routes and RFC 4861 section 4.6.2 on-link prefixes, not the default (#821).
	RoutesIPv6 []*StaticRoute
	// MacAddress is the one-shot's MAC, keying the DHCP identity (#371) and locating the macvlan link in the netns.
	MacAddress net.HardwareAddr
	// Ifname is the validated interface_name option, carried to Join's DstName since Join gets no endpoint options
	// (#125).
	Ifname string
	// RecordID is CreateEndpoint's durable lease record; empty means Join DISCOVERs (#899).
	RecordID string
}

// Options carries the plugin's runtime knobs from config.json's environment variables, zero meaning the default.
type Options struct {
	// AwaitTimeout caps the polling helpers (sandbox readiness, link rename, netns appearance).
	AwaitTimeout time.Duration

	// RequestCaptureDir tees libnetwork request bodies into that directory for replay fixtures, test builds only
	// (#644).
	RequestCaptureDir string
}

// Plugin is the DHCP network plugin.
type Plugin struct {
	awaitTimeout time.Duration
	startTime    time.Time
	// instanceID identifies this process, so two health reads compare only when it is unchanged (#405).
	instanceID string

	docker dockerClient

	// engine is the startup probe's result, swapped atomically so a reader never mixes two probes (#670).
	engine atomic.Pointer[engineIdentity]
	server http.Server

	// metricsServer is the optional METRICS_ADDR listener, a separate server serving only /metrics, since p.server
	// carries every libnetwork RPC with CAP_NET_ADMIN, CAP_SYS_ADMIN and CAP_SYS_PTRACE (#772).
	metricsServer *http.Server
	// metricsListener exposes the kernel-assigned address when METRICS_ADDR names port 0.
	metricsListener net.Listener

	// mu guards joinHints, persistentDHCP and endpointFingerprints against libnetwork's concurrent handlers.
	mu             sync.Mutex
	joinHints      map[string]joinHint
	persistentDHCP map[string]*dhcpManager
	// endpointFingerprints keeps each endpoint's MAC and IPv4 for DeleteEndpoint's tombstone, after Leave took the
	// manager (#46).
	endpointFingerprints map[string]endpointFingerprint

	// vlanMu serialises a vlan sub-interface's create, adoption and removal, and guards vlanPending, the creates
	// between their ensure and their save; it is never held with mu (#902).
	vlanMu      sync.Mutex
	vlanPending map[string]int

	// tombstones serialises tombstones.json and is never held with mu; scripts/check-lock-discipline.sh enforces it.
	tombstones tombstoneStore

	// recoveredOK and recoveryFailed count restart-recovery outcomes for /Plugin.Health.
	recoveredOK    atomic.Int32
	recoveryFailed stampedCounter

	// recoveryAlreadyManaged counts endpoints a recovery walk left to an existing manager, the only sign of recovery
	// racing a Join; not a failure (#480).
	recoveryAlreadyManaged atomic.Int32

	// recoveryDeferred counts recoveries retried after Listen because the daemon, which respawns the plugin during
	// its own startup, was not answering yet (#383); not healthy-affecting.
	recoveryDeferred atomic.Int32

	// recoveryPending is set by NewPlugin when recovery must be retried after Listen, and consumed there.
	recoveryPending bool

	// recoveryCancel stops the deferred-recovery goroutine at Close; nil when recovery finished synchronously.
	recoveryCancel context.CancelFunc

	// recoveryAbortedContainerGone counts recoveries abandoned because the container had exited (#376); not
	// healthy-affecting, as no running container lacks a renewal client.
	recoveryAbortedContainerGone atomic.Int32

	// recoveryNetworkGone counts networks removed between recovery's list and inspect, answered 404 (#648); not
	// healthy-affecting, but a steady climb shows network churn under a restarting daemon.
	recoveryNetworkGone atomic.Int32

	// recoveryFingerprintsSkipped counts adopted endpoints whose inspect gave no hostname, so no tombstone is laid and
	// the next `docker restart` loses the address (#721). Not healthy-affecting, and disjoint from
	// unsafeHostnamesRejected.
	recoveryFingerprintsSkipped atomic.Int32

	// joinStartFailures counts Join-time client Start failures: a running container with no renewal client (#317).
	// Healthy-affecting; an on_stop network still releases from the record (#962).
	joinStartFailures stampedCounter

	// joinAbortedContainerGone counts attaches abandoned because the container exited first (#373); not
	// healthy-affecting, but visible to show crash-loops.
	joinAbortedContainerGone atomic.Int32

	// joinAbortedNoContainer counts attaches abandoned because no container ever claimed the endpoint (#566, #567);
	// not healthy-affecting.
	joinAbortedNoContainer atomic.Int32

	// joinAttachSlow counts attaches that finished only after outlasting AwaitTimeout, carried by
	// attachDaemonBusyGrace (#406); not healthy-affecting.
	joinAttachSlow atomic.Int32

	// joinAttachCompleted, joinAttachUnder1s, joinAttach1sToBudget and joinAttachMsMax, with joinAttachSlow, partition
	// every successful attach, readable at the shipped info log level (#403). netnsSrc redirects the sandbox-netns
	// readings at fixtures, zero in production; see netnsSources.
	netnsSrc netnsSources

	joinAttachCompleted  atomic.Int32
	joinAttachUnder1s    atomic.Int32
	joinAttach1sToBudget atomic.Int32
	joinAttachMsMax      atomic.Int32

	// dhcpServerTierFallbacks counts acquisitions that fell to the next dhcp_servers entry, the only sign a preferred
	// server is down (#111); not healthy-affecting.
	dhcpServerTierFallbacks atomic.Int32

	// dhcpServerPolicyExhausted counts acquisitions no dhcp_servers entry answered, telling policy refusal from a
	// DHCP outage (#111).
	dhcpServerPolicyExhausted atomic.Int32

	// dhcpServerPolicyTimeouts counts unanswered renewals on allow-list-restricted clients, a strict subset of
	// dhcpTimeouts so the two rising together blames the list (#731). Not healthy-affecting and not family-split, as
	// the list is v4-only.
	dhcpServerPolicyTimeouts atomic.Int32

	// restartLinkUpWaited counts child links that came up after waiting out the departing link's address hold (#408);
	// restartLinkUpTimeouts counts the wait expiring, already loud through CreateEndpoint. Neither affects healthy
	// (#422).
	restartLinkUpWaited   atomic.Int32
	restartLinkUpTimeouts stampedCounter

	// joinAbortedEndpointLeft counts attaches cancelled by Leave (#406); not healthy-affecting.
	joinAbortedEndpointLeft atomic.Int32

	// unsafeHostnamesRejected counts hostnames with a control character dropped before option 12 (#692); a legitimate
	// hostname has none, so non-zero means someone is trying.
	unsafeHostnamesRejected atomic.Int32

	// hostnamesAppliedLate, hostnameLookupFailures and hostnameApplyFailures are the outcomes of naming a running v4
	// client (#961): handed over, the daemon inspect unanswered, or the client refusing the name. Only non-empty names
	// count, and none affects healthy.
	hostnamesAppliedLate   atomic.Int32
	hostnameLookupFailures stampedCounter
	hostnameApplyFailures  stampedCounter

	// hostIfnamesApplied, hostIfnameConflicts and hostIfnameFailures are the outcomes of naming a host-side link after
	// its container (#978): renamed, the name already taken on the host, or any other refusal, including an altname
	// the kernel would not keep, which is undone because DeleteEndpoint looks the link up by it. None affects healthy.
	hostIfnamesApplied  atomic.Int32
	hostIfnameConflicts stampedCounter
	hostIfnameFailures  stampedCounter

	// dnsPropagationPIDMismatches counts DNS writes refused because the Docker-supplied PID was recycled (#688).
	dnsPropagationPIDMismatches atomic.Int32

	// netnsPIDMismatches counts netns opens refused for a recycled PID, which fail the attach with an error reading
	// like a slow start; only this counter names the cause (#695).
	netnsPIDMismatches atomic.Int32

	// sandboxKeyEntries, sandboxKeyEntryFailures and sandboxPIDFallbacks show which route entered each netns: the
	// sandbox key, or the PID route needing CAP_SYS_PTRACE (#725). Entries give the domain, so "no fallbacks" means
	// something; none affects healthy.
	sandboxKeyEntries       atomic.Int32
	sandboxKeyEntryFailures atomic.Int32
	sandboxPIDFallbacks     atomic.Int32

	// The four arms of sandboxKeyEntryFailures, summing to it by construction, show the cause of a key refusal, e.g.
	// an unpropagated bind mount; sandboxKeyUnavailable is the residual arm (#725, #691). None affects healthy.
	sandboxKeyAbsent        atomic.Int32
	sandboxKeyNotPermitted  atomic.Int32
	sandboxKeyNotANamespace atomic.Int32
	sandboxKeyWrongNSType   atomic.Int32
	sandboxKeyUnavailable   atomic.Int32

	// dockerAPINonGETRefusals counts non-GET Docker API requests refused by the transport; it should stay zero (#691).
	dockerAPINonGETRefusals atomic.Int32

	// dhcpRoutesApplied counts option-121 routes handed to Docker, and dhcpDefaultRouteSuperseded the Joins whose
	// routes cover 0.0.0.0/0, e.g. `0.0.0.0/1` plus `128.0.0.0/1`, which the gateway field hides (#700).
	dhcpRoutesApplied          atomic.Int32
	dhcpDefaultRouteSuperseded atomic.Int32

	// mtuRefused counts option-26 MTUs outside propagateMTU's range, left unapplied; a 68-byte MTU would black-hole
	// path MTU discovery (#702).
	mtuRefused atomic.Int32

	// unsafeOptionValuesDropped counts server string values (options 66, 67, 100, 101, 252, and option 15 truncated at
	// its first space) refused for a control character, counted where pkg/dhcp decodes them (#703, #704).
	unsafeOptionValuesDropped atomic.Int32

	// networkOptionsRejected counts endpoint operations refusing a network's stored options, an invalid interface name
	// or unknown mode (#727, #705); DeleteEndpoint counts and still tears down. Not healthy-affecting: the fault is one
	// network's record, which only an operator can clear.
	networkOptionsRejected atomic.Int32

	// The IPAM driver's state (#110): ipamPools holds PoolIDs answered but not yet bound, ipamIndex maps a bound PoolID
	// to its network and is rebuilt at start, and ipamReserves holds one hardware address to one DHCP exchange.
	ipamPools    *issuedPools
	ipamIndex    *ipamIndex
	ipamReserves *ipamReserves
	// recordSweepStop ends the record sweeper; closed by Close, which refuses to run twice (#984).
	recordSweepStop chan struct{}

	// ipamReplayHits counts stored addresses confirmed from our lease record at a daemon restart (#110).
	ipamReplayHits atomic.Int32

	// ipamReplayMiss counts stored addresses refused because no lease record holds them, meaning the record and
	// Docker's store drifted apart (#110); not healthy-affecting.
	ipamReplayMiss stampedCounter

	// ipamRebindAmbiguous counts address requests matching several recently removed endpoints, the documented limit: a
	// RequestAddress carries no hostname or endpoint id, so the server decides (#110). Not healthy-affecting.
	ipamRebindAmbiguous stampedCounter

	// ipamReserveDuplicateMAC counts requests refused because two endpoints carry one MAC, e.g. `--mac-address X`
	// twice: libnetwork copies an operator MAC through (moby 28.5.2, libnetwork/network.go:1222 and :1240) (#110). A
	// daemon re-send after a timeout does not move it: moby reuses the drained body reader (pkg/plugins/client.go
	// callWithRetry), measured in integration run 34600486961 as "failed to parse request body: EOF".
	ipamReserveDuplicateMAC stampedCounter

	// ipamStrandedRecords counts created-phase records with no endpoint, left by a plugin process that ended mid-start
	// and given up at start-up so the address can be claimed again (#1047). Not healthy-affecting.
	ipamStrandedRecords stampedCounter

	// ipamReleaseUnknown counts released addresses no lease record holds, the normal order after retain or close
	// (#110).
	ipamReleaseUnknown atomic.Int32

	// tombstoneWriteFailures counts failed tombstone saves (disk full, EROFS); each loses one restart's address (#46).
	tombstoneWriteFailures stampedCounter

	// tombstonesConsumed counts CreateEndpoints that reused a tombstone's MAC and IP, telling that path apart from
	// recovery re-adopting a live endpoint (#386).
	tombstonesConsumed atomic.Int32

	// leaseChangedV4 counts renewals returning a different IP, which `docker inspect` does not show (#104).
	leaseChangedV4 stampedCounter

	// addressConflictsV4 and its v6 sibling count leased addresses found in use on the segment (#524, D12), healthy-
	// affecting since the server will reissue them. v4 comes from the library's RFC 5227 probes and listener (sections
	// 2.1 and 2.4) via Failed or Lost{ReasonConflict}, exclusive per conflict (#882); v6 from RFC 4862 section 5.4 DAD,
	// declined per RFC 9915 section 18.2.8. Split so the v4 half compares with acdConflictsDetected; read it against
	// acdProbesSent, since a zero with no probes is a detector not running.
	addressConflictsV4 stampedCounter
	addressConflictsV6 stampedCounter

	// acdProbesSent, acdAnnouncementsSent, acdConflictsDetected and acdARPSendFailures are the library's RFC 5227
	// counters, accumulated from every manager including one-shots (#882). acdConflictsDetected must equal the v4
	// addressConflicts; a mismatch is a seam defect.
	acdProbesSent        atomic.Int32
	acdAnnouncementsSent atomic.Int32
	acdConflictsDetected atomic.Int32
	acdARPSendFailures   stampedCounter

	// acdResumedUnchecked counts record-resumed endpoints whose RFC 5227 section 2.1 check had not finished (D23): the
	// window before the INIT-REBOOT ACK re-check, hence a warn check.
	acdResumedUnchecked stampedCounter

	// leasesObtainedV4 (bound), leasesRenewedV4 (renew), dhcpTimeoutsV4 (one per attempt ending in
	// Failed{ReasonNoServer}) and clientStopFailuresV4 (the client did not stop cleanly; releaseFailuresV4 counts
	// releases since #962) are the v4 halves; healthSnapshot sums each with its V6 sibling (#730).
	leasesObtainedV4     atomic.Int32
	leasesRenewedV4      atomic.Int32
	dhcpTimeoutsV4       atomic.Int32
	clientStopFailuresV4 atomic.Int32

	// renewalsUnansweredV4 counts unanswered renewal requests while the client runs (#940), at the first
	// retransmission and not the lease end: RFC 2131 section 4.4.5's half-remaining wait with a 60 s floor gives ~7.5 h
	// of warning on a 24 h lease. A request counts only once a later request proves it unanswered, so N silent requests
	// read N-1; see renewalWatch. Fed as a delta so it never falls.
	renewalsUnansweredV4 atomic.Int32

	// parentGate serialises child-link creation per parent NIC (parent_gate.go); its counters do not affect healthy.
	parentGate             parentGate
	parentLinkWaits        atomic.Int32
	parentLinkWaitTimeouts stampedCounter

	// naksReceivedV4 counts server NAKs (#128); with lease_changed rising too, containers are re-addressed mid-life
	// (#104).
	naksReceivedV4 atomic.Int32

	// The v6 half of each pair above (#212); healthSnapshot sums the pair, keeping totals monotonic (#730).
	leaseChangedV6   stampedCounter
	leasesObtainedV6 atomic.Int32
	leasesRenewedV6  atomic.Int32
	dhcpTimeoutsV6   atomic.Int32
	naksReceivedV6   atomic.Int32
	// clientStopFailuresV6 splits client_stop_failures by family (#608).
	clientStopFailuresV6 atomic.Int32
	// renewalsUnansweredV6 counts RFC 9915 section 18.2.4 Renews and 18.2.5 Rebinds the same way as v4 (#940).
	renewalsUnansweredV6 atomic.Int32

	// releasesSent counts releases that left the host, from the library's ReleasesSent; releaseFailures counts
	// attempts that sent none, since the library call is fire-and-forget (#962). Both stay zero on `never`; a failure
	// leaves the lease to expire upstream, not healthy-affecting.
	releasesSentV4 atomic.Int32
	releasesSentV6 atomic.Int32
	// releaseFailures is a warn check, so a stampedCounter renders when it last moved (checkStamps).
	releaseFailuresV4 stampedCounter
	releaseFailuresV6 stampedCounter

	// releasesReclaimed counts on_remove holds a running container reused at the window's end, so nothing was sent
	// (#984); claimNewerHold and claimInFlight also send nothing and are not counted. Not healthy-affecting.
	releasesReclaimedV4 atomic.Int32
	releasesReclaimedV6 atomic.Int32

	// dhcpv6ConfigOnly counts DHCPv6 information replies received, options with no address (#815); there is no v4
	// half, as the plugin sends no DHCPINFORM.
	dhcpv6ConfigOnly atomic.Int32

	// dhcpv6NotOffered counts endpoints without DHCPv6 because the RA lacked the managed flag (#868); healthy.
	dhcpv6NotOffered atomic.Int32

	// dhcpv6NoRouterAdvert counts endpoints without DHCPv6 because no RA arrived at all, a possible misconfiguration
	// kept apart from dhcpv6NotOffered (#868).
	dhcpv6NoRouterAdvert atomic.Int32

	// dhcpv6Refused counts endpoints a DHCPv6 server turned down with a non-Success RFC 9915 section 21.13 Status Code
	// (#816): a reachable server with no address for this client.
	dhcpv6Refused atomic.Int32

	// dhcpv6NoServer counts endpoints failed because the managed flag was set and no server answered in time (#816).
	dhcpv6NoServer atomic.Int32

	// dhcpv6SLAACNoPrefix counts endpoints failed because no advertised prefix formed an address for one of RFC 4862
	// section 5.5.3's reasons, a router to fix (#816, #817).
	dhcpv6SLAACNoPrefix atomic.Int32

	// dhcpv6AutoFallbacks counts `ipv6_mode=auto` endpoints that formed a SLAAC address after the advertised DHCPv6
	// server stayed silent, from lease.Stats.SLAACFallbacks (#817).
	dhcpv6AutoFallbacks atomic.Int32
	// autoFallbackCounted holds the endpoint IDs already in dhcpv6AutoFallbacks: the Join client and the persistent
	// client each fall back on a silent server, and the counter counts endpoints (#1016).
	autoFallbackCounted sync.Map

	// ipv6LinkEnableFailures counts links whose engine-set disable_ipv6=1 could not be cleared (#868); nothing IPv6
	// arrives on such a link, so it would otherwise read as a DHCPv6 timeout.
	ipv6LinkEnableFailures atomic.Int32

	// dhcpv6SLAACNoAddress counts SLAAC-mode endpoints whose advertisement formed no address in budget with no reason
	// named; see v6SLAACNoAddress (#816).
	dhcpv6SLAACNoAddress atomic.Int32

	// ipv6SLAACAddresses counts SLAAC addresses installed by netlink, one per autonomous prefix (RFC 4862 section
	// 5.5.3), not endpoints (#818).
	ipv6SLAACAddresses atomic.Int32

	// ipv6AddressesWithdrawn counts IPv6 addresses removed because the lease stopped holding them, the other half of
	// ipv6SLAACAddresses (#818, #819).
	ipv6AddressesWithdrawn atomic.Int32

	// ipv6SLAACPrefixesIgnored counts advertised prefixes that formed no address, for RFC 4862 section 5.5.3's reasons
	// or proto.MaxSLAACAddresses, telling "at the cap" from "nothing to form" (#818). The library's log names the rule.
	ipv6SLAACPrefixesIgnored atomic.Int32

	// ipv6MainPrefixUnmatched counts endpoints whose ipv6_main_prefix matched no address, a configuration signal
	// (#818).
	ipv6MainPrefixUnmatched atomic.Int32

	// routerAdvertGuardFailures counts RA guard steps that failed, a sysctl write or read-back, at most six per
	// endpoint (#875). A failed guard looks healthy until the router lifetime expires, since DHCPv6 carries no router
	// (RFC 9915 section 21, RFC 5942 section 4). A privileged container process rewriting the knobs is not counted
	// (D30 Q3).
	routerAdvertGuardFailures atomic.Int32

	// The library's RFC 4861 router-discovery counters, folded from every DHCPv6 manager (#814), describing the
	// segment. Read routerAdvertsSeen against routerSolicitsSent; routerTableEntriesDropped or Evicted above zero means
	// the router table's caps are in force.
	routerSolicitsSent         atomic.Int32
	routerAdvertsSeen          atomic.Int32
	routerAdvertsRefused       atomic.Int32
	routerAdvertOptionsIgnored atomic.Int32
	routerTableEntriesDropped  atomic.Int32
	routerTableEntriesEvicted  atomic.Int32

	// ipv6RouterWithdrawn counts default routes removed after a Router Lifetime 0 (RFC 4861 sections 4.2 and 6.3.4),
	// counting removals since a shutting-down router sends several (section 6.2.5) (#821). Not healthy-affecting.
	ipv6RouterWithdrawn atomic.Int32

	// displacedStops tracks Join's goroutines stopping a displaced manager so Close waits for them (#338), unbounded
	// to keep Join free of head-of-line blocking; a displacement sends no release (#962).
	displacedStops      sync.WaitGroup
	displacedStopsTotal atomic.Int32

	// ledger is the audit_log lease ledger (#109); ledgerWriteFailures counts failed appends without flipping healthy.
	ledger              *leaseLedger
	ledgerWriteFailures stampedCounter

	// ifnameUnsupported counts endpoints whose custom interface name the engine does not apply, a warn check (#125,
	// #670).
	ifnameUnsupported stampedCounter

	// stateFileChmodFailures counts state files the startup sweep could not tighten, a warn check (#804).
	stateFileChmodFailures stampedCounter

	// records is the durable lease record that lets a restart resume with INIT-REBOOT (#899); nil only in unit-test
	// literals, as NewPlugin refuses to run without one.
	records *dhcp.Records
}

func (p *Plugin) storeJoinHint(endpointID string, h joinHint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.joinHints[endpointID] = h
}

// updateJoinHint applies fn to the hint under the lock; fn must not call back into Plugin.
func (p *Plugin) updateJoinHint(endpointID string, fn func(*joinHint)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.joinHints[endpointID]
	fn(&h)
	p.joinHints[endpointID] = h
}

func (p *Plugin) takeJoinHint(endpointID string) (joinHint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.joinHints[endpointID]
	if ok {
		delete(p.joinHints, endpointID)
	}
	return h, ok
}

// registerDHCPManager registers a manager before its Start goroutine runs and returns any manager it displaced,
// which the caller must Stop or its client keeps running on the interface (#480).
func (p *Plugin) registerDHCPManager(endpointID string, m *dhcpManager) *dhcpManager {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.persistentDHCP[endpointID]
	p.persistentDHCP[endpointID] = m
	return old
}

// dhcpManagerExists is advisory and may be stale; recovery registers through registerDHCPManagerIfAbsent.
func (p *Plugin) dhcpManagerExists(endpointID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.persistentDHCP[endpointID]
	return exists
}

// registerDHCPManagerIfAbsent registers m only if the endpoint is unmanaged, as one operation: recovery is older
// truth and must yield to a Join, while a separate check-then-register evicted a live Join's manager (#383, #480).
func (p *Plugin) registerDHCPManagerIfAbsent(endpointID string, m *dhcpManager) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.persistentDHCP[endpointID]; exists {
		return false
	}
	p.persistentDHCP[endpointID] = m
	return true
}

// removeDHCPManagerIfSame deletes the entry only if it still holds m, so a failed Start cannot evict a successor
// registered by a fast Leave and Join (#480).
func (p *Plugin) removeDHCPManagerIfSame(endpointID string, m *dhcpManager) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.persistentDHCP[endpointID] == m {
		delete(p.persistentDHCP, endpointID)
	}
}

func (p *Plugin) takeDHCPManager(endpointID string) (*dhcpManager, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.persistentDHCP[endpointID]
	if ok {
		delete(p.persistentDHCP, endpointID)
	}
	return m, ok
}

// takeDHCPManagersForNetwork removes every manager of networkID, including recovered ones never sent a Leave.
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

// endpointFingerprint is a live endpoint's identity, turned into a tombstone at DeleteEndpoint (#46).
type endpointFingerprint struct {
	MAC      string
	IPv4     string // bare IPv4 without a mask, may be empty
	IPv6     string
	Hostname string
	// HostnameRefused marks an empty Hostname as refused, not absent, since an empty stored hostname matches any
	// container (#726).
	HostnameRefused bool
	// Ifname keeps the custom interface name across a restart's Leave and Join, when the hint is gone (#125).
	Ifname string
	// Released records that the lease actually went back at Leave, so DeleteEndpoint offers no tombstone (#962).
	Released bool
}

// dhcpHostname is a hostname with its trust bit, one value because safeHostname's "" means both refused and absent,
// and tombstoneStore.consume reads "" as matching every container on the network (#726).
type dhcpHostname struct {
	// name is the hostname for the DHCP exchange, "" for refused or none; read it with refused.
	name string
	// refused is true when the plugin declined the hostname for a control character (#692, #693).
	refused bool
}

// trusted reports whether name may narrow a tombstone match; an absent hostname is trusted and matches network-wide.
func (h dhcpHostname) trusted() bool { return !h.refused }

// rememberEndpoint stashes a created endpoint's fingerprint for DeleteEndpoint's tombstone; a no-op with no MAC.
// The hostname is a dhcpHostname parameter so the trust bit cannot be dropped: a bool parameter let `true` restore
// #726 with the package green, and TestHostnameTrustIsWired refuses a laundered value.
func (p *Plugin) rememberEndpoint(endpointID string, fp endpointFingerprint, h dhcpHostname) {
	fp.Hostname = h.name
	fp.HostnameRefused = h.refused
	if fp.MAC == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endpointFingerprints[endpointID] = fp
}

// updateEndpointIPs overwrites the fingerprint's addresses, an empty argument leaving its family untouched.
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

// markEndpointReleased records that the lease went back, so DeleteEndpoint lays no tombstone (#962).
func (p *Plugin) markEndpointReleased(endpointID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fp, ok := p.endpointFingerprints[endpointID]
	if !ok {
		return
	}
	fp.Released = true
	p.endpointFingerprints[endpointID] = fp
}

// hintIfname returns the join hint's custom interface name, or "".
func (p *Plugin) hintIfname(endpointID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.joinHints[endpointID].Ifname
}

// fingerprintIfname returns a live endpoint's remembered interface name, Join's fallback when the hint is gone.
func (p *Plugin) fingerprintIfname(endpointID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endpointFingerprints[endpointID].Ifname
}

func (p *Plugin) takeEndpoint(endpointID string) (endpointFingerprint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fp, ok := p.endpointFingerprints[endpointID]
	if ok {
		delete(p.endpointFingerprints, endpointID)
	}
	return fp, ok
}

// addTombstone records a deleted endpoint's MAC and addresses for the next CreateEndpoint within tombstoneTTL;
// a disk failure is logged and loses only that restart's stability (#46).
func (p *Plugin) addTombstone(networkID, hostname, mac, ipv4, ipv6 string) {
	if mac == "" {
		return
	}
	// A 169.254/16 address is no lease, and the next `request ADDR` would name one no server holds (#904).
	if isLinkLocalV4String(ipv4) {
		ipv4 = ""
	}
	if err := p.tombstones.add(networkID, hostname, mac, ipv4, ipv6); err != nil {
		p.tombstoneWriteFailures.Add(1)
		log.WithError(err).Warn("Failed to persist tombstone; container restart may pick a new MAC/IP")
	}
}

// consumeTombstone removes and returns the network's single matching fresh tombstone, narrowed by hostname so a
// `compose restart` cannot swap identities; an empty hostname matches network-wide (#46).
func (p *Plugin) consumeTombstone(networkID string, h dhcpHostname) (mac, ipv4, ipv6 string, ok bool) {
	// An untrusted hostname consumes nothing: its "" would be the network-wide wildcard, and one \x01 in --hostname
	// inherited another endpoint's MAC and address (#726).
	if !h.trusted() {
		return "", "", "", false
	}
	mac, ipv4, ipv6, ok = p.tombstones.consume(networkID, h.name)
	if !ok {
		return "", "", "", false
	}
	// Counted here, not at the call sites, so a new caller cannot under-report.
	p.tombstonesConsumed.Add(1)
	return mac, ipv4, ipv6, true
}

// listNetworksWhenReady retries NetworkList until ctx ends; a daemon answers /_ping before its network store is
// ready, so the real call is retried (#383).
func (p *Plugin) listNetworksWhenReady(ctx context.Context) ([]dNetwork.Summary, error) {
	var lastErr error
	for {
		nets, err := p.docker.NetworkList(ctx, dNetwork.ListOptions{})
		if err == nil {
			return nets, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(recoveryDaemonRetryInterval):
		}
	}
}

// recoverEndpoints rebuilds a dhcpManager for each attached endpoint of this plugin's networks after a restart.
// daemonWait is carved out of ctx so waiting cannot eat the endpoints' budget; daemonNotReady asks the caller to
// retry, and only the last attempt counts a failure (#383).
func (p *Plugin) recoverEndpoints(ctx context.Context, daemonWait time.Duration) (daemonNotReady bool) {
	// recordSyncFailure bumps the summary count and the /Plugin.Health atomic.
	var recovered, failed, gone, alreadyManaged int
	recordSyncFailure := func() {
		failed++
		p.recoveryFailed.Add(1)
	}
	// A network removed since the list is not a recovery failure (#648).
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
		// Per-network deadline so one hung call cannot consume recoveryBudget (#76).
		netCtx, netCancel := context.WithTimeout(ctx, recoveryPerNetworkTimeout)
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
			// netOptions reads the disk cache first, so a 404 here is the same removal race (#648).
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
			// libnetwork's "ep-<endpoint>" placeholder is a container mid-creation; Join will handle it.
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
		// The stranded-record rule, IPAM mode only, from the same inspect answer, so a failed inspect writes nothing
		// (#1047).
		if ipamBindingOf(n.ID) != nil {
			if listed, ok := ipamListedMACs(netInfo.Containers); ok {
				p.giveUpStrandedIPAMRecords(n.ID, listed, time.Now())
			}
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

// recoverEndpointsDeferred runs recovery after Listen when the daemon was still starting (#383); an endpoint a
// Join claimed meanwhile is left alone (TestPlugin_RecoverOneEndpointIsIdempotent).
func (p *Plugin) recoverEndpointsDeferred(ctx context.Context, wait time.Duration) {
	p.recoveryDeferred.Add(1)
	log.WithField("wait", wait).
		Info("recovery: daemon not ready yet; retrying after the socket comes up")

	runCtx, cancel := context.WithTimeout(ctx, wait+recoveryBudget)
	defer cancel()

	notReady := p.recoverEndpoints(runCtx, wait)

	// The startup engine probe missed the daemon (#670), so it is identified now, on its own context since recovery
	// may have spent runCtx; a no-op unless the startup probe came back empty.
	p.reprobeEngine(context.Background())

	if notReady {
		// Budget exhausted with no daemon: every recovered endpoint now lacks renewal.
		log.Error("recovery: daemon never became reachable; endpoints are running without a renewal client")
		p.recoveryFailed.Add(1)
	}
}

// containerGone reports whether containerID was removed or stopped, via a direct inspect: recovery has the ID and
// no sandbox key, unlike sandboxGone. An inspect error other than "no such container" returns false (#376).
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
	return ctr.State == nil || !ctr.State.Running
}

// recoveredHostname returns the hostname for a recovered fingerprint; ok=false, for no inspect answer or a refused
// hostname (#693), records no fingerprint, since an empty hostname would write a wildcard tombstone (#726).
func (p *Plugin) recoveredHostname(ctx context.Context, containerID string) (dhcpHostname, bool) {
	if containerID == "" {
		p.recoveryFingerprintsSkipped.Add(1)
		return dhcpHostname{}, false
	}
	// The CreateEndpoint budget, not a tighter one: dockerd blocks ContainerInspect for a container inside
	// ContainerStart while answering other calls (#406), and recovery runs while the daemon restarts every container,
	// so a tighter budget would expire exactly where #721 applies.
	ctx, cancel := context.WithTimeout(ctx, initialDHCPHostnameLookupTimeout)
	defer cancel()

	ctr, err := p.docker.ContainerInspect(ctx, containerID)
	if err != nil || ctr.Config == nil || ctr.Config.Hostname == "" {
		// Counted, since this endpoint loses its address on the next restart (#721).
		p.recoveryFingerprintsSkipped.Add(1)
		return dhcpHostname{}, false
	}
	// safeHostname counts a refusal itself (unsafeHostnamesRejected).
	h := p.safeHostname(ctr.Config.Hostname)
	return h, h.trusted()
}

// recoveredMAC returns the MAC recovery runs the endpoint under. Docker reports none for ipvlan, whose slaves take
// the parent's MAC and refuse a change with EOPNOTSUPP, so it is read from the parent; measured on the lane
// 2026-09-06, every ipvlan endpoint failed recovery with `parse MAC "": invalid MAC address` (#911). A macvlan
// passthru child also wears the parent's MAC and reports none to Docker (#905).
func recoveredMAC(opts DHCPNetworkOptions, macStr string) (net.HardwareAddr, error) {
	if macStr != "" {
		mac, err := net.ParseMAC(macStr)
		if err != nil {
			return nil, fmt.Errorf("parse MAC %q: %w", macStr, err)
		}
		return mac, nil
	}
	if !opts.childWearsParentMAC() {
		return nil, fmt.Errorf("parse MAC %q: %w", macStr, errNoRecoveryMAC)
	}
	parent, err := nlLinkByName(opts.linkParent())
	if err != nil {
		return nil, fmt.Errorf("%s parent %q: %w", opts.effectiveMode(), opts.linkParent(), err)
	}
	hw := parent.Attrs().HardwareAddr
	if len(hw) == 0 {
		return nil, fmt.Errorf("%s parent %q has no hardware address to inherit", opts.effectiveMode(), opts.linkParent())
	}
	return hw, nil
}

// errNoRecoveryMAC is the empty-MAC refusal for modes with their own MAC.
var errNoRecoveryMAC = errors.New("invalid MAC address")

// recoverOneEndpoint builds a manager for one existing endpoint and starts it, returning adopted=false when a
// manager already exists (#480); containerID lets a Start failure recognise an exited container (#376).
func (p *Plugin) recoverOneEndpoint(ctx context.Context, containerID, networkID, endpointID, macStr, ipv4Cidr, ipv6Cidr string, opts DHCPNetworkOptions) (adopted bool, err error) {
	// Before the MAC parse: an already-managed endpoint must not count recovery_failed; the compare-and-set below
	// closes the race (#480).
	if p.dhcpManagerExists(endpointID) {
		p.recoveryAlreadyManaged.Add(1)
		return false, nil
	}

	mac, err := recoveredMAC(opts, macStr)
	if err != nil {
		return false, err
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
	// Checked and registered in one operation, so a mid-recovery Join keeps its manager (#480).
	if !p.registerDHCPManagerIfAbsent(endpointID, m) {
		p.recoveryAlreadyManaged.Add(1)
		return false, nil
	}

	// Recovery records the fingerprint, or DeleteEndpoint lays no tombstone and the next `docker restart` loses the
	// address (#721); after the compare-and-set, so a winning Join's fingerprint stands. Ifname stays empty, as Docker
	// does not record the custom name (#125).
	if hostname, ok := p.recoveredHostname(ctx, containerID); ok {
		fpIPv4, fpIPv6 := "", ""
		if ipv4 != nil {
			fpIPv4 = ipv4.IP.String()
		}
		if ipv6 != nil {
			fpIPv6 = ipv6.IP.String()
		}
		// Only an accepted hostname reaches here; a refusal writes no fingerprint (#726).
		p.rememberEndpoint(endpointID, endpointFingerprint{
			// Docker's MAC, not the resolved one: a tombstone must not offer an address filed under the ipvlan
			// parent's MAC (#911).
			MAC:  macStr,
			IPv4: fpIPv4,
			IPv6: fpIPv6,
		}, hostname)
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
			// An exited container is not recovery_failed, which flips healthy (#376, #373). Checked after Start
			// fails, on a fresh context since startCtx may be expired.
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
			// Identity-checked, so a displacing Join's manager is not evicted (#480).
			p.removeDHCPManagerIfSame(endpointID, m)
			return
		}
		p.recoveredOK.Add(1)
	}()
	return true, nil
}

// lookupEndpointMAC reads Docker's stored MAC for an endpoint so a restart rebuilds the link with that MAC.
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

// reacquireEndpoint reruns CreateEndpoint for a Join with no hint, as on `docker restart`; ipvlan and passthru get
// no MAC, since their child wears the parent's (#905).
func (p *Plugin) reacquireEndpoint(ctx context.Context, r JoinRequest, opts DHCPNetworkOptions) error {
	macAddr := ""
	if !opts.childWearsParentMAC() {
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
		replay:     true,
	}
	if _, err := p.CreateEndpoint(ctx, fakeReq); err != nil {
		return fmt.Errorf("CreateEndpoint replay failed: %w", err)
	}
	return nil
}

// initialDHCPHostname polls up to initialDHCPHostnameLookupTimeout for the container's hostname for the first
// DISCOVER; Docker may call CreateEndpoint before the container is listed. refused separates a refused name from
// an absent one, which tombstone matching treats as a wildcard (#726).
func (p *Plugin) initialDHCPHostname(ctx context.Context, networkID, endpointID string) dhcpHostname {
	ctx, cancel := context.WithTimeout(ctx, initialDHCPHostnameLookupTimeout)
	defer cancel()

	// Each Docker call is capped at the poll interval, since the client's own 2 s timeout equals the whole budget.
	const dockerCallTimeout = 200 * time.Millisecond

	// The zero value is the honest unknown and keeps the network-wide tombstone match; only safeHostname sets refused.
	var hostname dhcpHostname
	_ = util.AwaitCondition(ctx, func() (bool, error) {
		inner, innerCancel := context.WithTimeout(ctx, dockerCallTimeout)
		defer innerCancel()
		dockerNet, err := p.docker.NetworkInspect(inner, networkID, dNetwork.InspectOptions{})
		if err != nil {
			return false, nil
		}
		for ctrID, info := range dockerNet.Containers {
			if info.EndpointID != endpointID {
				continue
			}
			// Docker uses an "ep-<endpointID>" placeholder until the real container ID is bound.
			if strings.HasPrefix(ctrID, "ep-") {
				return false, nil
			}
			ctr, err := p.docker.ContainerInspect(inner, ctrID)
			if err != nil {
				return false, nil
			}
			hostname = p.safeHostname(ctr.Config.Hostname)
			return true, nil
		}
		return false, nil
	}, 100*time.Millisecond)
	return hostname
}

// NewPlugin creates a Plugin, taking documented defaults for zero Options fields.
func NewPlugin(opts Options) (*Plugin, error) {
	warnIfStateDirIsNotThePersistentOne()
	if opts.AwaitTimeout <= 0 {
		opts.AwaitTimeout = defaultAwaitTimeout
	}
	p := Plugin{
		awaitTimeout: opts.AwaitTimeout,
		startTime:    time.Now(),
		instanceID:   newInstanceID(),

		joinHints:            make(map[string]joinHint),
		persistentDHCP:       make(map[string]*dhcpManager),
		endpointFingerprints: make(map[string]endpointFingerprint),

		ipamPools:    newIssuedPools(),
		ipamIndex:    newIPAMIndex(),
		ipamReserves: newIPAMReserves(),
	}

	// The Docker client is built after p, since the GET-only transport counts refusals on p (#691).
	client, err := newDockerClient(dockerHostFromEnv(os.Getenv), &p)
	if err != nil {
		return nil, err
	}
	p.docker = client

	// The engine floor check runs before anything creates files on the host (#670).
	if err := p.probeEngine(context.Background()); err != nil {
		return nil, err
	}

	// prepareStateDir creates the directory and runs the mode sweep before anything opens an older file (#804).
	dir, err := prepareStateDir(&p.stateFileChmodFailures)
	if err != nil {
		return nil, err
	}
	p.ledger = newLeaseLedger(filepath.Join(dir, ledgerFileName), &p.ledgerWriteFailures)

	// Opened before recovery, fatal on failure: a second process holding the lock mid-upgrade would interleave
	// sequence numbers, and each would drop the other's events as stale (#899).
	records, err := dhcp.OpenRecords(filepath.Join(dir, recordFileName), p.instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to open the lease record: %w", err)
	}
	p.records = records
	if d := records.Damage(); d.TornTail > 0 || d.Skipped > 0 {
		log.WithFields(log.Fields{"torn_tail": d.TornTail, "skipped": d.Skipped}).
			Warn("The lease record has unreadable lines; endpoints they described will be recovered from Docker instead of resumed")
	}

	// Before the socket listens: libnetwork.New replays RequestPool and RequestAddress before serving its API (#110).
	rebuildIPAMIndex(p.ipamIndex)
	retainOrphanedReservations(p.records, time.Now())

	mux := p.newServeMux()

	// Capture sits inside access logging and outside the mux, a no-op with no capture directory (#644); limitBody is
	// outermost so the cap applies before either reads. See http_limits.go.
	p.server = http.Server{
		Handler:           limitBody(handlers.CustomLoggingHandler(nil, captureHandler(mux, opts.RequestCaptureDir, capturablePaths(p.routes())), util.WriteAccessLog)),
		ReadHeaderTimeout: socketReadHeaderTimeout,
		ReadTimeout:       socketReadTimeout,
		WriteTimeout:      socketWriteTimeout,
		IdleTimeout:       socketIdleTimeout,
	}

	// No orphan sweep: the DHCP client is an in-process goroutine; a lease left unrenewed is resumed via the record.

	// Recovery runs before NewPlugin returns, so a CreateEndpoint cannot race recovery's Start; recoveryBudget bounds
	// enable latency. A daemon not serving yet defers recovery to Listen (#383).
	{
		ctx, cancel := context.WithTimeout(context.Background(), recoveryBudget)
		p.recoveryPending = p.recoverEndpoints(ctx, recoverySyncDaemonWait)
		cancel()
	}

	// Re-probe the engine if it came up after the probe above; a no-op otherwise (#670).
	p.reprobeEngine(context.Background())
	// The record sweeper starts last, after recovery adopted running containers: it drops unclaimed IPAM reservations
	// and runs `release_lease=on_remove`'s deferred releases (#984).
	p.recordSweepStop = make(chan struct{})
	go p.recordSweeper(p.recordSweepStop)

	return &p, nil
}

// Listen starts the plugin server
func (p *Plugin) Listen(bindSock string) error {
	// Remove a stale socket from a prior run; production runtimes recreate the workdir, so this matters for tests.
	_ = os.Remove(bindSock)

	l, err := net.Listen("unix", bindSock)
	if err != nil {
		return err
	}

	// A UNIX socket's mode is 0777 &^ umask; pin it owner-only, as SECURITY.md's case for serving /metrics here rests
	// on a root-only socket and the daemon connects as root (#687).
	if err := os.Chmod(bindSock, 0o600); err != nil {
		// An unknown socket mode is the state the chmod guards against, so refuse to serve.
		l.Close()
		return fmt.Errorf("restricting the plugin socket to the owner: %w", err)
	}

	// Deferred recovery starts once the socket exists, so the daemon it waits on can reach the plugin (#383).
	if p.recoveryPending {
		p.recoveryPending = false
		ctx, cancel := context.WithCancel(context.Background())
		p.recoveryCancel = cancel
		go p.recoverEndpointsDeferred(ctx, recoveryDeferredDaemonWait)
	}

	return p.server.Serve(l)
}

// pluginShutdownTimeout is one budget for the whole shutdown: HTTP grace, client stop fan-out and displaced-manager
// drain (#338). A total, since per-phase caps multiply the wait on `docker plugin disable`; a var for tests.
var pluginShutdownTimeout = 5 * time.Second

// waitBounded waits for wg up to d and reports whether it completed. On timeout the watcher goroutine leaks until
// process exit, which suits Close and no long-lived caller (#338).
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

// ListenMetrics starts the /metrics TCP listener, off unless METRICS_ADDR is set because the plugin runs in the
// host network namespace with CAP_NET_ADMIN and CAP_SYS_ADMIN (#651).
func (p *Plugin) ListenMetrics(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", p.apiMetrics)

	warnOnWildcardMetricsBind(addr)

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics listener on %q: %w", addr, err)
	}

	p.metricsListener = l
	// A scrape renders one snapshot, so unlike the plugin socket this server can carry a write timeout; see http_limits.go.
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

// Close shuts the HTTP server first, so no Join registers a manager mid-stop, then stops every DHCP client without
// releasing, as the containers still run and a release would invite a duplicate assignment (#800, #962).
func (p *Plugin) Close() error {
	// The deferred-recovery retry stops first: a manager it registered after the drain would never be stopped (#383).
	if p.recoveryCancel != nil {
		p.recoveryCancel()
	}
	if p.recordSweepStop != nil {
		close(p.recordSweepStop)
		p.recordSweepStop = nil
	}

	deadline := time.Now().Add(pluginShutdownTimeout)
	remaining := func() time.Duration {
		if d := time.Until(deadline); d > 0 {
			return d
		}
		return 0
	}

	// Shutdown, unlike Close, waits for handlers to return; registerDHCPManager runs inside the Join handler, so after
	// it the registry is final and one drain suffices (#338).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), remaining())
	defer cancel()

	// forced means a handler outlived the grace, so the one-drain guarantee does not hold and a second pass runs below.
	// The metrics listener holds no state and closes first, so no scrape reads a registry being emptied (#338).
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

	// stopSnapshot stops every registered manager in parallel, outside p.mu, and returns how many it stopped.
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
		if !waitBounded(&wg, remaining()) {
			log.Warn("Timeout waiting for persistent DHCP clients to stop; continuing shutdown")
		}
		return len(managers)
	}

	stopSnapshot()
	if forced {
		// Degraded path: a handler in flight at the grace deadline may have registered after the snapshot (#338).
		if n := stopSnapshot(); n > 0 {
			log.WithField("count", n).Info("Stopped late-registered DHCP clients in forced-shutdown sweep")
		}
	}

	// Drain displaced-manager stops spawned by Join; process exit would otherwise cut them short and leave a client
	// renewing (#338).
	if !waitBounded(&p.displacedStops, remaining()) {
		log.Warn("Timeout waiting for displaced DHCP manager stops; continuing shutdown")
	}

	if err := p.docker.Close(); err != nil {
		return fmt.Errorf("failed to close docker client: %w", err)
	}

	if serverErr != nil {
		return fmt.Errorf("failed to close http server: %w", serverErr)
	}

	return nil
}

// safeHostname returns h when it can go on the wire as option 12 unchanged, else ("", false) (#692). The name is
// container-chosen, Docker does not validate it and the library sends it verbatim, so this is the only guard; a
// drop is counted and the endpoint proceeds, since the hostname only decorates the exchange.
// refused keeps a refusal apart from an absence: tombstoneStore.consume treats "" as a network-wide match, so a
// bare "" let one control character in --hostname inherit another endpoint's MAC and address (#726).
func (p *Plugin) safeHostname(h string) dhcpHostname {
	if dhcp.SafeValue(h) {
		return dhcpHostname{name: h}
	}
	p.unsafeHostnamesRejected.Add(1)
	log.WithField("hostname", fmt.Sprintf("%q", h)).
		Warn("Dropping container hostname: it carries a control character and will not be sent as the DHCP hostname option")
	return dhcpHostname{refused: true}
}
