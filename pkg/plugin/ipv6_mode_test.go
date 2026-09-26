// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func TestIPv6Mode_TheOptionPairResolvesToOneMode(t *testing.T) {
	cases := []struct {
		name    string
		ipv6    bool
		mode    string
		want    proto.Mode6
		enabled bool
		wantErr bool
	}{
		{"nothing set", false, "", proto.Mode6Off, false, false},
		{"ipv6=true alone", true, "", proto.Mode6DHCP, true, false},
		{"ipv6_mode=off alone", false, "off", proto.Mode6Off, false, false},
		{"ipv6_mode=dhcp alone", false, "dhcp", proto.Mode6DHCP, true, false},
		{"ipv6_mode=slaac alone", false, "slaac", proto.Mode6SLAAC, true, false},
		{"ipv6_mode=auto alone", false, "auto", proto.Mode6Auto, true, false},
		{"ipv6=true and ipv6_mode=dhcp", true, "dhcp", proto.Mode6DHCP, true, false},
		{"ipv6=true and ipv6_mode=slaac", true, "slaac", proto.Mode6SLAAC, true, false},
		{"ipv6=true and ipv6_mode=off", true, "off", proto.Mode6Off, false, true},
		{"a typo", false, "slack", proto.Mode6Off, false, true},
	}

	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.mode] = true
	}
	for _, m := range dhcp.IPv6Modes() {
		if !covered[m] {
			t.Fatalf("no row for ipv6_mode=%s. Every accepted value belongs in this "+
				"table; the option's meaning is not stated anywhere else.", m)
		}
	}
	if !covered[""] {
		t.Fatal("no row for an unset ipv6_mode, which is what every network created " +
			"before this option looks like")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := DHCPNetworkOptions{Bridge: "br0", IPv6: tc.ipv6, IPv6Mode: tc.mode}
			got, err := opts.ipv6Mode()
			if (err != nil) != tc.wantErr {
				t.Fatalf("ipv6Mode() error = %v, want an error: %v", err, tc.wantErr)
			}
			if err != nil {
				if !errors.Is(err, util.ErrIPAM) {
					t.Errorf("the refusal is not an ErrIPAM, so Docker reports it as a "+
						"plugin fault rather than as bad input: %v", err)
				}
				// The zero value of proto.Mode6 is Mode6DHCP, so a refusal must return Mode6Off (#817).
				if got != proto.Mode6Off {
					t.Errorf("ipv6Mode() returned %v beside its error, want %v", got, proto.Mode6Off)
				}
				return
			}
			if got != tc.want {
				t.Errorf("ipv6Mode() = %v, want %v", got, tc.want)
			}
			if opts.ipv6Enabled() != tc.enabled {
				t.Errorf("ipv6Enabled() = %v, want %v — this is what decides whether the "+
					"endpoint gets a v6 identity, a v6 record and a v6 client at all",
					opts.ipv6Enabled(), tc.enabled)
			}
		})
	}

	for _, opts := range []DHCPNetworkOptions{
		{Bridge: "br0", IPv6: true, IPv6Mode: "off"},
		{Bridge: "br0", IPv6Mode: "slack"},
	} {
		if opts.ipv6Enabled() {
			t.Errorf("ipv6Enabled() is true for a refused pair %+v", opts)
		}
	}
}

// The decoded record cannot tell an absent ipv6 key from an explicit false, so this refusal lives at create (#817).
func TestValidateIPv6Options_TheWrittenOutContradiction(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]interface{}
		wantErr bool
	}{
		{"ipv6_mode alone", map[string]interface{}{"bridge": "br0", "ipv6_mode": "slaac"}, false},
		{"ipv6=false written out beside a mode", map[string]interface{}{"bridge": "br0", "ipv6": "false", "ipv6_mode": "slaac"}, true},
		{"ipv6=false written out beside dhcp", map[string]interface{}{"bridge": "br0", "ipv6": "false", "ipv6_mode": "dhcp"}, true},
		{"ipv6=false with no mode", map[string]interface{}{"bridge": "br0", "ipv6": "false"}, false},
		{"ipv6=false and ipv6_mode=off", map[string]interface{}{"bridge": "br0", "ipv6": "false", "ipv6_mode": "off"}, false},
		{"ipv6=true beside a mode", map[string]interface{}{"bridge": "br0", "ipv6": "true", "ipv6_mode": "slaac"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, set, err := decodeOptsSet(tc.input)
			if err != nil {
				t.Fatalf("decodeOptsSet: %v", err)
			}
			err = validateIPv6Options(opts, set)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateIPv6Options = %v, want an error: %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, util.ErrIPAM) {
				t.Errorf("the refusal is not an ErrIPAM: %v", err)
			}
		})
	}

	_, set, err := decodeOptsSet(map[string]interface{}{"bridge": "br0", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("decodeOptsSet: %v", err)
	}
	if set["IPv6"] {
		t.Error("decodeOptsSet reports `ipv6` as written for options that do not carry it")
	}
	_, set, err = decodeOptsSet(map[string]interface{}{"bridge": "br0", "ipv6": "false"})
	if err != nil {
		t.Fatalf("decodeOptsSet: %v", err)
	}
	if !set["IPv6"] {
		t.Error("decodeOptsSet does not report an explicitly written `ipv6=false`, so the " +
			"contradiction it exists to catch cannot be seen at all")
	}
}

// DHCPv6 stays allowed on ipvlan: its identity is a per-endpoint DUID-UUID, not the shared MAC (#895).
func TestValidateIPv6Options_SLAACOnIPvlan(t *testing.T) {
	cases := []struct {
		mode    string
		attach  string
		wantErr bool
	}{
		{"slaac", "ipvlan", true},
		{"auto", "ipvlan", true},
		{"dhcp", "ipvlan", false},
		{"slaac", "macvlan", false},
		{"auto", "macvlan", false},
		{"slaac", "bridge", false},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.attach, func(t *testing.T) {
			opts := DHCPNetworkOptions{Mode: tc.attach, IPv6Mode: tc.mode}
			if tc.attach == "bridge" {
				opts.Bridge = "br0"
			} else {
				opts.Parent = "eth0"
			}
			err := validateIPv6Options(opts, map[string]bool{"IPv6Mode": true})
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateIPv6Options(ipv6_mode=%s, mode=%s) = %v, want an error: %v",
					tc.mode, tc.attach, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, util.ErrModeMismatch) {
				t.Errorf("the refusal is not an ErrModeMismatch: %v", err)
			}
			if !strings.Contains(err.Error(), "ipv6_mode=dhcp") {
				t.Errorf("the refusal does not name the mode that does work on ipvlan: %v", err)
			}
		})
	}
}

// Docker replays CreateNetwork at every plugin start, so the create and stored paths must refuse the same set (#817).
func TestIPv6Mode_TheCreateAndStoredPathsRefuseTheSameSet(t *testing.T) {
	cases := []struct {
		name    string
		opts    DHCPNetworkOptions
		wantErr bool
	}{
		{"unset", DHCPNetworkOptions{Bridge: "br0"}, false},
		{"ipv6=true", DHCPNetworkOptions{Bridge: "br0", IPv6: true}, false},
		{"dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp"}, false},
		{"slaac", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac"}, false},
		{"auto", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto"}, false},
		{"off", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "off"}, false},
		{"auto and strict", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto", IPv6AutoStrict: true}, false},
		{"dhcp on ipvlan", DHCPNetworkOptions{Mode: "ipvlan", Parent: "eth0", IPv6Mode: "dhcp"}, false},
		{"the contradiction", DHCPNetworkOptions{Bridge: "br0", IPv6: true, IPv6Mode: "off"}, true},
		{"a typo", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slack"}, true},
		{"slaac on ipvlan", DHCPNetworkOptions{Mode: "ipvlan", Parent: "eth0", IPv6Mode: "slaac"}, true},
		{"auto on ipvlan", DHCPNetworkOptions{Mode: "ipvlan", Parent: "eth0", IPv6Mode: "auto"}, true},

		// ipv6_main_prefix is accepted in the two forming modes, and refused elsewhere and when it is not a prefix
		// (#818).
		{"a main prefix in slaac", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac",
			IPv6MainPrefix: "2001:db8:1::/64"}, false},
		{"a main prefix in auto", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto",
			IPv6MainPrefix: "2001:db8:1::/64"}, false},
		{"a main prefix in dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp",
			IPv6MainPrefix: "2001:db8:1::/64"}, true},
		{"a main prefix with no mode at all", DHCPNetworkOptions{Bridge: "br0", IPv6: true,
			IPv6MainPrefix: "2001:db8:1::/64"}, true},
		{"a main prefix in off", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "off",
			IPv6MainPrefix: "2001:db8:1::/64"}, true},
		{"a main prefix that is an address", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac",
			IPv6MainPrefix: "2001:db8:1::5"}, true},
		{"a main prefix with host bits", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac",
			IPv6MainPrefix: "2001:db8:1::5/64"}, true},
		{"a v4 main prefix", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac",
			IPv6MainPrefix: "192.168.99.0/24"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			createErr := validateIPv6Options(tc.opts, nil)
			if (createErr != nil) != tc.wantErr {
				t.Errorf("validateIPv6Options = %v, want an error: %v", createErr, tc.wantErr)
			}

			p := &Plugin{}
			storedErr := p.checkStoredOptions("n1", tc.opts)
			if (storedErr != nil) != tc.wantErr {
				t.Fatalf("checkStoredOptions = %v, want an error: %v", storedErr, tc.wantErr)
			}
			want := int32(0)
			if tc.wantErr {
				want = 1
			}
			if got := p.networkOptionsRejected.Load(); got != want {
				t.Errorf("network_options_rejected = %d, want %d: an operator's only "+
					"machine-readable sign that a stored option was refused", got, want)
			}
		})
	}
}

func TestV6Wiring_CarriesEveryFieldTheV6ClientNeeds(t *testing.T) {
	id6 := dhcp.Identity6{DUID: []byte{0, 4, 1, 2, 3, 4}, IAID: 0x11223344}

	cases := []struct {
		name        string
		opts        DHCPNetworkOptions
		wantMode    proto.Mode6
		wantStrict  bool
		wantCB      bool
		wantIgnored bool
		wantMain    string
	}{
		{"dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp"}, proto.Mode6DHCP, false, false, false, ""},
		{"ipv6=true alone", DHCPNetworkOptions{Bridge: "br0", IPv6: true}, proto.Mode6DHCP, false, false, false, ""},
		{"slaac", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac"}, proto.Mode6SLAAC, false, false, true, ""},
		{"auto", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto"}, proto.Mode6Auto, false, true, true, ""},
		{"auto and strict", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto", IPv6AutoStrict: true}, proto.Mode6Auto, true, true, true, ""},
		// The prefix Docker reports for an endpoint that holds
		// several, carried the same way (#818).
		{"slaac with a main prefix", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac",
			IPv6MainPrefix: "2001:db8:1::/64"}, proto.Mode6SLAAC, false, false, true, "2001:db8:1::/64"},
		{"strict in dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6AutoStrict: true}, proto.Mode6DHCP, true, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			var base dhcp.DHCPClientOptions
			if err := p.v6Wiring(&base, tc.opts, id6, "rec-1", "2001:db8::5", "endpoint-1"); err != nil {
				t.Fatalf("v6Wiring: %v", err)
			}
			if base.Mode6 != tc.wantMode {
				t.Errorf("Mode6 = %v, want %v", base.Mode6, tc.wantMode)
			}
			if base.StrictAuto6 != tc.wantStrict {
				t.Errorf("StrictAuto6 = %v, want %v: the option an operator set would be "+
					"stored, documented and never read", base.StrictAuto6, tc.wantStrict)
			}
			// The ignored-prefix callback is armed only in forming modes: on dhcp every advertisement's prefix is
			// refused, and routers readvertise every few seconds (RFC 4861 section 6.2.1), so the counter would climb
			// forever (#818).
			if (base.OnV6PrefixesIgnored != nil) != tc.wantIgnored {
				t.Errorf("OnV6PrefixesIgnored set = %v, want %v",
					base.OnV6PrefixesIgnored != nil, tc.wantIgnored)
			}
			switch {
			case tc.wantMain == "" && base.MainPrefix6.IsValid():
				t.Errorf("MainPrefix6 = %v on a network that named none", base.MainPrefix6)
			case tc.wantMain != "" && base.MainPrefix6.String() != tc.wantMain:
				t.Errorf("MainPrefix6 = %v, want %s: the option would be stored, documented "+
					"and never read", base.MainPrefix6, tc.wantMain)
			}
			if (base.OnV6Fallback != nil) != tc.wantCB {
				t.Errorf("OnV6Fallback set = %v, want %v: only auto can fall back, and a "+
					"callback armed elsewhere would log a sentence naming a mode the network "+
					"is not in", base.OnV6Fallback != nil, tc.wantCB)
			}
			if string(base.Identity6.DUID) != string(id6.DUID) || base.Identity6.IAID != id6.IAID {
				t.Errorf("Identity6 = %+v, want %+v", base.Identity6, id6)
			}
			if base.RecordID != "rec-1" {
				t.Errorf("RecordID = %q, want the v6 record", base.RecordID)
			}
			if base.PreferredV6 != "2001:db8::5" {
				t.Errorf("PreferredV6 = %q, want the stored hint. It is dropped where it "+
					"cannot be asked for, and that happens in buildParams6, which is the one "+
					"place that knows what a mode can ask", base.PreferredV6)
			}
		})
	}

	var base dhcp.DHCPClientOptions
	p := &Plugin{}
	if err := p.v6Wiring(&base, DHCPNetworkOptions{Bridge: "br0"}, id6, "rec-1", "", "endpoint-1"); err == nil {
		t.Error("v6Wiring accepted a network whose ipv6_mode is off")
	} else if !errors.Is(err, util.ErrIPAM) {
		t.Errorf("the refusal is not an ErrIPAM: %v", err)
	}
	if base.Mode6 != proto.Mode6DHCP || base.Identity6.IAID != 0 {
		t.Error("v6Wiring wrote into the options it refused, so a caller that ignored the " +
			"error would get a client rather than nothing")
	}

	var noPlugin dhcp.DHCPClientOptions
	var nilP *Plugin
	if err := nilP.v6Wiring(&noPlugin, DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto"}, id6, "rec-1", "", "e"); err != nil {
		t.Fatalf("v6Wiring with a nil plugin: %v", err)
	}
	if noPlugin.Mode6 != proto.Mode6Auto || noPlugin.Identity6.IAID != id6.IAID {
		t.Error("a manager with no plugin behind it would run a client in the wrong mode")
	}
	if noPlugin.OnV6Fallback != nil || noPlugin.OnV6PrefixesIgnored != nil {
		t.Error("a nil plugin armed a callback that would dereference it")
	}
}

// Measured against the pinned mapstructure: an empty string decodes to false and registers in Metadata.Keys,
// so `-o ipv6=` was refused as an explicit false (#817).
func TestDecodeOptsSet_AnEmptyValueIsNotAValue(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]interface{}
		wantSet bool
		wantErr bool
	}{
		{"-o ipv6= beside a mode", map[string]interface{}{"bridge": "br0", "ipv6": "", "ipv6_mode": "dhcp"}, false, false},
		{"-o ipv6= alone", map[string]interface{}{"bridge": "br0", "ipv6": ""}, false, false},
		{"ipv6=false written out", map[string]interface{}{"bridge": "br0", "ipv6": "false", "ipv6_mode": "dhcp"}, true, true},
		{"ipv6=true written out", map[string]interface{}{"bridge": "br0", "ipv6": "true", "ipv6_mode": "dhcp"}, true, false},
		{"no ipv6 at all", map[string]interface{}{"bridge": "br0", "ipv6_mode": "dhcp"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, set, err := decodeOptsSet(tc.input)
			if err != nil {
				t.Fatalf("decodeOptsSet: %v", err)
			}
			if set["IPv6"] != tc.wantSet {
				t.Errorf("decodeOptsSet reports ipv6 as written = %v, want %v", set["IPv6"], tc.wantSet)
			}
			if err := validateIPv6Options(opts, set); (err != nil) != tc.wantErr {
				t.Errorf("validateIPv6Options = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}

	opts, set, err := decodeOptsSet(map[string]interface{}{"bridge": "br0", "gateway": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("decodeOptsSet with an empty gateway: %v", err)
	}
	if opts.Bridge != "br0" || opts.IPv6Mode != "slaac" {
		t.Errorf("a written value was dropped with the empty one: %+v", opts)
	}
	if !set["Bridge"] || !set["IPv6Mode"] {
		t.Errorf("a written option is missing from the key set: %v", set)
	}
	if set["Gateway"] {
		t.Error("an option written with no value is reported as written")
	}

	_, set, err = decodeOptsSet(map[string]interface{}{"bridge": "br0", "ipv6_mode": "", "release_lease": ""})
	if err != nil {
		t.Fatalf("decodeOptsSet with empty tagged options: %v", err)
	}
	for _, f := range []string{"IPv6Mode", "ReleaseLease"} {
		if set[f] {
			t.Errorf("%s is reported as written although its option carried no value; "+
				"the tag-to-field mapping did not run", f)
		}
	}

	// lease_timeout is the one option this changes: an empty value was refused as a duration and is now unset (#817).
	got, set, err := decodeOptsSet(map[string]interface{}{"bridge": "br0", "lease_timeout": ""})
	if err != nil {
		t.Fatalf("-o lease_timeout= was refused: %v. An option written with no value is an "+
			"option the operator did not set, for every option; if this refusal is wanted "+
			"back, the reference sentence about empty values has to go with it.", err)
	}
	if got.LeaseTimeout != 0 {
		t.Errorf("-o lease_timeout= decoded to %v, want the zero the derived default is "+
			"computed from", got.LeaseTimeout)
	}
	if set["LeaseTimeout"] {
		t.Error("-o lease_timeout= is reported as an option the operator wrote")
	}
	got, set, err = decodeOptsSet(map[string]interface{}{"bridge": "br0", "lease_timeout": "45s"})
	if err != nil {
		t.Fatalf("-o lease_timeout=45s: %v", err)
	}
	if got.LeaseTimeout != 45*time.Second || !set["LeaseTimeout"] {
		t.Errorf("lease_timeout=45s decoded to %v, set=%v", got.LeaseTimeout, set["LeaseTimeout"])
	}
	if _, _, err := decodeOptsSet(map[string]interface{}{"bridge": "br0", "lease_timeout": "soon"}); err == nil {
		t.Error("-o lease_timeout=soon was accepted")
	}
}

// mapstructure reports a tagged field under its tag and an untagged one under its field name (#817).
func TestDecodeOptsSet_EveryOptionIsReportedUnderItsFieldName(t *testing.T) {
	typ := reflect.TypeOf(DHCPNetworkOptions{})
	if typ.NumField() == 0 {
		t.Fatal("DHCPNetworkOptions has no fields, so this test drives nothing")
	}
	tagged := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		key := f.Tag.Get("mapstructure")
		if key != "" {
			tagged++
		} else {
			key = strings.ToLower(f.Name)
		}

		var v interface{} = "x"
		switch f.Type.Kind() {
		case reflect.Bool:
			v = "true"
		case reflect.Int:
			v = "1400"
		case reflect.Int64:
			if f.Type == reflect.TypeOf(time.Duration(0)) {
				v = "30s"
			} else {
				v = "1"
			}
		}

		_, set, err := decodeOptsSet(map[string]interface{}{key: v})
		if err != nil {
			t.Errorf("decodeOptsSet(%s=%v): %v", key, v, err)
			continue
		}
		if !set[f.Name] {
			t.Errorf("writing -o %s=%v does not report the field %s as set; the set holds %v. "+
				"A presence check on that field would read an option the operator wrote as "+
				"one they did not.", key, v, f.Name, set)
		}
	}
	if tagged == 0 {
		t.Fatal("no field carries a mapstructure tag, so nothing here drives the " +
			"tag-to-field-name normalisation")
	}
}
