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

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestHostname_ReachesTheServersTableAfterTheClientStarts is #961's
// outside evidence.
//
// The plugin now starts the persistent client before it asks the daemon
// for the container's name, and gives the name to the running client
// afterwards. Every unit drive for that stops at the handover: the
// library's SetHostname returns before anything is on the wire and says
// so, so "the name was applied" is intent. THE SERVER'S OWN TABLE is the
// effect, and it is what this reads: dnsmasq writes the option-12 name
// into column four of its lease database, once per ACK.
//
// hostnames_applied_late is asserted BESIDE the lease file rather than
// instead of it. It is the only thing that says the name got there by
// the late path: a name carried in the client's opening parameters --
// the old order, and the order a register_dns network still takes --
// lands in the same column and looks identical here.
//
// v4 ONLY, and that is the library's boundary rather than this cell's.
// dhcp-golib v1.0.0 sends no name option for DHCPv6 at all and refuses
// SetHostname on a v6 client, so there is no v6 half of this property to
// measure.
func TestHostname_ReachesTheServersTableAfterTheClientStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-hostname-live"
	ctrName := "dh-itest-hostname-live-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli,
		"hostnames_applied_late", "hostname_lookup_failures", "hostname_apply_failures")

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s: id=%s ip=%s mac=%s", ctrName, id[:12], ip, mac)

	// The name lands one exchange after the bind: the client holds a
	// lease by the time the daemon answers, so it renews early to carry
	// the name (RFC 2131 section 4.4.5) and dnsmasq rewrites the lease
	// line. That is the whole delay being waited out here.
	if got, line := waitLeaseHostname(t, fixture.LeaseFile(), ip, ctrName, 30*time.Second); got != ctrName {
		t.Errorf("the DHCP server's table has %q as the name for %s, want %q. The lease line was:\n%s\n"+
			"An address with no name in it is what an endpoint gets when the name never reached the "+
			"running client (#961)", got, ip, ctrName, line)
	}

	after, ok := w.Await(15*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.HostnamesAppliedLate > before.HostnamesAppliedLate
	})
	before, _ := w.End()
	if !ok {
		t.Errorf("hostnames_applied_late did not advance (before=%d, last seen=%d). The name is in the "+
			"server's table, so it got there in the client's opening parameters: the attach waited for "+
			"the daemon before it started the client, which is the order #961 removed",
			before.HostnamesAppliedLate, after.HostnamesAppliedLate)
	}
	if got := after.HostnameLookupFailures - before.HostnameLookupFailures; got != 0 {
		t.Errorf("hostname_lookup_failures advanced by %d on an attach whose name did arrive", got)
	}
	if got := after.HostnameApplyFailures - before.HostnameApplyFailures; got != 0 {
		t.Errorf("hostname_apply_failures advanced by %d on an attach whose name did arrive", got)
	}
}

// waitLeaseHostname returns the name dnsmasq has recorded for addr, and
// the lease line it came from.
//
// dnsmasq's lease line is `<expiry> <mac> <ip> <hostname> <client-id>`,
// with `*` in the name column for a client that sent none. It polls for
// want rather than reading once: the first line for this address is
// written at the ACK that bound it, which is BEFORE the name arrives,
// so a single read here would measure the plugin's speed instead of its
// behaviour. The last line seen is returned either way, so a failure
// says what the server actually had.
func waitLeaseHostname(t *testing.T, leaseFile, addr, want string, budget time.Duration) (string, string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	got, line := "", ""
	for {
		data, err := os.ReadFile(leaseFile)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read lease file %s: %v", leaseFile, err)
		}
		for _, l := range strings.Split(string(data), "\n") {
			f := strings.Fields(l)
			if len(f) >= 4 && f[2] == addr {
				got, line = f[3], l
			}
		}
		if got == want || !time.Now().Before(deadline) {
			return got, line
		}
		time.Sleep(250 * time.Millisecond)
	}
}
