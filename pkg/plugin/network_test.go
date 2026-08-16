// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

func TestDHCPStaticRoutes(t *testing.T) {
	got := dhcpStaticRoutes([]dhcp.Route{
		{Destination: "10.0.0.0/8", Gateway: "192.168.99.2"}, // next-hop
		{Destination: "172.16.0.0/12"},                       // on-link (empty gateway)
	})
	want := []*StaticRoute{
		{Destination: "10.0.0.0/8", RouteType: RouteTypeNextHop, NextHop: "192.168.99.2"},
		{Destination: "172.16.0.0/12", RouteType: RouteTypeOnLink},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dhcpStaticRoutes mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestDHCPStaticRoutes_Empty(t *testing.T) {
	if got := dhcpStaticRoutes(nil); len(got) != 0 {
		t.Errorf("dhcpStaticRoutes(nil) = %+v, want empty", got)
	}
}

func TestValidateModeOptions(t *testing.T) {
	cases := []struct {
		name    string
		opts    DHCPNetworkOptions
		wantErr error
	}{
		{
			name:    "bridge_default_mode_with_bridge_set",
			opts:    DHCPNetworkOptions{Bridge: "br0"},
			wantErr: nil,
		},
		{
			name:    "bridge_mode_explicit_with_bridge",
			opts:    DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0"},
			wantErr: nil,
		},
		{
			name:    "bridge_mode_missing_bridge",
			opts:    DHCPNetworkOptions{Mode: ModeBridge},
			wantErr: util.ErrBridgeRequired,
		},
		{
			name:    "bridge_mode_with_parent_rejected",
			opts:    DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", Parent: "ens18"},
			wantErr: util.ErrModeMismatch,
		},
		{
			name:    "bridge_mode_validate_dhcp_rejected",
			opts:    DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", ValidateDHCP: true},
			wantErr: util.ErrModeMismatch,
		},
		{
			name:    "default_mode_missing_bridge",
			opts:    DHCPNetworkOptions{},
			wantErr: util.ErrBridgeRequired,
		},

		{
			name:    "macvlan_with_parent",
			opts:    DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "ens18"},
			wantErr: nil,
		},
		{
			name:    "macvlan_missing_parent",
			opts:    DHCPNetworkOptions{Mode: ModeMacvlan},
			wantErr: util.ErrParentRequired,
		},
		{
			name:    "macvlan_with_bridge_rejected",
			opts:    DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "ens18", Bridge: "br0"},
			wantErr: util.ErrModeMismatch,
		},

		{
			name:    "ipvlan_with_parent",
			opts:    DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "ens18"},
			wantErr: nil,
		},
		{
			name:    "ipvlan_missing_parent",
			opts:    DHCPNetworkOptions{Mode: ModeIPvlan},
			wantErr: util.ErrParentRequired,
		},
		{
			name:    "ipvlan_with_bridge_rejected",
			opts:    DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "ens18", Bridge: "br0"},
			wantErr: util.ErrModeMismatch,
		},

		{
			name:    "invalid_mode",
			opts:    DHCPNetworkOptions{Mode: "wireguard", Bridge: "br0"},
			wantErr: util.ErrInvalidMode,
		},
		{
			name:    "invalid_mode_typo",
			opts:    DHCPNetworkOptions{Mode: "macvlann", Parent: "ens18"},
			wantErr: util.ErrInvalidMode,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateModeOptions(c.opts)
			switch {
			case c.wantErr == nil && err != nil:
				t.Errorf("expected nil, got %v", err)
			case c.wantErr != nil && err == nil:
				t.Errorf("expected error %v, got nil", c.wantErr)
			case c.wantErr != nil && !errors.Is(err, c.wantErr):
				t.Errorf("expected errors.Is(%v), got %v", c.wantErr, err)
			}
		})
	}
}

func TestParseExplicitV4(t *testing.T) {
	cases := []struct {
		name    string
		iface   *EndpointInterface
		wantIP  string
		wantErr bool
	}{
		{name: "nil_interface", iface: nil, wantIP: ""},
		{name: "empty_address", iface: &EndpointInterface{}, wantIP: ""},
		{
			name:   "valid_v4_cidr",
			iface:  &EndpointInterface{Address: "192.168.0.50/24"},
			wantIP: "192.168.0.50",
		},
		{
			name:   "valid_v4_short_mask",
			iface:  &EndpointInterface{Address: "10.0.0.1/8"},
			wantIP: "10.0.0.1",
		},
		{
			name:    "bare_ip_no_mask_rejected",
			iface:   &EndpointInterface{Address: "192.168.0.50"},
			wantErr: true,
		},
		{
			name:    "v6_rejected",
			iface:   &EndpointInterface{Address: "fe80::1/64"},
			wantErr: true,
		},
		{
			name:    "garbage",
			iface:   &EndpointInterface{Address: "not-an-ip"},
			wantErr: true,
		},
		{
			name:    "unspecified_v4_rejected",
			iface:   &EndpointInterface{Address: "0.0.0.0/0"},
			wantErr: true,
		},
		{
			name:    "unspecified_host_rejected",
			iface:   &EndpointInterface{Address: "0.0.0.0/24"},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip, err := parseExplicitV4(c.iface)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (ip=%q)", ip)
				}
				if err != nil && !errors.Is(err, util.ErrIPAM) {
					t.Errorf("expected ErrIPAM, got %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if ip != c.wantIP {
				t.Errorf("ip mismatch: got %q want %q", ip, c.wantIP)
			}
		})
	}
}

func TestParseDriverOptIP(t *testing.T) {
	cases := []struct {
		name    string
		opts    map[string]interface{}
		wantIP  string
		wantErr bool
	}{
		{name: "nil_map", opts: nil, wantIP: ""},
		{name: "absent", opts: map[string]interface{}{"other": "x"}, wantIP: ""},
		{name: "valid_v4", opts: map[string]interface{}{"ip": "192.168.0.55"}, wantIP: "192.168.0.55"},
		{name: "v4_short_form", opts: map[string]interface{}{"ip": "10.0.0.1"}, wantIP: "10.0.0.1"},
		{name: "cidr_form_rejected", opts: map[string]interface{}{"ip": "192.168.0.55/24"}, wantErr: true},
		{name: "v6_rejected", opts: map[string]interface{}{"ip": "fe80::1"}, wantErr: true},
		{name: "non_string_value", opts: map[string]interface{}{"ip": 42}, wantErr: true},
		{name: "empty_string", opts: map[string]interface{}{"ip": ""}, wantErr: true},
		{name: "garbage", opts: map[string]interface{}{"ip": "not-an-ip"}, wantErr: true},
		{name: "unspecified_rejected", opts: map[string]interface{}{"ip": "0.0.0.0"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip, err := parseDriverOptIP(c.opts)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (ip=%q)", ip)
				}
				if err != nil && !errors.Is(err, util.ErrIPAM) {
					t.Errorf("expected ErrIPAM, got %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if ip != c.wantIP {
				t.Errorf("ip mismatch: got %q want %q", ip, c.wantIP)
			}
		})
	}
}

func TestResolveExplicitV4(t *testing.T) {
	cases := []struct {
		name    string
		r       CreateEndpointRequest
		wantIP  string
		wantErr bool
	}{
		{name: "neither", wantIP: ""},
		{
			name:   "iface_only",
			r:      CreateEndpointRequest{Interface: &EndpointInterface{Address: "192.168.0.50/24"}},
			wantIP: "192.168.0.50",
		},
		{
			name:   "driver_opt_only",
			r:      CreateEndpointRequest{Options: map[string]interface{}{"ip": "192.168.0.50"}},
			wantIP: "192.168.0.50",
		},
		{
			name: "both_agree",
			r: CreateEndpointRequest{
				Interface: &EndpointInterface{Address: "192.168.0.50/24"},
				Options:   map[string]interface{}{"ip": "192.168.0.50"},
			},
			wantIP: "192.168.0.50",
		},
		{
			name: "both_disagree",
			r: CreateEndpointRequest{
				Interface: &EndpointInterface{Address: "192.168.0.50/24"},
				Options:   map[string]interface{}{"ip": "192.168.0.51"},
			},
			wantErr: true,
		},
		{
			name:    "invalid_iface_address",
			r:       CreateEndpointRequest{Interface: &EndpointInterface{Address: "not-an-ip"}},
			wantErr: true,
		},
		{
			name:    "invalid_driver_opt_ip",
			r:       CreateEndpointRequest{Options: map[string]interface{}{"ip": "not-an-ip"}},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip, err := resolveExplicitV4(c.r)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (ip=%q)", ip)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if ip != c.wantIP {
				t.Errorf("ip mismatch: got %q want %q", ip, c.wantIP)
			}
		})
	}
}

func TestResolveExplicitV6(t *testing.T) {
	cases := []struct {
		name    string
		r       CreateEndpointRequest
		wantIP  string
		wantErr bool
	}{
		{name: "nil_interface", wantIP: ""},
		{name: "empty_address", r: CreateEndpointRequest{Interface: &EndpointInterface{}}, wantIP: ""},
		{
			name:   "valid_v6_cidr",
			r:      CreateEndpointRequest{Interface: &EndpointInterface{AddressIPv6: "fd00:dead:beef::5/64"}},
			wantIP: "fd00:dead:beef::5",
		},
		{
			name:    "v4_rejected",
			r:       CreateEndpointRequest{Interface: &EndpointInterface{AddressIPv6: "192.168.0.50/24"}},
			wantErr: true,
		},
		{
			name:    "bare_addr_no_mask_rejected",
			r:       CreateEndpointRequest{Interface: &EndpointInterface{AddressIPv6: "fd00::5"}},
			wantErr: true,
		},
		{
			name:    "garbage",
			r:       CreateEndpointRequest{Interface: &EndpointInterface{AddressIPv6: "not-an-ip"}},
			wantErr: true,
		},
		{
			name:    "unspecified_v6_rejected",
			r:       CreateEndpointRequest{Interface: &EndpointInterface{AddressIPv6: "::/0"}},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip, err := resolveExplicitV6(c.r)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (ip=%q)", ip)
				}
				if err != nil && !errors.Is(err, util.ErrIPAM) {
					t.Errorf("expected ErrIPAM, got %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if ip != c.wantIP {
				t.Errorf("ip mismatch: got %q want %q", ip, c.wantIP)
			}
		})
	}
}

func TestValidateIPAMData(t *testing.T) {
	cases := []struct {
		name    string
		ipv4    []*IPAMData
		wantErr bool
	}{
		{
			name: "null_pool_zero_zero",
			ipv4: []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
		},
		{
			name:    "missing_null_address_space",
			ipv4:    []*IPAMData{{AddressSpace: "default", Pool: "0.0.0.0/0"}},
			wantErr: true,
		},
		{
			name:    "non_zero_pool",
			ipv4:    []*IPAMData{{AddressSpace: "null", Pool: "10.0.0.0/8"}},
			wantErr: true,
		},
		{
			name: "empty_ipv4_data",
			ipv4: nil,
		},
		{
			name: "multiple_valid",
			ipv4: []*IPAMData{
				{AddressSpace: "null", Pool: "0.0.0.0/0"},
				{AddressSpace: "null", Pool: "0.0.0.0/0"},
			},
		},
		{
			name: "one_valid_one_invalid",
			ipv4: []*IPAMData{
				{AddressSpace: "null", Pool: "0.0.0.0/0"},
				{AddressSpace: "default", Pool: "0.0.0.0/0"},
			},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateIPAMData(c.ipv4)
			if c.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if c.wantErr && err != nil && !errors.Is(err, util.ErrIPAM) {
				t.Errorf("expected ErrIPAM, got %v", err)
			}
		})
	}
}

// TestSandboxGone is the discriminator behind #373: it decides whether a
// failed persistent-client start is a plugin fault (running container
// with no renewal client — healthy-affecting) or a container that simply
// exited mid-attach (benign).
//
// Getting this backwards is expensive in both directions: false "gone"
// hides a real fault from /Plugin.Health, false "present" pages an
// operator every time a short-lived container exits.
func TestSandboxGone(t *testing.T) {
	dir := t.TempDir()
	dirs := []string{dir}

	present := filepath.Join(dir, "netns-alive")
	if err := os.WriteFile(present, nil, 0o644); err != nil {
		t.Fatalf("seed sandbox file: %v", err)
	}

	t.Run("existing sandbox is not gone", func(t *testing.T) {
		if sandboxGoneIn(dirs, present) {
			t.Error("reported gone for a sandbox that exists; a real start failure would be silently downgraded and never reach healthy")
		}
	})

	t.Run("unlinked sandbox is gone", func(t *testing.T) {
		if !sandboxGoneIn(dirs, filepath.Join(dir, "netns-vanished")) {
			t.Error("reported present for a sandbox that does not exist; a normal fast container exit would flip healthy to false (#373)")
		}
	})

	t.Run("empty key is not gone", func(t *testing.T) {
		// No evidence is not evidence of absence. An empty SandboxKey
		// must fall back to treating the failure as real, rather than
		// swallowing every failure on a daemon that stops sending it.
		if sandboxGoneIn(dirs, "") {
			t.Error("empty sandbox key treated as gone; that would suppress every join-start failure")
		}
	})

	t.Run("key outside the permitted dirs is not gone", func(t *testing.T) {
		// The file genuinely does not exist, so an unvalidated stat
		// would say "gone". Rejecting on shape must win: an
		// unrecognised key is no evidence, not negative evidence.
		if sandboxGoneIn(dirs, filepath.Join(t.TempDir(), "elsewhere")) {
			t.Error("accepted a sandbox key outside the permitted netns dirs; unrecognised shapes must degrade to counting a real failure")
		}
	})

	t.Run("production dirs are wired in", func(t *testing.T) {
		// Guards against sandboxGone being left pointed at a test or
		// empty list, which would make it answer false for every real
		// Join and silently restore the pre-#373 behaviour.
		//
		// Asserted on the validation rather than on sandboxGone itself:
		// the stat result depends on privilege (see the EACCES subtest
		// below), and this test must mean the same thing whether it runs
		// as root on the integration runner or as an ordinary user.
		if len(sandboxNetnsDirs) == 0 {
			t.Fatal("sandboxNetnsDirs is empty; sandboxGone can never fire")
		}
		dir, name := splitSandboxKeyIn(sandboxNetnsDirs, "/var/run/docker/netns/36a98db54ebf")
		if dir == "" {
			t.Error("production dirs rejected a well-formed libnetwork sandbox key; sandboxGone would answer 'not gone' for every real Join")
		}
		if name != "36a98db54ebf" {
			t.Errorf("sandbox name = %q, want the bare netns id", name)
		}
	})

	t.Run("unreadable parent is not gone", func(t *testing.T) {
		// sandboxGone keys on ErrNotExist specifically, not on "stat
		// failed". A permission error is not evidence the container
		// went away, so it must degrade to counting a real failure.
		//
		// This is not hypothetical: /var/run/docker is 0700 root, so an
		// unprivileged caller gets EACCES for every sandbox key. The
		// plugin runs as root and sees ENOENT; anything else must not
		// quietly read as "gone".
		if os.Geteuid() == 0 {
			t.Skip("running as root; EACCES is not reachable")
		}
		locked := filepath.Join(t.TempDir(), "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatalf("seed unreadable dir: %v", err)
		}
		if sandboxGoneIn([]string{locked}, filepath.Join(locked, "absent")) {
			t.Error("a permission error was read as 'container gone'; only ErrNotExist may downgrade a start failure")
		}
	})
}

// TestSplitSandboxKeyIn covers the validation that keeps a Join
// request's path data out of an unconstrained filesystem call
// (CodeQL go/path-injection, #374). Every rejection here must return an
// empty dir, which sandboxGoneIn turns into "not gone" — the
// conservative answer that counts a real failure.
func TestSplitSandboxKeyIn(t *testing.T) {
	const okDir = "/var/run/docker/netns"
	dirs := []string{okDir, "/run/docker/netns"}

	tests := []struct {
		name     string
		key      string
		wantDir  string
		wantName string
	}{
		{"libnetwork sandbox key", "/var/run/docker/netns/36a98db54ebf", okDir, "36a98db54ebf"},
		{"alternate run prefix", "/run/docker/netns/abc123", "/run/docker/netns", "abc123"},
		{"uncleaned but equivalent", "/var/run/docker/netns/./36a98db54ebf", okDir, "36a98db54ebf"},
		{"traversal resolving back in", "/var/run/docker/netns/sub/../36a98db54ebf", okDir, "36a98db54ebf"},

		{"empty", "", "", ""},
		{"traversal escaping the dir", "/var/run/docker/netns/../../../etc/passwd", "", ""},
		{"unrelated absolute path", "/etc/passwd", "", ""},
		{"relative path", "netns/abc", "", ""},
		{"the dir itself", "/var/run/docker/netns", "", ""},
		{"nested one level deeper", "/var/run/docker/netns/sub/abc", "", ""},
		{"root", "/", "", ""},
		{"prefix lookalike", "/var/run/docker/netns-evil/abc", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, name := splitSandboxKeyIn(dirs, tc.key)
			if dir != tc.wantDir || name != tc.wantName {
				t.Errorf("splitSandboxKeyIn(%q) = (%q, %q), want (%q, %q)",
					tc.key, dir, name, tc.wantDir, tc.wantName)
			}
		})
	}
}

// The three error shapes below are verbatim from the integration run
// that #401 was filed on. Every one of them was counted as a plugin
// fault; every one of them means the container had already gone.
func TestJoinAbortedByVanish(t *testing.T) {
	// A sandbox key that does not resolve to a permitted directory, so
	// sandboxGone answers false and cannot rescue any of these cases.
	// That is deliberate: each subtest has to be classified by its
	// error alone, which is the whole point of the change.
	const unhelpfulKey = "/somewhere/else/abc123"

	t.Run("daemon says no such container", func(t *testing.T) {
		err := fmt.Errorf("failed to get Docker container info: %w",
			fmt.Errorf("Error response from daemon: No such container: deadbeef: %w", cerrdefs.ErrNotFound))
		if !joinAbortedByVanish(err, unhelpfulKey) {
			t.Error("a removed container was counted as a plugin fault")
		}
	})

	t.Run("sandbox netns is gone", func(t *testing.T) {
		// The shape AwaitNetNS now produces: the deadline, with the
		// last attempt kept in the chain rather than only in the text.
		err := fmt.Errorf("failed to get sandbox network namespace: %w",
			fmt.Errorf("%w (last attempt: %w)", context.DeadlineExceeded, syscall.ENOENT))
		if !joinAbortedByVanish(err, unhelpfulKey) {
			t.Error("a vanished sandbox netns was counted as a plugin fault")
		}
	})

	t.Run("v6 client cannot open the container netns", func(t *testing.T) {
		err := fmt.Errorf("failed to start DHCPv6 client: %w",
			fmt.Errorf("failed to open network namespace `/proc/8783/ns/net`: %w", syscall.ENOENT))
		if !joinAbortedByVanish(err, unhelpfulKey) {
			t.Error("a vanished container netns was counted as a plugin fault")
		}
	})

	// The other half of the contract, and the more important half: this
	// must not become a blanket excuse. #373 and #376 both took the
	// stance that no usable evidence is not evidence of absence.
	t.Run("a real fault for a container that is still there stays a fault", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"daemon unreachable", fmt.Errorf("failed to get Docker container info: %w",
				fmt.Errorf("%w (last attempt: %w)", context.DeadlineExceeded, errors.New("connection refused")))},
			{"permission denied on the netns", fmt.Errorf("failed to get sandbox network namespace: %w", syscall.EACCES)},
			{"dhcpcd refused to start", errors.New("failed to start DHCP client: exec format error")},
			{"nil error", nil},
		} {
			if joinAbortedByVanish(tc.err, unhelpfulKey) {
				t.Errorf("%s: excused as a vanished container; it is a real fault", tc.name)
			}
		}
	})

	t.Run("the sandbox-key answer still works on its own", func(t *testing.T) {
		dir := t.TempDir()
		saved := sandboxNetnsDirs
		sandboxNetnsDirs = []string{dir}
		t.Cleanup(func() { sandboxNetnsDirs = saved })

		// An error carrying no evidence either way, so only the key can
		// decide — this is the #373 path, which must be untouched.
		err := errors.New("failed to start DHCP client: exec format error")
		if !joinAbortedByVanish(err, filepath.Join(dir, "vanished")) {
			t.Error("#373's sandbox-key evidence stopped working")
		}
	})
}

// TestJoinFailure_TeardownCancelIsNotAFault pins the classification the
// #406 grace made necessary.
//
// Adding a cancellation path changed what a cancelled attach looks
// like: run 30700597210 reported six join_start_failures carrying
// `context canceled` — every one an endpoint that was being torn down
// while its attach was still running. Nothing was left without a
// renewal client, because nothing was left. Counting those as faults
// would have turned a normal Leave into a health-affecting error, which
// is the same mistake #373 and #376 each had to undo once.
//
// The flag is checked rather than the error, deliberately: a cancelled
// context can come from somewhere that is not a teardown, and excusing
// every context.Canceled would be the blanket amnesty those two issues
// were careful not to grant.
func TestJoinFailure_TeardownCancelIsNotAFault(t *testing.T) {
	m := &dhcpManager{startedCh: make(chan struct{})}

	if m.attachAborted.Load() {
		t.Fatal("a fresh manager already claims its attach was aborted")
	}

	// Stop is what sets it, and only Stop.
	ctx, cancel := context.WithCancel(context.Background())
	m.attachCancel = cancel
	m.startErr = context.Canceled
	close(m.startedCh)
	m.plugin = &Plugin{}
	_ = m.Stop()
	<-ctx.Done()

	if !m.attachAborted.Load() {
		t.Error("Stop cancelled the attach without recording that it did; " +
			"the resulting 'context canceled' will be counted as a plugin fault")
	}
}

// TestJoinFailureLeavesAddressUnused pins the predicate that decides
// whether a failed attach hands its address back (#566).
//
// The negative half is the important half. A reclaim that fires on the
// wrong error takes an address away from a container that is using it —
// the same duplicate assignment #524 was about, except caused by us —
// so this asserts the default is "do not release" and that only one
// error opts in.
func TestJoinFailureLeavesAddressUnused(t *testing.T) {
	t.Run("no container claimed the endpoint", func(t *testing.T) {
		if !joinFailureLeavesAddressUnused(util.ErrNoContainer) {
			t.Error("ErrNoContainer did not release the address; the lease it took is leaked (#566)")
		}
	})

	t.Run("wrapped, as the attach path actually produces it", func(t *testing.T) {
		err := fmt.Errorf("failed to find container: %w", util.ErrNoContainer)
		if !joinFailureLeavesAddressUnused(err) {
			t.Error("a wrapped ErrNoContainer was not recognised; errors.Is is required, not ==")
		}
	})

	// Every one of these is compatible with a RUNNING container holding
	// the address. Releasing on any of them is worse than the leak it
	// would fix.
	t.Run("a live container keeps its address", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"nil error", nil},
			{"daemon unreachable", fmt.Errorf("failed to get Docker container info: %w",
				fmt.Errorf("%w (last attempt: %w)", context.DeadlineExceeded, errors.New("connection refused")))},
			{"permission denied on the netns", fmt.Errorf("failed to get sandbox network namespace: %w", syscall.EACCES)},
			{"dhcpcd refused to start", errors.New("failed to start DHCP client: exec format error")},
			{"attach timed out", context.DeadlineExceeded},
			{"no sandbox", util.ErrNoSandbox},
		} {
			if joinFailureLeavesAddressUnused(tc.err) {
				t.Errorf("%s: released the address of a container that may still be running — "+
					"this is #524's duplicate assignment, manufactured by the plugin", tc.name)
			}
		}
	})
}

// TestSandboxNetnsVisibleIn pins the diagnostic that makes #567's dead
// branch observable.
//
// The point of separating this from sandboxGoneIn is that sandboxGoneIn
// answers false for BOTH "the entry is there" and "I cannot read the
// directory", correctly — for its purpose those mean the same thing.
// That folding is exactly what let an unreachable directory look
// healthy for every release up to #567. These cases exist to keep the
// two distinguishable from outside the process.
func TestSandboxNetnsVisibleIn(t *testing.T) {
	populated := t.TempDir()
	for _, name := range []string{"aaaa", "bbbb", "cccc"} {
		if err := os.WriteFile(filepath.Join(populated, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	empty := t.TempDir()
	missing := filepath.Join(t.TempDir(), "not-created")

	cases := []struct {
		name string
		dirs []string
		want int32
	}{
		{
			// The mount is missing — the state every shipped release
			// was in. Must be -1 and never 0: a zero here would be
			// indistinguishable from a host with no containers, which
			// is the confusion this field exists to end.
			name: "no readable directory is -1, not 0",
			dirs: []string{missing},
			want: -1,
		},
		{
			name: "no directories at all is -1",
			dirs: nil,
			want: -1,
		},
		{
			// Readable and genuinely empty. Legitimate on an idle host,
			// and only dangerous when endpoints are attached — which is
			// why the health field is documented to be read against
			// active_endpoints rather than alone.
			name: "a readable empty directory is 0",
			dirs: []string{empty},
			want: 0,
		},
		{
			name: "entries are counted",
			dirs: []string{populated},
			want: 3,
		},
		{
			// THE DOUBLE-COUNT GUARD. /var/run is a symlink to /run on
			// most hosts, so both entries in sandboxNetnsDirs name the
			// same directory. Summing would report six for three
			// sandboxes and make the number useless for the comparison
			// it exists to serve.
			name: "the same directory reached twice is not counted twice",
			dirs: []string{populated, populated},
			want: 3,
		},
		{
			// An unreadable first entry must not mask a readable
			// second one, or a host whose netns lives under /run gets
			// -1 while the evidence is right there.
			name: "an unreadable directory falls through to a readable one",
			dirs: []string{missing, populated},
			want: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandboxNetnsVisibleIn(tc.dirs); got != tc.want {
				t.Errorf("sandboxNetnsVisibleIn(%v) = %d, want %d", tc.dirs, got, tc.want)
			}
		})
	}
}

// The production directory list must be what the manifest mounts.
//
// This is the assertion that was missing. network_test.go tested
// sandboxGoneIn thoroughly by injecting t.TempDir()s, so it proved the
// logic and said nothing about whether production's input was
// reachable — the parameter that made the function testable is the
// parameter that let the tests never touch the failing case (#567).
//
// It cannot check that the mount WORKS from here; that needs a running
// plugin and is asserted by the integration suite against
// sandbox_netns_visible. It can check that nobody removes the mount
// while leaving the code that depends on it, which is the regression
// that would restore the dead branch silently.
func TestSandboxNetnsDirsAreMounted(t *testing.T) {
	for _, name := range pluginManifests {
		t.Run(name, func(t *testing.T) {
			mounted := make(map[string]bool)
			for _, m := range readPluginManifest(t, name) {
				mounted[m.Destination] = true
			}

			var found bool
			for _, dir := range sandboxNetnsDirs {
				for dest := range mounted {
					if mountCovers(dest, dir) {
						found = true
						break
					}
				}
			}
			if !found {
				t.Errorf("no mount in %s covers any of sandboxNetnsDirs %v — os.ReadDir will "+
					"fail on every one of them inside the plugin, sandboxGone will answer \"no "+
					"usable evidence\" forever, and nothing will say so (#567). Mounts "+
					"present: %v", name, sandboxNetnsDirs, mounted)
			}
		})
	}
}

// pluginManifests is every manifest that ships a plugin, because a gate
// that reads one of them cannot see a mount added to the other.
// config-cover.json builds the instrumented plugin the coverage lane
// enables, and it carried the same lazily-created bind source (#588) —
// a lane that runs once per release would have failed on it long after
// the PR that introduced it.
var pluginManifests = []string{"config.json", "config-cover.json"}

type manifestMount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Type        string `json:"type"`
}

func readPluginManifest(t *testing.T, name string) []manifestMount {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var manifest struct {
		Mounts []manifestMount `json:"mounts"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if len(manifest.Mounts) == 0 {
		t.Fatalf("%s declares no mounts; the guards below would pass on an empty list", name)
	}
	return manifest.Mounts
}

// mountCovers reports whether a mount at dest makes dir readable inside
// the plugin, which is true when dest IS dir or an ancestor of it.
//
// An ancestor counts because a bind mount shares the source's directory
// tree: entries created under it afterwards are visible through the
// mount without any propagation, since creating a directory is not
// creating a mount. That is not a technicality here, it is the fix for
// #588 — /var/run/docker/netns does not exist until the daemon's first
// sandbox, so the manifest mounts the parent and the netns directory
// appears inside the plugin when libnetwork creates it. Verified with a
// plugin enabled before the directory existed: sandbox_netns_visible
// went -1 -> 1 on the same process, matching the host's entry.
func mountCovers(dest, dir string) bool {
	if dest == dir {
		return true
	}
	return strings.HasPrefix(dir, strings.TrimSuffix(dest, "/")+"/")
}

// enableTimeMountSources are the only bind sources config.json may name,
// each with the reason it is present on a host that has just installed
// the plugin and done nothing else.
//
// The daemon does not create a missing bind source (#440), so a mount
// whose source does not exist fails the enable and takes the whole
// install with it. The question a new mount has to answer is therefore
// not "does this path exist on my machine" but "does it exist on a host
// whose daemon has never created a network sandbox" — which is every
// first install, and almost no machine anyone tests on.
var enableTimeMountSources = map[string]string{
	"/var/run/docker.sock": "the daemon's own socket; the plugin cannot be called at all without it",
	"/var/run/docker":      "created at daemon start — it holds plugins/, which must exist before any plugin can be enabled",
	"/var/lib/net-dhcp":    "STATE_DIR, created by the operator; every install doc says so (#440)",
	"/var/lib/dh-cover":    "GOCOVERDIR for the instrumented plugin; coverage.yml mkdir -p's it before enabling, and it never ships in config.json",
}

// lazyMountSources are paths the daemon creates on demand rather than at
// startup. Naming one as a bind source builds an install that works on
// every machine that has run a container and fails on every machine that
// has not.
var lazyMountSources = map[string]string{
	"/var/run/docker/netns": "libnetwork creates it on the first sandbox, not at daemon start",
	"/run/docker/netns":     "same directory by the other name",
}

// A bind source that does not exist yet fails the enable, so every one
// of them has to be a path the daemon has already made by the time it
// enables plugins.
//
// This is #588 written down as a check. v1.6.0-rc2 mounted
// /var/run/docker/netns, which libnetwork does not create until the
// first container sandbox. Every host we own had run a container, so the
// directory was always there: the integration suite passed, the coverage
// lane passed, and production would have upgraded without a murmur. The
// only machine that could see it was a hosted runner installing onto a
// daemon that had never started anything — verify-install, which caught
// it, but only by accident of being fresh rather than by asking.
//
// TestSandboxNetnsDirsAreMounted guards the opposite direction: that the
// mount does not disappear. Neither one implies the other, and this
// release needed both.
func TestPluginMountSourcesExistAtEnableTime(t *testing.T) {
	for _, name := range pluginManifests {
		t.Run(name, func(t *testing.T) {
			for _, m := range readPluginManifest(t, name) {
				if m.Type != "bind" {
					continue
				}
				if why, lazy := lazyMountSources[m.Source]; lazy {
					t.Errorf("%s bind-mounts %q, which the daemon creates lazily (%s). "+
						"`docker plugin install` fails on any host whose daemon has never "+
						"created a network sandbox, and succeeds on every host that has — so "+
						"CI and production both stay silent (#588). Mount the parent instead: "+
						"entries created under a bind mount afterwards are visible through it.",
						name, m.Source, why)
					continue
				}
				if _, ok := enableTimeMountSources[m.Source]; !ok {
					t.Errorf("%s bind-mounts %q, which is not a reviewed enable-time source. "+
						"A bind source that does not exist when the daemon enables the plugin "+
						"fails the enable and the install with it (#440, #588). If this path is "+
						"genuinely present on a host that has only just installed the plugin, "+
						"add it to enableTimeMountSources with the reason; do not delete this "+
						"check. Reviewed sources: %v", name, m.Source, enableTimeMountSources)
				}
			}
		})
	}
}
