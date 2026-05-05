package udhcpc

import (
	"strings"
	"testing"
)

// hasArg returns whether args contains exactly target.
func hasArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}

// TestNewDHCPClient_RequestedIPv4 covers the recovery hint: when the
// caller passed RequestedIP, udhcpc must get `-r ADDR` so the server
// has a chance to ACK the same lease the container is already using.
func TestNewDHCPClient_RequestedIPv4(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{
		RequestedIP: "192.168.0.50",
	})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	// Walk the args looking for the -r/value pair.
	found := false
	for i := 0; i < len(c.cmd.Args)-1; i++ {
		if c.cmd.Args[i] == "-r" && c.cmd.Args[i+1] == "192.168.0.50" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected `-r 192.168.0.50`, got args: %v", c.cmd.Args)
	}
}

// TestNewDHCPClient_RequestedIPNotPassedToV6 covers the v6 carve-out.
// busybox udhcpc6 has no -r equivalent, so a v6 client must never get
// the flag — passing it would make udhcpc6 reject its own arg list.
func TestNewDHCPClient_RequestedIPNotPassedToV6(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{
		V6:          true,
		RequestedIP: "fe80::1",
	})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if hasArg(c.cmd.Args, "-r") {
		t.Errorf("DHCPv6 client must not receive -r; args: %v", c.cmd.Args)
	}
}

// TestNewDHCPClient_VendorIDv4Only covers the same v4/v6 split for
// the vendor-id flag (-V). udhcpc6 doesn't accept -V either.
func TestNewDHCPClient_VendorIDv4Only(t *testing.T) {
	c4, err := NewDHCPClient("eth0", &DHCPClientOptions{})
	if err != nil {
		t.Fatalf("NewDHCPClient v4: %v", err)
	}
	if !hasArg(c4.cmd.Args, "-V") || !hasArg(c4.cmd.Args, VendorID) {
		t.Errorf("v4 client should set -V %s; args: %v", VendorID, c4.cmd.Args)
	}

	c6, err := NewDHCPClient("eth0", &DHCPClientOptions{V6: true})
	if err != nil {
		t.Fatalf("NewDHCPClient v6: %v", err)
	}
	if hasArg(c6.cmd.Args, "-V") {
		t.Errorf("v6 client must not set -V; args: %v", c6.cmd.Args)
	}
}

// TestNewDHCPClient_VendorClassOverride covers the v0.9.0 / T2-3
// driver-opt: when DHCPClientOptions.VendorClass is set, that value
// is sent as -V instead of the package default. Empty VendorClass
// falls back to VendorID.
func TestNewDHCPClient_VendorClassOverride(t *testing.T) {
	custom := "my-corp-vlan"

	c, err := NewDHCPClient("eth0", &DHCPClientOptions{VendorClass: custom})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if !hasArg(c.cmd.Args, "-V") || !hasArg(c.cmd.Args, custom) {
		t.Errorf("v4 client with VendorClass=%q should set -V %s; args: %v", custom, custom, c.cmd.Args)
	}
	if hasArg(c.cmd.Args, VendorID) {
		t.Errorf("VendorClass override leaked default %q into args: %v", VendorID, c.cmd.Args)
	}

	// v6 still must not receive -V even with an override (udhcpc6 rejects it).
	c6, err := NewDHCPClient("eth0", &DHCPClientOptions{V6: true, VendorClass: custom})
	if err != nil {
		t.Fatalf("NewDHCPClient v6: %v", err)
	}
	if hasArg(c6.cmd.Args, "-V") {
		t.Errorf("v6 client must not set -V even with VendorClass override; args: %v", c6.cmd.Args)
	}
}

// TestNewDHCPClient_BinarySelection asserts the v4/v6 client process
// path lookup. Wrong binary name = silent fail-to-spawn; this test
// catches a refactor that swapped the constants.
func TestNewDHCPClient_BinarySelection(t *testing.T) {
	c4, err := NewDHCPClient("eth0", &DHCPClientOptions{})
	if err != nil {
		t.Fatalf("v4: %v", err)
	}
	if c4.cmd.Args[0] != "udhcpc" {
		t.Errorf("v4 binary: got %q want udhcpc", c4.cmd.Args[0])
	}

	c6, err := NewDHCPClient("eth0", &DHCPClientOptions{V6: true})
	if err != nil {
		t.Fatalf("v6: %v", err)
	}
	if c6.cmd.Args[0] != "udhcpc6" {
		t.Errorf("v6 binary: got %q want udhcpc6", c6.cmd.Args[0])
	}
}

// TestNewDHCPClient_HandlerScriptDefault covers the empty-string
// fallback. A regression that dropped the default would surface as
// `udhcpc` exiting immediately because `-s` got an empty arg.
func TestNewDHCPClient_HandlerScriptDefault(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	for i := 0; i < len(c.cmd.Args)-1; i++ {
		if c.cmd.Args[i] == "-s" {
			if c.cmd.Args[i+1] != DefaultHandler {
				t.Errorf("default handler: got %q want %q", c.cmd.Args[i+1], DefaultHandler)
			}
			return
		}
	}
	t.Errorf("no -s flag found; args: %v", c.cmd.Args)
}

// TestNewDHCPClient_HandlerScriptOverride covers the explicit override —
// the handler-script knob exists primarily so tests can point at a
// scratch script; making sure the override is respected guards that
// path.
func TestNewDHCPClient_HandlerScriptOverride(t *testing.T) {
	custom := "/tmp/my-handler.sh"
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{HandlerScript: custom})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	for i := 0; i < len(c.cmd.Args)-1; i++ {
		if c.cmd.Args[i] == "-s" {
			if c.cmd.Args[i+1] != custom {
				t.Errorf("custom handler: got %q want %q", c.cmd.Args[i+1], custom)
			}
			return
		}
	}
	t.Errorf("no -s flag found; args: %v", c.cmd.Args)
}

// TestNewDHCPClient_HostnameV6FQDNEncoding covers the v6 hostname
// option encoding (RFC4704). On v6 the hostname goes through option
// 27 (0x27) with one flags byte + length byte + ASCII hostname.
// A regression that emitted the v4-style "hostname:..." form for v6
// would silently drop the hostname from DHCPv6 leases.
func TestNewDHCPClient_HostnameV6FQDNEncoding(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{
		V6:       true,
		Hostname: "my-host", // 7 ASCII bytes
	})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}

	// expected payload: flags=0x01, len=0x07, "my-host" => 010706d6d792d686f7374... no
	// actually: 01 07 then ASCII "my-host" = 6d 79 2d 68 6f 73 74
	const want = "0x27:0107" + "6d792d686f7374"
	found := false
	for i := 0; i < len(c.cmd.Args)-1; i++ {
		if c.cmd.Args[i] == "-x" && c.cmd.Args[i+1] == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected v6 hostname encoded as %q; args: %v", want, c.cmd.Args)
	}

	// And the v4 hostname form must NOT also leak through on v6.
	for i := 0; i < len(c.cmd.Args)-1; i++ {
		if c.cmd.Args[i] == "-x" && strings.HasPrefix(c.cmd.Args[i+1], "hostname:") {
			t.Errorf("v6 client should not emit v4 `hostname:` form; args: %v", c.cmd.Args)
		}
	}
}

// TestNewDHCPClient_ForegroundAndInterface covers the always-on flags.
// `-f` (foreground) is what makes process supervision work — without
// TestNewDHCPClient_BroadcastV4 covers the ipvlan-required broadcast
// flag: when Broadcast is set on a v4 client, `-B` makes udhcpc set
// the BROADCAST bit in DISCOVER so the server replies with an L2
// broadcast OFFER. ipvlan slaves share the parent's MAC and have no
// way to receive a unicast OFFER addressed to that shared MAC, so
// without -B the lease handshake hangs.
func TestNewDHCPClient_BroadcastV4(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{Broadcast: true})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if !hasArg(c.cmd.Args, "-B") {
		t.Errorf("expected -B for Broadcast=true; got args: %v", c.cmd.Args)
	}
}

// TestNewDHCPClient_BroadcastNotPassedToV6 covers the v6 carve-out:
// DHCPv6 has no equivalent client-broadcast-flag concept; passing -B
// to udhcpc6 (which doesn't recognise it) would just confuse the
// argv. Match the existing v4-only carve-outs (-r, -V).
func TestNewDHCPClient_BroadcastNotPassedToV6(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{V6: true, Broadcast: true})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if hasArg(c.cmd.Args, "-B") {
		t.Errorf("v6 client must not get -B; got args: %v", c.cmd.Args)
	}
}

// TestNewDHCPClient_BroadcastDefaultOff: regression guard for macvlan
// and bridge modes. These don't need -B (their kernel paths dispatch
// per-MAC and per-veth respectively), and we want the simpler RFC
// default for callers that don't opt in.
func TestNewDHCPClient_BroadcastDefaultOff(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if hasArg(c.cmd.Args, "-B") {
		t.Errorf("-B must be off by default; got args: %v", c.cmd.Args)
	}
}

// it udhcpc daemonises and we lose the child PID. `-i <iface>` is the
// link to attach to. Both are non-negotiable.
func TestNewDHCPClient_ForegroundAndInterface(t *testing.T) {
	c, err := NewDHCPClient("ens18", &DHCPClientOptions{})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if !hasArg(c.cmd.Args, "-f") {
		t.Errorf("expected -f (foreground); args: %v", c.cmd.Args)
	}
	for i := 0; i < len(c.cmd.Args)-1; i++ {
		if c.cmd.Args[i] == "-i" {
			if c.cmd.Args[i+1] != "ens18" {
				t.Errorf("interface: got %q want ens18", c.cmd.Args[i+1])
			}
			return
		}
	}
	t.Errorf("no -i flag found; args: %v", c.cmd.Args)
}
