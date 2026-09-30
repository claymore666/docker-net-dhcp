// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// #1116 item 1: the verdict of an existing bridge is logged at create, never refused.
func TestCreateNetwork_ExistingBridgeFirewallVerdict(t *testing.T) {
	const accept = 1
	unread := errors.New("no such file or directory")
	existing := map[string]interface{}{"bridge": bridgeTestBridge}
	rule := fmt.Sprintf("iptables -A FORWARD -i %s -j ACCEPT", bridgeTestBridge)
	for _, tc := range []struct {
		name      string
		nfCall    bool
		nfErr     error
		policy    uint32
		policyErr error
		want      string
	}{
		{name: "DROP with br_netfilter on", nfCall: true, policy: nfDrop, want: "the policy of the ip filter FORWARD chain is DROP"},
		{name: "an unreadable policy", nfCall: true, policyErr: unread, want: "cannot be read over nf_tables"},
		{name: "an unreadable sysctl", nfErr: unread, want: "bridge-nf-call-iptables cannot be read"},
		{name: "ACCEPT stays silent", nfCall: true, policy: accept},
		{name: "br_netfilter off stays silent", nfCall: false, policy: nfDrop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			stubFirewall(t, tc.nfCall, tc.nfErr, tc.policy, tc.policyErr)
			stubBridgeKernel(t, bridgeTestBridgeLink(""))
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			var err error
			logged := captureLog(t, func() { err = bridgeCreate(p, existing) })
			if err != nil {
				t.Fatalf("err = %v; the create never refuses on the verdict", err)
			}
			if _, lerr := loadOptions(vlanNetA); lerr != nil {
				t.Errorf("the network was not stored: %v", lerr)
			}
			if tc.want == "" {
				if strings.Contains(logged, "level=warning") {
					t.Errorf("log %q; want no warning", logged)
				}
				return
			}
			// The "Network created" line also carries bridge=, so the fields are read off the warning line alone.
			var warning string
			for _, line := range strings.Split(logged, "\n") {
				if strings.Contains(line, "level=warning") {
					warning = line
				}
			}
			for _, want := range []string{tc.want, "bridge=" + bridgeTestBridge, rule, "docs/bridge-mode.md"} {
				if !strings.Contains(warning, want) {
					t.Errorf("warning %q; want it to carry %q", warning, want)
				}
			}
			if n := strings.Count(logged, "level=warning"); n != 1 {
				t.Errorf("%d warning lines, want 1", n)
			}
		})
	}
	t.Run("an owned bridge is checked once, by the refusal", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		reads := stubFirewall(t, true, nil, nfDrop, nil)
		stubBridgeKernel(t, bridgeTestParentLink())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		var err error
		logged := captureLog(t, func() {
			err = bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent, "force_create": "true"})
		})
		if err != nil || *reads != 1 || strings.Count(logged, "level=warning") != 1 || strings.Contains(logged, "docs/bridge-mode.md") {
			t.Errorf("err %v, reads %d, log %q; want one check and only force_create's own line", err, *reads, logged)
		}
	})
	t.Run("an owned bridge without a verdict logs no warning", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, false, nil, nfDrop, nil)
		stubBridgeKernel(t, bridgeTestParentLink())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		logged := captureLog(t, func() {
			_ = bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent})
		})
		if strings.Contains(logged, "level=warning") {
			t.Errorf("log %q; want no warning", logged)
		}
	})
}

// #1116 item 2: the deadline error carries the verdict, any other error comes back untouched.
func TestWithFirewallVerdict(t *testing.T) {
	const accept = 1
	unread := errors.New("no such file or directory")
	deadline := fmt.Errorf("attempt: %w", context.DeadlineExceeded)
	boom := errors.New("server NAK")
	t.Run("a deadline under DROP names the firewall and stays a deadline", func(t *testing.T) {
		stubFirewall(t, true, nil, nfDrop, nil)
		got := withFirewallVerdict("br0", deadline)
		if !errors.Is(got, context.DeadlineExceeded) || !strings.HasPrefix(got.Error(), deadline.Error()) {
			t.Errorf("err = %v; want the original first and still a deadline", got)
		}
		if want := firewallAdvice("br0", firewallDropReason("br0")); !strings.HasSuffix(got.Error(), want) {
			t.Errorf("err = %v; want the create-time text %q", got, want)
		}
		for _, want := range []string{"is DROP", "br0", "iptables -A FORWARD -i br0 -j ACCEPT"} {
			if !strings.Contains(got.Error(), want) {
				t.Errorf("err = %v; want it to name %q", got, want)
			}
		}
	})
	t.Run("unreadable reads add their verdict and keep the timeout first", func(t *testing.T) {
		for name, stub := range map[string]func(){
			"policy": func() { stubFirewall(t, true, nil, 0, unread) },
			"sysctl": func() { stubFirewall(t, false, unread, 0, nil) },
		} {
			stub()
			got := withFirewallVerdict("br0", deadline)
			if !errors.Is(got, context.DeadlineExceeded) || !strings.HasPrefix(got.Error(), deadline.Error()) || got.Error() == deadline.Error() {
				t.Errorf("%s: err = %v; want the timeout first and a verdict after it", name, got)
			}
		}
	})
	t.Run("an empty verdict returns the error as it is", func(t *testing.T) {
		for name, stub := range map[string]func(){
			"ACCEPT":           func() { stubFirewall(t, true, nil, accept, nil) },
			"br_netfilter off": func() { stubFirewall(t, false, nil, nfDrop, nil) },
		} {
			stub()
			if got := withFirewallVerdict("br0", deadline); got != deadline {
				t.Errorf("%s: err = %v; want the same error value", name, got)
			}
		}
	})
	t.Run("another failure is never blamed on the firewall", func(t *testing.T) {
		reads := stubFirewall(t, true, nil, nfDrop, nil)
		for _, err := range []error{boom, context.Canceled, fmt.Errorf("wrapped: %w", boom), nil} {
			if got := withFirewallVerdict("br0", err); got != err {
				t.Errorf("err %v came back as %v; want it unchanged", err, got)
			}
		}
		if *reads != 0 {
			t.Errorf("%d policy reads; want the check kept for the deadline", *reads)
		}
	})
}
