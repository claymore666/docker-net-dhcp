// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return mac
}

// TestDUIDLL_StructureAndValue pins the DUID-LL wire layout: 0x0003
// (link-layer type) + 0x0001 (Ethernet) + the MAC, colon-hex.
func TestDUIDLL_StructureAndValue(t *testing.T) {
	got := duidLL(mustMAC(t, "de:ad:be:ef:00:01"))
	want := "00:03:00:01:de:ad:be:ef:00:01"
	if got != want {
		t.Errorf("duidLL = %q, want %q", got, want)
	}
}

// TestIdentity_DeterministicInMAC is the heart of #152: the one-shot
// and persistent clients run as separate processes (and in different
// netns) but share the endpoint MAC, so they MUST derive the same DUID
// and IAID — that identity is what collapses the two associations into
// one binding.
func TestIdentity_DeterministicInMAC(t *testing.T) {
	// A second, independent parse of the same MAC string is the
	// realistic case: the one-shot and persistent clients each parse the
	// endpoint MAC fresh and must derive an identical DUID and IAID.
	mac := mustMAC(t, "02:42:ac:11:00:05")
	mac2 := mustMAC(t, "02:42:ac:11:00:05")
	if duidLL(mac) != duidLL(mac2) {
		t.Errorf("DUID differs across equal MACs: %q vs %q", duidLL(mac), duidLL(mac2))
	}
	if iaidFromMAC(mac) != iaidFromMAC(mac2) {
		t.Errorf("IAID differs across equal MACs: %q vs %q", iaidFromMAC(mac), iaidFromMAC(mac2))
	}
}

// TestIdentity_DistinctForDistinctMACs: different endpoints (different
// MACs) must get different identities so their leases don't collide on
// the server.
func TestIdentity_DistinctForDistinctMACs(t *testing.T) {
	a := mustMAC(t, "02:42:ac:11:00:05")
	b := mustMAC(t, "02:42:ac:11:00:06")
	if duidLL(a) == duidLL(b) {
		t.Error("distinct MACs produced the same DUID")
	}
	if iaidFromMAC(a) == iaidFromMAC(b) {
		t.Error("distinct MACs produced the same IAID")
	}
}

// TestIAIDFromMAC_DecimalLow4Bytes: the IAID is the low 4 bytes of the
// MAC as a decimal uint32 (the form dhcpcd's `iaid` parses as a number).
func TestIAIDFromMAC_DecimalLow4Bytes(t *testing.T) {
	// low 4 bytes be:ef:00:01 => 0xbeef0001 => 3203334145
	if got := iaidFromMAC(mustMAC(t, "de:ad:be:ef:00:01")); got != "3203334145" {
		t.Errorf("iaidFromMAC = %q, want 3203334145", got)
	}
}

// TestFormatClientID_PrependsOpaqueType matches the busybox wire form
// (type byte 0x00 + payload) so existing server reservations keep
// matching after the migration.
func TestFormatClientID_PrependsOpaqueType(t *testing.T) {
	if got := formatClientID([]byte{0xab, 0xcd, 0xef}); got != "00:ab:cd:ef" {
		t.Errorf("formatClientID = %q, want 00:ab:cd:ef", got)
	}
	if got := formatClientID(nil); got != "00" {
		t.Errorf("formatClientID(nil) = %q, want 00", got)
	}
}

// helper: assert a config line is present / absent
func hasLine(conf, line string) bool {
	for _, l := range strings.Split(conf, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

func TestRenderConfig_V4_FullOptions(t *testing.T) {
	mac := mustMAC(t, "de:ad:be:ef:00:01")
	conf := renderConfig(dhcpcdParams{
		Iface:       "eth0",
		MAC:         mac,
		Hostname:    "my-host",
		VendorClass: "docker-net-dhcp",
		ClientID:    []byte{0xab, 0xcd},
		RequestedIP: "192.168.0.50",
	})

	for _, want := range []string{
		"duid 00:03:00:01:de:ad:be:ef:00:01",
		"nohook resolv.conf",
		"nohook hostname",
		"nohook ntp.conf",
		"nohook yp.conf",
		"hostname my-host",
		"vendorclassid docker-net-dhcp",
		"clientid 00:ab:cd",
		"interface eth0",
		"iaid 3203334145",
		"request 192.168.0.50",
	} {
		if !hasLine(conf, want) {
			t.Errorf("config missing line %q\n---\n%s", want, conf)
		}
	}
	// v4 config must not request a v6 IA_NA.
	if strings.Contains(conf, "ia_na") {
		t.Errorf("v4 config leaked ia_na:\n%s", conf)
	}
}

func TestRenderConfig_V6_PinsIAAndPreferredAddress(t *testing.T) {
	mac := mustMAC(t, "de:ad:be:ef:00:01")
	conf := renderConfig(dhcpcdParams{
		Iface:       "eth0",
		MAC:         mac,
		V6:          true,
		Hostname:    "my-host",
		PreferredV6: "fd00::42",
		// v4-only knobs must be ignored on v6.
		VendorClass: "should-not-appear",
		ClientID:    []byte{0xab, 0xcd},
		RequestedIP: "192.168.0.50",
	})

	for _, want := range []string{
		"duid 00:03:00:01:de:ad:be:ef:00:01",
		"interface eth0",
		"iaid 3203334145",
		"ia_na 3203334145 / fd00::42",
		"hostname my-host",
	} {
		if !hasLine(conf, want) {
			t.Errorf("v6 config missing line %q\n---\n%s", want, conf)
		}
	}
	// DHCPv4-only directives must never appear on a v6 client.
	for _, banned := range []string{"vendorclassid", "clientid", "request"} {
		if strings.Contains(conf, banned) {
			t.Errorf("v6 config leaked v4-only directive %q:\n%s", banned, conf)
		}
	}
}

// TestRenderConfig_V6_NoPreferredOmitsIANaButKeepsIAID: without a
// preferred address we still pin the IAID (dhcpcd requests IA_NA for it
// by default); we just don't emit an explicit ia_na line.
func TestRenderConfig_V6_NoPreferredOmitsIANaButKeepsIAID(t *testing.T) {
	conf := renderConfig(dhcpcdParams{
		Iface: "eth0",
		MAC:   mustMAC(t, "de:ad:be:ef:00:01"),
		V6:    true,
	})
	if !hasLine(conf, "iaid 3203334145") {
		t.Errorf("v6 config dropped the pinned iaid:\n%s", conf)
	}
	if strings.Contains(conf, "ia_na") {
		t.Errorf("v6 config without a preferred address should omit ia_na:\n%s", conf)
	}
}

// TestRenderConfig_OmitsAbsentOptionals: a minimal endpoint emits only
// identity + nohooks + the interface/iaid block.
func TestRenderConfig_OmitsAbsentOptionals(t *testing.T) {
	conf := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	for _, banned := range []string{"hostname ", "fqdn ", "vendorclassid", "clientid", "request", "ia_na"} {
		if strings.Contains(conf, banned) {
			t.Errorf("minimal config contains unexpected directive %q:\n%s", banned, conf)
		}
	}
}

func TestRenderConfig_EventFIFO(t *testing.T) {
	mac := mustMAC(t, "de:ad:be:ef:00:01")
	// present → emitted as an `env` directive (dhcpcd scrubs the process
	// environment, so this is how the hook learns the FIFO path).
	conf := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, EventFIFO: "/run/net-dhcp/x/events"})
	if !hasLine(conf, "env NETDHCP_EVENT_FIFO=/run/net-dhcp/x/events") {
		t.Errorf("config missing env FIFO directive:\n%s", conf)
	}
	// absent → no env line.
	if strings.Contains(renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac}), "env ") {
		t.Error("config emitted an env directive with no FIFO set")
	}
}

func TestRenderConfig_CoverDir(t *testing.T) {
	mac := mustMAC(t, "de:ad:be:ef:00:01")
	// present → GOCOVERDIR rides the `env` directive so the hook's -cover
	// counters survive dhcpcd's environment scrub (cover build only).
	conf := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, CoverDir: "/coverage"})
	if !hasLine(conf, "env GOCOVERDIR=/coverage") {
		t.Errorf("config missing env GOCOVERDIR directive:\n%s", conf)
	}
	// absent (production) → no GOCOVERDIR line.
	if strings.Contains(renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac}), "GOCOVERDIR") {
		t.Error("config emitted GOCOVERDIR with no CoverDir set")
	}
}

// TestRenderConfig_ReleaseOnlyForPersistent: the persistent client emits
// `release` (busybox -R: DHCPRELEASE on graceful stop, which the
// docker-restart / daemon-restart IP-stability tests rely on); the
// one-shot client must NOT, since -1 -p deliberately keeps the lease for
// the persistent client to re-claim.
func TestRenderConfig_ReleaseOnlyForPersistent(t *testing.T) {
	mac := mustMAC(t, "de:ad:be:ef:00:01")

	persistent := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac})
	if !hasLine(persistent, "release") {
		t.Errorf("persistent config missing release directive:\n%s", persistent)
	}

	oneShot := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, Once: true})
	if hasLine(oneShot, "release") {
		t.Errorf("one-shot config must not release (it keeps the lease via -1 -p):\n%s", oneShot)
	}

	// True for both families: a persistent v6 client should also release.
	persistentV6 := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, V6: true})
	if !hasLine(persistentV6, "release") {
		t.Errorf("persistent v6 config missing release directive:\n%s", persistentV6)
	}
}

// TestRenderConfig_BroadcastOnlyForIPvlanV4 covers the ipvlan-required
// broadcast directive (#243): when Broadcast is set on a v4 client the
// config emits the standalone `broadcast` directive (the dhcpcd
// equivalent of busybox `-B`), so the server replies with an L2
// broadcast OFFER that all ipvlan slaves on the shared parent MAC can
// receive. It must be absent when Broadcast is unset, and absent for v6
// (the BROADCAST flag is a DHCPv4 concept). Uses hasLine so the always-
// present `broadcast_address` option-request line can't false-positive.
func TestRenderConfig_BroadcastOnlyForIPvlanV4(t *testing.T) {
	mac := mustMAC(t, "de:ad:be:ef:00:01")

	withBcast := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, Broadcast: true})
	if !hasLine(withBcast, "broadcast") {
		t.Errorf("v4 broadcast client missing `broadcast` directive:\n%s", withBcast)
	}

	noBcast := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac})
	if hasLine(noBcast, "broadcast") {
		t.Errorf("non-broadcast client must not emit `broadcast`:\n%s", noBcast)
	}

	v6Bcast := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, V6: true, Broadcast: true})
	if hasLine(v6Bcast, "broadcast") {
		t.Errorf("v6 client must not emit `broadcast` (v4-only flag):\n%s", v6Bcast)
	}
}

// TestRenderConfig_FQDN: the `fqdn` directive (opt-in DDNS, #261) is
// emitted exactly when FQDN is set, for BOTH families (option 81 v4 /
// option 39 v6 — one directive covers both), and omitted otherwise.
func TestRenderConfig_FQDN(t *testing.T) {
	mac := mustMAC(t, "de:ad:be:ef:00:01")

	for _, v6 := range []bool{false, true} {
		conf := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, V6: v6, Hostname: "web1", FQDN: "both"})
		if !hasLine(conf, "fqdn both") {
			t.Errorf("v6=%v: FQDN set but `fqdn both` not emitted:\n%s", v6, conf)
		}
		// The name rides the hostname directive — both must be present.
		if !hasLine(conf, "hostname web1") {
			t.Errorf("v6=%v: fqdn directive without the hostname it names:\n%s", v6, conf)
		}
	}

	// Absent → no fqdn directive (default off).
	noFQDN := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, Hostname: "web1"})
	if strings.Contains(noFQDN, "fqdn ") {
		t.Errorf("FQDN unset must not emit a `fqdn` directive:\n%s", noFQDN)
	}
}

// TestRenderConfig_RequestsPropagatedOptions: because `-f <config>`
// bypasses /etc/dhcpcd.conf, the config must explicitly request every
// option the plugin propagates, or dhcpcd never learns them (the busybox
// `-O` set). The same v4-style names serve both families — dhcpcd maps
// them to the right per-protocol codes (e.g. domain_name_servers ->
// option 6 on v4, option 23 on v6).
func TestRenderConfig_RequestsPropagatedOptions(t *testing.T) {
	mac := mustMAC(t, "de:ad:be:ef:00:01")
	wantOpts := []string{
		"interface_mtu",       // option 26 (MTU propagation)
		"domain_name_servers", // option 6 / v6 23 (DNS)
		"domain_search",       // option 119 / v6 24 (search list)
		"ntp_servers",         // option 42
		"tftp_server_name",    // option 66
		"bootfile_name",       // option 67
		"routers",             // option 3 (gateway)
		"time_offset",         // option 2 (#262)
		"posix_timezone",      // option 100 (#262)
		"tzdb_timezone",       // option 101 (#262)
		"wpad",                // option 252 (#262, requires the define below)
	}
	for _, v6 := range []bool{false, true} {
		conf := renderConfig(dhcpcdParams{Iface: "eth0", MAC: mac, V6: v6})
		var optLine string
		for _, ln := range strings.Split(conf, "\n") {
			if strings.HasPrefix(ln, "option ") {
				optLine = ln
				break
			}
		}
		if optLine == "" {
			t.Fatalf("v6=%v: config has no `option` request line:\n%s", v6, conf)
		}
		for _, o := range wantOpts {
			if !strings.Contains(optLine, o) {
				t.Errorf("v6=%v: option request line missing %q\n%s", v6, o, optLine)
			}
		}
		// WPAD (252) is non-standard: dhcpcd rejects bare `option wpad`
		// without an explicit define first (#262).
		if !hasLine(conf, "define 252 string wpad") {
			t.Errorf("v6=%v: config requests `wpad` but is missing `define 252 string wpad`:\n%s", v6, conf)
		}
	}
}

func TestRenderArgs_OneShotV4(t *testing.T) {
	got := renderArgs(dhcpcdParams{
		Iface:      "eth0",
		Once:       true,
		Handler:    "/usr/lib/net-dhcp/dhcp-handler",
		ConfigPath: "/run/net-dhcp/eth0-v4.conf",
	})
	// argv[0] is the LITERAL absolute path, not dhcpcdBin. A want-list
	// built from the constant would assert the constant equals itself
	// and say nothing about its value — and its value is the whole
	// point: this argument is execed by a shell, which resolves a bare
	// name through PATH (#707).
	want := []string{
		"/sbin/dhcpcd", "-B", "--noconfigure", "-L", "-A",
		"-c", "/usr/lib/net-dhcp/dhcp-handler",
		"-f", "/run/net-dhcp/eth0-v4.conf",
		"-1", "-p", "-4", "eth0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args mismatch:\ngot:  %v\nwant: %v", got, want)
	}
}

func TestRenderArgs_PersistentV6(t *testing.T) {
	got := renderArgs(dhcpcdParams{
		Iface:      "eth0",
		V6:         true,
		Handler:    "/h",
		ConfigPath: "/c",
	})
	want := []string{
		"/sbin/dhcpcd", "-B", "--noconfigure", "-L", "-A",
		"-c", "/h", "-f", "/c", "-6", "eth0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args mismatch:\ngot:  %v\nwant: %v", got, want)
	}
	// Persistent client must not get the one-shot flag, and must NOT be
	// -p (it should release its lease when the plugin stops it).
	if hasArg(got, "-1") {
		t.Errorf("persistent client got -1: %v", got)
	}
	if hasArg(got, "-p") {
		t.Errorf("persistent client got -p; it must release on stop: %v", got)
	}
}

// TestRenderArgs_FamilyExclusive: exactly one of -4/-6 is present.
func TestRenderArgs_FamilyExclusive(t *testing.T) {
	v4 := renderArgs(dhcpcdParams{Iface: "eth0"})
	if !hasArg(v4, "-4") || hasArg(v4, "-6") {
		t.Errorf("v4 args family flags wrong: %v", v4)
	}
	v6 := renderArgs(dhcpcdParams{Iface: "eth0", V6: true})
	if !hasArg(v6, "-6") || hasArg(v6, "-4") {
		t.Errorf("v6 args family flags wrong: %v", v6)
	}
}

// TestDockerfileGuaranteesEveryAbsoluteBinary keeps the absolute paths
// from becoming two independent facts.
//
// This package names three binaries absolutely — dhcpcdBin, mountBin
// and mkdirBin — and each exists only because the Dockerfile installs
// it. The Dockerfile asserts them with `test -x` so an Alpine or
// busybox relocation fails the build instead of failing a container's
// first lease with "not found". Those are two copies of one set, and a
// fix that reaches one copy and not the other is this repository's most
// repeated failure.
//
// It compares SETS, in both directions, which is the part that earns
// its keep. The previous version filtered operands to the one
// containing "dhcpcd", so when mountBin and mkdirBin were added it went
// on reporting parity while guaranteeing neither. A test that inspects
// one element of a set it claims to check is green over an incomplete
// examination.
//
// It also collects EVERY `test -x` on a line, not just the first, which
// is what makes the chained `a && test -x b && test -x c` spelling
// readable back. The one-line `test -x a b c` form is not an option to
// re-introduce: busybox sh answers it "unknown operand" and exits 2
// whatever the files are, and this test would then see one operand
// against three constants and fail — which is the outcome we want,
// since that line cannot build. An `-a` chain fails here too, by
// putting "-a" and "-x" in the operand set.
//
// This reads the real Dockerfile rather than a fixture, so there is
// nothing to keep in sync. It fails loudly when no assertion is found,
// because a parity test with nothing to compare against is the
// green-over-an-empty-examination case.
func TestDockerfileGuaranteesEveryAbsoluteBinary(t *testing.T) {
	const marker = "test -x "

	b, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("reading Dockerfile: %v", err)
	}

	got := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for i := strings.Index(trimmed, marker); i >= 0; i = strings.Index(trimmed, marker) {
			rest := strings.TrimSpace(trimmed[i+len(marker):])
			if f := strings.Fields(rest); len(f) > 0 {
				got[f[0]] = true
			}
			trimmed = trimmed[i+len(marker):]
		}
	}

	want := map[string]bool{dhcpcdBin: true, mountBin: true, mkdirBin: true}

	if len(got) == 0 {
		t.Fatalf("the Dockerfile no longer asserts `test -x <path>` for anything; "+
			"nothing now guarantees %v exist in the image, and this test would "+
			"otherwise pass having compared nothing", keysOf(want))
	}

	for p := range want {
		if !got[p] {
			t.Errorf("this package runs %q by absolute path, but the Dockerfile "+
				"does not `test -x` it; a relocation would break a container at "+
				"run time instead of breaking the build", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("the Dockerfile asserts %q, which no constant in this package "+
				"names; either a binary lost its constant, or the assertion line "+
				"is not the chained `test -x a && test -x b` form this parses "+
				"(operands got: %v)", p, keysOf(got))
		}
	}
}

// keysOf renders a set in a stable order so a failure message reads the
// same on every run; map iteration order would otherwise make two
// identical failures look like two different ones.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
