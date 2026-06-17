package plugin

import (
	"bytes"
	"errors"
	"testing"

	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

// TestStableLeaseSeed_Precedence pins the most-specific-first ordering:
// lease_seed > Compose project+service+number > container name > none.
func TestStableLeaseSeed_Precedence(t *testing.T) {
	const net = "net1"
	full := containerIdentity{
		Name:           "/proj-web-1",
		ComposeProject: "proj",
		ComposeService: "web",
		ComposeNumber:  "1",
	}

	cases := []struct {
		name   string
		id     containerIdentity
		opts   DHCPNetworkOptions
		expect string // discriminator the chosen seed tier must contain
		empty  bool
	}{
		{"lease_seed wins", full, DHCPNetworkOptions{LeaseSeed: "k"}, "seed\x00k", false},
		{"compose when no seed", full, DHCPNetworkOptions{}, "compose\x00proj\x00web\x001", false},
		{
			"name when compose incomplete",
			containerIdentity{Name: "/solo", ComposeProject: "proj", ComposeService: "web"}, // no number
			DHCPNetworkOptions{}, "name\x00/solo", false,
		},
		{"name when only name", containerIdentity{Name: "/solo"}, DHCPNetworkOptions{}, "name\x00/solo", false},
		{"none when anonymous", containerIdentity{}, DHCPNetworkOptions{}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stableLeaseSeed(c.id, net, c.opts)
			if c.empty {
				if got != "" {
					t.Fatalf("want empty seed, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatal("want non-empty seed, got empty")
			}
			if !bytes.Contains([]byte(got), []byte(c.expect)) {
				t.Fatalf("seed %q does not contain discriminator %q", got, c.expect)
			}
		})
	}
}

// TestStableLeaseSeed_NetworkScoped: the same identity on two networks
// yields distinct seeds (so the same container gets a distinct client-id
// per DHCP network it joins).
func TestStableLeaseSeed_NetworkScoped(t *testing.T) {
	id := containerIdentity{Name: "/c"}
	a := stableLeaseSeed(id, "netA", DHCPNetworkOptions{})
	b := stableLeaseSeed(id, "netB", DHCPNetworkOptions{})
	if a == b {
		t.Fatalf("seeds must differ across networks, both = %q", a)
	}
}

// TestStableLeaseSeed_TagNamespaced: every seed carries the versioned
// scheme tag as a prefix, so the hash input is namespaced. The tag value
// is deliberately distinct from the deterministic-MAC scheme (#218) so a
// MAC seed and a client-id seed built from the same identity can never
// coincide once that work lands; this asserts the lease tag here (the MAC
// tag lives on the #218 branch).
func TestStableLeaseSeed_TagNamespaced(t *testing.T) {
	if stableLeaseSeedTag != "stable-lease-clientid/v1" {
		t.Fatalf("seed tag changed to %q — bumping it remaps every lease; do so deliberately", stableLeaseSeedTag)
	}
	seed := stableLeaseSeed(containerIdentity{Name: "/c"}, "n", DHCPNetworkOptions{})
	if !bytes.HasPrefix([]byte(seed), []byte(stableLeaseSeedTag)) {
		t.Fatalf("seed %q must be prefixed with the scheme tag", seed)
	}
}

// TestDeriveStableLeaseClientID pins determinism, width, and that
// distinct seeds map to distinct ids.
func TestDeriveStableLeaseClientID(t *testing.T) {
	a := deriveStableLeaseClientID("seed-a")
	if len(a) != stableLeaseClientIDLen {
		t.Fatalf("width: got %d want %d", len(a), stableLeaseClientIDLen)
	}
	if !bytes.Equal(a, deriveStableLeaseClientID("seed-a")) {
		t.Fatal("derivation must be deterministic")
	}
	if bytes.Equal(a, deriveStableLeaseClientID("seed-b")) {
		t.Fatal("distinct seeds must map to distinct client-ids")
	}
}

// TestResolveEndpointClientID covers the full precedence with stable_lease.
func TestResolveEndpointClientID(t *testing.T) {
	const eid = "0123456789abcdef0123456789abcdef"
	const net = "net1"
	named := containerIdentity{Name: "/web"}

	t.Run("operator client_id wins over stable", func(t *testing.T) {
		got := resolveEndpointClientID(
			DHCPNetworkOptions{StableLease: true, ClientID: "op"}, net, eid, named, nil)
		if string(got) != "op" {
			t.Fatalf("got %q want op", got)
		}
	})

	t.Run("stable off falls back to endpoint-derived", func(t *testing.T) {
		got := resolveEndpointClientID(DHCPNetworkOptions{}, net, eid, named, nil)
		if !bytes.Equal(got, clientIDFromEndpoint(eid)) {
			t.Fatalf("got %x want endpoint-derived %x", got, clientIDFromEndpoint(eid))
		}
	})

	t.Run("stable on derives from identity", func(t *testing.T) {
		opts := DHCPNetworkOptions{StableLease: true}
		got := resolveEndpointClientID(opts, net, eid, named, nil)
		want := deriveStableLeaseClientID(stableLeaseSeed(named, net, opts))
		if !bytes.Equal(got, want) {
			t.Fatalf("got %x want stable %x", got, want)
		}
		if bytes.Equal(got, clientIDFromEndpoint(eid)) {
			t.Fatal("stable id must not equal the endpoint-derived id")
		}
	})

	t.Run("stable id is identical across two endpoint IDs (the recreate guarantee)", func(t *testing.T) {
		opts := DHCPNetworkOptions{StableLease: true}
		a := resolveEndpointClientID(opts, net, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", named, nil)
		b := resolveEndpointClientID(opts, net, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", named, nil)
		if !bytes.Equal(a, b) {
			t.Fatalf("same identity, different endpoints must give same client-id: %x vs %x", a, b)
		}
	})

	t.Run("anonymous fallback invokes onFallback and uses endpoint-derived", func(t *testing.T) {
		called := 0
		got := resolveEndpointClientID(
			DHCPNetworkOptions{StableLease: true}, net, eid, containerIdentity{}, func() { called++ })
		if called != 1 {
			t.Fatalf("onFallback called %d times, want 1", called)
		}
		if !bytes.Equal(got, clientIDFromEndpoint(eid)) {
			t.Fatalf("got %x want endpoint-derived %x", got, clientIDFromEndpoint(eid))
		}
	})

	t.Run("nil onFallback is safe on anonymous (renewal/recovery path)", func(t *testing.T) {
		got := resolveEndpointClientID(
			DHCPNetworkOptions{StableLease: true}, net, eid, containerIdentity{}, nil)
		if !bytes.Equal(got, clientIDFromEndpoint(eid)) {
			t.Fatalf("got %x want endpoint-derived %x", got, clientIDFromEndpoint(eid))
		}
	})
}

// TestValidateModeOptions_StableLease pins the ipvlan-only contract:
// stable_lease is accepted for ipvlan and rejected (with ErrStableLeaseMode)
// for bridge and macvlan.
func TestValidateModeOptions_StableLease(t *testing.T) {
	cases := []struct {
		name    string
		opts    DHCPNetworkOptions
		wantErr bool
	}{
		{"ipvlan accepts", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "eth0", StableLease: true}, false},
		{"macvlan rejects", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0", StableLease: true}, true},
		{"bridge rejects", DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", StableLease: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateModeOptions(c.opts)
			if c.wantErr {
				if !errors.Is(err, util.ErrStableLeaseMode) {
					t.Fatalf("got %v, want ErrStableLeaseMode", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
