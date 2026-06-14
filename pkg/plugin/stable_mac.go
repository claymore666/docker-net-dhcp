package plugin

import (
	"crypto/sha256"
	"net"
	"strconv"

	log "github.com/sirupsen/logrus"
)

// Compose label keys. Docker Compose stamps every container it creates
// with these, and they survive a `compose up -d` recreate (same project
// + service ⇒ same values), which is exactly the stable identity the
// deterministic MAC needs. Defined here rather than imported because the
// plugin otherwise has no Compose dependency.
const (
	composeProjectLabel = "com.docker.compose.project"
	composeServiceLabel = "com.docker.compose.service"
	// composeNumberLabel is the per-replica index Compose stamps on each
	// container of a service (1, 2, 3 for `--scale svc=3`). It is the
	// piece that keeps replicas of one service from hashing to the same
	// MAC: project+service alone are identical across replicas, so the
	// number is what makes the seed replica-unique while staying stable
	// across recreate (replica N keeps its number).
	composeNumberLabel = "com.docker.compose.container-number"
)

// containerIdentity is the best-effort stable identity of the container
// an endpoint is being created for, resolved once at create time (see
// initialContainerIdentity). Every field is best-effort: a non-Compose
// `docker run` container has empty Compose fields, and an anonymous
// container (`--rm` with no name) can even have an empty Name. The seed
// logic degrades through these gracefully.
type containerIdentity struct {
	// Hostname is the container's configured hostname, consumed by the
	// option-12 DHCP hint (initialDHCPHostname). Not used as a MAC seed:
	// it defaults to the short container ID, which is NOT stable across
	// recreate.
	Hostname string
	// Name is the Docker container name (e.g. "/myproj-web-1"). Stable
	// across recreate for named and Compose-managed containers.
	Name string
	// ComposeProject / ComposeService / ComposeNumber come from the
	// Compose labels above. Project+Service+Number together form a
	// replica-unique identity that's stable across `compose up -d` even
	// if the container name changes. Number may be empty on older
	// Compose releases that don't stamp it; the seed logic falls back to
	// the container name (which also carries the replica index) in that
	// case.
	ComposeProject string
	ComposeService string
	ComposeNumber  string
}

// stableMACSeedTag versions the seed-construction scheme. Bumping it
// deliberately remaps every container's deterministic MAC; keep it fixed
// so leases stay stable across plugin upgrades. It also namespaces the
// hash input so a future second use of deriveStableMAC can't accidentally
// collide with MAC seeds.
const stableMACSeedTag = "stable-mac/v1"

// stableMACSeed picks the stable seed for a container's deterministic MAC,
// or returns "" when no stable identity is available (in which case the
// caller must fall back to a kernel-random MAC — a stable MAC derived from
// an unstable identity would be a lie). Precedence, most-specific first:
//
//  1. explicit `-o mac_seed=<key>` driver-opt — full operator control
//  2. Compose project + service + container-number — survives
//     `compose up -d`, and the number keeps service replicas distinct
//  3. container name — explicit `--name`, or Compose without the
//     container-number label (the name carries the replica index)
//  4. none — anonymous container, no stable identity
//
// networkID is folded into every seed so the same container attached to
// two DHCP networks gets a distinct MAC on each. Each tier carries a
// discriminator string so a name that happens to equal a mac_seed value
// (or a project) can't hash to the same MAC.
func stableMACSeed(id containerIdentity, networkID string, opts DHCPNetworkOptions) string {
	prefix := stableMACSeedTag + "\x00" + networkID + "\x00"
	switch {
	case opts.MACSeed != "":
		return prefix + "seed\x00" + opts.MACSeed
	case id.ComposeProject != "" && id.ComposeService != "" && id.ComposeNumber != "":
		return prefix + "compose\x00" + id.ComposeProject + "\x00" + id.ComposeService + "\x00" + id.ComposeNumber
	case id.Name != "":
		return prefix + "name\x00" + id.Name
	default:
		return ""
	}
}

// deriveStableMAC turns a seed into a deterministic, valid unicast MAC.
// It takes the first 6 bytes of SHA-256(seed), then forces the high byte
// to a locally-administered unicast address: set the LAA bit (0x02) so we
// never collide with a manufacturer OUI, and clear the multicast bit
// (0x01) so it's a valid chaddr. This is the same bit fixup newProbeMAC
// applies to its random bytes, so synthesised MACs are recognisably
// "ephemeral / locally-administered" in upstream DHCP logs either way.
func deriveStableMAC(seed string) net.HardwareAddr {
	sum := sha256.Sum256([]byte(seed))
	mac := make(net.HardwareAddr, 6)
	copy(mac, sum[:6])
	mac[0] = (mac[0] | 0x02) & 0xfe // set LAA bit, clear multicast bit
	return mac
}

// stableMACMaxPerturb caps collision retries. The MAC space here is ~46
// effective bits (48 minus the 2 forced high-byte bits), so a collision
// among the handful of endpoints on one network is astronomically
// unlikely; this cap only exists so a pathological `taken` predicate
// can't spin forever. Hitting it is a should-never-happen we surface
// rather than loop on.
const stableMACMaxPerturb = 256

// deriveStableMACAvoiding derives a deterministic MAC for seed, perturbing
// the seed until the result is not reported as in-use by taken. It returns
// the chosen MAC and the number of perturbations needed (0 = the base seed
// was free). taken may be nil, meaning "no collision check". If the cap is
// exhausted it returns the last candidate with attempts == stableMACMaxPerturb
// so the caller can log the anomaly; correctness still holds (the MAC is
// valid), only uniqueness is no longer guaranteed.
func deriveStableMACAvoiding(seed string, taken func(net.HardwareAddr) bool) (net.HardwareAddr, int) {
	for n := 0; n < stableMACMaxPerturb; n++ {
		candidate := seed
		if n > 0 {
			candidate = seed + "\x00" + strconv.Itoa(n)
		}
		mac := deriveStableMAC(candidate)
		if taken == nil || !taken(mac) {
			return mac, n
		}
	}
	return deriveStableMAC(seed + "\x00" + strconv.Itoa(stableMACMaxPerturb)), stableMACMaxPerturb
}

// resolveStableMAC returns the deterministic MAC string an endpoint should
// use, or "" to signal "no stable MAC — fall back to the kernel-random /
// server-default path". It returns "" when stable_mac is off, or when the
// container has no stable identity to hash (anonymous container), because
// a "stable" MAC derived from an unstable identity would drift on the next
// recreate anyway — worse than honestly using a random one. networkID
// scopes both the seed and the collision check. Perturbations bump the
// stableMACCollisions health counter.
func (p *Plugin) resolveStableMAC(id containerIdentity, networkID string, opts DHCPNetworkOptions) string {
	if !opts.StableMAC {
		return ""
	}
	seed := stableMACSeed(id, networkID, opts)
	if seed == "" {
		log.WithFields(log.Fields{
			"network": shortID(networkID),
		}).Warn("stable_mac is enabled but the container has no stable identity (anonymous container); using a kernel-random MAC")
		return ""
	}

	taken := p.stableMACTakenSet(networkID)
	mac, attempts := deriveStableMACAvoiding(seed, func(m net.HardwareAddr) bool {
		return taken[m.String()]
	})
	if attempts > 0 {
		p.stableMACCollisions.Add(int32(attempts))
		log.WithFields(log.Fields{
			"network":     shortID(networkID),
			"mac_address": mac.String(),
			"attempts":    attempts,
		}).Warn("deterministic stable MAC collided with an existing endpoint; used a perturbed MAC")
	}
	return mac.String()
}

// stableMACTakenSet collects the MAC strings already in use on a network,
// for the deterministic-MAC collision backstop: the live endpoints'
// macvlan MAC hints plus any still-fresh tombstones. It is best-effort by
// design — distinct container identities already hash to distinct seeds,
// so this set only guards the degenerate "two identities collide in the
// ~46-bit space" case; a missing entry at worst causes a perturbation
// that wasn't strictly required, never a duplicate MAC for a live pair.
func (p *Plugin) stableMACTakenSet(networkID string) map[string]bool {
	taken := map[string]bool{}

	p.mu.Lock()
	for _, m := range p.persistentDHCP {
		if m.joinReq.NetworkID != networkID {
			continue
		}
		if len(m.MacAddress) > 0 {
			taken[m.MacAddress.String()] = true
		}
	}
	p.mu.Unlock()

	if ts, err := loadTombstones(); err == nil {
		for _, t := range pruneTombstones(ts) {
			if t.NetworkID == networkID && t.MacAddress != "" {
				taken[t.MacAddress] = true
			}
		}
	}
	return taken
}
