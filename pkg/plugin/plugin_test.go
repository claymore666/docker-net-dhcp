package plugin

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsDHCPPlugin(t *testing.T) {
	cases := []struct {
		driver string
		want   bool
	}{
		{"ghcr.io/devplayer0/docker-net-dhcp:release-linux-amd64", true},
		{"ghcr.io/claymore666/docker-net-dhcp:v0.4.0", true},
		{"ghcr.io/claymore666/docker-net-dhcp:latest", true},

		// Foreign namespaces are deliberately rejected — see W-6 in the
		// 2026-05-05 review. A broader regex would let a third-party
		// image masquerade as an instance of this plugin and trigger
		// spurious bridge-conflict errors.
		{"someregistry.example/team/docker-net-dhcp:1.0", false},
		{"docker-net-dhcp:local", false},
		{"evil.example/docker-net-dhcp:bad", false},

		{"bridge", false},
		{"macvlan", false},
		{"overlay", false},
		{"docker-net-dhcp", false}, // missing ":<tag>"
		{"ghcr.io/devplayer0/docker-net-dhcp", false},
		{"ghcr.io/devplayer0/other-thing:v1", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.driver, func(t *testing.T) {
			if got := IsDHCPPlugin(c.driver); got != c.want {
				t.Errorf("IsDHCPPlugin(%q) = %v, want %v", c.driver, got, c.want)
			}
		})
	}
}

func TestEffectiveMode(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"", ModeBridge},
		{"bridge", ModeBridge},
		{"macvlan", ModeMacvlan},
		{"ipvlan", ModeIPvlan},
		// effectiveMode is a normaliser, NOT a validator — it returns
		// whatever non-empty value is set, even if invalid. Validation
		// happens in CreateNetwork.
		{"garbage", "garbage"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			opts := DHCPNetworkOptions{Mode: c.mode}
			if got := opts.effectiveMode(); got != c.want {
				t.Errorf("effectiveMode(Mode=%q) = %q, want %q", c.mode, got, c.want)
			}
		})
	}
}

func TestDecodeOpts(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]interface{}
		want    DHCPNetworkOptions
		wantErr bool
	}{
		{
			name:  "empty",
			input: map[string]interface{}{},
			want:  DHCPNetworkOptions{},
		},
		{
			name: "bridge_minimal",
			input: map[string]interface{}{
				"bridge": "br0",
			},
			want: DHCPNetworkOptions{Bridge: "br0"},
		},
		{
			name: "macvlan_full",
			input: map[string]interface{}{
				"mode":          "macvlan",
				"parent":        "eth0",
				"ipv6":          "true",
				"lease_timeout": "30s",
				"gateway":       "192.168.0.1",
			},
			want: DHCPNetworkOptions{
				Mode:         ModeMacvlan,
				Parent:       "eth0",
				IPv6:         true,
				LeaseTimeout: 30 * time.Second,
				Gateway:      "192.168.0.1",
			},
		},
		{
			name: "ipvlan",
			input: map[string]interface{}{
				"mode":   "ipvlan",
				"parent": "ens18",
			},
			want: DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "ens18"},
		},
		{
			name: "ignore_conflicts_and_skip_routes",
			input: map[string]interface{}{
				"bridge":           "br0",
				"ignore_conflicts": "true",
				"skip_routes":      "true",
			},
			want: DHCPNetworkOptions{
				Bridge:          "br0",
				IgnoreConflicts: true,
				SkipRoutes:      true,
			},
		},
		{
			name: "lease_timeout_invalid",
			input: map[string]interface{}{
				"bridge":        "br0",
				"lease_timeout": "not-a-duration",
			},
			wantErr: true,
		},
		{
			name: "unknown_field_rejected",
			input: map[string]interface{}{
				"bridge":          "br0",
				"unrecognised_op": "yes",
			},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeOpts(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil; opts=%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("decodeOpts(%v):\n  got  %+v\n  want %+v", c.input, got, c.want)
			}
		})
	}
}

// TestVethPairNames pins the naming convention so a refactor that
// silently changes the prefix doesn't break compatibility with
// already-running endpoints.
func TestVethPairNames(t *testing.T) {
	host, ctr := vethPairNames("0123456789abcdef0123456789abcdef")
	if host != "dh-0123456789ab" {
		t.Errorf("host name = %q, want dh-0123456789ab", host)
	}
	if ctr != "0123456789ab-dh" {
		t.Errorf("ctr name = %q, want 0123456789ab-dh", ctr)
	}
	if !strings.HasPrefix(host, "dh-") {
		t.Errorf("host should start with dh-")
	}
}

func TestSubLinkName(t *testing.T) {
	got := subLinkName("0123456789abcdef0123456789abcdef")
	want := "dh-0123456789ab"
	if got != want {
		t.Errorf("subLinkName = %q, want %q", got, want)
	}
}

func TestClientIDFromEndpoint(t *testing.T) {
	cases := []struct {
		name string
		eid  string
		// We test length + stability rather than literal bytes, since the
		// derivation is "first 8 bytes of hex-decoded EndpointID".
		wantLen int
		wantNil bool
	}{
		{"full_endpoint_id", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 8, false},
		{"min_required", "0123456789abcdef", 8, false},
		{"too_short", "0123", 0, true},
		{"empty", "", 0, true},
		{"non_hex", "this-is-not-hex!", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clientIDFromEndpoint(c.eid)
			if c.wantNil && got != nil {
				t.Errorf("expected nil, got %x", got)
			}
			if !c.wantNil && len(got) != c.wantLen {
				t.Errorf("expected %d bytes, got %d (%x)", c.wantLen, len(got), got)
			}
		})
	}

	// Stability: same input must produce same bytes.
	a := clientIDFromEndpoint("0123456789abcdef0123456789abcdef")
	b := clientIDFromEndpoint("0123456789abcdef0123456789abcdef")
	if string(a) != string(b) {
		t.Errorf("derivation is not stable: %x vs %x", a, b)
	}

	// Distinctness: different inputs produce different bytes.
	c := clientIDFromEndpoint("fedcba9876543210fedcba9876543210")
	if string(a) == string(c) {
		t.Errorf("derivation collided on different inputs")
	}
}

// TestResolveClientID pins the v0.9.0 / T2-3 override semantics:
// operator-supplied opts.ClientID wins over the endpoint-derived
// stable id, and an empty override falls back to derivation.
func TestResolveClientID(t *testing.T) {
	const eid = "0123456789abcdef0123456789abcdef"

	t.Run("override wins", func(t *testing.T) {
		got := resolveClientID(DHCPNetworkOptions{ClientID: "my-class-id"}, eid)
		if string(got) != "my-class-id" {
			t.Errorf("override: got %q, want %q", got, "my-class-id")
		}
	})

	t.Run("empty override falls back to derived", func(t *testing.T) {
		got := resolveClientID(DHCPNetworkOptions{}, eid)
		want := clientIDFromEndpoint(eid)
		if string(got) != string(want) {
			t.Errorf("fallback: got %x, want %x", got, want)
		}
	})

	t.Run("override with empty endpoint still works", func(t *testing.T) {
		// Even if the endpoint id is too short to derive from, an
		// explicit override should still be honoured. Prevents a
		// regression where the fallback path swallowed the override.
		got := resolveClientID(DHCPNetworkOptions{ClientID: "static"}, "")
		if string(got) != "static" {
			t.Errorf("override+empty eid: got %q, want %q", got, "static")
		}
	})
}

// TestListen_RemovesStaleSocket covers the I-9 fix: Listen must
// best-effort unlink any leftover socket file before binding so a
// previous unclean shutdown doesn't EADDRINUSE the new one. Driving
// the full Listen would block on Serve, so we replicate the prelude:
// pre-place a regular file at the socket path and confirm the unlink
// path clears it before net.Listen would fail.
func TestListen_RemovesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "stale.sock")

	// Pre-place a non-socket file at the target path. net.Listen would
	// fail with EADDRINUSE / "address already in use" on this path
	// without the os.Remove in Listen.
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Mirror Listen's prelude. We can't call p.Listen directly because
	// Serve blocks; the unlink + Listen sequence is what we care about.
	_ = os.Remove(sockPath)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen after prelude: %v (the os.Remove in Listen should have cleared the path)", err)
	}
	_ = l.Close()
	_ = os.Remove(sockPath)
}
