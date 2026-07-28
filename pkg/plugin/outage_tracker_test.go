package plugin

import (
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/pkg/dhcp"
)

// outageTracker is the fix for #353: dhcp_timeouts stayed at zero
// through a complete DHCP outage, because the watchdog only counted
// while the client was in the "acquiring" state and nothing ever put it
// there. dhcpcd under --noconfigure reports a lapsed lease as RELEASE,
// which is indistinguishable from a graceful stop and so is dropped.
//
// These tests drive the state machine on a synthetic clock, so the
// 2-minute and 24-hour lease cases cost the same nothing to check.

const testGrace = 25 * time.Second

func lease(seconds int) dhcp.Info {
	return dhcp.Info{IP: "192.168.0.10/24", LeaseSeconds: seconds}
}

func TestOutageTracker_SilentLapseIsCounted(t *testing.T) {
	// The regression. A client binds, the server dies, and dhcpcd says
	// NOTHING for the rest of the lease — no EXPIRE, no leasefail. The
	// tracker must still call the outage, from the lease alone.
	t0 := time.Unix(1_700_000_000, 0)
	o := newOutageTracker(t0)
	o.observe("bound", lease(120), t0)

	// Mid-lease: silent, and rightly so — the client holds a valid lease.
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second, 119 * time.Second} {
		if count, _ := o.due(t0.Add(at), testGrace); count {
			t.Fatalf("counted a timeout at t+%v, inside a valid 120s lease", at)
		}
	}

	// Past lease+grace with nothing heard: this is the outage.
	count, silent := o.due(t0.Add(150*time.Second), testGrace)
	if !count {
		t.Fatal("no timeout counted after the lease lapsed unheard — this is exactly #353")
	}
	if !silent {
		t.Error("lapse not reported as silent; the log line would claim dhcpcd told us, and it did not")
	}

	// And it must keep climbing while the outage lasts (the counter is
	// documented as a rate, not a one-shot).
	if count, _ := o.due(t0.Add(180*time.Second), testGrace); !count {
		t.Error("outage stopped being counted on the following tick; dhcp_timeouts must keep climbing")
	}
}

func TestOutageTracker_HealthyRenewalNeverCounts(t *testing.T) {
	// The false-positive guard, and the reason the deadline is the LEASE
	// and not T1: under --noconfigure the T1 unicast renewal always
	// fails and the lease is renewed at T2 by broadcast rebind (which
	// arrives as "renew"). A T1-derived deadline would fire here.
	t0 := time.Unix(1_700_000_000, 0)
	o := newOutageTracker(t0)
	o.observe("bound", lease(120), t0)

	now := t0
	for cycle := 0; cycle < 20; cycle++ {
		// Tick across the whole cycle at the real watchdog cadence.
		for at := 1 * time.Second; at < 105*time.Second; at += dhcpOutageTick {
			if count, _ := o.due(now.Add(at), testGrace); count {
				t.Fatalf("cycle %d: counted a timeout at t+%v on a client that renews at T2", cycle, at)
			}
		}
		// T2 rebind lands, exactly as dhcpcd delivers it.
		now = now.Add(105 * time.Second)
		o.observe("renew", lease(120), now)
	}
}

func TestOutageTracker_LongLeaseStaysSilent(t *testing.T) {
	// A 24h lease is the production case. Nothing may be counted for
	// almost a day — the old comment's worry about "false-counting the
	// quiet gap between renewals" is handled by knowing the lease.
	t0 := time.Unix(1_700_000_000, 0)
	o := newOutageTracker(t0)
	o.observe("bound", lease(86400), t0)

	for _, at := range []time.Duration{time.Hour, 12 * time.Hour, 23 * time.Hour} {
		if count, _ := o.due(t0.Add(at), testGrace); count {
			t.Errorf("counted a timeout at t+%v of a 24h lease", at)
		}
	}
	if count, _ := o.due(t0.Add(24*time.Hour+time.Minute), testGrace); !count {
		t.Error("a 24h lease that lapsed unheard was never counted")
	}
}

func TestOutageTracker_NoLeaseTimeFallsBackToEventsOnly(t *testing.T) {
	// A server that supplies no lease lifetime must not produce a
	// deadline we invented. The acquiring trigger still applies.
	t0 := time.Unix(1_700_000_000, 0)
	o := newOutageTracker(t0)
	o.observe("bound", dhcp.Info{IP: "192.168.0.10/24"}, t0)

	if count, _ := o.due(t0.Add(72*time.Hour), testGrace); count {
		t.Error("counted a timeout with no lease lifetime known; the deadline was guessed")
	}

	// An explicit lease loss still works without any lifetime.
	o.observe("leasefail", dhcp.Info{}, t0.Add(72*time.Hour))
	if count, silent := o.due(t0.Add(72*time.Hour+time.Minute), testGrace); !count || silent {
		t.Errorf("leasefail path broken: count=%v silent=%v, want count=true silent=false", count, silent)
	}
}

func TestOutageTracker_AcquiringFromStart(t *testing.T) {
	// A client that never binds at all: the original trigger, preserved.
	t0 := time.Unix(1_700_000_000, 0)
	o := newOutageTracker(t0)

	if count, _ := o.due(t0.Add(10*time.Second), testGrace); count {
		t.Error("counted a timeout inside the grace period")
	}
	if count, silent := o.due(t0.Add(30*time.Second), testGrace); !count || silent {
		t.Errorf("never-bound client not counted past the grace: count=%v silent=%v", count, silent)
	}
}

func TestOutageTracker_RecoveryStopsCounting(t *testing.T) {
	// When the server comes back, the counter must go quiet again —
	// otherwise an operator alerting on the rate never sees it clear.
	t0 := time.Unix(1_700_000_000, 0)
	o := newOutageTracker(t0)
	o.observe("bound", lease(120), t0)

	if count, _ := o.due(t0.Add(150*time.Second), testGrace); !count {
		t.Fatal("setup: expected the lapse to be counted")
	}
	o.observe("bound", lease(120), t0.Add(160*time.Second))
	if count, _ := o.due(t0.Add(200*time.Second), testGrace); count {
		t.Error("still counting timeouts after the client re-bound")
	}
}

func TestOutageTracker_NAKIsNotService(t *testing.T) {
	// A NAK leaves the acquiring state alone (nextAcquiring), but it must
	// not restart the lease deadline either — a refusal is not service.
	t0 := time.Unix(1_700_000_000, 0)
	o := newOutageTracker(t0)
	o.observe("bound", lease(120), t0)
	o.observe("nak", dhcp.Info{}, t0.Add(100*time.Second))

	if count, _ := o.due(t0.Add(150*time.Second), testGrace); !count {
		t.Error("a NAK reset the lease deadline; the outage went uncounted")
	}
}
