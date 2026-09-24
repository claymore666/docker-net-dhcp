// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const requireMACTestMAC = "02:42:aa:bb:cc:01"

// macMarker is the endpoint option libnetwork adds for a user-set MAC, measured as base64 of the six bytes.
func macMarker(t *testing.T, mac string) map[string]interface{} {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", mac, err)
	}
	return map[string]interface{}{ipamOptMacAddress: base64.StdEncoding.EncodeToString(hw)}
}

func assertRequireMACRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("an endpoint with no user-set MAC was accepted on a require_mac=true network")
	}
	for _, want := range []string{"require_mac", "--mac-address", "mac_address", "docker network connect"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q, so the operator is not told how to fix it", err, want)
		}
	}
}

func TestDecodeOpts_RequireMACAcceptsOnlyABoolean(t *testing.T) {
	for _, v := range []string{"true", "false", "1", "0"} {
		if _, err := decodeOpts(map[string]interface{}{"require_mac": v}); err != nil {
			t.Errorf("require_mac=%q was refused: %v", v, err)
		}
	}
	for _, v := range []string{"yes", "on", "required", "tru"} {
		if _, err := decodeOpts(map[string]interface{}{"require_mac": v}); err == nil {
			t.Errorf("require_mac=%q was accepted; a value that is not a boolean must be refused at create", v)
		}
	}
	opts, err := decodeOpts(map[string]interface{}{})
	if err != nil || opts.RequireMAC {
		t.Errorf("require_mac defaults to %v (err %v), want false", opts.RequireMAC, err)
	}
}

func TestValidateModeOptions_RequireMACIsRefusedOnIPvlanOnly(t *testing.T) {
	err := validateModeOptions(DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "eth0", RequireMAC: true})
	if !errors.Is(err, util.ErrModeMismatch) || !strings.Contains(err.Error(), "require_mac") ||
		!strings.Contains(err.Error(), "parent's MAC") {
		t.Errorf("ipvlan with require_mac=true: got %v, want a mode refusal naming the option and why", err)
	}
	for _, o := range []DHCPNetworkOptions{
		{Mode: ModeIPvlan, Parent: "eth0"},
		{Mode: ModeMacvlan, Parent: "eth0", RequireMAC: true},
		{Mode: ModeBridge, Bridge: "br0", RequireMAC: true},
	} {
		if err := validateModeOptions(o); err != nil {
			t.Errorf("%+v was refused: %v", o, err)
		}
	}
}

func TestRefuseWithoutUserMAC_ReadsTheMarkerNotTheInterfaceMAC(t *testing.T) {
	on := DHCPNetworkOptions{RequireMAC: true}
	marker := macMarker(t, requireMACTestMAC)
	cases := []struct {
		name    string
		opts    DHCPNetworkOptions
		req     CreateEndpointRequest
		refused bool
	}{
		{"off, no MAC", DHCPNetworkOptions{}, CreateEndpointRequest{Interface: &EndpointInterface{}}, false},
		{"off, nil interface", DHCPNetworkOptions{}, CreateEndpointRequest{}, false},
		{"no MAC at all", on, CreateEndpointRequest{Interface: &EndpointInterface{}}, true},
		{"nil interface", on, CreateEndpointRequest{}, true},
		{"a generated MAC with no marker, as IPAM mode sends", on,
			CreateEndpointRequest{Interface: &EndpointInterface{MacAddress: requireMACTestMAC}}, true},
		{"user-set MAC", on,
			CreateEndpointRequest{Interface: &EndpointInterface{MacAddress: requireMACTestMAC}, Options: marker}, false},
		{"marker text from --driver-opt, no interface MAC", on,
			CreateEndpointRequest{Interface: &EndpointInterface{},
				Options: map[string]interface{}{ipamOptMacAddress: "x"}}, true},
		{"marker for another MAC", on,
			CreateEndpointRequest{Interface: &EndpointInterface{MacAddress: "02:42:aa:bb:cc:02"}, Options: marker}, true},
		{"marker as the MAC's text form", on,
			CreateEndpointRequest{Interface: &EndpointInterface{MacAddress: requireMACTestMAC},
				Options: map[string]interface{}{ipamOptMacAddress: requireMACTestMAC}}, true},
		{"marker not a string", on,
			CreateEndpointRequest{Interface: &EndpointInterface{MacAddress: requireMACTestMAC},
				Options: map[string]interface{}{ipamOptMacAddress: 1}}, true},
		{"replay of an accepted endpoint", on,
			CreateEndpointRequest{Interface: &EndpointInterface{MacAddress: requireMACTestMAC}, replay: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := refuseWithoutUserMAC(c.opts, c.req)
			if c.refused {
				assertRequireMACRefusal(t, err)
			} else if err != nil {
				t.Errorf("refused: %v", err)
			}
		})
	}
}

func TestCreateEndpoint_RequireMACRefusesBeforeAnythingIsBuilt(t *testing.T) {
	withStateDir(t, t.TempDir())
	const network = "net-reqmac-null"
	if err := saveOptions(network, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-reqmac-none", RequireMAC: true}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	p := &Plugin{joinHints: make(map[string]joinHint)}

	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: network, EndpointID: "ep-refused", Interface: &EndpointInterface{},
		Options: map[string]interface{}{"com.docker.network.endpoint.ifname": "lan0"},
	})
	assertRequireMACRefusal(t, err)
	if _, ok := p.takeJoinHint("ep-refused"); ok {
		t.Error("a refused endpoint left a Join hint behind")
	}

	_, err = p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: network, EndpointID: "ep-accepted",
		Interface: &EndpointInterface{MacAddress: requireMACTestMAC}, Options: macMarker(t, requireMACTestMAC),
	})
	if err == nil || !strings.Contains(err.Error(), "failed to get bridge interface") {
		t.Errorf("a user-set MAC did not pass the check to the bridge lookup: %v", err)
	}
}

func TestReacquireEndpoint_IsNotSubjectToRequireMAC(t *testing.T) {
	withStateDir(t, t.TempDir())
	const network, endpoint = "net-reqmac-replay", "ep-replay"
	opts := DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-reqmac-none", RequireMAC: true}
	if err := saveOptions(network, opts); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	f := &fakeDocker{inspectResult: map[string]dNetwork.Inspect{
		network: {Containers: map[string]dNetwork.EndpointResource{
			"ctr": {EndpointID: endpoint, MacAddress: requireMACTestMAC},
		}},
	}}
	p := &Plugin{docker: f, joinHints: make(map[string]joinHint)}

	err := p.reacquireEndpoint(context.Background(), JoinRequest{NetworkID: network, EndpointID: endpoint}, opts)
	if err == nil || strings.Contains(err.Error(), "require_mac") || !strings.Contains(err.Error(), "failed to get bridge interface") {
		t.Errorf("the replay did not pass require_mac to the bridge lookup: %v", err)
	}
}

func requireMACIPAMFixture(t *testing.T) (*Plugin, *ipamBinding) {
	t.Helper()
	p, b := ipamFixture(t)
	if err := saveNetwork(ipamTestNetwork, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test", RequireMAC: true}, b); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}
	return p, b
}

// seedReservation is what RequestAddress leaves for CreateEndpoint: a finished reservation holding a record.
func seedReservation(t *testing.T, p *Plugin, b *ipamBinding, mac string, rebound bool) string {
	t.Helper()
	hw, _ := net.ParseMAC(mac)
	rec := p.recordReserved(ipamTestNetwork, hw, []byte{1, 2, 3})
	if rec == "" {
		t.Fatal("recordReserved wrote no record")
	}
	key := ipamReserveKey(b.PoolID, hw)
	res, mine := p.ipamReserves.begin(key, time.Now())
	if !mine {
		t.Fatal("the reservation was already begun")
	}
	p.ipamReserves.finish(key, res, ipamReservation{record: rec, rebound: rebound}, nil)
	return rec
}

func requireMACRecord(t *testing.T, p *Plugin, id string) lease.Record {
	t.Helper()
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	rec, ok := rb.ByID(id)
	if !ok {
		t.Fatalf("record %v is not in the journal", id)
	}
	return rec
}

func TestCreateEndpoint_RequireMACRefusalClosesAFreshIPAMReservation(t *testing.T) {
	p, b := requireMACIPAMFixture(t)
	rec := seedReservation(t, p, b, ipamTestMAC, false)

	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: ipamTestNetwork, EndpointID: "ep-ipam-refused",
		Interface: &EndpointInterface{MacAddress: ipamTestMAC, Address: "192.168.99.10/24"},
	})
	assertRequireMACRefusal(t, err)

	hw, _ := net.ParseMAC(ipamTestMAC)
	if _, ok := p.ipamReserves.take(ipamReserveKey(b.PoolID, hw)); ok {
		t.Error("the refused endpoint's reservation is still held")
	}
	if got := requireMACRecord(t, p, rec).Phase; got != lease.PhaseClosed {
		t.Errorf("the refused endpoint's record is %v, want CLOSED: a live one is the next start's re-bind candidate", got)
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	if cands := len(rb.Tombstones(ipamTestNetwork, time.Now())); cands != 0 {
		t.Errorf("%d re-bind candidates remain after the refusal, want 0", cands)
	}
}

func TestCreateEndpoint_RequireMACRefusalReturnsARebindToItsWindow(t *testing.T) {
	p, b := requireMACIPAMFixture(t)
	rec := seedReservation(t, p, b, ipamTestMAC, true)

	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: ipamTestNetwork, EndpointID: "ep-ipam-refused",
		Interface: &EndpointInterface{MacAddress: ipamTestMAC, Address: "192.168.99.10/24"},
	})
	assertRequireMACRefusal(t, err)

	got := requireMACRecord(t, p, rec)
	if got.Phase != lease.PhaseRetained || !got.Deadline.After(time.Now()) {
		t.Errorf("the re-bound record is %v with deadline %v, want RETAINED inside its window", got.Phase, got.Deadline)
	}
}

func TestCreateEndpoint_RequireMACPassesAUserMACToTheIPAMBranch(t *testing.T) {
	p, _ := requireMACIPAMFixture(t)

	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: ipamTestNetwork, EndpointID: "ep-ipam-user",
		Interface: &EndpointInterface{MacAddress: ipamTestMAC, Address: "192.168.99.10/24"},
		Options:   macMarker(t, ipamTestMAC),
	})
	if err == nil || !strings.Contains(err.Error(), "no reservation is held") {
		t.Errorf("a user-set MAC did not reach the IPAM branch: %v", err)
	}
}
