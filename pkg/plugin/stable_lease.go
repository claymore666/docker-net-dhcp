package plugin

import (
	"crypto/sha256"
)

// stableLeaseSeedTag versions the seed-construction scheme for the
// stable-lease client-id and namespaces its hash input. Bumping it
// deliberately remaps every container's derived client-id (and therefore
// its lease); keep it fixed so leases stay stable across plugin upgrades.
// It is distinct from stable-mac/v1 (#218) so a MAC seed and a client-id
// seed built from the *same* container identity never coincide on the
// wire.
const stableLeaseSeedTag = "stable-lease-clientid/v1"

// stableLeaseClientIDLen is the byte width of the derived option-61
// payload. Matches clientIDFromEndpoint's 8 bytes: long enough to be
// unique in any realistic deployment, short enough to keep the option
// well below the 255-byte wire limit.
const stableLeaseClientIDLen = 8

// stableLeaseSeed picks the stable seed for a container's derived DHCP
// client-id, or returns "" when no stable identity is available (in which
// case the caller must fall back to the endpoint-derived id — a "stable"
// client-id hashed from an unstable identity would just drift on the next
// recreate, defeating the point). Precedence, most-specific first:
//
//  1. explicit `-o lease_seed=<key>` driver-opt — full operator control
//  2. Compose project + service + container-number — survives
//     `compose up -d`, and the number keeps service replicas distinct
//  3. container name — explicit `--name`, or Compose without the
//     container-number label (the name carries the replica index)
//  4. none — anonymous container, no stable identity
//
// networkID is folded into every seed so the same container attached to
// two DHCP networks gets a distinct client-id on each. Each tier carries
// a discriminator string so a name that happens to equal a lease_seed
// value (or a project) can't hash to the same id.
func stableLeaseSeed(id containerIdentity, networkID string, opts DHCPNetworkOptions) string {
	prefix := stableLeaseSeedTag + "\x00" + networkID + "\x00"
	switch {
	case opts.LeaseSeed != "":
		return prefix + "seed\x00" + opts.LeaseSeed
	case id.ComposeProject != "" && id.ComposeService != "" && id.ComposeNumber != "":
		return prefix + "compose\x00" + id.ComposeProject + "\x00" + id.ComposeService + "\x00" + id.ComposeNumber
	case id.Name != "":
		return prefix + "name\x00" + id.Name
	default:
		return ""
	}
}

// deriveStableLeaseClientID turns a seed into a deterministic option-61
// payload: the first stableLeaseClientIDLen bytes of SHA-256(seed). The
// dhcpcd client wraps these with the type byte 0x00 (RFC 2132 opaque) on
// the wire, the same as the operator-supplied and endpoint-derived paths.
//
// Unlike the deterministic MAC (#218) there is no perturbation/collision
// avoidance: a MAC must be unique on the L2 segment, but two identical
// client-ids only matter on a true hash collision of *distinct*
// identities — negligible at 64 bits — and there is no live "taken" set
// at the DHCP layer to check against anyway.
func deriveStableLeaseClientID(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	out := make([]byte, stableLeaseClientIDLen)
	copy(out, sum[:stableLeaseClientIDLen])
	return out
}

// resolveEndpointClientID picks the option-61 payload for an endpoint's
// DHCP exchange, honoring stable_lease. Precedence:
//
//  1. operator `-o client_id=` — explicit override always wins (unchanged)
//  2. stable_lease=true with a resolvable stable identity → a client-id
//     derived from that identity, stable across recreate (#219)
//  3. otherwise → the endpoint-derived id (today's default)
//
// When stable_lease is set but no stable identity is available (an
// anonymous container), onFallback is invoked once — the CreateEndpoint
// caller uses it to bump the stable_lease_no_identity health counter and
// warn. The persistent/renewal and recovery callers pass nil: they
// re-derive the same id deterministically from the same container and
// must not double-count (or re-warn about) the one fallback the create
// path already recorded.
func resolveEndpointClientID(opts DHCPNetworkOptions, networkID, endpointID string, id containerIdentity, onFallback func()) []byte {
	if opts.StableLease && opts.ClientID == "" {
		if seed := stableLeaseSeed(id, networkID, opts); seed != "" {
			return deriveStableLeaseClientID(seed)
		}
		if onFallback != nil {
			onFallback()
		}
	}
	return resolveClientID(opts, endpointID)
}
