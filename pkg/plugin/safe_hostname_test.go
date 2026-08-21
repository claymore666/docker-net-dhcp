// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "testing"

// The drop itself is guaranteed structurally by dhcp.directive. What this
// pins is that the event is COUNTED, because a silent safe-failure is the
// shape this repo has been bitten by: nothing is broken afterwards, so
// nothing draws attention to the fact that somebody tried. #692.
func TestSafeHostname_DropsAndCounts(t *testing.T) {
	p := &Plugin{}

	if got, ok := p.safeHostname("web1"); got != "web1" || !ok {
		t.Errorf("an ordinary hostname was altered: %q, ok=%v", got, ok)
	}
	if n := p.unsafeHostnamesRejected.Load(); n != 0 {
		t.Errorf("counted an ordinary hostname: %d", n)
	}

	got, ok := p.safeHostname("web1\nduid 00:03:00:01:be:ef:be:ef:be:ef")
	if got != "" {
		t.Errorf("an injecting hostname survived: %q", got)
	}
	// The bool is the half that matters to the caller making an identity
	// decision; "" alone cannot tell it apart from "no hostname found".
	if ok {
		t.Error("a refused hostname reported itself as safe")
	}
	if n := p.unsafeHostnamesRejected.Load(); n != 1 {
		t.Errorf("unsafe_hostnames_rejected = %d, want 1", n)
	}

	p.safeHostname("a\rb")
	if n := p.unsafeHostnamesRejected.Load(); n != 2 {
		t.Errorf("unsafe_hostnames_rejected = %d, want 2", n)
	}
}

// An underscore is not a legal RFC 1123 hostname and Docker accepts it
// anyway, so a well-formedness check here would have broken working
// deployments to fix a structural problem. Keep the rule about structure.
func TestSafeHostname_KeepsWhatDockerAccepts(t *testing.T) {
	p := &Plugin{}
	for _, h := range []string{"my_app", "MY-APP.example.com", "a.b.c", "hôte", ""} {
		if got, ok := p.safeHostname(h); got != h || !ok {
			t.Errorf("safeHostname(%q) = %q (ok=%v), want it unchanged and accepted", h, got, ok)
		}
	}
	if n := p.unsafeHostnamesRejected.Load(); n != 0 {
		t.Errorf("counted %d ordinary hostnames", n)
	}
}
