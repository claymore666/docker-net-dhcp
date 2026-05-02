package plugin

import (
	"errors"
	"testing"

	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

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
