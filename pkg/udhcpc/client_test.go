package udhcpc

import (
	"strings"
	"testing"
)

// hasArgPair checks whether c.cmd.Args contains "-x" immediately
// followed by an argument starting with prefix.
func hasArgPair(args []string, flag, prefix string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && strings.HasPrefix(args[i+1], prefix) {
			return true
		}
	}
	return false
}

func TestNewDHCPClient_ClientIDOption(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{
		ClientID: []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef},
	})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	// Expect -x 0x3d:00<16hex> for type-byte 0 + 8 bytes of payload.
	if !hasArgPair(c.cmd.Args, "-x", "0x3d:00") {
		t.Errorf("expected option 61 (0x3d) with type byte 0, got args: %v", c.cmd.Args)
	}
	want := "0x3d:000123456789abcdef"
	found := false
	for _, a := range c.cmd.Args {
		if a == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected exact arg %q, got args: %v", want, c.cmd.Args)
	}
}

func TestNewDHCPClient_NoClientIDForV6(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{
		V6:       true,
		ClientID: []byte{0x01, 0x23, 0x45, 0x67},
	})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if hasArgPair(c.cmd.Args, "-x", "0x3d:") {
		t.Errorf("DHCPv6 should not get a v4 client-id; args: %v", c.cmd.Args)
	}
}

func TestNewDHCPClient_NoClientIDOmitted(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if hasArgPair(c.cmd.Args, "-x", "0x3d:") {
		t.Errorf("client-id should not be set when ClientID is nil; args: %v", c.cmd.Args)
	}
}

func TestNewDHCPClient_HostnameOption(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{
		Hostname: "my-container",
	})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if !hasArgPair(c.cmd.Args, "-x", "hostname:my-container") {
		t.Errorf("expected hostname option; args: %v", c.cmd.Args)
	}
}

func TestNewDHCPClient_ReleaseFlagInPersistentMode(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{Once: false})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	// Persistent client must use -R to send DHCPRELEASE on SIGTERM.
	hasR := false
	for _, a := range c.cmd.Args {
		if a == "-R" {
			hasR = true
			break
		}
	}
	if !hasR {
		t.Errorf("persistent udhcpc must include -R; args: %v", c.cmd.Args)
	}
}

func TestNewDHCPClient_QuietFlagInOnceMode(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{Once: true})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	hasQ := false
	for _, a := range c.cmd.Args {
		if a == "-q" {
			hasQ = true
			break
		}
	}
	if !hasQ {
		t.Errorf("once-mode udhcpc must include -q; args: %v", c.cmd.Args)
	}
}
