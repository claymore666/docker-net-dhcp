// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "testing"

func TestSafeHostname_DropsAndCounts(t *testing.T) {
	p := &Plugin{}

	if got := p.safeHostname("web1"); got.name != "web1" || !got.trusted() {
		t.Errorf("an ordinary hostname was altered: %q, trusted=%v", got.name, got.trusted())
	}
	if n := p.unsafeHostnamesRejected.Load(); n != 0 {
		t.Errorf("counted an ordinary hostname: %d", n)
	}

	got := p.safeHostname("web1\nduid 00:03:00:01:be:ef:be:ef:be:ef")
	if got.name != "" {
		t.Errorf("an injecting hostname survived: %q", got.name)
	}
	if got.trusted() {
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

// Docker accepts an underscore in a hostname, which RFC 1123 does not allow.
func TestSafeHostname_KeepsWhatDockerAccepts(t *testing.T) {
	p := &Plugin{}
	for _, h := range []string{"my_app", "MY-APP.example.com", "a.b.c", "hôte", ""} {
		if got := p.safeHostname(h); got.name != h || !got.trusted() {
			t.Errorf("safeHostname(%q) = %q (trusted=%v), want it unchanged and accepted", h, got.name, got.trusted())
		}
	}
	if n := p.unsafeHostnamesRejected.Load(); n != 0 {
		t.Errorf("counted %d ordinary hostnames", n)
	}
}
