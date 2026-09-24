// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func TestNetOptions_RefusesStoredIllegalNames(t *testing.T) {
	cases := []struct {
		name       string
		opts       DHCPNetworkOptions
		wantRefuse bool
		reason     string
	}{
		{
			name:   "a legal bridge is served",
			opts:   DHCPNetworkOptions{Bridge: "br0"},
			reason: "the ordinary case must not become a refusal",
		},
		{
			name:   "a legal parent is served",
			opts:   DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0"},
			reason: "the ordinary macvlan case must not become a refusal",
		},
		{
			name:       "a NUL in the stored bridge",
			opts:       DHCPNetworkOptions{Bridge: "br0\x00evil"},
			wantRefuse: true,
			reason:     "netlink hands the name to the kernel zero-terminated, so this resolves br0 while reading as something else to every Go comparison we make",
		},
		{
			name:       "a NUL in the stored parent",
			opts:       DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0\x00evil"},
			wantRefuse: true,
			reason:     "the parent reaches the same netlink calls the bridge does",
		},
		{
			name:       "a newline in the stored bridge",
			opts:       DHCPNetworkOptions{Bridge: "br0\nname servers 1.2.3.4"},
			wantRefuse: true,
			reason:     "the name is interpolated into a generated dhcpcd config, where a newline is a directive (#692's shape, reached through the record instead of the option)",
		},
		{
			name:       "a path separator in the stored bridge",
			opts:       DHCPNetworkOptions{Bridge: "../../etc/br0"},
			wantRefuse: true,
			reason:     "names are used to build per-interface paths as well as netlink calls",
		},
		{
			name:       "an over-length stored bridge",
			opts:       DHCPNetworkOptions{Bridge: strings.Repeat("b", 16)},
			wantRefuse: true,
			reason:     "IFNAMSIZ is 16 including the terminator; the kernel would refuse it, and we should say so with the network id rather than as an opaque netlink error",
		},
		{
			name:       "an unknown stored mode",
			opts:       DHCPNetworkOptions{Mode: "brdige", Bridge: "br0"},
			wantRefuse: true,
			reason:     "effectiveMode normalises only the empty value, so an unrecognised one is neither rejected nor defaulted -- it just fails every == test it meets and lands in whichever branch is written last",
		},
		{
			name:       "an illegal parent that this mode does not use",
			opts:       DHCPNetworkOptions{Bridge: "br0", Parent: "eth0\x00evil"},
			wantRefuse: true,
			reason:     "deciding which fields to distrust by reading a field from the same record trusts it to say it is untrustworthy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			if err := saveOptions("n1", tc.opts); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}
			p := &Plugin{docker: &fakeDocker{inspectErr: errors.New("docker must not be called")}}

			_, err := p.netOptions(context.Background(), "n1")

			if !tc.wantRefuse {
				if err != nil {
					t.Fatalf("netOptions: %v — %s", err, tc.reason)
				}
				if got := p.networkOptionsRejected.Load(); got != 0 {
					t.Errorf("networkOptionsRejected: got %d, want 0", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("netOptions returned no error for %+v — %s", tc.opts, tc.reason)
			}
			if got := util.ErrToStatus(err); got != http.StatusBadRequest {
				t.Errorf("error %v maps to HTTP %d, want 400 — an unwrapped sentinel reports a broken plugin instead of a broken record", err, got)
			}
			if got := p.networkOptionsRejected.Load(); got != 1 {
				t.Errorf("networkOptionsRejected: got %d, want 1 — a refusal nothing counts is the invisibility that hid #721", got)
			}
		})
	}
}

func TestNetOptions_RefusesADockerServedIllegalName(t *testing.T) {
	withStateDir(t, t.TempDir())
	f := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"n1": {ID: "n1", Driver: testDHCPDriver, Options: map[string]string{"bridge": "br0\x00evil"}},
		},
	}
	p := &Plugin{docker: f}

	if _, err := p.netOptions(context.Background(), "n1"); err == nil {
		t.Fatal("netOptions accepted a NUL-bearing bridge served by NetworkInspect")
	}
	if got := p.networkOptionsRejected.Load(); got != 1 {
		t.Errorf("networkOptionsRejected: got %d, want 1", got)
	}
}

func TestDeleteEndpoint_TearsDownDespiteARefusedName(t *testing.T) {
	withStateDir(t, t.TempDir())
	if err := saveOptions("n1", DHCPNetworkOptions{Bridge: "br0\x00evil"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	p := &Plugin{
		docker:               &fakeDocker{inspectErr: errors.New("docker must not be called")},
		endpointFingerprints: make(map[string]endpointFingerprint),
	}
	p.rememberEndpoint("ep-1", endpointFingerprint{
		MAC: "02:42:ac:11:00:02", IPv4: "192.168.0.50",
	}, dhcpHostname{name: "web"})

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: "n1", EndpointID: "ep-1",
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v — teardown must not be blocked by a stored name it never reads", err)
	}

	mac, ipv4, _, ok := p.tombstones.consume("n1", "web")
	if !ok {
		t.Fatal("no tombstone was written; the next start of this container loses its MAC because a name it never touches was rejected")
	}
	if mac != "02:42:ac:11:00:02" || ipv4 != "192.168.0.50" {
		t.Errorf("tombstone carries mac=%q ipv4=%q, want the recorded pair", mac, ipv4)
	}
	if got := p.networkOptionsRejected.Load(); got != 0 {
		t.Errorf("networkOptionsRejected: got %d, want 0 — DeleteEndpoint refused nothing, so it must not report a refusal", got)
	}
}

func TestNetOptionsRaw_HasNoOtherCallers(t *testing.T) {
	funcRe := regexp.MustCompile(`^func (?:\([^)]*\) )?(\w+)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	got := map[string]bool{}
	scanned := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		scanned++
		enclosing := ""
		for _, line := range strings.Split(string(src), "\n") {
			if m := funcRe.FindStringSubmatch(line); m != nil {
				enclosing = m[1]
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") ||
				strings.Contains(line, "func (p *Plugin) netOptionsRaw(") {
				continue
			}
			if strings.Contains(line, "netOptionsRaw") {
				got[enclosing] = true
			}
		}
	}

	if scanned == 0 {
		t.Fatal("scanned no non-test .go files; the check would pass for the wrong reason")
	}

	want := map[string]bool{"netOptions": true, "netMode": true}
	for fn := range got {
		if !want[fn] {
			t.Errorf("%s calls netOptionsRaw: it reads stored options WITHOUT the interface-name check. "+
				"Call netOptions (which validates) or netMode (which returns no name) — or, if this really is a "+
				"third legitimate funnel, say why here and add it to the allowed set (#727)", fn)
		}
	}
	for fn := range want {
		if !got[fn] {
			t.Errorf("%s no longer calls netOptionsRaw; this test's allowed set has drifted from the code and is "+
				"no longer checking what it claims to", fn)
		}
	}
}

func TestTeardownBranchesResolveTheSameLinkName(t *testing.T) {
	for _, id := range []string{
		"ep-1",
		"0123456789ab",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"",
	} {
		host, _ := vethPairNames(id)
		if sub := subLinkName(id); sub != host {
			t.Errorf("endpoint %q: bridge teardown looks up %q, parent-attached teardown looks up %q. "+
				"DeleteEndpoint tolerates an unreadable mode by relying on these being equal; they are not, "+
				"so the branch it guesses now determines whether the link survives (#727)", id, host, sub)
		}
	}
}

func TestDeleteEndpoint_UnknownModeSkipsTheTombstone(t *testing.T) {
	withStateDir(t, t.TempDir())
	if err := saveOptions("n1", DHCPNetworkOptions{Mode: "brdige", Bridge: "br0"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}

	restore := nlLinkByName
	nlLinkByName = func(string) (netlink.Link, error) { return nil, netlink.LinkNotFoundError{} }
	t.Cleanup(func() { nlLinkByName = restore })

	p := &Plugin{
		docker:               &fakeDocker{inspectErr: errors.New("docker must not be called")},
		endpointFingerprints: make(map[string]endpointFingerprint),
	}
	p.rememberEndpoint("ep-1", endpointFingerprint{MAC: "02:42:ac:11:00:02", IPv4: "192.168.0.50"}, dhcpHostname{name: "web"})

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: "n1", EndpointID: "ep-1",
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v — an unusable record must not block teardown", err)
	}

	if _, _, _, ok := p.tombstones.consume("n1", "web"); ok {
		t.Error("a tombstone was written for a network whose mode could not be read; if that mode was ipvlan " +
			"it carries the parent MAC into whichever container consumes it next")
	}
	if got := p.networkOptionsRejected.Load(); got != 1 {
		t.Errorf("networkOptionsRejected: got %d, want 1 — teardown continued, but it continued over a record "+
			"it could not read, and that has to be visible", got)
	}
}
