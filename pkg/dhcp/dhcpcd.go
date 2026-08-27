// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
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

// dhcpcdBin is the ABSOLUTE path to dhcpcd(8) in the plugin rootfs.
//
// It reaches execve as `$0` of `unshare -m /bin/sh -c '… exec "$0" "$@"'`,
// and sh resolves a bare name through PATH exactly as exec.Command's
// LookPath does. So while #707 pinned unshare, it also recorded that
// unshare "was the one binary in the tree whose identity depended on the
// environment" — and that was not true when it was written. This one was
// the other, sitting one argv position away, hidden by being a shell's
// PATH lookup rather than Go's (#707).
//
// The path is Alpine's, matching the pinned base image, and it is
// measured rather than assumed: on
// alpine:3.24.1@sha256:28bd5fe8… with dhcpcd=10.3.2-r0 installed,
// `command -v dhcpcd` is /sbin/dhcpcd and /usr/sbin/dhcpcd does not
// exist. dhcpcd does not vary its behaviour on argv[0]; the absolute
// and bare forms produce identical output, checked under the same
// `sh -c 'exec "$0" "$@"'` wrapper this code uses.
//
// The Dockerfile asserts the binary is there at build time, and
// TestDockerfileGuaranteesEveryAbsoluteBinary asserts the two name the same path, so
// an Alpine relocation fails the build instead of failing a container's
// first lease with "not found".
const dhcpcdBin = "/sbin/dhcpcd"

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

// SafeDirectiveValue reports whether s can be interpolated into a dhcpcd
// config directive without changing the FILE'S STRUCTURE.
//
// Exported because pkg/plugin applies the same rule one step earlier, so
// the drop can reach a health counter (see Plugin.safeHostname).
//
// renderConfig writes one directive per line as "<keyword> <value>", so a
// value carrying a newline does not produce a malformed directive — it
// produces an ADDITIONAL, attacker-chosen one, and dhcpcd applies it. That
// matters most for the values the plugin does not originate: the hostname
// is the container's own (Docker performs no validation on it, verified),
// and the vendor class and server lists come from network options.
//
// dhcpcd resolves a repeated directive last-wins, and renderConfig writes
// `duid` near the top while the hostname lands near the bottom, so an
// injected `duid` overrides the identity this plugin pinned — the DUID is
// derived from the endpoint MAC, and every endpoint's MAC is observable on
// a shared L2 segment. The same trick voids a `blacklist` written from
// dhcp_deny_servers, because dhcpcd stops consulting a blacklist once a
// whitelist exists.
//
// The rule is deliberately about control characters rather than about
// well-formedness: an over-strict check here would reject hostnames Docker
// and real deployments accept (underscores, for one) and break containers
// that are doing nothing wrong. Structure is what must be protected;
// anything dhcpcd itself dislikes about a flat token is dhcpcd's to
// complain about.
//
// This is the config-file sibling of ValidIfaceName, which guards the same
// class of problem on the argv side.
func SafeDirectiveValue(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
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

	// AllowServers restricts which DHCPv4 servers may be accepted, as
	// dhcpcd `whitelist` entries (IPv4 only — dhcp6.c never consults
	// them). Empty imposes no restriction. Set from the network's
	// dhcp_servers preference list (#111).
	AllowServers []string
	// DenyServers rejects specific DHCPv4 servers, as dhcpcd
	// `blacklist` entries (#669). The caller must leave this empty
	// whenever AllowServers is set: dhcpcd ignores a blacklist once a
	// whitelist exists (src/dhcp.c:3181-3196), so emitting both would
	// advertise a denial that is not enforced. serverPolicy.denyList
	// is what guarantees this.
	DenyServers []string

	Handler    string // hook script path (-c)
	ConfigPath string // where the rendered config will be written (-f)
	EventFIFO  string // FIFO the handler writes events to (env directive); "" omits
	CoverDir   string // GOCOVERDIR to forward to the hook (cover build only); "" omits
}

// directive writes one "<keyword> <value>" line, and DROPS it entirely
// when the value would change the file's structure (see
// SafeDirectiveValue). Dropping rather than escaping is deliberate:
// dhcpcd has no quoting for directive values, so there is nothing to
// escape into — the only two options are "omit" and "let the caller
// append directives".
//
// Every interpolated, non-constant value in renderConfig goes through
// here. That is the structural guarantee
// TestRenderConfig_NoValueCanIntroduceADirective pins: it walks the
// string fields, feeds each an embedded directive, and fails if one
// reaches the output. A future field added to the renderer without using
// this helper fails that test rather than reopening the hole quietly.
//
// A drop is COUNTED as well as logged (#780). The operator set an
// option and it never reaches the DHCP server; a warning in a log
// nobody reads on a healthy plugin is not a way for them to find that
// out. See directivesRefused for why the counter is package-level.
func directive(b *configBuilder, keyword, value string) {
	if !SafeDirectiveValue(value) {
		directivesRefused.Add(1)
		log.WithFields(log.Fields{"directive": keyword, "value": fmt.Sprintf("%q", value)}).
			Warn("Refusing to write dhcpcd directive with a control character in its value")
		return
	}
	fmt.Fprintf(b, "%s %s\n", keyword, value)
}

// configBuilder is the config text under construction.
//
// It embeds strings.Builder rather than wrapping it so *configBuilder
// still satisfies io.Writer through the promoted Write method: every
// fmt.Fprintf(&b, ...) in renderConfig — the constant lines that carry
// no operator input and so need no refusal check — compiles unchanged,
// and only the values that go through directive() are subject to it.
//
// A distinct type rather than a bare strings.Builder so that a directive
// cannot be written to some other builder that has no refusal handling.
type configBuilder struct {
	strings.Builder
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

	var b configBuilder
	// %q, not %s: this is the one line that interpolates a value without
	// going through directive(), and a comment is just as capable of
	// carrying a newline into the file as a directive is. ValidIfaceName
	// already makes that unreachable from production; quoting makes it
	// unreachable from a hand-built dhcpcdParams too.
	fmt.Fprintf(&b, "# Generated by docker-net-dhcp for endpoint interface %q (#152).\n", p.Iface)
	fmt.Fprintf(&b, "# dhcpcd is observe-only (--noconfigure); the plugin applies all\n")
	fmt.Fprintf(&b, "# interface state via netlink.\n")

	// Pinned identity (the core of the IA unification).
	directive(&b, "duid", duidLL(p.MAC))

	// Tell the hook where to deliver events (dhcpcd scrubs the
	// environment, so this rides the `env` directive rather than the
	// process environment).
	if p.EventFIFO != "" {
		directive(&b, "env", EventFIFOEnv+"="+p.EventFIFO)
	}

	// Forward GOCOVERDIR to the hook in the coverage-instrumented build
	// so cmd/dhcp-handler's `-cover` counters are written and merged.
	// dhcpcd scrubs the environment, so — like the FIFO above — it has to
	// ride the `env` directive; otherwise the handler (a separate process
	// dhcpcd execs per event) loses GOCOVERDIR and emits nothing, and the
	// package drops out of the coverage ratchet entirely. Empty in
	// production (GOCOVERDIR unset), so this is a no-op there.
	if p.CoverDir != "" {
		directive(&b, "env", "GOCOVERDIR="+p.CoverDir)
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

	// Server preference / denial (#111, #669). These match the packet's
	// IP SOURCE address, not the Server Identifier it advertises
	// (dhcpcd 10.3.2 src/dhcp.c:3641 takes `from` from ip->ip_src, and
	// :3181/:3190 test that), so behind a DHCP relay every offer looks
	// like it came from the relay and neither list can tell servers
	// apart. v4 only: dhcp6.c never reads them.
	// The if/else is a GUARD, not a shortcut. dhcpcd stops consulting the
	// blacklist entirely once a whitelist exists, so emitting both would
	// write a directive that is never read — a config file claiming a
	// denial the client does not enforce. Callers are supposed to prevent
	// this (serverPolicy.denyList returns nil when a preference is set),
	// but a caller assembling dhcpcdParams by hand would not, and a
	// comment asking them not to would decay silently.
	if len(p.AllowServers) > 0 {
		for _, srv := range p.AllowServers {
			directive(&b, "whitelist", srv)
		}
	} else {
		for _, srv := range p.DenyServers {
			directive(&b, "blacklist", srv)
		}
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
		directive(&b, "hostname", p.Hostname)
	}
	// FQDN option (81 v4 / 39 v6): opt-in dynamic-DNS registration (#261).
	// dhcpcd sends the FQDN built from the hostname directive above and,
	// per RFC 4702, sends it *instead of* option 12 — same name, plus the
	// server-update request. Applies to both families, so it sits outside
	// the v4-only block below. "both" asks the server to update forward
	// (A/AAAA) and reverse (PTR); the client runs no DNS updater of its own.
	if p.FQDN != "" {
		directive(&b, "fqdn", p.FQDN)
	}
	// vendorclassid / clientid are DHCPv4 concepts (option 60 / 61);
	// the v6 identity is carried entirely by DUID + IAID.
	if !p.V6 {
		if p.VendorClass != "" {
			directive(&b, "vendorclassid", p.VendorClass)
		}
		if len(p.ClientID) > 0 {
			directive(&b, "clientid", formatClientID(p.ClientID))
		}
	}

	directive(&b, "interface", p.Iface)
	directive(&b, "    iaid", iaid)
	if !p.V6 {
		if p.RequestedIP != "" {
			directive(&b, "    request", p.RequestedIP)
		}
	} else if p.PreferredV6 != "" {
		// Request our pinned IAID's IA_NA with a preferred address; the
		// iaid defaults to the directive above, but we name it
		// explicitly for clarity.
		directive(&b, "    ia_na", iaid+" / "+p.PreferredV6)
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
//	             deadline. That latency argument is why the flag stays.
//
//	             It previously also claimed the plugin's own preflight
//	             probe covered the conflict case. It did not: that probe
//	             is validate_dhcp, which is opt-in, runs once per
//	             *network* at CreateNetwork, and only checks that a DHCP
//	             server answers on the parent NIC. Nothing checked
//	             whether the leased address was free, and a production
//	             endpoint was handed an address a statically-configured
//	             host already held — silently, with every counter at
//	             zero (#524).
//
//	             Conflict detection now lives in
//	             pkg/plugin/conflict_probe.go: after the lease, off the
//	             critical path, comparing the answering MAC against the
//	             endpoint's. Do not restore the claim above without a
//	             check that fails when it stops being true.
//	             (v4-only flag; a no-op under -6.)
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
