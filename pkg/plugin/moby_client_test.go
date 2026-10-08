// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	cerrdefs "github.com/containerd/errdefs"
	docker "github.com/moby/moby/client"
)

// engineDaemon answers from canned engine JSON keyed by path suffix, so the real client decodes what an engine
// writes rather than what a fake assumes (#178).
func engineDaemon(t *testing.T, api string, routes map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", api)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			_, _ = w.Write([]byte("OK"))
			return
		}
		for suffix, body := range routes {
			if strings.HasSuffix(r.URL.Path, suffix) {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String()
}

func realClient(t *testing.T, host string) *docker.Client {
	t.Helper()
	cli, err := newDockerClient(host, nil)
	if err != nil {
		t.Fatalf("newDockerClient: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestMobyClient_RecoveryKeepsTheHostAddressAndReadsNoAddressAsNone(t *testing.T) {
	const netID = "net-engine-json"
	const driver = `"Driver":"ghcr.io/claymore666/docker-net-dhcp:v2"`
	host := engineDaemon(t, "1.45", map[string]string{
		"/networks": `[{"Name":"lan","Id":"` + netID + `",` + driver + `}]`,
		"/networks/" + netID: `{"Name":"lan","Id":"` + netID + `",` + driver + `,"Containers":{` +
			`"ctr-a":{"EndpointID":"abcdef0123456789aaaa","MacAddress":"02:42:c0:a8:63:05",` +
			`"IPv4Address":"192.168.99.5/24","IPv6Address":"2001:db8::5/64"},` +
			`"ctr-b":{"EndpointID":"abcdef0123456789bbbb","MacAddress":"02:42:c0:a8:63:06",` +
			`"IPv4Address":"","IPv6Address":""}}}`,
		"/containers/ctr-a/json": `{"Id":"ctr-a","State":{"Running":true,"Status":"running","Pid":4242},"Config":{"Hostname":"app-a"}}`,
		"/containers/ctr-b/json": `{"Id":"ctr-b","State":{"Running":true,"Status":"running","Pid":4243},"Config":{"Hostname":"app-b"}}`,
	})
	p := recoveryPlugin(t, netID, realClient(t, host))

	p.recoverEndpoints(context.Background(), time.Second)

	for _, want := range []struct{ endpoint, hostname, mac, ipv4, ipv6 string }{
		{"abcdef0123456789aaaa", "app-a", "02:42:c0:a8:63:05", "192.168.99.5", "2001:db8::5"},
		{"abcdef0123456789bbbb", "app-b", "02:42:c0:a8:63:06", "", ""},
	} {
		if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{NetworkID: netID, EndpointID: want.endpoint}); err != nil {
			t.Fatalf("DeleteEndpoint %s: %v", want.hostname, err)
		}
		mac, ipv4, ipv6, ok := p.consumeTombstone(netID, dhcpHostname{name: want.hostname})
		if !ok {
			t.Errorf("%s: recovery from the engine's answer left no tombstone", want.hostname)
			continue
		}
		if mac != want.mac || ipv4 != want.ipv4 || ipv6 != want.ipv6 {
			t.Errorf("%s: tombstone (%q, %q, %q), want (%q, %q, %q)", want.hostname, mac, ipv4, ipv6, want.mac, want.ipv4, want.ipv6)
		}
	}
}

func TestPrefixString_NoPrefixIsNoTextAndAHostPrefixKeepsItsHost(t *testing.T) {
	for in, want := range map[netip.Prefix]string{
		{}:                                       "",
		netip.MustParsePrefix("192.168.99.5/24"): "192.168.99.5/24",
		netip.MustParsePrefix("2001:db8::5/64"):  "2001:db8::5/64",
	} {
		if got := prefixString(in); got != want {
			t.Errorf("prefixString(%#v) = %q, want %q", in, got, want)
		}
	}
}

func TestPrefixIPNet_IsWhatParseCIDRGaveForTheSameText(t *testing.T) {
	for _, cidr := range []string{"192.168.99.0/24", "192.168.99.5/24", "10.0.0.0/8", "0.0.0.0/0", "2001:db8::/64", "2001:db8::5/64", "::/0"} {
		_, want, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("ParseCIDR %s: %v", cidr, err)
		}
		if got, ok := prefixIPNet(netip.MustParsePrefix(cidr)); !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: prefixIPNet = %#v (%v), net.ParseCIDR = %#v", cidr, got, ok, want)
		}
	}
	if got, ok := prefixIPNet(netip.Prefix{}); ok {
		t.Errorf("a network with no subnet gave %v; the caller must refuse it", got)
	}

	host := engineDaemon(t, "1.45", map[string]string{
		"/networks": `[{"Name":"other","Id":"n1","Driver":"bridge","IPAM":{"Driver":"default","Config":[{"Subnet":"192.168.99.0/24"}]}}]`,
	})
	nets, err := listNetworks(context.Background(), realClient(t, host))
	if err != nil || len(nets) != 1 || len(nets[0].IPAM.Config) != 1 {
		t.Fatalf("listNetworks: %v, %+v", err, nets)
	}
	got, ok := prefixIPNet(nets[0].IPAM.Config[0].Subnet)
	if !ok {
		t.Fatal("the engine's subnet did not survive the decode")
	}
	// The bridge's address comes from netlink as 16 bytes; the overlap check must still see it inside (#178).
	if !got.Contains(net.ParseIP("192.168.99.1")) {
		t.Errorf("%v does not contain the 16-byte form of 192.168.99.1", got)
	}
}

func TestNewDockerClient_ADaemonThatNeverAnswersEndsTheCallWithinTheTimeout(t *testing.T) {
	release := make(chan struct{})
	// The ping answers, so the version negotiation the client runs first takes no part of the bound.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.45")
			return
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)
	cli, err := newDockerClient("tcp://"+srv.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatalf("newDockerClient: %v", err)
	}
	defer cli.Close()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := cli.NetworkList(context.Background(), docker.NetworkListOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a daemon that never answered the list gave one")
		}
		if d := time.Since(start); d > 3500*time.Millisecond {
			t.Errorf("the call ended after %v; the per-request bound is 2 s (#383)", d)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("a call with no deadline of its own is still waiting after 6 s; the client lost its timeout (#383)")
	}
}

// A daemon below the client's oldest API answers the ping and refuses everything else.
func belowClientFloorDaemon(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.39")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			_, _ = w.Write([]byte("OK"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"client version is too new. Maximum supported API version is 1.39"}`))
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String()
}

func TestProbeEngine_AnEngineBelowTheClientFloorIsRefusedByItsAPIVersion(t *testing.T) {
	p := &Plugin{docker: realClient(t, belowClientFloorDaemon(t))}

	err := p.probeEngine(context.Background())
	if !errors.Is(err, errEngineTooOld) {
		t.Fatalf("an engine on API 1.39 was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), "1.39") {
		t.Errorf("the refusal does not name what the engine reported: %v", err)
	}
	if got := p.engineSnapshot().APIVersion; got != "1.39" {
		t.Errorf("health api_version = %q, want 1.39", got)
	}
	if !p.daemonAnsweredAtStart() {
		t.Error("a daemon that answered the ping was recorded as unreachable")
	}
}

func TestReprobeEngine_AnEngineBelowTheClientFloorIsLoggedAndPublished(t *testing.T) {
	p := &Plugin{docker: realClient(t, belowClientFloorDaemon(t))}

	out := captureLog(t, func() { p.reprobeEngine(context.Background()) })

	if !strings.Contains(out, "below the minimum engine version") {
		t.Errorf("the re-probe did not report the old engine: %s", out)
	}
	if got := p.engineSnapshot().APIVersion; got != "1.39" {
		t.Errorf("health api_version = %q, want 1.39", got)
	}
}

func TestStaleNetworks_AnAnswerTheClientCannotDecodeIsNotGone(t *testing.T) {
	for name, body := range map[string]string{
		"truncated":         `{"Id":`,
		"unreadable subnet": `{"Id":"` + staleNet + `","IPAM":{"Config":[{"Subnet":"not-a-prefix"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p, bound := stalePlugin(t, staleNet)
			recID := retainedOn(t, p, staleNet)
			p.docker = realClient(t, engineDaemon(t, "1.45", map[string]string{"/networks": "[]", "/networks/" + staleNet: body}))
			if _, err := inspectNetwork(context.Background(), p.docker, staleNet); err == nil || cerrdefs.IsNotFound(err) {
				t.Fatalf("the client read this answer as %v; the case needs an error that is not not-found", err)
			}

			recoverOnce(p)

			if !fileExists(t, staleNet) {
				t.Error("the file was removed on a 200 the client could not decode")
			}
			if n, ok := p.ipamIndex.network(bound[staleNet].PoolID); !ok || n != staleNet {
				t.Errorf("the pool binding is (%q, %v), want it kept", n, ok)
			}
			if got := recordPhase(t, p, recID); got != lease.PhaseRetained {
				t.Errorf("the record is %v, want RETAINED: the network may still exist", got)
			}
		})
	}
}
