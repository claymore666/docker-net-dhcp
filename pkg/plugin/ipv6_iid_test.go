// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func TestDecodeOpts_IPv6IIDKeepsTheSpellingItWasGiven(t *testing.T) {
	for _, v := range []string{"eui64", "stable-privacy"} {
		opts, err := decodeOpts(map[string]interface{}{"ipv6_iid": v})
		if err != nil || opts.IPv6IID != v {
			t.Errorf("ipv6_iid=%q decoded to %q, %v, want it unchanged (#1032)", v, opts.IPv6IID, err)
		}
	}
	opts, err := decodeOpts(map[string]interface{}{"ipv6_iid": ""})
	if err != nil || opts.IPv6IID != "" {
		t.Errorf("ipv6_iid written with no value decoded to %q, %v, want unset", opts.IPv6IID, err)
	}
}

func TestValidateIPv6Options_IPv6IIDValues(t *testing.T) {
	slaac := func(iid string) DHCPNetworkOptions {
		return DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac", IPv6IID: iid}
	}
	for _, tc := range []struct {
		name    string
		opts    DHCPNetworkOptions
		wantErr bool
		need    []string
	}{
		{"unset on slaac", slaac(""), false, nil},
		{"eui64 on slaac", slaac("eui64"), false, nil},
		{"stable-privacy on slaac", slaac("stable-privacy"), false, nil},
		{"stable-privacy on auto", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto", IPv6IID: "stable-privacy"}, false, nil},
		{"stable-privacy on macvlan slaac", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0", IPv6Mode: "slaac", IPv6IID: "stable-privacy"}, false, nil},
		{"a misspelling", slaac("stable_privacy"), true, []string{"ipv6_iid", "stable-privacy"}},
		{"another case", slaac("EUI64"), true, []string{"ipv6_iid", "eui64"}},
		{"a misspelling with no IPv6 at all", DHCPNetworkOptions{Bridge: "br0", IPv6IID: "random"}, true, []string{"ipv6_iid"}},
		{"stable-privacy on dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6IID: "stable-privacy"}, true, []string{"ipv6_iid=stable-privacy", "ipv6_mode=dhcp", "issue #1032"}},
		{"stable-privacy on the old ipv6 switch", DHCPNetworkOptions{Bridge: "br0", IPv6: true, IPv6IID: "stable-privacy"}, true, []string{"ipv6_mode=dhcp", "issue #1032"}},
		{"stable-privacy on off", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "off", IPv6IID: "stable-privacy"}, true, []string{"ipv6_mode=off", "issue #1032"}},
		{"stable-privacy with no IPv6 at all", DHCPNetworkOptions{Bridge: "br0", IPv6IID: "stable-privacy"}, true, []string{"ipv6_mode=off", "issue #1032"}},
		{"eui64 on dhcp does nothing wrong", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6IID: "eui64"}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIPv6Options(tc.opts, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateIPv6Options = %v, want an error: %v", err, tc.wantErr)
			}
			for _, need := range tc.need {
				if err == nil || !strings.Contains(err.Error(), need) {
					t.Errorf("the refusal %v does not say %q", err, need)
				}
			}
		})
	}
}

// The stored options pass the same function, so a hand-edited file cannot start a client that cannot act on them
// (#1032).
func TestCheckStoredOptions_RefusesStablePrivacyWhereItCannotAct(t *testing.T) {
	p := &Plugin{}
	if err := p.checkStoredOptions("n1", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6IID: "stable-privacy"}); err == nil {
		t.Error("checkStoredOptions accepted ipv6_iid=stable-privacy on ipv6_mode=dhcp")
	}
	if got := p.networkOptionsRejected.Load(); got != 1 {
		t.Errorf("network_options_rejected = %d, want 1", got)
	}
}

func TestSaveLoadOptions_KeepsIPv6IID(t *testing.T) {
	withStateDir(t, t.TempDir())
	want := DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac", IPv6IID: "stable-privacy"}
	if err := saveOptions("net123", want); err != nil {
		t.Fatal(err)
	}
	got, err := loadOptions("net123")
	if err != nil || got.IPv6IID != "stable-privacy" {
		t.Errorf("loadOptions = %+v, %v, want ipv6_iid=stable-privacy: the option would not survive a plugin restart (#1032)", got, err)
	}
}

func TestV6Wiring_StablePrivacyCarriesTheModeTheSecretAndTheNetworkID(t *testing.T) {
	id6 := dhcp.Identity6{DUID: []byte{0, 4, 1, 2, 3, 4}, IAID: 1}
	const netA, netB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, mode := range []string{"slaac", "auto"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			withStateDir(t, dir)
			opts := DHCPNetworkOptions{Bridge: "br0", IPv6Mode: mode, IPv6IID: "stable-privacy"}
			p := &Plugin{}
			var a, b dhcp.DHCPClientOptions
			if err := p.v6Wiring(&a, opts, id6, "rec-1", "", "endpoint-1", netA); err != nil {
				t.Fatalf("v6Wiring: %v", err)
			}
			if err := p.v6Wiring(&b, opts, id6, "rec-1", "", "endpoint-2", netB); err != nil {
				t.Fatalf("v6Wiring: %v", err)
			}
			if a.IPv6IID != proto.IIDModeStablePrivacy {
				t.Errorf("IPv6IID = %v, want stable-privacy: the option reaches no client (#1032)", a.IPv6IID)
			}
			file, err := os.ReadFile(filepath.Join(dir, "ipv6-iid-secret"))
			if err != nil || len(file) == 0 || !bytes.Equal(a.IPv6IIDSecret, file) || !bytes.Equal(b.IPv6IIDSecret, file) {
				t.Errorf("the clients carry secrets %x and %x, want the one in STATE_DIR %x (%v)", a.IPv6IIDSecret, b.IPv6IIDSecret, file, err)
			}
			if string(a.IPv6IIDNetworkID) != netA || string(b.IPv6IIDNetworkID) != netB {
				t.Errorf("Network_ID is %q and %q, want each client's own Docker network id", a.IPv6IIDNetworkID, b.IPv6IIDNetworkID)
			}
		})
	}
}

func TestV6Wiring_AnEUI64NetworkNeverTouchesTheSecret(t *testing.T) {
	id6 := dhcp.Identity6{DUID: []byte{0, 4, 1, 2, 3, 4}, IAID: 1}
	for _, iid := range []string{"", "eui64"} {
		dir := t.TempDir()
		withStateDir(t, dir)
		var base dhcp.DHCPClientOptions
		opts := DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac", IPv6IID: iid}
		if err := (&Plugin{}).v6Wiring(&base, opts, id6, "rec-1", "", "endpoint-1", "net-1"); err != nil {
			t.Fatalf("ipv6_iid=%q: v6Wiring: %v", iid, err)
		}
		if base.IPv6IID != proto.IIDModeEUI64 || base.IPv6IIDSecret != nil || base.IPv6IIDNetworkID != nil {
			t.Errorf("ipv6_iid=%q: client carries mode %v, secret %x, network %q, want eui64 and nothing else", iid, base.IPv6IID, base.IPv6IIDSecret, base.IPv6IIDNetworkID)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("ipv6_iid=%q: an eui64 network wrote %d file(s) into STATE_DIR (#1032)", iid, len(entries))
		}
	}
	// A STATE_DIR that does not exist is no obstacle to an eui64 network.
	withStateDir(t, filepath.Join(t.TempDir(), "gone"))
	var base dhcp.DHCPClientOptions
	if err := (&Plugin{}).v6Wiring(&base, DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac"}, id6, "rec-1", "", "e", "n"); err != nil {
		t.Errorf("an eui64 network failed on a missing STATE_DIR it never reads: %v", err)
	}
}

func TestV6Wiring_AShortSecretFileFailsTheClientStart(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	path := filepath.Join(dir, "ipv6-iid-secret")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	var base dhcp.DHCPClientOptions
	opts := DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac", IPv6IID: "stable-privacy"}
	err := (&Plugin{}).v6Wiring(&base, opts, dhcp.Identity6{DUID: []byte{0, 4, 1}, IAID: 1}, "rec-1", "", "e", "n")
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("v6Wiring = %v, want a refusal naming %s: the client must not start on a weak secret (#1032)", err, path)
	}
	if got, _ := os.ReadFile(path); string(got) != "short" {
		t.Errorf("the short file was rewritten to %q", got)
	}
}

// Every DHCPv6 client is wired by v6Wiring, which is the one place the identifier mode is assigned: a literal that sets it
// alone would run stable-privacy with an empty secret or without the option (#1032).
func TestIPv6IIDIsAssignedOnlyByTheV6Wiring(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var owner int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for i, line := range pluginSourceLines(t, f) {
			set := false
			for _, field := range []string{"IPv6IID", "IPv6IIDSecret", "IPv6IIDNetworkID"} {
				set = set || strings.Contains(line, "."+field+" =") || strings.Contains(line, field+":")
			}
			if !set {
				continue
			}
			if f != v6WiringOwner {
				t.Errorf("%s:%d sets a stable-privacy input outside %s:\n  %s", f, i+1, v6WiringOwner, strings.TrimSpace(line))
				continue
			}
			owner++
		}
	}
	if owner < 3 {
		t.Errorf("%s sets %d of the three stable-privacy inputs, want all of them", v6WiringOwner, owner)
	}
}

// A stable-privacy client has to carry the Docker network's id and the secret from every site that opens a DHCPv6 client;
// a site that passed no network id would form every network's address from the same input (#1032).
func TestStablePrivacy_TheOneShotCarriesTheNetworkIDAndTheSecret(t *testing.T) {
	const netID = absenceNetA
	var got *dhcp.DHCPClientOptions
	restore := dhcpGetIP
	dhcpGetIP = func(_ context.Context, _ string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		c := *o
		got = &c
		return dhcp.Info{IP: "2001:db8::1/64", SLAAC: true}, dhcp.RAObservation{Seen: true}, nil
	}
	t.Cleanup(func() { dhcpGetIP = restore })

	p := newTestPlugin(t)
	dir := t.TempDir()
	withStateDir(t, dir)
	opts := DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac", IPv6IID: "stable-privacy"}
	_, err := p.acquireInitialV6(t.Context(), opts, dhcp.DHCPClientOptions{}, v6Acquire{iface: "eth0", networkID: netID,
		endpointID: "ep-1", callStart: time.Now(), timeout: 30 * time.Second})
	if err != nil || got == nil {
		t.Fatalf("acquireInitialV6 = %v, client options %v", err, got)
	}
	if got.IPv6IID != proto.IIDModeStablePrivacy || string(got.IPv6IIDNetworkID) != netID {
		t.Errorf("one-shot options carry IID %v network id %q, want stable-privacy and %q (#1032)", got.IPv6IID, got.IPv6IIDNetworkID, netID)
	}
	want, err := os.ReadFile(filepath.Join(dir, "ipv6-iid-secret"))
	if err != nil || len(want) != iidSecretLen || !bytes.Equal(got.IPv6IIDSecret, want) {
		t.Errorf("one-shot secret %x, want the %d bytes in the secret file (%v)", got.IPv6IIDSecret, iidSecretLen, err)
	}
}

func TestStablePrivacy_ThePersistentClientCarriesTheNetworkIDAndTheSecret(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	m, _ := daemonFreeManager(t, &fakeDocker{})
	m.opts.Bridge, m.opts.IPv6Mode, m.opts.IPv6IID = "br0", "slaac", "stable-privacy"
	m.ctrLink = &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: 1, Name: "lo", HardwareAddr: m.MacAddress}}
	var got *dhcp.DHCPClientOptions
	prev := newDHCPClient
	newDHCPClient = func(_ string, o *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
		c := *o
		got = &c
		return nil, errors.New("no client in this test")
	}
	t.Cleanup(func() { newDHCPClient = prev })
	_, _ = m.setupClient(true)
	if got == nil {
		t.Fatal("setupClient(true) opened no client")
	}
	if got.IPv6IID != proto.IIDModeStablePrivacy || string(got.IPv6IIDNetworkID) != m.joinReq.NetworkID {
		t.Errorf("persistent client carries IID %v network id %q, want stable-privacy and %q (#1032)", got.IPv6IID, got.IPv6IIDNetworkID, m.joinReq.NetworkID)
	}
	want, err := os.ReadFile(filepath.Join(dir, "ipv6-iid-secret"))
	if err != nil || !bytes.Equal(got.IPv6IIDSecret, want) {
		t.Errorf("persistent client secret %x, want the secret file's bytes (%v)", got.IPv6IIDSecret, err)
	}
}
