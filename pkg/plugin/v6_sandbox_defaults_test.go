// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// sandboxDefaultsDir stands in for /proc/sys/net/ipv6/conf with the kernel's fresh-namespace defaults (#1145).
func sandboxDefaultsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"default", "all"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for _, knob := range []string{"accept_ra", "autoconf", "keep_addr_on_down"} {
			seed := "1"
			if knob == "keep_addr_on_down" {
				seed = "0"
			}
			if err := os.WriteFile(filepath.Join(dir, sub, knob), []byte(seed+"\n"), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	return dir
}

func sandboxKnob(t *testing.T, dir, sub, knob string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, sub, knob))
	if err != nil {
		t.Fatalf("read %s/%s: %v", sub, knob, err)
	}
	return strings.TrimSpace(string(b))
}

func TestWriteV6SandboxDefaultsUnder_WritesTheTwoDefaultsAndReadsThemBack(t *testing.T) {
	dir := sandboxDefaultsDir(t)
	res := writeV6SandboxDefaultsUnder(dir)
	if res.Failures != 0 || res.Err != nil {
		t.Fatalf("a writable directory reported %d failure(s): %v", res.Failures, res.Err)
	}
	if res.PriorAcceptRA != "1" {
		t.Errorf("PriorAcceptRA = %q, want the seeded 1", res.PriorAcceptRA)
	}
	contract := dhcp.RouterAdvertGuardContract()
	for _, knob := range []string{"accept_ra", "autoconf"} {
		if got := sandboxKnob(t, dir, "default", knob); got != "0" || got != contract[knob] {
			t.Errorf("default/%s reads %q after the write, want 0 (contract %q)", knob, got, contract[knob])
		}
	}
}

func TestWriteV6SandboxDefaultsUnder_NeverWritesAllOrKeepAddrOnDown(t *testing.T) {
	dir := sandboxDefaultsDir(t)
	writeV6SandboxDefaultsUnder(dir)
	for _, knob := range []string{"accept_ra", "autoconf"} {
		if got := sandboxKnob(t, dir, "all", knob); got != "1" {
			t.Errorf("all/%s reads %q: the 1.x record says all/ does not reach an existing link, so it is not written (#821)", knob, got)
		}
	}
	if got := sandboxKnob(t, dir, "all", "keep_addr_on_down"); got != "0" {
		t.Errorf("all/keep_addr_on_down reads %q, want the seed", got)
	}
	if got := sandboxKnob(t, dir, "default", "keep_addr_on_down"); got != "0" {
		t.Errorf("default/keep_addr_on_down reads %q: it is per-link state and means nothing as a default", got)
	}

	bare := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bare, "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeV6SandboxDefaultsUnder(bare)
	entries, err := os.ReadDir(filepath.Join(bare, "default"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "accept_ra" && e.Name() != "autoconf" {
			t.Errorf("the write created default/%s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(bare, "all")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the write touched all/: stat err = %v", err)
	}
}

func TestWriteV6SandboxDefaultsUnder_AnUnwritableDirectoryCountsOneFailurePerKnob(t *testing.T) {
	dir := sandboxDefaultsDir(t)
	for _, knob := range []string{"accept_ra", "autoconf"} {
		p := filepath.Join(dir, "default", knob)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	res := writeV6SandboxDefaultsUnder(dir)
	if res.Failures != 2 {
		t.Errorf("Failures = %d, want 2 (one per knob): %v", res.Failures, res.Err)
	}
	if res.Err == nil {
		t.Error("Err is nil beside counted failures, so the Warn would name nothing")
	}
}

func TestWriteV6SandboxDefaults_AnUnopenableSandboxIsOneCountedFailureNotAPanic(t *testing.T) {
	res := writeV6SandboxDefaults("/nonexistent/sandbox")
	if res.Failures != 1 || res.Err == nil {
		t.Errorf("an unopenable sandbox gave %d failure(s), err %v; want 1 and a cause", res.Failures, res.Err)
	}
}

func TestJoin_WritesTheSandboxDefaultsForAnIPv6NetworkAndCountsAFailureWithoutRefusing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     DHCPNetworkOptions
		gateway6 string
		noHint   bool
		wantCall bool
	}{
		{name: "ipv6 on, gateway in the hint", opts: DHCPNetworkOptions{Bridge: "lo", IPv6: true}, gateway6: "fe80::1", wantCall: true},
		// A hint without a router is as guarded as one with it (#1145).
		{name: "ipv6 on, no gateway in the hint", opts: DHCPNetworkOptions{Bridge: "lo", IPv6: true}, wantCall: true},
		// The reacquisition supplies the hint after Join found none (#1145).
		{name: "ipv6 on, no stored hint, reacquired", opts: DHCPNetworkOptions{Bridge: "lo", IPv6: true}, gateway6: "fe80::1", noHint: true, wantCall: true},
		{name: "ipv6 off", opts: DHCPNetworkOptions{Bridge: "lo"}, wantCall: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			if err := saveOptions("n1145", tc.opts); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}
			var keys []string
			prev := v6SandboxDefaultsWriter
			v6SandboxDefaultsWriter = func(key string) v6SandboxDefaultsResult {
				keys = append(keys, key)
				return v6SandboxDefaultsResult{Failures: 2, Err: errors.New("seam: refused")}
			}
			t.Cleanup(func() { v6SandboxDefaultsWriter = prev })

			p := &Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
				joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)}
			mac, _ := net.ParseMAC("02:42:c0:a8:63:3d")
			a6, _ := netlink.ParseAddr("fd00:99::61/64")
			hint := joinHint{IPv6: a6, MacAddress: mac, GatewayIPv6: tc.gateway6}
			if tc.noHint {
				prevRe := reacquireEndpointFn
				reacquireEndpointFn = func(p *Plugin, _ context.Context, r JoinRequest, _ DHCPNetworkOptions) error {
					p.storeJoinHint(r.EndpointID, hint)
					return nil
				}
				t.Cleanup(func() { reacquireEndpointFn = prevRe })
			} else {
				p.storeJoinHint("e1145", hint)
			}
			res, err := p.Join(context.Background(), JoinRequest{NetworkID: "n1145", EndpointID: "e1145", SandboxKey: "/var/run/docker/netns/abc"})
			if err != nil {
				t.Fatalf("Join refused over a failed sandbox write: %v", err)
			}
			p.mu.Lock()
			m := p.persistentDHCP["e1145"]
			p.mu.Unlock()
			if m != nil {
				abortJoinAttach(t, p, "e1145", m)
			}

			if tc.wantCall {
				if len(keys) != 1 || keys[0] != "/var/run/docker/netns/abc" {
					t.Errorf("writer called with %v, want once with the request's sandbox key", keys)
				}
				if got := p.routerAdvertGuardFailures.Load(); got != 2 {
					t.Errorf("router_advert_guard_failures = %d after a write that failed twice, want 2", got)
				}
				if res.GatewayIPv6 != tc.gateway6 {
					t.Errorf("Join returned IPv6 gateway %q after a failed write, want the hint's %q", res.GatewayIPv6, tc.gateway6)
				}
				return
			}
			if len(keys) != 0 {
				t.Errorf("writer called %d time(s) for a network with IPv6 off", len(keys))
			}
			if got := p.routerAdvertGuardFailures.Load(); got != 0 {
				t.Errorf("router_advert_guard_failures = %d for a network with IPv6 off", got)
			}
		})
	}
}

func TestWriteV6SandboxDefaultsUnder_ASecondWriteIntoASandboxAlreadyAtZeroCountsNoFailure(t *testing.T) {
	dir := sandboxDefaultsDir(t)
	for i := 0; i < 2; i++ {
		if res := writeV6SandboxDefaultsUnder(dir); res.Failures != 0 || res.Err != nil {
			t.Fatalf("write %d counted %d failure(s): %v", i+1, res.Failures, res.Err)
		}
	}
}
