package dhcp

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// dhcpcd client identity + config/argv generation (#152).
//
// busybox udhcpc6 filled the DHCPv6 IAID with rand() per process and
// offered no override, so the CreateEndpoint one-shot (host netns) and
// the persistent client (container netns) landed in different identity
// associations and the server handed out two addresses — only one of
// which (the one Docker was told) was ever used, and nothing renewed
// it. dhcpcd lets us PIN both the DUID and the IAID, so both clients
// present an identical (DUID, IAID) and the server returns one binding.
//
// Both identifiers are derived purely from the endpoint's (pinned) MAC.
// The MAC is identical across the one-shot/persistent eras and stable
// across plugin restarts, so the derived DUID/IAID are reproducible
// without depending on dhcpcd's persisted state file (which is anyway
// shared host-wide and unusable for per-endpoint identity — see the
// mount-namespace isolation in the client runtime).

const dhcpcdBin = "dhcpcd"

// EventFIFOEnv is the environment variable, pushed to dhcpcd's hook via
// the `env` config directive, that tells the handler where to write its
// JSON events. dhcpcd's hook stdout is unusable as a data channel
// (/dev/null once daemonised, interleaved with dhcpcd's log in
// foreground), so the parent opens a FIFO and passes its path here.
const EventFIFOEnv = "NETDHCP_EVENT_FIFO"

// duidLL renders a DUID-LL (RFC 8415 §11.4) for mac in the colon-hex
// "value" form dhcpcd's `duid` directive accepts (dhcpcd.conf(5): "If
// not ll, lt or uuid then value will be converted from 00:11:22:33
// format"). Layout: 2-byte DUID type (0x0003 = link-layer) + 2-byte
// hardware type (0x0001 = Ethernet) + the link-layer address.
//
// We emit the literal value rather than the `duid ll` keyword because a
// keyword is overridden by any pre-existing /var/lib/dhcpcd/duid, while
// a literal value is honoured — and it must be honoured identically by
// the one-shot and persistent clients for the IA to unify.
func duidLL(mac net.HardwareAddr) string {
	parts := make([]string, 0, len(mac)+4)
	parts = append(parts, "00", "03", "00", "01") // type=3 (LL), hwtype=1 (Ethernet)
	for _, b := range mac {
		parts = append(parts, fmt.Sprintf("%02x", b))
	}
	return strings.Join(parts, ":")
}

// iaidFromMAC derives a stable 4-byte IAID from the low 4 bytes of mac,
// rendered as the decimal number dhcpcd's `iaid` directive parses into
// a uint32. Deterministic in the MAC, so the one-shot and persistent
// clients compute the same IAID.
func iaidFromMAC(mac net.HardwareAddr) string {
	b := mac
	if len(b) >= 4 {
		b = b[len(b)-4:]
	}
	// Right-align into 4 bytes for MACs shorter than 4 (defensive; real
	// Ethernet MACs are 6 bytes).
	var buf [4]byte
	copy(buf[4-len(b):], b)
	return strconv.FormatUint(uint64(binary.BigEndian.Uint32(buf[:])), 10)
}

// formatClientID renders a raw option-61 payload as the colon-hex
// string dhcpcd's `clientid` directive sends verbatim. We prepend the
// type byte 0x00 (RFC 2132 "opaque") to match exactly what the busybox
// path put on the wire, so any server reservation keyed on the prior
// client-id keeps matching after the migration.
func formatClientID(id []byte) string {
	parts := make([]string, 0, len(id)+1)
	parts = append(parts, "00") // type 0 = opaque, no DUID
	for _, b := range id {
		parts = append(parts, fmt.Sprintf("%02x", b))
	}
	return strings.Join(parts, ":")
}

// dhcpcdParams is the per-endpoint, per-family input to the dhcpcd
// config and argv generators. The client runtime derives it from the
// endpoint MAC and the caller's DHCPClientOptions.
type dhcpcdParams struct {
	Iface string
	MAC   net.HardwareAddr
	V6    bool
	Once  bool // one-shot acquisition (CreateEndpoint) vs persistent daemon

	Hostname string // hostname directive; "" omits
	FQDN     string // fqdn directive mode (e.g. "both"); "" omits. Sends
	//            the DHCP FQDN option (81 v4 / 39 v6) using Hostname, asking
	//            the server to register it in DNS (#261).
	VendorClass string // v4 option 60; "" omits (v4 only)
	ClientID    []byte // v4 option 61 raw payload; nil/empty omits (v4 only)
	RequestedIP string // v4 preferred address (request directive); "" omits
	PreferredV6 string // v6 IA_NA preferred address; "" omits
	Broadcast   bool   // request a broadcast reply (v4 only; ipvlan-L2 shared MAC)

	Handler    string // hook script path (-c)
	ConfigPath string // where the rendered config will be written (-f)
	EventFIFO  string // FIFO the handler writes events to (env directive); "" omits
	CoverDir   string // GOCOVERDIR to forward to the hook (cover build only); "" omits
}

// renderConfig produces the dhcpcd.conf text for p. Only directives
// confirmed against dhcpcd.conf(5) are emitted: duid, nohook, release,
// option, hostname, vendorclassid, clientid, interface, iaid, request,
// ia_na.
//
// dhcpcd runs observe-only (--noconfigure) so it never touches the
// link; the nohook lines are belt-and-braces in case --noconfigure is
// ever dropped. The interface block pins the IAID (and, for v6, the
// IA_NA — optionally with a preferred address); the v4 preferred
// address rides the `request` directive (the dhcpcd equivalent of the
// old busybox `-r`).
//
// The persistent client emits `release` so a graceful stop (Leave /
// daemon shutdown sends SIGTERM) sends a DHCPRELEASE, freeing the lease
// — the busybox `-R` behaviour the docker-restart / daemon-restart
// IP-stability tests depend on. Without it, the server keeps the old
// lease (keyed on the now-stale endpoint-derived client-id) and hands
// the post-restart endpoint a different address. The one-shot client
// must NOT release: it exits with `-1 -p` precisely to KEEP the lease
// so the persistent client can re-claim the same address moments later.
func renderConfig(p dhcpcdParams) string {
	iaid := iaidFromMAC(p.MAC)

	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by docker-net-dhcp for endpoint interface %s (#152).\n", p.Iface)
	fmt.Fprintf(&b, "# dhcpcd is observe-only (--noconfigure); the plugin applies all\n")
	fmt.Fprintf(&b, "# interface state via netlink.\n")

	// Pinned identity (the core of the IA unification).
	fmt.Fprintf(&b, "duid %s\n", duidLL(p.MAC))

	// Tell the hook where to deliver events (dhcpcd scrubs the
	// environment, so this rides the `env` directive rather than the
	// process environment).
	if p.EventFIFO != "" {
		fmt.Fprintf(&b, "env %s=%s\n", EventFIFOEnv, p.EventFIFO)
	}

	// Forward GOCOVERDIR to the hook in the coverage-instrumented build
	// so cmd/dhcp-handler's `-cover` counters are written and merged.
	// dhcpcd scrubs the environment, so — like the FIFO above — it has to
	// ride the `env` directive; otherwise the handler (a separate process
	// dhcpcd execs per event) loses GOCOVERDIR and emits nothing, and the
	// package drops out of the coverage ratchet entirely. Empty in
	// production (GOCOVERDIR unset), so this is a no-op there.
	if p.CoverDir != "" {
		fmt.Fprintf(&b, "env GOCOVERDIR=%s\n", p.CoverDir)
	}

	// Keep dhcpcd off host/system files.
	for _, h := range []string{"resolv.conf", "hostname", "ntp.conf", "yp.conf"} {
		fmt.Fprintf(&b, "nohook %s\n", h)
	}

	// WPAD (option 252) is non-standard, so dhcpcd has no built-in name
	// for it — define one (as a string) before requesting it below.
	// dhcpcd 10.x does not pre-define 252, so this never conflicts
	// (verified against the image's dhcpcd; bare `option wpad` is rejected
	// without this). #262.
	fmt.Fprintf(&b, "define 252 string wpad\n")

	// Explicitly request the options the plugin propagates. Passing
	// `-f <config>` bypasses the distro /etc/dhcpcd.conf, so dhcpcd would
	// otherwise fall back to a minimal built-in request set and never
	// learn the MTU / DNS / domain-search / NTP / TFTP values — the
	// busybox client requested these via `-O`. dhcpcd maps these names to
	// the right per-protocol option codes (e.g. domain_name_servers ->
	// option 6 on v4 and option 23 on v6), so one list serves both
	// families; options that don't apply to the active protocol are
	// ignored. Routers/subnet/classless-static-routes are in dhcpcd's
	// defaults but are listed for explicitness and to be robust to a
	// default change. posix_timezone/tzdb_timezone/time_offset/wpad
	// (options 100/101/2/252) are observe-only informational extras
	// surfaced in logs (#262) — these are dhcpcd's actual names for
	// those codes (NOT pcode/tcode, which dhcpcd rejects).
	fmt.Fprintf(&b, "option %s\n", strings.Join([]string{
		"subnet_mask",
		"broadcast_address",
		"routers",
		"domain_name_servers",
		"domain_name",
		"host_name",
		"domain_search",
		"interface_mtu",
		"ntp_servers",
		"tftp_server_name",
		"bootfile_name",
		"classless_static_routes",
		"time_offset",
		"posix_timezone",
		"tzdb_timezone",
		"wpad",
	}, ", "))

	// Persistent client only: release the lease on graceful stop (busybox
	// `-R`). The one-shot acquisition deliberately keeps its lease (-1 -p).
	if !p.Once {
		fmt.Fprintf(&b, "release\n")
	}

	// ipvlan-L2 slaves all share the parent NIC's MAC, so a unicast
	// OFFER/ACK addressed to that MAC during initial acquisition can't be
	// demuxed to the right slave (the slave's IP isn't on the link yet).
	// The `broadcast` directive sets the DHCP BROADCAST flag so the server
	// replies via L2 broadcast — the dhcpcd equivalent of busybox `-B`
	// (#243). v4-only: the flag is a DHCPv4 concept. dhcpcd only auto-sets
	// it for non-Ethernet links, so ipvlan needs it forced.
	if p.Broadcast && !p.V6 {
		fmt.Fprintf(&b, "broadcast\n")
	}

	if p.Hostname != "" {
		fmt.Fprintf(&b, "hostname %s\n", p.Hostname)
	}
	// FQDN option (81 v4 / 39 v6): opt-in dynamic-DNS registration (#261).
	// dhcpcd sends the FQDN built from the hostname directive above and,
	// per RFC 4702, sends it *instead of* option 12 — same name, plus the
	// server-update request. Applies to both families, so it sits outside
	// the v4-only block below. "both" asks the server to update forward
	// (A/AAAA) and reverse (PTR); the client runs no DNS updater of its own.
	if p.FQDN != "" {
		fmt.Fprintf(&b, "fqdn %s\n", p.FQDN)
	}
	// vendorclassid / clientid are DHCPv4 concepts (option 60 / 61);
	// the v6 identity is carried entirely by DUID + IAID.
	if !p.V6 {
		if p.VendorClass != "" {
			fmt.Fprintf(&b, "vendorclassid %s\n", p.VendorClass)
		}
		if len(p.ClientID) > 0 {
			fmt.Fprintf(&b, "clientid %s\n", formatClientID(p.ClientID))
		}
	}

	fmt.Fprintf(&b, "interface %s\n", p.Iface)
	fmt.Fprintf(&b, "    iaid %s\n", iaid)
	if !p.V6 {
		if p.RequestedIP != "" {
			fmt.Fprintf(&b, "    request %s\n", p.RequestedIP)
		}
	} else if p.PreferredV6 != "" {
		// Request our pinned IAID's IA_NA with a preferred address; the
		// iaid defaults to the directive above, but we name it
		// explicitly for clarity.
		fmt.Fprintf(&b, "    ia_na %s / %s\n", iaid, p.PreferredV6)
	}

	return b.String()
}

// renderArgs produces the dhcpcd argv for p. All flags are confirmed
// against dhcpcd(8):
//
//	-B           foreground (the Go process owns the lifecycle)
//	--noconfigure observe-only (plugin owns interface config)
//	-L           no IPv4LL/APIPA fallback
//	-A           no ARP claim/conflict-detection on the offered address.
//	             dhcpcd's RFC 5227 ACD adds ~5s between offer and lease,
//	             which busybox udhcpc never did and which pushed the
//	             one-shot CreateEndpoint acquisition over its lease
//	             deadline. The DHCP server is authoritative for allocation
//	             and the plugin runs its own preflight probe, so the
//	             client-side ARP claim is redundant latency. (v4-only flag;
//	             a no-op under -6.)
//	-c <handler> hook script (emits events to the parent FIFO)
//	-f <config>  the rendered per-endpoint config
//	-1           one-shot: exit after the first lease (acquisition only)
//	-4 / -6      restrict to one family (one process per family, mirroring
//	             the existing v4/v6 dual-channel structure)
//	<iface>      positional interface name
func renderArgs(p dhcpcdParams) []string {
	args := []string{
		dhcpcdBin,
		"-B",
		"--noconfigure",
		"-L",
		"-A",
		"-c", p.Handler,
		"-f", p.ConfigPath,
	}
	if p.Once {
		// One-shot acquisition (CreateEndpoint): exit after the first
		// lease, and -p (persistent) so the binding is NOT released on
		// that exit — the persistent client claims the same address
		// moments later. The persistent client omits -p so it releases
		// the lease when the plugin stops it (the old busybox -R
		// behaviour).
		args = append(args, "-1", "-p")
	}
	if p.V6 {
		args = append(args, "-6")
	} else {
		args = append(args, "-4")
	}
	args = append(args, p.Iface)
	return args
}
