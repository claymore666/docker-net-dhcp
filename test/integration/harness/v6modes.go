// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// A per-test dual-stack segment whose IPv6 service mode is chosen by
// the test: managed DHCPv6, stateless DHCPv6, SLAAC only, or a DHCPv6
// server that sends no router advertisements at all.
//
// WHY THIS EXISTS AT ALL, AND WHY HERE. #815 needs exactly one of
// these modes (stateless) to show that the plugin receives an
// INFORMATION-REQUEST reply and then discards it. #816, #820 and #821
// each need a different one. Building a stateless-only fixture for
// #815 and widening it three times would mean the design is reviewed
// as an increment three times and never on its merits; the four modes
// are one mechanism with one parameter, so it lands once, under the
// first issue that needs it.
//
// WHY A SEPARATE L2, AND NOT THE SUITE-STATIC SEGMENTS. This is the
// load-bearing design decision, not an implementation detail. The
// thing that distinguishes these modes is the M and O flags in the
// router advertisement, and RA is a property of the BROADCAST DOMAIN,
// not of a server. Two RA senders with different flags on one segment
// do not produce a conflict the test can see: the client simply acts
// on whichever advertisement it heard last. A test written that way
// measures ARRIVAL ORDER and reports it as protocol behaviour --
// intermittently, and most often correctly, which is the worst
// available failure mode. The suite-static macvlan and bridge
// segments both already run an --enable-ra dnsmasq, so neither can
// host these tests at any price. Hence: own bridge, own prefix, one
// mode at a time, torn down per test. Same rationale as the distinct
// v4 subnets bridge.go documents, one layer down.
//
// WHY DUAL-STACK. The plugin has no IPv6-only network mode -- every
// mode leases v4 and treats v6 as an addition (see network.go). A
// v6-only fixture would therefore be testing a configuration the
// product does not have, so the segment carries a working v4 pool in
// every mode and the v6 half is the part under test.
//
// WHAT EACH TEST OWES. A test on this fixture asserts against ITS OWN
// segment and ITS OWN container. Do not write a sweep that concludes
// "the stateless segment behaves correctly" across several consumers:
// an aggregate over a population is not a claim about its members,
// and "each consumer observes the stateless behaviour" is the claim
// #816, #820 and #821 will actually be relying on.
const (
	// V6BridgeName is this fixture's own Linux bridge. Distinct from
	// BridgeName so the two never share a broadcast domain; still on
	// the dh-itest-* prefix the orphan cleanup keys on, and within
	// IFNAMSIZ.
	V6BridgeName = "dh-itest-br6"

	// The v4 half. 192.168.99/100/101/102/123 are all spoken for by
	// other fixtures; this is the next free one.
	V6BridgeAddr = "192.168.103.1/24"
	V6PoolStart  = "192.168.103.10"
	V6PoolEnd    = "192.168.103.99"
	V6SubnetCIDR = "192.168.103.0/24"

	// The v6 half. fd00:6470:6863::/64 is the macvlan fixture and
	// fd00:6470:6864::/64 is the bridge fixture, so this is the next
	// free ULA prefix.
	V6BridgeAddrV6 = "fd00:6470:6865::1/64"
	V6Prefix       = "fd00:6470:6865::"
	V6PoolStartV6  = "fd00:6470:6865::10"
	V6PoolEndV6    = "fd00:6470:6865::99"
	V6SubnetV6CIDR = "fd00:6470:6865::/64"

	// V6DNSServer and V6SearchDomain are what the server advertises
	// over DHCPv6. Both are deliberately values no other fixture
	// uses, so a test that finds them in a container's resolver
	// configuration knows they came from THIS segment and not from a
	// leaked v4 lease or another fixture's server.
	//
	// The search domain is not decoration: option6:domain-search is
	// the second defect in #815 -- it is carried on every DHCPv6
	// reply, stateful ones included, and the plugin dropped it in
	// both paths. Measured reaching the client as
	// new_dhcp6_domain_search on INFORM6 and on BOUND6 alike.
	V6DNSServer    = "fd00:6470:6865::53"
	V6SearchDomain = "v6mode.example"
)

// V6Mode selects what the segment offers over IPv6.
type V6Mode int

const (
	// V6Managed is stateful DHCPv6: an address pool plus RAs with the
	// M flag. The client gets its address over DHCPv6.
	V6Managed V6Mode = iota
	// V6Stateless is the #815 shape: RAs with the O flag and no
	// address pool. The client forms its own address from the prefix
	// and asks DHCPv6 only for configuration -- an
	// INFORMATION-REQUEST answered with a REPLY carrying DNS servers
	// and a search list, and no address at all.
	V6Stateless
	// V6SLAAC is RAs with neither flag and no DHCPv6 server. Address
	// and nothing else; there is nobody to ask for configuration.
	V6SLAAC
	// V6NoRA is a DHCPv6 server on a segment with no router
	// advertisements. Nothing tells the client to speak DHCPv6, so
	// the server sits there unused. This is the negative control: it
	// separates "the plugin handled the DHCPv6 reply" from "a DHCPv6
	// server was running nearby".
	V6NoRA
	// V6ManagedSilent is managed DHCPv6 whose server answers nothing:
	// RAs carry the M flag, so the segment says an address is
	// available over DHCPv6, and every SOLICIT is dropped.
	//
	// This is the mode that keeps #868's fix honest. Tolerating a
	// missing DHCPv6 address is correct exactly when the segment never
	// offered one; here it did. A fix that read "v6 acquisition failed,
	// carry on" rather than "the advertisement said no DHCPv6, carry
	// on" passes every other mode in this file and fails only this one.
	V6ManagedSilent
)

func (m V6Mode) String() string {
	switch m {
	case V6Managed:
		return "managed"
	case V6Stateless:
		return "stateless"
	case V6SLAAC:
		return "slaac"
	case V6NoRA:
		return "nora"
	case V6ManagedSilent:
		return "managed-silent"
	}
	return fmt.Sprintf("V6Mode(%d)", int(m))
}

// The advertisement cadence and the advertised Router Lifetime, in
// seconds and as numbers rather than as strings, so the observation
// window can be DERIVED from the cadence instead of the two being
// typed independently and drifting apart. v6RAIntervalSec is what
// V6ObserveWindow is checked against in v6read.go.
const (
	v6RAIntervalSec = 30
	v6RALifetimeSec = 9000
)

// raParam PINS the advertised Router Lifetime instead of letting
// dnsmasq derive it.
//
// THIS IS A CONTROL, and it exists because the derivation is a trap.
// dnsmasq computes the router lifetime FROM the advertisement interval
// unless it is given one, so shortening the interval silently shortens
// the lifetime -- which manufactures the exact defect #875 is
// investigating (a default route that stops being valid) and would let
// a fixture change masquerade as a product finding.
//
// Pinning it decouples route survival from advertisement cadence. If a
// container's default route still disappears well inside 9000s, the
// route did NOT age out and something removed it or stopped owning it.
// 9000 is the ceiling RFC 4861 6.2.1 allows for AdvDefaultLifetime, and
// the interval stays comfortably below it as that section requires.
//
// Solicited advertisements are unaffected: dnsmasq answers a Router
// Solicitation immediately regardless of this interval, and a container
// sends one at startup.
func raParam() string {
	return fmt.Sprintf("--ra-param=%s,%d,%d", V6BridgeName, v6RAIntervalSec, v6RALifetimeSec)
}

// rangeArgs is the dnsmasq spelling of the mode.
//
// THESE ARE DNSMASQ FLAGS, NOT PROTOCOL NAMES. "ra-stateless" and
// "ra-only" are one server's vocabulary for the M/O flag
// combinations; another server spells them differently and the
// protocol itself has no such words. Measured against dnsmasq 2.92.
func (m V6Mode) rangeArgs() []string {
	switch m {
	case V6Managed:
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime,
			"--enable-ra",
			raParam(),
		}
	case V6Stateless:
		return []string{
			"--dhcp-range=" + V6Prefix + ",ra-stateless," + LeaseTime,
			"--enable-ra",
			raParam(),
		}
	case V6SLAAC:
		return []string{
			"--dhcp-range=" + V6Prefix + ",ra-only," + LeaseTime,
			"--enable-ra",
			raParam(),
		}
	case V6NoRA:
		// --enable-ra deliberately omitted; that is the whole mode.
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime,
		}
	case V6ManagedSilent:
		// Identical to V6Managed plus one directive. `dhcpv6` is a tag
		// dnsmasq sets on its own for every DHCPv6 request, so the
		// ignore reaches the v6 half and only the v6 half -- the v4
		// pool every mode carries keeps serving, which matters because
		// the plugin leases v4 on every network and a broken v4 half
		// would fail the endpoint for the wrong reason.
		//
		// Measured 2026-08-27, dnsmasq 2.92rel2 / dhcpcd 10.3.2, one
		// netns per arm:
		//
		//   --dhcp-ignore=tag:dhcpv6  -> DHCPSOLICIT ... ignored, no
		//                                IA_NA, v4 DHCPACK still sent,
		//                                client hook still sees three
		//                                ROUTERADVERTs with nd1_flags=MO
		//
		// The obvious spelling does NOT work and was tried first:
		// tagging the v6 range with `set:` and ignoring that tag leaves
		// dnsmasq serving the address anyway, with the tag visibly in
		// the request's tag set. Do not "simplify" this back to it.
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime,
			"--enable-ra",
			raParam(),
			"--dhcp-ignore=tag:dhcpv6",
		}
	}
	return nil
}

// wantPool and wantRA are the mode's observable signature, measured
// 2026-08-27 against dnsmasq 2.92rel2:
//
//	mode       pool-start in log   RTR-ADVERT sent
//	managed    yes                 yes
//	stateless  no                  yes
//	slaac      no                  yes
//	nora       yes                 no
//
// Both facts are LOCALE-PROOF, which is why they and not the obvious
// startup prose are what the fixture checks. dnsmasq translates its
// log strings -- "IP range" is "IP-Bereich" under the German locale
// the integration runner speaks -- but an address is an address in
// every language and RTR-ADVERT is a protocol token dnsmasq prints
// verbatim like DHCPDISCOVER. Matching "DHCPv6 stateless on" would
// have been the natural check and would go quietly green-to-red on a
// locale nobody changed on purpose.
//
// That signature does not separate stateless from slaac, and
// deliberately does not try to. The only thing that distinguishes
// them is the range keyword, and dnsmasq VALIDATES THAT ITSELF at
// config-parse time: a keyword it does not recognise is "bad
// dhcp-range" and the process never starts, so the fixture's
// readiness poll fails loudly rather than the segment coming up
// silently in the other mode. Measured: ra-stateless and ra-only pass
// --test, ra-bogus does not.
func (m V6Mode) wantPool() bool {
	return m == V6Managed || m == V6NoRA || m == V6ManagedSilent
}
func (m V6Mode) wantRA() bool { return m != V6NoRA }

// V6Fixture is a per-test dual-stack segment in one V6Mode. Create it
// with NewV6Fixture; it registers its own teardown.
type V6Fixture struct {
	t    *testing.T
	mode V6Mode

	cmd       *exec.Cmd
	tmpDir    string
	leaseFile string
	logFile   string

	linkUp            bool
	iptablesInstalled bool
}

// NewV6Fixture brings up the bridge, the addresses, the FORWARD rules
// and a dnsmasq in the requested mode, and does not return until the
// segment has been observed to be in that mode.
func NewV6Fixture(t *testing.T, mode V6Mode) *V6Fixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("V6Fixture needs root (got uid=%d)", os.Geteuid())
	}

	// Defensively, so a previous panicked run cannot poison this one.
	cleanupV6Links()

	f := &V6Fixture{t: t, mode: mode}
	t.Cleanup(f.teardown)

	la := netlink.NewLinkAttrs()
	la.Name = V6BridgeName
	if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: la}); err != nil {
		t.Fatalf("LinkAdd bridge %s: %v", V6BridgeName, err)
	}
	f.linkUp = true
	link, err := netlink.LinkByName(V6BridgeName)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", V6BridgeName, err)
	}

	// Same reason as bridge.go: the kernel's 15s STP forward delay
	// holds the port in LEARNING and no DHCP passes.
	fdPath := filepath.Join("/sys/class/net", V6BridgeName, "bridge/forward_delay")
	if err := os.WriteFile(fdPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("disable STP forward_delay on %s: %v", V6BridgeName, err)
	}
	// Turn duplicate address detection off on this bridge BEFORE any
	// address is added, and before the link comes up.
	//
	// THIS IS NOT A TIDINESS SETTING, IT IS THE RA TIMING. Measured
	// 2026-08-27: with DAD on, dnsmasq starts while the bridge's
	// global address is still `tentative`, cannot send from it, and
	// emits its first router advertisement about NINE seconds later.
	// With the address settled it advertises in about one. The
	// difference is invisible in the log -- dnsmasq says "IPv6 router
	// advertisement enabled" either way -- so the mode check below
	// reads a segment that is merely slow as a segment in the wrong
	// mode, and the obvious repair is to widen the budget until the
	// symptom stops, which buys a fixture that no longer checks the
	// thing it exists to check.
	//
	// DAD has no question to answer here: this bridge is a private
	// segment on a ULA prefix nothing else uses, with exactly one
	// router on it, which is us. All it can do is hold the address
	// unusable for a second.
	dadPath := filepath.Join("/proc/sys/net/ipv6/conf", V6BridgeName, "accept_dad")
	if err := os.WriteFile(dadPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("disable DAD on %s: %v", V6BridgeName, err)
	}

	// This segment advertises a router. Make the kernel behind that
	// address actually be one.
	//
	// WHAT WAS WRONG, stated precisely, because the obvious phrasing is
	// wrong. RFC 4861 2.1 defines a router by FORWARDING, not by
	// advertising. So the fixture bridge was a HOST, and answering a
	// neighbour solicitation with R=0 was truthful and conformant
	// (4.4, 7.2.4). The false statement was the ROUTER ADVERTISEMENT:
	// we ran an RA daemon on a host that was not a router, which 6.2.1
	// describes as accidentally starting to act as one. Two conformant
	// assertions about one fact, and 2.1 says which was false. The
	// BOUND: no single MUST is violated by dnsmasq in isolation --
	// 6.2.1 constrains a default, not an operator -- so this was a
	// self-contradictory SEGMENT, and the contradiction was ours.
	//
	// WHAT THE CONTAINER THEN DID, correctly. 7.2.5 requires a host
	// that sees IsRouter go true->false to remove that router from its
	// Default Router List. It did.
	//
	// MEASURED, run 33208729673 job 98976446841, managed mode, before
	// this line existed. Two default-route deletions in a three-minute
	// window, each in the SAME MILLISECOND as a neighbour advertisement
	// carrying R=0 from fe80::d86b:d4ff:fe29:204a -- the default route's
	// own gateway, and the EUI-64 of da:6b:d4:29:20:4a, this bridge.
	// Each route came back 1ms after the next advertisement. Ten of ten
	// advertisements carried Router Lifetime 9000, read off the wire, so
	// an expiring or withdrawn router was already excluded.
	//
	// WHY PER-DEVICE, AND WHY conf/all STAYS 0. MEASURED by the
	// protocol consultant on kernel 6.12.105 under `unshare -Urn`: with
	// all=0 default=0 dev=0 the advertisement carries R=0; with
	// all=0 default=0 dev=1 it carries R=1. And the acceptability half
	// -- packets are forwarded only when `all` is 1; all=0 with dev=1
	// forwards nothing. So this buys the R bit without turning the
	// runner into a router. `default` is NOT used: it is no narrower,
	// because every device created afterwards inherits it, which in
	// this namespace means every other fixture's bridge and veth.
	//
	// INFERRED for the runner kernel. That table is one kernel version,
	// and these knobs' semantics are exactly the kind of thing that
	// moves between releases. The wire capture is what settles it on
	// the kernel that actually runs the suite.
	//
	// THE CLOBBER TRAP. Writing conf/all/forwarding 1 and then 0 resets
	// a per-device 1 back to 0 (measured, same kernel). Anything else
	// in this namespace touching `all` silently reverts this, and the
	// failure returns looking like flake. Today nothing does -- start()
	// has exactly one call site, so there is no dnsmasq restart to
	// re-assert after -- but that is a fact about today's tree, not a
	// property. The standing observer is the probe, which samples this
	// device's forwarding flag repeatedly through the run rather than
	// once at setup.
	//
	// WHY THIS IS NOT A WEAKENING, and how to check that claim rather
	// than take it. No assertion moves, no timeout grows, nothing is
	// skipped, and no test is opted out of. The discriminator is that
	// the WIRE must change: the same capture that recorded R=0 and two
	// deletions must, after this, record R=1 and none. If it does not,
	// this line did not do what this comment says and the change is not
	// defensible.
	//
	// THE PURGE IS NAMESPACE-WIDE, AND IT HITS THIS RUNNER.
	// addrconf_fixup_forwarding ends in
	// `if (newf) rt6_purge_dflt_routers(net)`, so a 0->1 transition on
	// ANY device drops every `proto ra` default route in the whole
	// network namespace -- not only the ones on the device written.
	//
	// This was written as a bound and it turned out to be a live
	// defect. MEASURED, run 33211208318 job 98984641078: the write
	// purged TWO default routes, `::/0 via fe80::a893:a7ff:fe55:6c2d`
	// on ifindex 14 and 16 -- the runner's own IPv6 default routing,
	// removed by a test fixture. It reproduced in three shards, twice
	// per shard.
	//
	// So the routes are put back. The snapshot is taken before the
	// write and anything missing afterwards is reinstalled with the
	// attributes it had, protocol included, so the namespace ends the
	// way it started. Restoring is not cosmetic: without it the rest
	// of the job runs with no IPv6 default route and fails somewhere
	// else entirely, which is the shape of bug that costs days.
	//
	// WHY NOT AVOID THE PURGE INSTEAD. Nothing avoids it. The R bit
	// comes from the advertising device's own forwarding flag and
	// nowhere else, the purge fires on the transition rather than on
	// the device, and the fixture shares a namespace with the runner
	// because the engine and the plugin do. A namespace of its own is
	// the real fix and it is not a release-day change.
	raBefore, err := v6ProtoRADefaultRoutes()
	if err != nil {
		t.Fatalf("enumerate `proto ra` IPv6 default routes before enabling forwarding "+
			"on %s: %v. Refusing to write a sysctl whose blast radius could not be "+
			"measured first.", V6BridgeName, err)
	}
	fwdPath := filepath.Join("/proc/sys/net/ipv6/conf", V6BridgeName, "forwarding")
	if err := os.WriteFile(fwdPath, []byte("1"), 0o644); err != nil {
		t.Fatalf("enable IPv6 forwarding on %s: %v", V6BridgeName, err)
	}
	restoreV6ProtoRADefaults(t, raBefore)
	// Read back rather than trust the write. A sysctl write that is
	// silently refused or immediately overwritten looks exactly like one
	// that took, and this fixture has already paid once for a premise
	// nobody checked.
	if got, err := os.ReadFile(fwdPath); err != nil {
		t.Fatalf("read back forwarding on %s: %v", V6BridgeName, err)
	} else if strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("forwarding on %s did not take: wrote 1, read %q. The bridge would "+
			"advertise a router and then answer neighbour solicitations with R=0, and "+
			"RFC 4861 7.2.5 would have the container drop the route -- a fixture defect "+
			"that reads as a product failure.", V6BridgeName, strings.TrimSpace(string(got)))
	}

	// ...and add each address with IFA_F_NODAD, which is a stronger
	// statement than the sysctl and answers a different question.
	//
	// accept_dad=0 does not stop the address being CREATED tentative;
	// it stops the probe, and the kernel then clears the flag from
	// addrconf's work queue some time after the link comes up. That
	// window is real: measured 2026-08-27 in run 33096065817, three of
	// the four modes read the address settled and the fourth read it
	// still tentative 60ms after LinkSetUp, on the same code path with
	// the same sysctl written. Nothing about the fourth mode differs
	// here -- the mode only reaches dnsmasq's argv, further down -- so
	// what the guard caught was its own premise, not a mode.
	//
	// NODAD is set at creation, so the flag is never raised at all and
	// there is no window to lose. The sysctl stays because it governs
	// the address this loop does NOT add: the kernel's own link-local,
	// which is what a router advertisement is sent FROM.
	for _, cidr := range []string{V6BridgeAddr, V6BridgeAddrV6} {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", cidr, err)
		}
		addr.Flags |= unix.IFA_F_NODAD
		if err := netlink.AddrAdd(link, addr); err != nil {
			t.Fatalf("AddrAdd %s on %s: %v", cidr, V6BridgeName, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("LinkSetUp %s: %v", V6BridgeName, err)
	}
	awaitNoTentativeAddr(t)

	if err := installBridgeForward(V6BridgeName); err != nil {
		t.Fatalf("install FORWARD rules for %s: %v", V6BridgeName, err)
	}
	f.iptablesInstalled = true

	tmp, err := os.MkdirTemp("", "dh-itest-v6-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	f.tmpDir = tmp
	f.leaseFile = filepath.Join(tmp, "leases")
	f.logFile = filepath.Join(tmp, "dnsmasq.log")

	t.Logf("v6 fixture: mode=%s bridge=%s v4=%s-%s v6=%s",
		mode, V6BridgeName, V6PoolStart, V6PoolEnd, V6SubnetV6CIDR)

	f.start()
	return f
}

func (f *V6Fixture) start() {
	f.t.Helper()
	logF, err := os.Create(f.logFile)
	if err != nil {
		f.t.Fatalf("create v6 dnsmasq log: %v", err)
	}
	defer logF.Close()

	args := []string{
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",
		"--interface=" + V6BridgeName,
		"--bind-interfaces",
		"--except-interface=lo",
		// The v4 half, present in every mode -- see WHY DUAL-STACK.
		"--dhcp-range=" + V6PoolStart + "," + V6PoolEnd + "," + LeaseTime,
	}
	args = append(args, f.mode.rangeArgs()...)
	args = append(args,
		"--dhcp-option=option6:dns-server,["+V6DNSServer+"]",
		"--dhcp-option=option6:domain-search,"+V6SearchDomain,
		"--dhcp-leasefile="+f.leaseFile,
		"--dhcp-no-override",
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
	)

	f.cmd = withCLocale(exec.Command("/usr/sbin/dnsmasq", args...))
	f.cmd.Stdout = logF
	f.cmd.Stderr = logF
	f.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := f.cmd.Start(); err != nil {
		f.t.Fatalf("start v6 dnsmasq: %v", err)
	}

	f.waitReady()
	f.assertMode()
}

// waitReady blocks until the v4 pool is logged, which every mode has
// and which dnsmasq prints once its ranges are configured. Keyed on
// the pool's start ADDRESS for the locale reason above.
func (f *V6Fixture) waitReady() {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(f.readLog(), V6PoolStart) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Fatalf("v6 fixture dnsmasq (mode=%s) did not become ready; log:\n%s",
		f.mode, f.readLog())
}

// assertMode checks the segment is in the mode the test asked for,
// rather than assuming the flags did what they did the day they were
// measured. Both halves matter: the positive says the mode is on, and
// the negative says the OTHER mode is off. A check with one possible
// verdict is not a check.
//
// The RA half has to wait rather than sample: an advertisement is
// emitted about a second after startup, so "no RTR-ADVERT yet" and
// "no RTR-ADVERT ever" are the same string until enough time has
// passed. The positive case therefore polls, and the negative case
// spends the same budget before concluding absence -- otherwise the
// no-RA mode would pass instantly and for the wrong reason.
func (f *V6Fixture) assertMode() {
	f.t.Helper()

	if got := strings.Contains(f.readLog(), V6PoolStartV6); got != f.mode.wantPool() {
		f.t.Fatalf("v6 fixture mode=%s: DHCPv6 address pool present=%v, want %v; log:\n%s",
			f.mode, got, f.mode.wantPool(), f.readLog())
	}

	const raBudget = 5 * time.Second
	deadline := time.Now().Add(raBudget)
	sawRA := false
	for time.Now().Before(deadline) {
		if strings.Contains(f.readLog(), "RTR-ADVERT(") {
			sawRA = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if sawRA != f.mode.wantRA() {
		f.t.Fatalf("v6 fixture mode=%s: router advertisement sent=%v within %v, want %v; log:\n%s",
			f.mode, sawRA, raBudget, f.mode.wantRA(), f.readLog())
	}
}

// Mode is the mode this segment was brought up in.
func (f *V6Fixture) Mode() V6Mode { return f.mode }

// Bridge is the bridge name to hand the driver as `bridge=`.
func (f *V6Fixture) Bridge() string { return V6BridgeName }

// CountLogLines counts server log lines containing all of substrings.
// Mirrors Fixture.CountBridgeLogLines: this is the outside evidence a
// test should assert on, the plugin's own counters being evidence of
// intent rather than of effect.
func (f *V6Fixture) CountLogLines(substrings ...string) int {
	n := 0
	for _, line := range strings.Split(f.readLog(), "\n") {
		all := true
		for _, s := range substrings {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all && line != "" {
			n++
		}
	}
	return n
}

// AwaitIgnoredSolicit blocks until the server has logged a DHCPv6
// request it refused to answer, and fails the test if none arrives.
//
// It exists because V6ManagedSilent has the same startup signature as
// V6Managed -- same pool line, same RA -- so assertMode cannot tell
// them apart, and the thing that separates them only becomes visible
// once a client actually solicits. Without this a mistyped ignore
// directive would produce a segment that quietly serves addresses
// while the test believed the server was silent, which turns the
// strongest assertion in the #868 set into a tautology.
//
// "ignored" is dnsmasq's own word for the outcome and, like
// DHCPSOLICIT, it is a protocol/state token it prints verbatim rather
// than one of the translated prose strings -- see wantPool's note on
// why the locale rules the choice of substring here.
func (f *V6Fixture) AwaitIgnoredSolicit(budget time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if f.CountLogLines("DHCPSOLICIT", "ignored") > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	f.t.Fatalf("v6 fixture mode=%s: no DHCPv6 solicit was refused within %v — "+
		"either no client asked, or the server answered and this segment is not "+
		"silent at all; log:\n%s", f.mode, budget, f.readLog())
}

// LeaseFile is the path to the server's own lease database.
//
// A third record, independent of both the plugin's report and the
// server's log. CountLogLines says what the server SAID it replied;
// this says what it recorded having handed out. A test that needs to
// know an address really came from this server should read both --
// two records with one observer is the failure mode, and the plugin's
// view of its own lease is not one of the two.
func (f *V6Fixture) LeaseFile() string { return f.leaseFile }

func (f *V6Fixture) readLog() string {
	data, err := os.ReadFile(f.logFile)
	if err != nil {
		return fmt.Sprintf("(could not read v6 fixture log: %v)", err)
	}
	return string(data)
}

// DumpLogs mirrors Fixture.DumpBridgeLogs for failure diagnostics.
func (f *V6Fixture) DumpLogs(write func(string)) {
	write(fmt.Sprintf("--- v6 fixture dnsmasq log (mode=%s) ---\n%s", f.mode, f.readLog()))
}

func (f *V6Fixture) teardown() {
	if f.cmd != nil && f.cmd.Process != nil {
		_ = f.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = f.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = f.cmd.Process.Kill()
			<-done
		}
	}
	if f.tmpDir != "" {
		_ = os.RemoveAll(f.tmpDir)
	}
	if f.iptablesInstalled {
		removeBridgeForward(V6BridgeName)
		f.iptablesInstalled = false
	}
	cleanupV6Links()
	f.linkUp = false
}

// tentativeBudget bounds awaitNoTentativeAddr below.
//
// IT IS DELIBERATELY SHORTER THAN THE FASTEST POSSIBLE REAL DAD, and
// that is the whole design. Duplicate address detection sends its
// probe after a random delay of up to a second and waits a further
// RetransTimer -- a second by default -- so a segment on which DAD is
// genuinely running cannot clear inside 250ms and this still fails.
// What it absorbs is the other thing: with DAD off the kernel does not
// skip the tentative flag, it clears it from addrconf's work queue
// shortly after the link comes up, and that is sub-millisecond.
//
// Measured 2026-08-27, 200 bring-ups per arm, one variable each, all
// tentative readings on the kernel's own link-local:
//
//	accept_dad=0, no NODAD, no wait  -> 15/200 tentative
//	accept_dad=0, NODAD,    no wait  -> 11/200 tentative
//	accept_dad=0, NODAD,    wait     ->  0/200, longest wait 2ms
//	accept_dad=1, NODAD,    wait     -> 200/200 failed at the budget
//
// The last line is the control, and it is why this is a discriminator
// rather than a timeout: turn DAD back on and the guard still goes
// red on every single run. The third line is why the budget costs
// nothing in the healthy case.
//
// The second line is why the wait is here at all, and it is worth
// reading twice: NODAD alone is NOT enough. At 40 runs per arm it
// looked like it was -- 0/40, against 6/40 without it -- and that
// reading would have shipped a wait-free fixture that flaked twice a
// week. Four hundred more bring-ups put both arms at the same rate.
// NODAD closes the two addresses this fixture ADDS; nothing it can
// set reaches the link-local the kernel generates.
const tentativeBudget = 250 * time.Millisecond

// awaitNoTentativeAddr checks the two settings above actually took,
// rather than trusting that writing them was enough. An address left
// tentative cannot be a send source, and the only symptom downstream
// is a router advertisement that arrives late -- which presents as the
// wrong mode. Naming the cause here costs one netlink call and saves
// that diagnosis.
//
// It reads EVERY v6 address on the link, not just the two added above.
// The kernel's own link-local is the one this fixture cannot set
// NODAD on -- it does not add it -- and it is also the address a
// router advertisement is sent FROM, so it is the one that matters
// most and the only one left racing the work queue.
func awaitNoTentativeAddr(t *testing.T) {
	t.Helper()
	link, err := netlink.LinkByName(V6BridgeName)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", V6BridgeName, err)
	}
	deadline := time.Now().Add(tentativeBudget)
	for {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatalf("AddrList %s: %v", V6BridgeName, err)
		}
		var stuck []string
		for _, a := range addrs {
			if a.Flags&unix.IFA_F_TENTATIVE != 0 {
				stuck = append(stuck, a.IPNet.String())
			}
		}
		if len(stuck) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still tentative on %s after %s, with accept_dad=0 "+
				"and IFA_F_NODAD set — router advertisements will be "+
				"delayed and the mode check will misreport. This budget is "+
				"too short for a real DAD probe to clear, so DAD is on: a "+
				"configured address here means NODAD was not honoured, the "+
				"link-local means the sysctl did not take",
				strings.Join(stuck, ", "), V6BridgeName, tentativeBudget)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// cleanupV6Links removes the fixture's bridge, on teardown and
// defensively at setup.
func cleanupV6Links() {
	if link, err := netlink.LinkByName(V6BridgeName); err == nil {
		_ = netlink.LinkDel(link)
	}
}

// v6ProtoRADefaultRoutes lists the IPv6 default routes in THIS network
// namespace that the kernel installed from a router advertisement --
// exactly the set rt6_purge_dflt_routers removes.
//
// It returns the routes rather than a count because they have to be
// put back, and an error rather than an empty slice because "none
// present" and "could not look" must not be the same value. Reading
// the second as the first would make the restore silently do nothing.
func v6ProtoRADefaultRoutes() ([]netlink.Route, error) {
	routes, err := netlink.RouteList(nil, unix.AF_INET6)
	if err != nil {
		return nil, err
	}
	var out []netlink.Route
	for _, r := range routes {
		if r.Dst != nil && !r.Dst.IP.IsUnspecified() {
			continue
		}
		if int(r.Protocol) != unix.RTPROT_RA {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// restoreV6ProtoRADefaults reinstalls any route from `before` that the
// forwarding write purged, and fails the fixture if it cannot.
//
// It fails rather than warns. A fixture that has removed the runner's
// IPv6 default routing and cannot put it back has broken every job
// that follows it, and the failure has to land HERE, naming the cause,
// rather than as an unexplained network error in something unrelated.
func restoreV6ProtoRADefaults(t *testing.T, before []netlink.Route) {
	t.Helper()
	if len(before) == 0 {
		return
	}
	present := func() map[string]bool {
		m := map[string]bool{}
		if now, err := v6ProtoRADefaultRoutes(); err == nil {
			for _, r := range now {
				m[r.String()] = true
			}
		}
		return m
	}
	have := present()
	var restored, failed []string
	for _, r := range before {
		if have[r.String()] {
			continue
		}
		// MEASURED, kernel 6.12.105 under `unshare -Urn`: userspace
		// CAN install a default route tagged `proto ra`, so the
		// protocol is preserved rather than downgraded to static.
		//
		// The flags are the part that could not be measured without a
		// real advertisement to generate one. An RA-derived route
		// carries kernel-set rtm_flags that the kernel may refuse
		// coming the other way, so a refusal is retried once with the
		// flags cleared. That is a DEGRADED restore and it says so --
		// a default route that is back but not byte-identical beats a
		// namespace with none, and silently pretending the two are
		// the same is what would hide it.
		route := r
		err := netlink.RouteReplace(&route)
		if err != nil && route.Flags != 0 {
			bare := r
			bare.Flags = 0
			if err2 := netlink.RouteReplace(&bare); err2 == nil {
				restored = append(restored,
					fmt.Sprintf("%s (DEGRADED: reinstalled with flags cleared; "+
						"original refused with %v)", bare.String(), err))
				continue
			}
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", route.String(), err))
			continue
		}
		restored = append(restored, route.String())
	}
	if len(failed) > 0 {
		t.Fatalf("enabling forwarding on %s purged the namespace's `proto ra` IPv6 "+
			"default routes (rt6_purge_dflt_routers) and %d of them could NOT be "+
			"restored: %v. The runner is now without the IPv6 default routing it "+
			"started with. Restored: %v. Present before: %v.",
			V6BridgeName, len(failed), failed, restored, before)
	}
	if len(restored) > 0 {
		t.Logf("enabling forwarding on %s purged %d `proto ra` IPv6 default route(s) "+
			"namespace-wide; all were reinstalled: %v",
			V6BridgeName, len(restored), restored)
	}
	// Verify rather than trust the return codes: RouteReplace can
	// succeed against a table that then does not contain what was
	// asked for, and this guard exists precisely because the obvious
	// reading of a success was wrong once already.
	have = present()
	var still []string
	for _, r := range before {
		if !have[r.String()] {
			still = append(still, r.String())
		}
	}
	if len(still) > 0 {
		t.Fatalf("after reinstalling them, %d `proto ra` IPv6 default route(s) are STILL "+
			"missing from this namespace: %v. netlink reported success, the table "+
			"disagrees, and the runner's IPv6 routing is not what it was.", len(still), still)
	}
}

// AssertGatewayIsRouter asserts the PROPERTY the forwarding write is
// there to produce, from inside the container, using the container's
// own neighbour table.
//
// WHY NOT ASSERT THE SYSCTL. Asserting conf/<dev>/forwarding asserts
// the MECHANISM: it passes whenever the write landed, including on a
// kernel where that knob no longer fills the R bit, and it is the
// fixture reporting on itself. The neighbour table is outside
// evidence -- it is the container's record of what the wire actually
// said -- and it goes red for the right reason if anything in the
// namespace clobbers conf/all/forwarding mid-run.
//
// RFC 4861 7.2.5: the container marks a neighbour a router exactly
// when an advertisement from it carried IsRouter, and unmarks it when
// one arrives without. `ip -6 neigh` prints that state as the word
// `router`, so this reads the same bit the standard turns on.
//
// The BOUND, stated because a green here is narrower than it looks:
// this observes the entry at ONE instant. A gateway that is a router
// now can stop being one a second later, which is the whole defect
// this guards against. It is a spot check that can only ever prove the
// bad state was not present when it ran.
func AssertGatewayIsRouter(t V6Reporter, neigh, gateway string) {
	t.Helper()
	if gateway == "" {
		t.Errorf("no default-route gateway was resolved, so the IsRouter property could "+
			"not be checked at all. Neighbour table:\n%s", neigh)
		return
	}
	for _, line := range strings.Split(neigh, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != gateway {
			continue
		}
		for _, tok := range f {
			if tok == "router" {
				return
			}
		}
		t.Errorf("the default gateway %s is in the container's neighbour table but is "+
			"NOT marked `router`, so its advertisements carried IsRouter=0 and RFC 4861 "+
			"7.2.5 will have the container drop the default route through it. Check "+
			"that net.ipv6.conf.%s.forwarding is still 1 -- writing conf/all/forwarding "+
			"anywhere in this namespace silently resets it. Entry: %q",
			gateway, V6BridgeName, strings.TrimSpace(line))
		return
	}
	t.Errorf("the default gateway %s has no entry at all in the container's neighbour "+
		"table, so this says nothing about IsRouter either way -- absent is not a pass. "+
		"Table:\n%s", gateway, neigh)
}

// V6DefaultGateway pulls the gateway address out of `ip -6 route show`
// output. It returns "" when there is no default route, which callers
// must treat as "unknown", never as "no gateway".
func V6DefaultGateway(routeText string) string {
	for _, line := range strings.Split(routeText, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "default" || f[1] != "via" {
			continue
		}
		return f[2]
	}
	return ""
}
