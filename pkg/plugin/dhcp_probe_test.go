package plugin

import (
	"strings"
	"testing"
)

// TestNewProbeMAC pins the LAA + unicast bit semantics. Stable
// even though the rest of the bytes are random — the constraint is
// what avoids collision with any manufacturer-assigned MAC on the
// upstream's reservation table.
func TestNewProbeMAC(t *testing.T) {
	mac, err := newProbeMAC()
	if err != nil {
		t.Fatalf("newProbeMAC: %v", err)
	}
	if len(mac) != 6 {
		t.Fatalf("MAC length = %d, want 6", len(mac))
	}
	if mac[0]&0x02 != 0x02 {
		t.Errorf("LAA bit not set on first byte (%#x); upstream may treat this as a manufacturer MAC", mac[0])
	}
	if mac[0]&0x01 != 0x00 {
		t.Errorf("multicast bit set on first byte (%#x); not a valid unicast address", mac[0])
	}

	// Two consecutive calls must produce different MACs (else the
	// rand source is broken and probes on different runs would
	// collide in the dnsmasq lease table).
	m2, err := newProbeMAC()
	if err != nil {
		t.Fatalf("newProbeMAC #2: %v", err)
	}
	if mac.String() == m2.String() {
		t.Errorf("two consecutive newProbeMAC calls returned identical MAC %s — randomness broken", mac)
	}
}

// TestNewProbeLinkName guards uniqueness + the dh-probe- prefix that
// makes orphans easy to spot in `ip link` output if a probe ever
// fails to clean up.
func TestNewProbeLinkName(t *testing.T) {
	a, err := newProbeLinkName()
	if err != nil {
		t.Fatalf("newProbeLinkName: %v", err)
	}
	if !strings.HasPrefix(a, "dh-probe-") {
		t.Errorf("missing prefix; got %q", a)
	}
	// Linux's IFNAMSIZ is 16 (including null terminator) → max 15
	// printable chars. dh-probe- (9) + 8 hex = 17. We're 2 over the
	// limit and need to rely on the kernel truncating, OR we use a
	// shorter random suffix. Pin the length here so a refactor
	// doesn't regress past the limit silently.
	if len(a) > 15 {
		t.Errorf("link name %q exceeds Linux IFNAMSIZ-1 (15) — kernel will refuse it", a)
	}

	b, _ := newProbeLinkName()
	if a == b {
		t.Errorf("two consecutive newProbeLinkName calls returned identical name %q — randomness broken", a)
	}
}
