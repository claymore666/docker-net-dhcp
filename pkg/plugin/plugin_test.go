package plugin

import (
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
		{"someregistry.example/team/docker-net-dhcp:1.0", true},
		{"docker-net-dhcp:local", true},

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
