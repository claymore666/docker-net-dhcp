package plugin

import (
	"testing"

	"github.com/vishvananda/netlink"
)

// TestHintIfname covers the join-hint accessor that carries a custom
// interface name from CreateEndpoint to Join (#125): present, absent,
// and empty-Ifname hint all return the right string without panicking
// on a missing key.
func TestHintIfname(t *testing.T) {
	p := &Plugin{joinHints: make(map[string]joinHint)}
	p.storeJoinHint("ep-named", joinHint{Ifname: "lan0"})
	p.storeJoinHint("ep-unnamed", joinHint{Gateway: "192.168.0.1"})

	if got := p.hintIfname("ep-named"); got != "lan0" {
		t.Errorf("hintIfname(ep-named) = %q, want %q", got, "lan0")
	}
	if got := p.hintIfname("ep-unnamed"); got != "" {
		t.Errorf("hintIfname(ep-unnamed) = %q, want empty", got)
	}
	if got := p.hintIfname("ep-absent"); got != "" {
		t.Errorf("hintIfname(absent) = %q, want empty", got)
	}
}

// TestFingerprintIfname covers the restart-path fallback: when the
// join hint has already been consumed, Join recovers the custom
// interface name from the live-endpoint fingerprint (#125).
func TestFingerprintIfname(t *testing.T) {
	p := &Plugin{endpointFingerprints: make(map[string]endpointFingerprint)}
	p.endpointFingerprints["ep-named"] = endpointFingerprint{MAC: "02:00:00:00:00:01", Ifname: "wan0"}
	p.endpointFingerprints["ep-unnamed"] = endpointFingerprint{MAC: "02:00:00:00:00:02"}

	if got := p.fingerprintIfname("ep-named"); got != "wan0" {
		t.Errorf("fingerprintIfname(ep-named) = %q, want %q", got, "wan0")
	}
	if got := p.fingerprintIfname("ep-unnamed"); got != "" {
		t.Errorf("fingerprintIfname(ep-unnamed) = %q, want empty", got)
	}
	if got := p.fingerprintIfname("ep-absent"); got != "" {
		t.Errorf("fingerprintIfname(absent) = %q, want empty", got)
	}
}

// TestAuditIP pins the bare-address helper the ledger uses for v4/v6
// release entries (#109): nil addr and nil IP degrade to "" rather
// than panicking, a real address renders bare.
func TestAuditIP(t *testing.T) {
	if got := auditIP(nil); got != "" {
		t.Errorf("auditIP(nil) = %q, want empty", got)
	}
	if got := auditIP(&netlink.Addr{}); got != "" {
		t.Errorf("auditIP(zero) = %q, want empty", got)
	}
	addr, err := netlink.ParseAddr("192.168.0.42/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	if got := auditIP(addr); got != "192.168.0.42" {
		t.Errorf("auditIP(192.168.0.42/24) = %q, want %q", got, "192.168.0.42")
	}
}
