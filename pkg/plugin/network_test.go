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
