package udhcpc

import (
	"net"
	"reflect"
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
	for _, banned := range []string{"hostname ", "vendorclassid", "clientid", "request", "ia_na"} {
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

func TestRenderArgs_OneShotV4(t *testing.T) {
	got := renderArgs(dhcpcdParams{
		Iface:      "eth0",
		Once:       true,
		Handler:    "/usr/lib/net-dhcp/udhcpc-handler",
		ConfigPath: "/run/net-dhcp/eth0-v4.conf",
	})
	want := []string{
		"dhcpcd", "-B", "--noconfigure", "-L",
		"-c", "/usr/lib/net-dhcp/udhcpc-handler",
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
		"dhcpcd", "-B", "--noconfigure", "-L",
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
