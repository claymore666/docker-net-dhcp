// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// releaseVisibleBudget bounds the wait for a release to reach the
// server's lease DB.
//
// It bounds a POSITIVE event and is therefore spent only when something
// is wrong: the loop below returns as soon as the entry is gone. RFC
// 2131 section 4.4.6 defines no answer to a DHCPRELEASE, so what is
// being waited on is dnsmasq receiving one datagram on a loopback-speed
// veth and rewriting its lease file, not a round trip.
const releaseVisibleBudget = 30 * time.Second

const releaseVisiblePoll = 250 * time.Millisecond

// leaseFileHolds reports whether dnsmasq's lease DB still has an entry
// for addr.
//
// THE LEASE FILE AND NOT THE LOG TOKEN, and the difference is the whole
// reason this helper exists. dnsmasq prints `DHCPRELEASE` on both
// families, and on its v4 path it prints the same token for a release
// it did not act on, with `ignored` appended to the same line
// (rfc2131.c). So a token count says a release ARRIVED; only the lease
// DB says the server gave the address up. Both families are read the
// same way: a v4 line is `<expiry> <mac> <addr> <hostname> <client-id>`
// and a v6 line is `<expiry> <iaid> <addr> <hostname> <duid>`, so the
// address is the third field in either.
func leaseFileHolds(t *testing.T, leaseFile, addr string) bool {
	t.Helper()
	data, err := os.ReadFile(leaseFile)
	if err != nil {
		t.Fatalf("read lease file %s: %v", leaseFile, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && strings.EqualFold(fields[2], addr) {
			return true
		}
	}
	return false
}

func waitLeaseFile(t *testing.T, leaseFile, addr string, want bool) bool {
	t.Helper()
	deadline := time.Now().Add(releaseVisibleBudget)
	for {
		if leaseFileHolds(t, leaseFile, addr) == want {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(releaseVisiblePoll)
	}
}

// TestReleaseLease_TheOptionIsRefusedAtCreateOrItIsNot pins the
// option's domain at the only place an operator meets it.
//
// `on_remove` is refused BY NAME, and the refusal says it is not
// available yet: libnetwork deletes an endpoint when its container
// STOPS, not when it is removed -- the tombstone that keeps a MAC across
// `docker restart` is written at `DeleteEndpoint` and consumed by the
// next `CreateEndpoint` inside 60 seconds, which is only possible if
// both run during the restart. A release hung off that handler would
// fire on every `docker stop`, which is this option's `on_stop`, and
// would never fire for `docker rm` of an already-stopped container.
// `on_remove` therefore arrives as a TIMED release in the next change on
// this milestone, so this test asserts today's refusal and says nothing
// about whether the behaviour can exist.
//
// The accepted rows are the half that stops the refusal from being
// "refuse everything": a create that fails for both values would pass a
// test written only in the refusing direction.
func TestReleaseLease_TheOptionIsRefusedAtCreateOrItIsNot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	for _, tc := range []struct {
		name     string
		value    string
		wantErr  bool
		mentions string
	}{
		{name: "on_stop is implemented", value: "on_stop"},
		{name: "never is the default and is spellable", value: "never"},
		{name: "on_remove is refused by name", value: "on_remove", wantErr: true, mentions: "on_remove"},
		{name: "a typo is refused", value: "on_stpo", wantErr: true, mentions: "release_lease"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netName := "dhcptest-rl-" + strings.ReplaceAll(tc.value, "_", "-")
			_, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
				Driver: harness.DriverName,
				IPAM:   &network.IPAM{Driver: "null"},
				Options: map[string]string{
					"mode": "macvlan", "parent": harness.HostVeth, "release_lease": tc.value,
				},
			})
			t.Cleanup(func() { _ = cli.NetworkRemove(context.Background(), netName) })

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("release_lease=%q was refused: %v", tc.value, err)
				}
				if _, err := cli.NetworkInspect(ctx, netName, network.InspectOptions{}); err != nil {
					t.Errorf("the create was accepted and the network does not exist: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("release_lease=%q was accepted; an operator gets a network that "+
					"looks configured and behaves as `never`", tc.value)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("the refusal does not say %q, so the operator cannot tell which "+
					"option was wrong: %v", tc.mentions, err)
			}
		})
	}
}

// TestReleaseLease_OnStopHandsTheAddressBack is #962 asserted where it
// can actually be seen: the server's own lease database.
//
// # Why the lease DB and not the counter
//
// `releases_sent` says what the plugin believes it put on the wire.
// Only dnsmasq says whether it gave the address up, and the two can
// come apart in both directions -- a release the plugin counted and the
// server ignored (dnsmasq prints `DHCPRELEASE ... ignored` and moves
// nothing), or an address the server dropped for an unrelated reason.
// The counter is read here as well, afterwards, and only as the
// operator's view of the same event.
//
// # The MAC is the other half
//
// A released endpoint lays no tombstone, so its successor comes back
// under a different MAC. That is the cost of the option and it is
// asserted, because an implementation that released AND kept the
// tombstone would hand the next container an address the server has
// already put back in its pool.
func TestReleaseLease_OnStopHandsTheAddressBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-release"
		ctrName = "dh-itest-release-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"release_lease": "on_stop"})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)

	// The precondition and the positive control in one: an address the
	// server never recorded cannot be seen to go back, and a lease file
	// this test cannot read would produce the same "gone" as a release.
	//
	// IT WAITS FOR THE LEASE AND DELIBERATELY NOT FOR THE CLIENT. The
	// lease this finds is the one CreateEndpoint's one-shot won, and
	// the stop below may well arrive before the persistent client has
	// attached or bound. That is not a flaw in the test, it is the
	// case the option exists for, meaning `docker run --rm` and
	// anything else short-lived, and while the release was asked of a
	// running client it was also the case that could not work: this
	// test went red because the client that was asked did not exist
	// yet. The release is built from the lease record now, so the
	// client's state does not enter into it.
	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s, so the assertion below cannot "+
			"tell a release from a lease that was never recorded", ip)
	}

	// A counter window rather than two hand-rolled reads: the counters
	// live in the plugin process, and a delta taken across a recycle
	// would subtract two different processes' numbers and read as "no
	// change" (#405).
	w := harness.BeginCounterWindow(t, ctx, cli,
		"releases_sent_v4", "release_failures_v4", "releases_sent_v6", "release_failures_v6")
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	if !waitLeaseFile(t, fixture.LeaseFile(), ip, false) {
		t.Errorf("after `docker stop`, dnsmasq's lease DB still holds %s. "+
			"release_lease=on_stop asks for the address back at Leave, and the server "+
			"has not given it up.", ip)
	}
	if got := fixture.CountLogLines("DHCPRELEASE", ip) - releasesBefore; got < 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for %s across the stop, want at "+
			"least 1", got, ip)
	}

	// The operator's view of the same event, read after the outside
	// evidence and never instead of it.
	before, after := w.End()
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got < 1 {
		t.Errorf("releases_sent_v4 moved by %d across the stop, want at least 1: the "+
			"server gave the address up and the plugin did not count it", got)
	}
	if got := after.ReleaseFailuresV4 - before.ReleaseFailuresV4; got != 0 {
		t.Errorf("release_failures_v4 moved by %d on a release that reached the server", got)
	}
	if got := after.ReleasesSentV6 + after.ReleaseFailuresV6 - before.ReleasesSentV6 - before.ReleaseFailuresV6; got != 0 {
		t.Errorf("the v6 pair moved by %d on a v4-only network", got)
	}

	// And the cost. No tombstone was laid, so the restart is a new
	// endpoint with a new MAC asking for a new address.
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var macAfter, ipAfter string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				ipAfter, macAfter = ep.IPAddress, ep.MacAddress
			}
		}
		if ipAfter != "" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if ipAfter == "" {
		t.Fatalf("the container did not come back with an address after a release")
	}
	if strings.EqualFold(macAfter, mac) {
		t.Errorf("the restarted container came back on the same MAC %s. A released "+
			"endpoint must lay no tombstone: inheriting the MAC would have this "+
			"container ask for addresses the server has already put back in its pool",
			macAfter)
	}
}

// TestReleaseLease_DefaultNetworksStillKeepTheirAddresses is the
// preservation control for the whole option, and it pins the finding
// that decided its shape.
//
// Two things are asserted about a network that does not set
// `release_lease`, which is every network created before v2.2.0:
//
//   - neither release counter moves across a full stop/start cycle. A
//     release folded from intent, or an option read with the wrong
//     default, would move them here.
//   - the MAC survives the stop, and `tombstones_consumed` moves. That
//     is DELETE-ENDPOINT-RUNS-ON-STOP stated as a check: Docker tears
//     the endpoint down when the container stops, and the tombstone
//     written there is what the next start consumes. `docker rm` is
//     never called in this test. It is why `on_remove` cannot be built
//     on `DeleteEndpoint`, and it is measured here rather than asserted
//     in a comment.
func TestReleaseLease_DefaultNetworksStillKeepTheirAddresses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-norelease"
		ctrName = "dh-itest-norelease-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s; nothing below can be read", ip)
	}
	w := harness.BeginCounterWindow(t, ctx, cli,
		"tombstones_consumed", "releases_sent", "release_failures")

	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	time.Sleep(leaseRetentionSettle)
	if !leaseFileHolds(t, fixture.LeaseFile(), ip) {
		t.Errorf("dnsmasq gave %s up across a stop on a network that did not ask for it. "+
			"The default is `never`: the address stays leased until it expires, exactly "+
			"as it would for a physical host that was switched off", ip)
	}

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var macAfter, ipAfter string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				ipAfter, macAfter = ep.IPAddress, ep.MacAddress
			}
		}
		if ipAfter != "" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if !strings.EqualFold(macAfter, mac) {
		t.Errorf("MAC changed across a stop/start without a `docker rm`: before=%s "+
			"after=%q. The tombstone written at DeleteEndpoint is what keeps it, and "+
			"this is the evidence that DeleteEndpoint runs on a STOP", mac, macAfter)
	}
	if ipAfter != ip {
		t.Errorf("address changed across a stop/start: before=%s after=%q", ip, ipAfter)
	}

	before, after := w.End()
	if got := after.TombstonesConsumed - before.TombstonesConsumed; got < 1 {
		t.Errorf("tombstones_consumed moved by %d across a stop/start with no `docker rm`, "+
			"want at least 1. Docker deletes an endpoint when its container STOPS, which "+
			"is why release_lease=on_remove cannot be built on DeleteEndpoint", got)
	}
	for _, c := range []struct {
		name        string
		before, now int32
	}{
		{"releases_sent", before.ReleasesSent, after.ReleasesSent},
		{"release_failures", before.ReleaseFailures, after.ReleaseFailures},
	} {
		if got := c.now - c.before; got != 0 {
			t.Errorf("%s moved by %d on a network that does not set release_lease. "+
				"A counter that moves here is folded from something other than a "+
				"release this network never asked for", c.name, got)
		}
	}
}

// TestReleaseLease_OnStopHandsTheV6AddressBackToo is the v6 half, and
// it is a separate test because the two families release differently.
//
// RFC 9915 section 18.2.7: "The client MUST stop using all of the
// leases being released before the client begins the Release message
// exchange process. For an address, this means the address MUST have
// been removed from the interface." So the v6 arm takes the address off
// the container link first and sends nothing if that fails, while the
// v4 arm leaves its address in place because RFC 2131 section 3.1(6)
// identifies the binding by `ciaddr`. One arm can work with the other
// broken, in either direction, and only a dual-stack endpoint shows it.
//
// The observer is again the server's lease DB, per family. dnsmasq
// prints the token `DHCPRELEASE` on both paths, so a token count cannot
// tell a v6 release from the v4 one beside it.
func TestReleaseLease_OnStopHandsTheV6AddressBackToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-release6"
		ctrName = "dh-itest-release6-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"release_lease": "on_stop",
		"ipv6":          "true",
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, _ := harness.RunContainer(t, ctx, netName, ctrName)
	v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6 == "" {
		t.Fatalf("the endpoint took no DHCPv6 address, so there is no v6 release to observe")
	}
	t.Logf("container %s holds ip=%s v6=%s", ctrName, ip, v6)

	for _, addr := range []string{ip, v6} {
		// Same as the v4 test: this waits for the LEASE and not for the
		// v6 client. A persistent DHCPv6 client that never bound inside
		// the endpoint's life is exactly the shape this test used to go
		// red on, and the release no longer needs one.
		if !waitLeaseFile(t, fixture.LeaseFile(), addr, true) {
			t.Fatalf("dnsmasq's lease DB has no entry for %s; a release for it could not "+
				"be told from a lease that was never recorded", addr)
		}
	}

	w := harness.BeginCounterWindow(t, ctx, cli,
		"releases_sent_v4", "releases_sent_v6", "release_failures")

	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	for _, f := range []struct{ family, addr string }{{"v4", ip}, {"v6", v6}} {
		if !waitLeaseFile(t, fixture.LeaseFile(), f.addr, false) {
			t.Errorf("after `docker stop`, dnsmasq's lease DB still holds the %s address %s. "+
				"Both families release on a release_lease=on_stop network, and one arm "+
				"working is not the other arm working", f.family, f.addr)
		}
	}

	before, after := w.End()
	for _, c := range []struct {
		name        string
		before, now int32
	}{
		{"releases_sent_v4", before.ReleasesSentV4, after.ReleasesSentV4},
		{"releases_sent_v6", before.ReleasesSentV6, after.ReleasesSentV6},
	} {
		if got := c.now - c.before; got < 1 {
			t.Errorf("%s moved by %d across the stop, want at least 1", c.name, got)
		}
	}
	if got := after.ReleaseFailures - before.ReleaseFailures; got != 0 {
		t.Errorf("release_failures moved by %d on a teardown both families completed. "+
			"A v6 address that could not be taken off the link before the exchange "+
			"(RFC 9915 section 18.2.7) counts here and sends nothing", got)
	}
}
