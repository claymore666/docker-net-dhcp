// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	dNetwork "github.com/moby/moby/api/types/network"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	cidNetwork  = "net-cid"
	cidHolderEP = "ep-holder"
	cidHolderCt = "web-1"
	cidMAC      = "02:42:aa:bb:cc:10"
)

func cidPlugin(t *testing.T, opts DHCPNetworkOptions) *Plugin {
	t.Helper()
	withStateDir(t, t.TempDir())
	if err := saveOptions(cidNetwork, opts); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	r, err := dhcp.OpenRecords(filepath.Join(t.TempDir(), recordFileName), "test-instance")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	f := &fakeDocker{inspectResult: map[string]dNetwork.Inspect{
		cidNetwork: {Containers: map[string]dNetwork.EndpointResource{
			"ctr-holder": {Name: cidHolderCt, EndpointID: cidHolderEP},
			"ctr-replay": {Name: "replayed", EndpointID: "ep-replay", MacAddress: engineMAC(cidMAC)},
		}},
	}}
	return &Plugin{docker: f, records: r, joinHints: make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager), endpointFingerprints: make(map[string]endpointFingerprint)}
}

func cidBridge(clientID string) DHCPNetworkOptions {
	return DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-cid-none", ClientID: clientID}
}

func cidCreate(p *Plugin, endpoint string, options map[string]interface{}) error {
	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: cidNetwork, EndpointID: endpoint,
		Interface: &EndpointInterface{MacAddress: cidMAC}, Options: options,
	})
	return err
}

func cidOpt(v string) map[string]interface{} { return map[string]interface{}{"client_id": v} }

// reachedBridge is the bridge lookup's error, which comes after the client_id step and before any DHCP packet.
func reachedBridge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "failed to get bridge interface")
}

func TestParseClientIDOption(t *testing.T) {
	max := strings.Repeat("a", maxOptionOctets-1)
	for _, tc := range []struct {
		name    string
		options map[string]interface{}
		want    string
		wantErr bool
	}{
		{"absent", map[string]interface{}{}, "", false},
		{"nil map", nil, "", false},
		{"value", cidOpt("web"), "web", false},
		{"254 octets", cidOpt(max), max, false},
		{"255 octets", cidOpt(max + "a"), "", true},
		{"empty", cidOpt(""), "", true},
		{"not a string", map[string]interface{}{"client_id": 7}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClientIDOption(tc.options)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("parseClientIDOption = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
			if err != nil && !errors.Is(err, util.ErrIPAM) {
				t.Errorf("the refusal %v does not wrap ErrIPAM", err)
			}
		})
	}
}

func TestApplyEndpointClientID_PrecedenceEndpointThenNetworkThenDerived(t *testing.T) {
	mac, _ := net.ParseMAC(cidMAC)
	for _, tc := range []struct {
		name, network string
		options       map[string]interface{}
		want          []byte
	}{
		{"endpoint over network", "netwide", cidOpt("web"), []byte("web")},
		{"endpoint alone", "", cidOpt("web"), []byte("web")},
		{"network alone", "netwide", nil, []byte("netwide")},
		{"derived", "", nil, []byte(mac)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := cidPlugin(t, cidBridge(tc.network))
			opts := cidBridge(tc.network)
			r := CreateEndpointRequest{NetworkID: cidNetwork, EndpointID: "ep-a", Options: tc.options}
			if err := p.applyEndpointClientID(context.Background(), r, &opts, false); err != nil {
				t.Fatalf("applyEndpointClientID: %v", err)
			}
			if got := resolveClientID(opts, "ep-a", mac); !bytes.Equal(got, tc.want) {
				t.Errorf("the exchange sends %q, want %q", got, tc.want)
			}
			hint, _ := p.takeJoinHint("ep-a")
			if want, _ := tc.options["client_id"].(string); hint.ClientID != want {
				t.Errorf("the Join hint carries %q, want %q", hint.ClientID, want)
			}
		})
	}
}

func TestNewJoinManager_TakesTheEndpointClientIDFromTheHint(t *testing.T) {
	p := cidPlugin(t, cidBridge("netwide"))
	r := JoinRequest{NetworkID: cidNetwork, EndpointID: "ep-a"}
	m := p.newJoinManager(r, cidBridge("netwide"), joinHint{ClientID: "web"}, JoinResponse{})
	if got := m.clientID(nil); string(got) != "web" {
		t.Errorf("with no record the persistent client sends %q, want the endpoint's %q", got, "web")
	}
	if got := m.clientID(dhcp.ClientIdentity([]byte("old"))); string(got) != "old" {
		t.Errorf("a record's identity no longer wins: the client sends %q", got)
	}
	m = p.newJoinManager(r, cidBridge("netwide"), joinHint{}, JoinResponse{})
	if got := m.clientID(nil); string(got) != "netwide" {
		t.Errorf("with no endpoint option the client sends %q, want the network's", got)
	}
}

func TestCreateEndpoint_RefusesAClientIDAnotherEndpointHolds(t *testing.T) {
	p := cidPlugin(t, cidBridge(""))
	p.claimClientID(cidHolderEP, clientIDClaim{networkID: cidNetwork, id: "web", perEndpoint: true}, false)

	err := cidCreate(p, "ep-second", cidOpt("web"))
	if err == nil || reachedBridge(err) {
		t.Fatalf("a second endpoint with the same client_id was not refused before the link: %v", err)
	}
	for _, want := range []string{`"web"`, cidHolderCt, shortID(cidHolderEP), "client_id driver option"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if _, held := p.clientIDClaims["ep-second"]; held {
		t.Error("the refused endpoint left a claim behind")
	}
	if !reachedBridge(cidCreate(p, "ep-third", cidOpt("db"))) {
		t.Error("a distinct client_id did not pass")
	}
}

func TestCreateEndpoint_SameClientIDOnAnotherNetworkPasses(t *testing.T) {
	p := cidPlugin(t, cidBridge(""))
	p.claimClientID(cidHolderEP, clientIDClaim{networkID: "net-other", id: "web", perEndpoint: true}, false)
	if err := cidCreate(p, "ep-second", cidOpt("web")); !reachedBridge(err) {
		t.Errorf("a client_id held on another network was refused: %v", err)
	}
}

func TestCreateEndpoint_FailedCreateReleasesItsClaim(t *testing.T) {
	p := cidPlugin(t, cidBridge(""))
	if err := cidCreate(p, "ep-first", cidOpt("web")); !reachedBridge(err) {
		t.Fatalf("first create: %v", err)
	}
	if err := cidCreate(p, "ep-second", cidOpt("web")); !reachedBridge(err) {
		t.Errorf("a failed create's claim refused the next endpoint: %v", err)
	}
}

func TestDeleteEndpoint_ReleasesTheClaim(t *testing.T) {
	p := cidPlugin(t, cidBridge(""))
	p.claimClientID(cidHolderEP, clientIDClaim{networkID: cidNetwork, id: "web", perEndpoint: true}, false)
	_ = p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{NetworkID: cidNetwork, EndpointID: cidHolderEP})
	if err := cidCreate(p, "ep-second", cidOpt("web")); !reachedBridge(err) {
		t.Errorf("a removed endpoint's claim still refuses: %v", err)
	}
}

func TestApplyEndpointClientID_SharedNetworkClientID(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       DHCPNetworkOptions
		secondOpt  map[string]interface{}
		wantRefuse bool
	}{
		{"bridge keeps sharing", cidBridge("netwide"), nil, false},
		{"macvlan keeps sharing", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0", ClientID: "netwide"}, nil, false},
		{"ipvlan refuses", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "eth0", ClientID: "netwide"}, nil, true},
		{"endpoint option equal to the network's", cidBridge("netwide"), cidOpt("netwide"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := cidPlugin(t, tc.opts)
			first := tc.opts
			if err := p.applyEndpointClientID(context.Background(),
				CreateEndpointRequest{NetworkID: cidNetwork, EndpointID: cidHolderEP}, &first, false); err != nil {
				t.Fatalf("first endpoint: %v", err)
			}
			second := tc.opts
			err := p.applyEndpointClientID(context.Background(),
				CreateEndpointRequest{NetworkID: cidNetwork, EndpointID: "ep-second", Options: tc.secondOpt}, &second, false)
			if (err != nil) != tc.wantRefuse {
				t.Errorf("second endpoint: %v, want refused %v", err, tc.wantRefuse)
			}
		})
	}
}

func TestCreateEndpoint_ClientIDOptionRefusedInIPAMMode(t *testing.T) {
	p, b := ipamFixture(t)
	if err := saveNetwork(ipamTestNetwork, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test"}, b); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}
	seedReservation(t, p, b, ipamTestMAC, false)

	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: ipamTestNetwork, EndpointID: "ep-ipam-cid",
		Interface: &EndpointInterface{MacAddress: ipamTestMAC, Address: "192.168.99.10/24"},
		Options:   cidOpt("web"),
	})
	if err == nil || !strings.Contains(err.Error(), "--ipam-driver null") {
		t.Fatalf("the option was not refused in IPAM mode: %v", err)
	}
	hw, _ := net.ParseMAC(ipamTestMAC)
	if _, ok := p.ipamReserves.take(ipamReserveKey(b.PoolID, hw)); ok {
		t.Error("the refused endpoint's reservation is still held")
	}
}

func TestReacquireEndpoint_CarriesTheRecordedClientID(t *testing.T) {
	mac, _ := net.ParseMAC(cidMAC)
	for _, tc := range []struct {
		name       string
		recorded   []byte
		wantRefuse bool
	}{
		{"per-endpoint id carried", []byte("web"), true},
		{"derived id not carried", []byte(mac), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := cidPlugin(t, cidBridge(""))
			if p.recordCreated(cidNetwork, mac, dhcp.ClientIdentity(tc.recorded)) == "" {
				t.Fatal("recordCreated wrote no record")
			}
			// A holder of "web" makes the carried id visible as a refusal before the bridge lookup.
			p.claimClientID(cidHolderEP, clientIDClaim{networkID: cidNetwork, id: "web", perEndpoint: true}, false)

			err := p.reacquireEndpoint(context.Background(), JoinRequest{NetworkID: cidNetwork, EndpointID: "ep-replay"},
				cidBridge(""))
			refused := err != nil && strings.Contains(err.Error(), "already in use")
			if refused != tc.wantRefuse || (!refused && !reachedBridge(err)) {
				t.Errorf("replay: %v; want the carried id refused %v", err, tc.wantRefuse)
			}
		})
	}
}

func TestReacquireEndpoint_ANetworkClientIDStaysShared(t *testing.T) {
	mac, _ := net.ParseMAC(cidMAC)
	p := cidPlugin(t, cidBridge("netwide"))
	if p.recordCreated(cidNetwork, mac, dhcp.ClientIdentity([]byte("netwide"))) == "" {
		t.Fatal("recordCreated wrote no record")
	}
	p.claimClientID(cidHolderEP, clientIDClaim{networkID: cidNetwork, id: "netwide"}, false)

	err := p.reacquireEndpoint(context.Background(), JoinRequest{NetworkID: cidNetwork, EndpointID: "ep-replay"},
		cidBridge("netwide"))
	if !reachedBridge(err) {
		t.Fatalf("the replay of an endpoint sharing the bridge network's client_id: %v", err)
	}
	if err := cidCreate(p, "ep-sibling", nil); !reachedBridge(err) {
		t.Errorf("a sibling sharing the bridge network's client_id was refused after the replay: %v", err)
	}
}

func TestRecoverOneEndpoint_RestoresTheClientIDClaim(t *testing.T) {
	mac, _ := net.ParseMAC(cidMAC)
	p := cidPlugin(t, cidBridge(""))
	if p.recordCreated(cidNetwork, mac, dhcp.ClientIdentity([]byte("web"))) == "" {
		t.Fatal("recordCreated wrote no record")
	}
	p.docker = &lockedDocker{}
	if _, err := p.recoverOneEndpoint(context.Background(), "ctr-holder", cidNetwork, cidHolderEP, cidMAC,
		"192.168.99.113/24", "", cidBridge("")); err != nil {
		t.Fatalf("recoverOneEndpoint: %v", err)
	}
	if c := p.clientIDClaims[cidHolderEP]; c.id != "web" || !c.perEndpoint {
		t.Fatalf("recovered claim = %+v, want web from the endpoint option", c)
	}
	if err := cidCreate(p, "ep-second", cidOpt("web")); err == nil || reachedBridge(err) {
		t.Errorf("a duplicate of a recovered endpoint's client_id passed: %v", err)
	}
}

// parentWithMAC stubs the ipvlan parent with a hardware address and, when addr is set, one IPv4 address.
func TestRecoverOneEndpoint_ANetworkClientIDStaysShared(t *testing.T) {
	mac, _ := net.ParseMAC(cidMAC)
	p := cidPlugin(t, cidBridge("netwide"))
	if p.recordCreated(cidNetwork, mac, dhcp.ClientIdentity([]byte("netwide"))) == "" {
		t.Fatal("recordCreated wrote no record")
	}
	p.docker = &lockedDocker{}
	if _, err := p.recoverOneEndpoint(context.Background(), "ctr-holder", cidNetwork, cidHolderEP, cidMAC,
		"192.168.99.113/24", "", cidBridge("netwide")); err != nil {
		t.Fatalf("recoverOneEndpoint: %v", err)
	}
	if c := p.clientIDClaims[cidHolderEP]; c.id != "netwide" || c.perEndpoint {
		t.Fatalf("recovered claim = %+v, want the network's netwide, not per endpoint", c)
	}
	if err := cidCreate(p, "ep-second", nil); !reachedBridge(err) {
		t.Errorf("a second endpoint sharing the bridge network's client_id was refused: %v", err)
	}
}

func parentWithMAC(t *testing.T, name string, hw net.HardwareAddr, addr string) {
	t.Helper()
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 9, HardwareAddr: hw}}
	prevByName, prevList := nlLinkByName, nlAddrList
	nlLinkByName = func(n string) (netlink.Link, error) {
		if n != name {
			return nil, netlink.LinkNotFoundError{}
		}
		return link, nil
	}
	nlAddrList = func(_ netlink.Link, family int) ([]netlink.Addr, error) {
		if addr == "" || family != unix.AF_INET {
			return nil, nil
		}
		a, err := netlink.ParseAddr(addr)
		if err != nil {
			t.Fatalf("ParseAddr: %v", err)
		}
		return []netlink.Addr{*a}, nil
	}
	t.Cleanup(func() { nlLinkByName, nlAddrList = prevByName, prevList })
}

func TestReleaseFromRecord_IpvlanSendsTheParentMACAsChaddr(t *testing.T) {
	parentMAC, _ := net.ParseMAC("9a:4f:c5:0d:c2:c5")
	key := endpointRecordKey(ModeIPvlan, strings.Repeat("d6", 32), nil)
	for _, tc := range []struct {
		name, mode, addr string
		want             []byte
	}{
		{"ipvlan from a host address", ModeIPvlan, "192.168.99.2/24", parentMAC},
		{"ipvlan over the link", ModeIPvlan, "", parentMAC},
		{"macvlan keeps the record's", ModeMacvlan, "192.168.99.2/24", key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parentWithMAC(t, "eth0", parentMAC, tc.addr)
			sender := installSender(t, nil)
			rec := lease.Record{ID: "r1", CHAddr: append([]byte(nil), key...),
				Identity: dhcp.ClientIdentity([]byte("web")),
				Lease:    lease.Lease{Addr: netip.MustParsePrefix("192.168.99.113/24")}}

			_, _ = releaseFromRecord(rec, DHCPNetworkOptions{Mode: tc.mode, Parent: "eth0"}, false,
				netip.MustParseAddr("192.168.99.113"), nil, nil)

			sent := append(append([]lease.Record(nil), sender.recs...), sender.linkRecs...)
			if len(sent) != 1 {
				t.Fatalf("%d releases sent, want 1", len(sent))
			}
			if !bytes.Equal(sent[0].CHAddr, tc.want) {
				t.Errorf("the release carries chaddr %x, want %x", sent[0].CHAddr, tc.want)
			}
			if !bytes.Equal(rec.CHAddr, key) {
				t.Errorf("the caller's record changed to %x", rec.CHAddr)
			}
		})
	}
}

func TestReleaseFromRecord_IpvlanWithoutParentKeepsTheRecordChaddr(t *testing.T) {
	sender := installSender(t, nil)
	prev := nlLinkByName
	nlLinkByName = func(string) (netlink.Link, error) { return nil, netlink.LinkNotFoundError{} }
	t.Cleanup(func() { nlLinkByName = prev })
	key := endpointRecordKey(ModeIPvlan, strings.Repeat("d6", 32), nil)
	rec := lease.Record{ID: "r1", CHAddr: key, Lease: lease.Lease{Addr: netip.MustParsePrefix("192.168.99.113/24")}}
	if got := withWireCHAddr(rec, DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "eth0"}); !bytes.Equal(got.CHAddr, key) {
		t.Errorf("a failed parent lookup changed chaddr to %x", got.CHAddr)
	}
	if sender.callCount() != 0 {
		t.Error("withWireCHAddr sent something")
	}
}
