// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"testing"

	dNetwork "github.com/moby/moby/api/types/network"
)

// A v2.5.0 state file has no IPv4Off key; the upgraded --ipv4=false network must still run no DHCPv4 (#1341).
func TestNetOptions_AnUpgradedV4OffNetworkRunsNoDHCPv4(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	if err := saveOptions("n1", DHCPNetworkOptions{Bridge: "br9", IPv6: true}); err != nil {
		t.Fatal(err)
	}
	path := dir + "/n1.json"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(data, []byte(`"IPv4Off":false,`), nil, 1)
	if bytes.Equal(old, data) {
		t.Fatalf("the v2.6.0 file carries no IPv4Off key to strip: %s", data)
	}
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{docker: &fakeDocker{inspectResult: map[string]dNetwork.Inspect{
		"n1": {ID: "n1", Driver: testDHCPDriver, EnableIPv4: false, Options: map[string]string{"bridge": "br9", "ipv6": "true"}},
	}}}
	p.engine.Store(&engineIdentity{Version: "28.5.2", APIVersion: "1.51"})
	got, err := p.netOptions(context.Background(), "n1")
	if err != nil {
		t.Fatalf("netOptions: %v", err)
	}
	if !got.IPv4Off {
		t.Errorf("an upgraded --ipv4=false network reads IPv4Off = false, so its endpoints still run DHCPv4")
	}
}

func writeV25State(t *testing.T, id string, opts DHCPNetworkOptions, binding *ipamBinding) string {
	t.Helper()
	dir := t.TempDir()
	withStateDir(t, dir)
	if err := saveNetwork(id, opts, binding); err != nil {
		t.Fatal(err)
	}
	path := dir + "/" + id + ".json"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(data, []byte(`"IPv4Off":false,`), nil, 1)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func v25Plugin(version string, enableIPv4 bool) *Plugin {
	p := &Plugin{docker: &fakeDocker{inspectResult: map[string]dNetwork.Inspect{
		"n1": {ID: "n1", Driver: testDHCPDriver, EnableIPv4: enableIPv4, Options: map[string]string{"bridge": "br9", "ipv6": "true"}},
	}}}
	if version != "" {
		p.engine.Store(&engineIdentity{Version: version, APIVersion: "1.51"})
	}
	return p
}

func TestNetOptions_AnUpgradedNetworkIsStoredWithTheEngineFlagOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		version    string
		enableIPv4 bool
		wantOff    bool
		wantStored bool
	}{
		{"v4-off on 28", "28.5.2", false, true, true},
		{"v4-on on 28", "28.5.2", true, false, true},
		{"engine below 28 reports no flag", "27.3.1", false, false, true},
		{"engine not probed yet", "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeV25State(t, "n1", DHCPNetworkOptions{Bridge: "br9", IPv6: true}, nil)
			p := v25Plugin(tc.version, tc.enableIPv4)
			got, err := p.netOptions(context.Background(), "n1")
			if err != nil {
				t.Fatal(err)
			}
			if got.IPv4Off != tc.wantOff {
				t.Errorf("IPv4Off = %v, want %v", got.IPv4Off, tc.wantOff)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if stored := bytes.Contains(data, []byte(`"IPv4Off"`)); stored != tc.wantStored {
				t.Errorf("IPv4Off key stored = %v, want %v: %s", stored, tc.wantStored, data)
			}
			if tc.wantStored {
				p2 := v25Plugin(tc.version, !tc.enableIPv4)
				again, err := p2.netOptions(context.Background(), "n1")
				if err != nil || again.IPv4Off != tc.wantOff {
					t.Errorf("second read = %v, %v; want the stored %v", again.IPv4Off, err, tc.wantOff)
				}
			}
		})
	}
}

func TestNetOptions_AnUpgradedNetworkKeepsItsIPAMBinding(t *testing.T) {
	want := ipamBinding{PoolID: "p1", Space: "s1", Pool: "192.168.99.0/24", Gateway: "192.168.99.1", Aux: []string{"192.168.99.2"}}
	writeV25State(t, "n1", DHCPNetworkOptions{Bridge: "br9"}, &want)
	if _, err := v25Plugin("28.5.2", true).netOptions(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	sn, err := loadNetwork("n1")
	if err != nil {
		t.Fatal(err)
	}
	if sn.Binding == nil || !reflect.DeepEqual(*sn.Binding, want) || sn.IPv4OffUnset {
		t.Errorf("after the read the file holds binding %+v, key unset %v; want %+v and the key stored", sn.Binding, sn.IPv4OffUnset, want)
	}
}

// The issue's own shape: --ipv4=false --ipv6 with no plugin IPv6 option leaves an upgraded network no address at
// all if v4 goes off, a pair CreateNetwork refuses (#1341, #1135); it keeps leasing v4 as it did.
func TestNetOptions_AnUpgradedV4OffNetworkWithNoOtherFamilyKeepsLeasingV4(t *testing.T) {
	writeV25State(t, "n1", DHCPNetworkOptions{Bridge: "br0"}, nil)
	got, err := v25Plugin("28.5.2", false).netOptions(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got.IPv4Off {
		t.Errorf("IPv4Off = true with ipv6_mode off: an endpoint would get no address at all")
	}
	if err := validateIPv4Off(got); err != nil {
		t.Errorf("the stored pair would be refused at create: %v", err)
	}
}
