//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestDNSPropagate_OptInWritesResolvConf is the v0.9.0 / T1-1
// guard: when `propagate_dns=true` is set on the network, the
// container's /etc/resolv.conf must contain the DHCP-supplied DNS
// server (option 6, advertised by the fixture's dnsmasq as
// harness.TestDNSServer).
//
// Without this opt-in, Docker's embedded resolver handles DNS and
// the fixture's address never appears in resolv.conf. Together with
// the negative side of TestDNSPropagate_DefaultIsUnchanged below,
// this pins both the opt-in behaviour and the historical default.
func TestDNSPropagate_OptInWritesResolvConf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-dns"
	ctrName := "dh-itest-dns-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"propagate_dns": "true",
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	// resolv.conf is written from the persistent client's `bound`
	// event, which fires after libnetwork's Join — i.e. after
	// RunContainer's "got an IP" return. Poll briefly: the write
	// is fast but not synchronous with the inspect IP.
	deadline := time.Now().Add(5 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if strings.Contains(out, harness.TestDNSServer) {
			t.Logf("resolv.conf inside container:\n%s", out)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("DHCP DNS server %s never appeared in container's resolv.conf within 5s\nlast contents:\n%s",
		harness.TestDNSServer, out)
}

// TestDNSPropagate_DefaultIsUnchanged confirms the v0.7.0 baseline
// behaviour: without the propagate_dns opt-in, the container's
// resolv.conf is whatever Docker's resolver wrote (typically a
// 127.0.0.11 stub, or the host's nameservers — never our fixture's
// 192.168.99.53). Guards against an accidental flip of the default
// during refactors.
func TestDNSPropagate_DefaultIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-dns-default"
	ctrName := "dh-itest-dns-default-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
	if strings.Contains(out, harness.TestDNSServer) {
		t.Errorf("propagate_dns is off but DHCP DNS server %s still ended up in resolv.conf — default flipped?\ncontents:\n%s",
			harness.TestDNSServer, out)
	}
}
