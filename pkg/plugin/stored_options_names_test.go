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

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// TestNetOptions_RefusesStoredIllegalNames is the #727 regression.
//
// #705 validates opts.Bridge and opts.Parent in validateModeOptions,
// which CreateNetwork calls. Every case below writes the record
// DIRECTLY, without going through CreateNetwork, because that is the
// state the defect is about: a network whose options were persisted
// before name validation existed, or backfilled from NetworkInspect,
// or written into the state directory by hand. CreateNetwork is not in
// the picture and cannot be.
//
// Against the pre-fix tree every refusal case returns the options
// happily and the caller hands the name to netlink.
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
			// The mode says bridge, the record also carries a parent.
			// CreateNetwork forbids the pairing, so this record could
			// only come from outside CreateNetwork -- which is the
			// whole premise. A mode-gated check would read the mode
			// (from the same untrusted record) and skip the parent.
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
			// Docker errors on any call: a refusal must come from the
			// record on disk, not from a fallback that happened to
			// fail for an unrelated reason.
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
			// Assert the property, not the sentinel: two different
			// sentinels are correct here (a bad name is ErrIPAM, a bad
			// mode is ErrInvalidMode) and what both must deliver is a
			// 4xx. An unwrapped error falls through ErrToStatus's
			// default and 500s what is really a bad request, which
			// tells an operator the plugin broke rather than that
			// their network record did.
			if got := util.ErrToStatus(err); got != http.StatusBadRequest {
				t.Errorf("error %v maps to HTTP %d, want 400 — an unwrapped sentinel reports a broken plugin instead of a broken record", err, got)
			}
			if got := p.networkOptionsRejected.Load(); got != 1 {
				t.Errorf("networkOptionsRejected: got %d, want 1 — a refusal nothing counts is the invisibility that hid #721", got)
			}
		})
	}
}

// TestNetOptions_RefusesADockerServedIllegalName covers the fallback
// arm separately.
//
// The disk arm and the NetworkInspect arm are different code with
// different provenance, and the fallback is the one that serves
// networks predating option persistence -- the oldest records on any
// host, and the least likely to have been validated. It also backfills
// what it decodes, so a refusal here must ALSO not leave the bad name
// on disk for the next call to find.
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

// TestDeleteEndpoint_TearsDownDespiteARefusedName pins the exemption in
// the direction that costs something.
//
// Every other handler must refuse. DeleteEndpoint must not: it reads
// only the mode, and a refusal here strands the veth pair, the ledger
// entry and the DHCP lease of a container that is already gone. That
// is a leak caused by a guard, which is worse than the name the guard
// refused.
//
// Asserting the tombstone rather than the return value is deliberate:
// a DeleteEndpoint that returned nil while skipping the tombstone would
// pass a return-value check and still cost the next container its MAC.
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

	// The host veth does not exist, which DeleteEndpoint treats as
	// already-torn-down and reports as success -- the same path a
	// forced teardown takes in production.
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

// TestNetOptionsRaw_HasNoOtherCallers is the executable half of the
// funnel.
//
// netOptionsRaw is the decode without the name check. Its whole safety
// argument is that exactly two functions call it: netOptions, which
// adds the check, and netMode, which returns no name to misuse. A third
// caller would be a sink reading a stored name with the guard
// bypassed -- which is precisely the defect this file fixes, reappearing
// one function at a time.
//
// A comment saying "do not call this" is the shape of guard this repo
// has already been burned by. This one goes red.
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
			// Skip the declaration itself, and comments -- this file's
			// own prose names the function repeatedly.
			if strings.HasPrefix(strings.TrimSpace(line), "//") ||
				strings.Contains(line, "func (p *Plugin) netOptionsRaw(") {
				continue
			}
			// No trailing "(" in the match, deliberately: a method
			// VALUE carries none.
			//
			//	f := p.netOptionsRaw
			//	o, err := f(ctx, id)
			//
			// survived the "netOptionsRaw(" form, and so did passing
			// p.netOptionsRaw as an argument to a wrapper -- the more
			// plausible of the two. A gate whose comment claims "no
			// other callers" while its pattern only sees direct calls
			// is a sentence wider than its check, which is the #758
			// defect one file over.
			if strings.Contains(line, "netOptionsRaw") {
				got[enclosing] = true
			}
		}
	}

	// An empty subject set would make every assertion below vacuous.
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

// TestTeardownBranchesResolveTheSameLinkName is why DeleteEndpoint can
// survive a mode it cannot read.
//
// subLinkName and vethPairNames' host half are both "dh-" + the first
// 12 bytes of the endpoint ID. They live in different files and nothing
// makes them agree except that both were written that way. DeleteEndpoint
// now depends on the agreement: it lets an unrecognised mode fall into
// the bridge branch, and that is only harmless while the bridge branch
// looks up the same link the parent-attached branch would have.
//
// Change either prefix and this goes red. Without it, the change would
// instead show up as a macvlan child and its lease quietly surviving a
// delete that reported success -- in production, on a network whose
// options were already broken, which is nobody's first suspect.
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

// TestDeleteEndpoint_UnknownModeSkipsTheTombstone pins what an
// unreadable mode ACTUALLY costs.
//
// The fear it does not cost: a stranded link. The two teardown branches
// resolve the same name (above), so the branch the guess lands in still
// deletes the link. This test exists because the first version of this
// fix ran BOTH teardowns to avoid a leak that could not happen, and the
// mutant that should have proven it necessary passed -- which is how
// the equality above was found at all.
//
// What it does cost is the tombstone, which is gated on the mode and on
// nothing else. `mode != ModeIPvlan` reads true for garbage, so an
// ipvlan endpoint whose mode cannot be read gets a tombstone laid for
// it. ipvlan children all share the PARENT's MAC. That tombstone hands
// the parent MAC to whichever container consumes it next, and occupies
// the slot the "exactly one match" rule counts.
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
