// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"testing"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// TestValidateModeOptions_RejectsUnusableInterfaceNames closes the
// bridge-reuse bypass. netlink hands a name to the kernel
// zero-terminated and the kernel reads it as a C string, so
// "docker0\x00evil" resolves docker0 -- measured, index 7, while
// "docker0evil" is not found -- while the reuse guard compares the full
// Go string and misses. Two DHCP networks then share one bridge.
//
// The daemon really does forward a NUL: a create carrying one reached
// fork/exec of iptables, which rejected it only because execve refuses
// NUL in argv. So this is reachable, not latent.
//
// Removing either ValidIfaceName call in validateModeOptions turns this
// red.
func TestValidateModeOptions_RejectsUnusableInterfaceNames(t *testing.T) {
	bad := []struct {
		name  string
		value string
	}{
		{"NUL truncation", "br0\x00evil"},
		{"path traversal", "../../x"},
		{"a path separator", "br/0"},
		{"over IFNAMSIZ", "bridge0123456789"},
		{"flag-shaped", "-cfoo"},
		{"a bare dash", "-"},
		{"dot-leading", ".x"},
		{"whitespace", "br0 evil"},
		{"a newline", "br0\nevil"},
	}

	for _, tt := range bad {
		t.Run("bridge/"+tt.name, func(t *testing.T) {
			err := validateModeOptions(DHCPNetworkOptions{Mode: ModeBridge, Bridge: tt.value})
			if err == nil {
				t.Fatalf("bridge=%q accepted", tt.value)
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("bridge=%q rejected with %v, want an ErrIPAM", tt.value, err)
			}
		})
		t.Run("parent/"+tt.name, func(t *testing.T) {
			err := validateModeOptions(DHCPNetworkOptions{Mode: ModeMacvlan, Parent: tt.value})
			if err == nil {
				t.Fatalf("parent=%q accepted", tt.value)
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("parent=%q rejected with %v, want an ErrIPAM", tt.value, err)
			}
		})
	}

	// The other direction: names real deployments use must still pass.
	for _, good := range []string{"br0", "docker0", "eth0.100", "enp3s0", "ens18", "veth_a1-b2", "bridge012345678"} {
		if err := validateModeOptions(DHCPNetworkOptions{Mode: ModeBridge, Bridge: good}); err != nil {
			t.Errorf("bridge=%q rejected: %v", good, err)
		}
		if err := validateModeOptions(DHCPNetworkOptions{Mode: ModeMacvlan, Parent: good}); err != nil {
			t.Errorf("parent=%q rejected: %v", good, err)
		}
	}
}

// TestValidateModeOptions_ReuseGuardIsNotBypassableByTruncation states
// the property the validation exists to protect, in the terms the guard
// itself uses: the guard compares Go strings, so if a name that the
// KERNEL treats as equal to another can get past validation, two
// networks share a bridge and ErrBridgeUsed never fires.
//
// Written against the pair rather than against a regex so it survives a
// change of validation mechanism.
func TestValidateModeOptions_ReuseGuardIsNotBypassableByTruncation(t *testing.T) {
	const real = "br0"
	// Every string the kernel would resolve to `real` while Go compares
	// them as different.
	for _, alias := range []string{real + "\x00evil", real + "\x00", real + "\x00" + real} {
		if alias == real {
			t.Fatal("test bug: the alias is not distinct from the real name")
		}
		if err := validateModeOptions(DHCPNetworkOptions{Mode: ModeBridge, Bridge: alias}); err == nil {
			t.Errorf("bridge=%q accepted; it resolves %q in the kernel but compares unequal in the reuse guard", alias, real)
		}
	}
}

// TestParseIfnameOption_RejectsFlagShapedNames covers the argv end. The
// kernel accepts these as link names -- measured: -cfoo, -c, - and .x
// were all accepted, and only "x y" refused -- and the name is read back
// and placed LAST in the dhcpcd argv, where getopt permutation re-reads
// a flag-shaped positional as an option (#706).
func TestParseIfnameOption_RejectsFlagShapedNames(t *testing.T) {
	for _, bad := range []string{"-cfoo", "-c", "-", ".x", "-rf", "_eth0"} {
		got, err := parseIfnameOption(map[string]interface{}{ifnameOption: bad})
		if err == nil {
			t.Errorf("parseIfnameOption(%q) accepted it, returning %q", bad, got)
			continue
		}
		if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("parseIfnameOption(%q) rejected with %v, want an ErrIPAM", bad, err)
		}
	}

	for _, good := range []string{"eth0", "net1", "eth0.100", "my-iface_2"} {
		got, err := parseIfnameOption(map[string]interface{}{ifnameOption: good})
		if err != nil {
			t.Errorf("parseIfnameOption(%q) rejected a legitimate name: %v", good, err)
		}
		if got != good {
			t.Errorf("parseIfnameOption(%q) = %q", good, got)
		}
	}

	// Absent is not an error, and stays that way.
	if got, err := parseIfnameOption(map[string]interface{}{}); err != nil || got != "" {
		t.Errorf("parseIfnameOption(absent) = (%q, %v), want (\"\", nil)", got, err)
	}
}
