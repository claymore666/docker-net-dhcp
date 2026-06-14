package plugin

import (
	"net"
	"testing"
	"time"
)

// laBitSet reports whether the locally-administered bit is set and the
// multicast bit is clear — the two invariants every synthesised MAC must
// satisfy to be a valid, OUI-free unicast address.
func validUnicastLAA(mac net.HardwareAddr) bool {
	return len(mac) == 6 && mac[0]&0x02 != 0 && mac[0]&0x01 == 0
}

func TestDeriveStableMAC_Deterministic(t *testing.T) {
	const seed = "stable-mac/v1\x00net-abc\x00name\x00/proj-web-1"
	a := deriveStableMAC(seed)
	b := deriveStableMAC(seed)
	if a.String() != b.String() {
		t.Fatalf("same seed produced different MACs: %s vs %s", a, b)
	}
}

func TestDeriveStableMAC_BitValidity(t *testing.T) {
	// Sweep a range of seeds; every result must be a valid LAA unicast
	// MAC regardless of what the hash's high byte happened to be.
	for i := 0; i < 1000; i++ {
		seed := "seed-" + string(rune('a'+i%26)) + itoaTiny(i)
		mac := deriveStableMAC(seed)
		if !validUnicastLAA(mac) {
			t.Fatalf("seed %q produced invalid MAC %s (high byte %#02x)", seed, mac, mac[0])
		}
	}
}

func TestDeriveStableMAC_DifferentSeedsDiffer(t *testing.T) {
	a := deriveStableMAC("alpha")
	b := deriveStableMAC("beta")
	if a.String() == b.String() {
		t.Fatalf("distinct seeds collided: both %s", a)
	}
}

func TestDeriveStableMACAvoiding_NoCollision(t *testing.T) {
	mac, attempts := deriveStableMACAvoiding("seed-x", nil)
	if attempts != 0 {
		t.Fatalf("expected 0 perturbations with nil taken, got %d", attempts)
	}
	if mac.String() != deriveStableMAC("seed-x").String() {
		t.Fatalf("nil taken should yield the base MAC, got %s", mac)
	}
}

func TestDeriveStableMACAvoiding_PerturbsOnCollision(t *testing.T) {
	base := deriveStableMAC("seed-x").String()
	// Mark the base MAC as taken; the helper must perturb to a different
	// one and report a non-zero attempt count.
	mac, attempts := deriveStableMACAvoiding("seed-x", func(m net.HardwareAddr) bool {
		return m.String() == base
	})
	if attempts == 0 {
		t.Fatal("expected perturbation when base MAC is taken, got 0")
	}
	if mac.String() == base {
		t.Fatalf("perturbation returned the taken MAC %s", mac)
	}
	if !validUnicastLAA(mac) {
		t.Fatalf("perturbed MAC %s is not a valid LAA unicast", mac)
	}
}

func TestDeriveStableMACAvoiding_PerturbationIsDeterministic(t *testing.T) {
	base := deriveStableMAC("seed-x").String()
	taken := func(m net.HardwareAddr) bool { return m.String() == base }
	first, _ := deriveStableMACAvoiding("seed-x", taken)
	second, _ := deriveStableMACAvoiding("seed-x", taken)
	if first.String() != second.String() {
		t.Fatalf("perturbation not deterministic: %s vs %s", first, second)
	}
}

func TestDeriveStableMACAvoiding_CapExhaustion(t *testing.T) {
	// A predicate that marks everything taken must terminate at the cap
	// rather than spin forever, returning a valid MAC and the cap count.
	mac, attempts := deriveStableMACAvoiding("seed-x", func(net.HardwareAddr) bool { return true })
	if attempts != stableMACMaxPerturb {
		t.Fatalf("expected cap %d attempts, got %d", stableMACMaxPerturb, attempts)
	}
	if !validUnicastLAA(mac) {
		t.Fatalf("cap-exhausted MAC %s is not valid", mac)
	}
}

func TestStableMACSeed_Precedence(t *testing.T) {
	full := containerIdentity{
		Name:           "/proj-web-1",
		ComposeProject: "proj",
		ComposeService: "web",
		ComposeNumber:  "1",
	}

	t.Run("mac_seed wins", func(t *testing.T) {
		s := stableMACSeed(full, "net1", DHCPNetworkOptions{StableMAC: true, MACSeed: "key"})
		if s == "" {
			t.Fatal("expected a seed")
		}
		// Changing identity must not change the seed when mac_seed is set.
		other := stableMACSeed(containerIdentity{Name: "/other"}, "net1",
			DHCPNetworkOptions{StableMAC: true, MACSeed: "key"})
		if s != other {
			t.Fatalf("mac_seed should ignore identity: %q vs %q", s, other)
		}
	})

	t.Run("compose before name", func(t *testing.T) {
		s := stableMACSeed(full, "net1", DHCPNetworkOptions{StableMAC: true})
		// Same compose identity but a different container name → same seed.
		renamed := full
		renamed.Name = "/renamed"
		if s != stableMACSeed(renamed, "net1", DHCPNetworkOptions{StableMAC: true}) {
			t.Fatal("compose-tier seed should not depend on container name")
		}
	})

	t.Run("compose without number falls back to name", func(t *testing.T) {
		noNum := containerIdentity{Name: "/proj-web-1", ComposeProject: "proj", ComposeService: "web"}
		s := stableMACSeed(noNum, "net1", DHCPNetworkOptions{StableMAC: true})
		want := stableMACSeed(containerIdentity{Name: "/proj-web-1"}, "net1", DHCPNetworkOptions{StableMAC: true})
		if s != want {
			t.Fatalf("missing container-number should fall through to name seed: %q vs %q", s, want)
		}
	})

	t.Run("anonymous yields empty", func(t *testing.T) {
		if s := stableMACSeed(containerIdentity{}, "net1", DHCPNetworkOptions{StableMAC: true}); s != "" {
			t.Fatalf("anonymous identity should yield empty seed, got %q", s)
		}
	})
}

func TestStableMACSeed_NetworkScoping(t *testing.T) {
	id := containerIdentity{Name: "/app"}
	a := stableMACSeed(id, "net-A", DHCPNetworkOptions{StableMAC: true})
	b := stableMACSeed(id, "net-B", DHCPNetworkOptions{StableMAC: true})
	if a == b {
		t.Fatal("same container on two networks should get distinct seeds")
	}
	if deriveStableMAC(a).String() == deriveStableMAC(b).String() {
		t.Fatal("same container on two networks should get distinct MACs")
	}
}

func TestStableMACSeed_ReplicasDiffer(t *testing.T) {
	// The core replica-safety property: scaled replicas share project +
	// service but differ in container-number, so they must not collide.
	r1 := containerIdentity{ComposeProject: "p", ComposeService: "web", ComposeNumber: "1"}
	r2 := containerIdentity{ComposeProject: "p", ComposeService: "web", ComposeNumber: "2"}
	s1 := stableMACSeed(r1, "net", DHCPNetworkOptions{StableMAC: true})
	s2 := stableMACSeed(r2, "net", DHCPNetworkOptions{StableMAC: true})
	if s1 == s2 {
		t.Fatal("replicas with different container-number produced the same seed")
	}
	if deriveStableMAC(s1).String() == deriveStableMAC(s2).String() {
		t.Fatal("replicas produced the same MAC")
	}
}

func TestStableClientIDMAC(t *testing.T) {
	const mac = "02:11:22:33:44:55"

	t.Run("off by default", func(t *testing.T) {
		if got := stableClientIDMAC(DHCPNetworkOptions{}, ModeBridge, mac); got != "" {
			t.Fatalf("stable_mac off should yield empty, got %q", got)
		}
	})
	t.Run("bridge on returns link MAC", func(t *testing.T) {
		if got := stableClientIDMAC(DHCPNetworkOptions{StableMAC: true}, ModeBridge, mac); got != mac {
			t.Fatalf("got %q, want %q", got, mac)
		}
	})
	t.Run("macvlan on returns link MAC", func(t *testing.T) {
		if got := stableClientIDMAC(DHCPNetworkOptions{StableMAC: true}, ModeMacvlan, mac); got != mac {
			t.Fatalf("got %q, want %q", got, mac)
		}
	})
	t.Run("ipvlan is excluded", func(t *testing.T) {
		if got := stableClientIDMAC(DHCPNetworkOptions{StableMAC: true}, ModeIPvlan, mac); got != "" {
			t.Fatalf("ipvlan should yield empty (no-op), got %q", got)
		}
	})
}

func TestResolveStableMAC_DisabledAndAnonymous(t *testing.T) {
	p := newTestPlugin(t)
	id := containerIdentity{Name: "/app"}

	t.Run("off returns empty", func(t *testing.T) {
		if got := p.resolveStableMAC(id, "net1", DHCPNetworkOptions{}); got != "" {
			t.Fatalf("stable_mac off should yield empty, got %q", got)
		}
	})
	t.Run("anonymous returns empty", func(t *testing.T) {
		got := p.resolveStableMAC(containerIdentity{}, "net1", DHCPNetworkOptions{StableMAC: true})
		if got != "" {
			t.Fatalf("anonymous identity should yield empty, got %q", got)
		}
	})
}

func TestResolveStableMAC_DeterministicAndMatchesCore(t *testing.T) {
	p := newTestPlugin(t)
	id := containerIdentity{Name: "/app"}
	opts := DHCPNetworkOptions{StableMAC: true}

	got := p.resolveStableMAC(id, "net1", opts)
	if got == "" {
		t.Fatal("expected a MAC for a named container")
	}
	// Stable across calls and equal to the pure-core derivation (no
	// collision ⇒ no perturbation).
	if again := p.resolveStableMAC(id, "net1", opts); again != got {
		t.Fatalf("not deterministic: %q vs %q", got, again)
	}
	want := deriveStableMAC(stableMACSeed(id, "net1", opts)).String()
	if got != want {
		t.Fatalf("resolve %q != core derivation %q", got, want)
	}
	if p.stableMACCollisions.Load() != 0 {
		t.Fatalf("no collision expected, counter=%d", p.stableMACCollisions.Load())
	}
}

func TestResolveStableMAC_CollisionPerturbsAndCounts(t *testing.T) {
	p := newTestPlugin(t)
	id := containerIdentity{Name: "/app"}
	opts := DHCPNetworkOptions{StableMAC: true}

	// Pre-seed a live manager on the same network already holding the
	// base MAC, forcing resolveStableMAC down the perturbation path.
	base := deriveStableMAC(stableMACSeed(id, "net1", opts))
	m := stoppableManager("net1")
	m.MacAddress = base
	p.persistentDHCP["ep-existing"] = m

	got := p.resolveStableMAC(id, "net1", opts)
	if got == base.String() {
		t.Fatalf("expected a perturbed MAC, got the taken base %q", got)
	}
	if got == "" {
		t.Fatal("expected a valid perturbed MAC, got empty")
	}
	if p.stableMACCollisions.Load() == 0 {
		t.Fatal("collision counter was not bumped on perturbation")
	}
}

func TestStableMACTakenSet(t *testing.T) {
	p := newTestPlugin(t)

	macLive, _ := net.ParseMAC("02:00:00:00:00:01")
	macOther, _ := net.ParseMAC("02:00:00:00:00:02")
	macTomb, _ := net.ParseMAC("02:00:00:00:00:03")

	live := stoppableManager("net1")
	live.MacAddress = macLive
	p.persistentDHCP["ep-live"] = live

	// A manager on a different network must not leak into the set.
	other := stoppableManager("net2")
	other.MacAddress = macOther
	p.persistentDHCP["ep-other"] = other

	if err := saveTombstones([]tombstone{
		{NetworkID: "net1", MacAddress: macTomb.String(), DeletedAt: time.Now()},
		{NetworkID: "net2", MacAddress: "02:00:00:00:00:09", DeletedAt: time.Now()},
	}); err != nil {
		t.Fatalf("saveTombstones: %v", err)
	}

	set := p.stableMACTakenSet("net1")
	if !set[macLive.String()] {
		t.Errorf("live MAC %s missing from taken set", macLive)
	}
	if !set[macTomb.String()] {
		t.Errorf("fresh tombstone MAC %s missing from taken set", macTomb)
	}
	if set[macOther.String()] {
		t.Errorf("other-network MAC %s should not be in the set", macOther)
	}
	if set["02:00:00:00:00:09"] {
		t.Error("other-network tombstone MAC should not be in the set")
	}
}

// itoaTiny is a tiny base-10 itoa so the sweep test stays dependency-free
// and doesn't pull strconv into the test's intent.
func itoaTiny(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
