// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

func newHealthPlugin() *Plugin {
	return &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
}

// macsEqual decides whether the device that answered is us. Getting it
// wrong in either direction is a shipped bug: a false "equal" hides a
// real conflict, a false "different" reports every bridge-mode endpoint
// as one.
func TestMACsEqual(t *testing.T) {
	mustMAC := func(s string) net.HardwareAddr {
		t.Helper()
		m, err := net.ParseMAC(s)
		if err != nil {
			t.Fatalf("ParseMAC(%q): %v", s, err)
		}
		return m
	}

	cases := []struct {
		name string
		a, b net.HardwareAddr
		want bool
	}{
		{"identical", mustMAC("00:00:5e:00:53:01"), mustMAC("00:00:5e:00:53:01"), true},
		{"different", mustMAC("00:00:5e:00:53:01"), mustMAC("00:00:5e:00:53:02"), false},
		{"last octet only", mustMAC("00:00:5e:00:53:01"), mustMAC("00:00:5e:00:53:03"), false},
		// Both nil must NOT compare equal. A bytes.Equal would say yes,
		// and "we don't know either MAC" would then read as "the device
		// that answered is us" — silently discarding the conflict.
		{"both nil", nil, nil, false},
		{"ours unknown", mustMAC("00:00:5e:00:53:01"), nil, false},
		{"theirs unknown", nil, mustMAC("00:00:5e:00:53:01"), false},
		// A EUI-64 answer against a EUI-48 ours is not a match, and
		// must not panic on the length difference.
		{"different lengths", mustMAC("00:00:5e:00:53:01"), mustMAC("00:00:5e:ff:fe:00:53:01"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := macsEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("macsEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// A probe that cannot run must say so — as a probe failure, never as a
// clean address. #524 is a check that silently did not happen; a
// detector that silently declines to run is the same bug again.
func TestCheckAddressConflict_UnrunnableCountsAsFailure(t *testing.T) {
	cases := []struct {
		name              string
		parent, cidr, mac string
		wantFailures      int32
	}{
		{"unparseable address", "eth0", "not-an-address", "00:00:5e:00:53:01", 1},
		{"unparseable MAC", "eth0", "192.0.2.10/24", "not-a-mac", 1},
		{"empty MAC", "eth0", "192.0.2.10/24", "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newHealthPlugin()
			p.checkAddressConflict(tc.parent, tc.cidr, tc.mac, "endpoint-id", "network-id")
			if got := p.conflictProbeFailures.Load(); got != tc.wantFailures {
				t.Errorf("conflict_probe_failures = %d, want %d", got, tc.wantFailures)
			}
			if got := p.addressConflicts.Load(); got != 0 {
				t.Errorf("address_conflicts = %d, want 0 — an unrunnable probe is not a conflict", got)
			}
		})
	}
}

// No parent or no address means there is nothing to probe and nothing
// went wrong. Distinct from the cases above: those are a probe that
// should have run and could not.
func TestCheckAddressConflict_NothingToProbeIsNotAFailure(t *testing.T) {
	for _, tc := range []struct{ name, parent, cidr string }{
		{"no parent", "", "192.0.2.10/24"},
		{"no address", "eth0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newHealthPlugin()
			p.checkAddressConflict(tc.parent, tc.cidr, "00:00:5e:00:53:01", "e", "n")
			if got := p.conflictProbeFailures.Load(); got != 0 {
				t.Errorf("conflict_probe_failures = %d, want 0", got)
			}
			if got := p.addressConflicts.Load(); got != 0 {
				t.Errorf("address_conflicts = %d, want 0", got)
			}
		})
	}
}

// The counter has to reach the wire, and it has to move Healthy. A
// conflict that is only logged is what production already had.
func TestApiHealth_AddressConflictIsUnhealthy(t *testing.T) {
	p := newHealthPlugin()

	rec := httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var clean HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &clean); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !clean.Healthy {
		t.Fatalf("baseline is already unhealthy; the assertion below would pass for the wrong reason")
	}
	if clean.AddressConflicts != 0 {
		t.Errorf("address_conflicts = %d on a fresh plugin, want 0", clean.AddressConflicts)
	}

	p.addressConflicts.Add(1)
	rec = httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AddressConflicts != 1 {
		t.Errorf("address_conflicts = %d, want 1", got.AddressConflicts)
	}
	if got.Healthy {
		t.Error("healthy = true with an address conflict recorded; the endpoint is up on an address that belongs to someone else")
	}

	// Pin the wire keys — an operator's alert is written against these.
	for _, key := range []string{"address_conflicts", "conflict_probe_failures", "address_conflict_probes"} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Errorf("Health JSON missing %q field", key)
		}
	}
}

// A probe that could not run must NOT count as a probe that reached a
// verdict. If it did, address_conflict_probes would climb while nothing
// was actually being checked — a detector that reports itself working
// while blind, which is worse than one that reports nothing.
func TestCheckAddressConflict_FailedProbeIsNotAVerdict(t *testing.T) {
	p := newHealthPlugin()
	p.checkAddressConflict("eth0", "192.0.2.10/24", "not-a-mac", "e", "n")
	if got := p.addressConflictProbes.Load(); got != 0 {
		t.Errorf("address_conflict_probes = %d after an unrunnable probe, want 0", got)
	}
	if got := p.conflictProbeFailures.Load(); got != 1 {
		t.Errorf("conflict_probe_failures = %d, want 1", got)
	}
}

// A probe that could not run is not a broken address, so it must not
// latch the plugin unhealthy. Operators still need to see it, which is
// why it is a counter and not just a log line.
func TestApiHealth_ProbeFailureIsNotUnhealthy(t *testing.T) {
	p := newHealthPlugin()
	p.conflictProbeFailures.Add(3)

	rec := httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ConflictProbeFailures != 3 {
		t.Errorf("conflict_probe_failures = %d, want 3", got.ConflictProbeFailures)
	}
	if !got.Healthy {
		t.Error("healthy = false on probe failures alone; an unasked question is not a known-broken address")
	}
}

// The probe's source address must never come out of the operator's own
// subnet: the address it picked could be the next one their DHCP server
// hands out, which is the exact fault this file exists to detect.
func TestNewProbeLinkLocal(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		a, err := newProbeLinkLocal()
		if err != nil {
			t.Fatalf("newProbeLinkLocal: %v", err)
		}
		ip := a.IPNet.IP.To4()
		if ip == nil {
			t.Fatalf("not an IPv4 address: %v", a.IPNet.IP)
		}
		if ip[0] != 169 || ip[1] != 254 {
			t.Fatalf("address %v is outside 169.254.0.0/16 — it could collide with the pool being probed", ip)
		}
		// RFC 3927 reserves the first and last /24, and .0/.255 hosts
		// are not usable.
		if ip[2] == 0 || ip[2] == 255 || ip[3] == 0 || ip[3] == 255 {
			t.Errorf("address %v falls in a reserved or non-host range", ip)
		}
		if ones, bits := a.IPNet.Mask.Size(); ones != 16 || bits != 32 {
			t.Errorf("mask is /%d of %d, want /16 of 32", ones, bits)
		}
		seen[ip.String()]++
	}
	// Two probes can run concurrently on one host, so a constant
	// address would make them collide. Not a randomness test — just
	// proof it is not a fixed value.
	if len(seen) < 2 {
		t.Errorf("200 calls produced %d distinct address(es); concurrent probes on one host would collide", len(seen))
	}
}

// stubAddrList swaps nlAddrList for the test's duration.
func stubAddrList(t *testing.T, addrs []netlink.Addr, err error) {
	t.Helper()
	prev := nlAddrList
	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) { return addrs, err }
	t.Cleanup(func() { nlAddrList = prev })
}

// addrOn is mustAddr as a value, which is the shape netlink.AddrList
// returns. mustAddr itself lives in orphan_release_test.go.
func addrOn(t *testing.T, cidr string) netlink.Addr {
	t.Helper()
	return *mustAddr(t, cidr)
}

// TestPickProbeSource is the unit-level half of the #524 root cause.
//
// A responder only answers an ARP request whose sender address it can
// route back to. Sending from a link-local address the responder has no
// route for is therefore answered by silence — indistinguishable, to the
// probe, from a free address. Measured on 6.12 over a veth pair: with no
// routes on the responder the link-local sender got NUD_INCOMPLETE while
// an on-subnet sender got the squatter's MAC.
//
// So which source is chosen is the difference between a detector and a
// counter that always reads zero, and it is asserted here rather than
// left to the integration suite.
func TestPickProbeSource(t *testing.T) {
	_, subnet, err := net.ParseCIDR("192.168.101.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	link := &fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: "parent0"}}

	t.Run("prefers_an_on_subnet_address_the_parent_already_has", func(t *testing.T) {
		stubAddrList(t, []netlink.Addr{
			addrOn(t, "10.9.9.9/8"),
			addrOn(t, "192.168.101.2/24"),
		}, nil)
		src, err := pickProbeSource(link, net.IPv4(192, 168, 101, 42), subnet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !src.onSubnet {
			t.Error("an address inside the leased subnet was available and was not marked on-subnet; " +
				"silence from this probe would be wrongly reported as a clean segment")
		}
		if src.borrowed {
			t.Error("borrowed = true for an address the parent already holds; the probe would add and " +
				"remove an address it did not need to touch")
		}
		if got := src.addr.IP.String(); got != "192.168.101.2" {
			t.Errorf("source = %s, want the on-subnet 192.168.101.2", got)
		}
	})

	t.Run("falls_back_to_link_local_when_no_address_is_on_subnet", func(t *testing.T) {
		stubAddrList(t, []netlink.Addr{addrOn(t, "10.9.9.9/8")}, nil)
		src, err := pickProbeSource(link, net.IPv4(192, 168, 101, 42), subnet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if src.onSubnet {
			t.Fatal("a link-local fallback was marked on-subnet; that is exactly the claim that made " +
				"the probe report a squatted address as clean")
		}
		if !src.borrowed {
			t.Error("the fallback address must be marked borrowed, or it is never removed again")
		}
		if ip := src.addr.IP.To4(); ip == nil || ip[0] != 169 || ip[1] != 254 {
			t.Errorf("fallback source %v is not link-local; it could collide with the pool being probed", src.addr.IP)
		}
	})

	t.Run("falls_back_on_a_bare_parent", func(t *testing.T) {
		stubAddrList(t, nil, nil)
		src, err := pickProbeSource(link, net.IPv4(192, 168, 101, 42), subnet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if src.onSubnet || !src.borrowed {
			t.Errorf("bare parent: got onSubnet=%v borrowed=%v, want false/true", src.onSubnet, src.borrowed)
		}
	})

	t.Run("falls_back_when_the_lease_carried_no_mask", func(t *testing.T) {
		// A bare address with no prefix cannot say what "on-subnet"
		// means, so there is nothing to match against.
		stubAddrList(t, []netlink.Addr{addrOn(t, "192.168.101.2/24")}, nil)
		src, err := pickProbeSource(link, net.IPv4(192, 168, 101, 42), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if src.onSubnet {
			t.Error("claimed on-subnet with no subnet to be on")
		}
	})

	// The parent holding the leased address IS the conflict — the host
	// is the squatter. Sending from it would ask the address about
	// itself and resolve locally, reporting clean.
	t.Run("never_sends_from_the_address_under_test", func(t *testing.T) {
		stubAddrList(t, []netlink.Addr{
			addrOn(t, "192.168.101.42/24"), // the leased address itself
			addrOn(t, "192.168.101.2/24"),
		}, nil)
		src, err := pickProbeSource(link, net.IPv4(192, 168, 101, 42), subnet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := src.addr.IP.String(); got == "192.168.101.42" {
			t.Fatal("the probe would have sent from the very address it is asking about, which " +
				"resolves locally and reports the host's own squat as clean")
		} else if got != "192.168.101.2" {
			t.Errorf("source = %s, want the other on-subnet address 192.168.101.2", got)
		}
	})

	t.Run("falls_back_when_the_only_on_subnet_address_is_the_target", func(t *testing.T) {
		stubAddrList(t, []netlink.Addr{addrOn(t, "192.168.101.42/24")}, nil)
		src, err := pickProbeSource(link, net.IPv4(192, 168, 101, 42), subnet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if src.onSubnet {
			t.Error("claimed an on-subnet source while the only candidate was the target itself")
		}
		if !src.borrowed {
			t.Error("expected the link-local fallback")
		}
	})

	t.Run("an_unreadable_address_list_is_an_error_not_a_silent_downgrade", func(t *testing.T) {
		// Downgrading here would be the #524 shape one level up: the
		// probe would quietly become the weak one and still report
		// verdicts in the strong one's name.
		stubAddrList(t, nil, errors.New("netlink boom"))
		if _, err := pickProbeSource(link, net.IPv4(192, 168, 101, 42), subnet); err == nil {
			t.Fatal("expected an error when the parent's addresses cannot be read")
		}
	})
}
