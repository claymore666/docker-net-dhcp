// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// The values of the `release_lease` network option (#962).
//
// RELEASE IS OFF BY DEFAULT AND THAT IS #800's RULE, NOT AN OVERSIGHT.
// A container is a host on this segment and a host does not hand its
// address back when it is switched off; the lease expires on the
// server's clock and a container that comes back before then re-claims
// it. Networks that would rather have the address back early say so.
const (
	// ReleaseNever is the default: no path sends a DHCPRELEASE or a
	// DHCPv6 Release, which is exactly the v1.9.0 behaviour (#800).
	ReleaseNever = "never"
	// ReleaseOnStop hands the lease back when the endpoint leaves its
	// sandbox, which is every `docker stop`, every `docker rm` of a
	// running container and every `docker network disconnect`. A
	// container that restarts asks for a fresh lease.
	ReleaseOnStop = "on_stop"
	// ReleaseOnRemove is #962's third value. The name is RESERVED and
	// the value is refused for now. See releaseOnRemoveRefusal.
	ReleaseOnRemove = "on_remove"
)

// releaseOnRemoveRefusal is why `release_lease=on_remove` is not
// available in this version, in the words an operator gets back from
// `docker network create`.
//
// MEASURED, and it is a fact about libnetwork rather than about this
// plugin: `DeleteEndpoint` runs when a container STOPS, not when it is
// removed. The tombstone that keeps a MAC stable across `docker
// restart` is written at `DeleteEndpoint` and consumed at the next
// `CreateEndpoint` inside a 60-second TTL (docs/reference.md, "Restart
// stability"), which is only possible if both run during the restart.
// So a release hung off `DeleteEndpoint` fires on every `docker stop` --
// that is this option's `on_stop` -- and a `docker rm` of a container
// that is already stopped reaches this plugin not at all, because its
// endpoint was deleted when it stopped.
//
// WHAT THE MESSAGE SAYS AND WHAT IT DOES NOT. It says the value is not
// available in this version. It does not say the behaviour cannot
// exist: a release that waits for a stopped container's endpoint to go
// unclaimed for the tombstone window would be `on_remove` built on
// something other than `DeleteEndpoint`, and that is a design decision
// held open, not one closed here. The NAME stays reserved for it, which
// is why the value is refused by name and never folded into the
// default.
//
// Refusing it now is the fail-closed answer. Accepting it and releasing
// on every stop would give two names to one behaviour and would make
// the reference page false for whoever read it.
const releaseOnRemoveRefusal = "release_lease=on_remove is not available in this version: " +
	"Docker deletes an endpoint when its container STOPS, not when the container is removed, " +
	"so a release sent there would fire on every `docker stop` (which is release_lease=on_stop) " +
	"and would never fire for `docker rm` of an already-stopped container. " +
	"The name is reserved. Use release_lease=on_stop or release_lease=never. See issue #962."

// releaseSendBudget is how long releaseHeldLease waits for the release
// packet to leave the host before writing the attempt off as failed.
//
// IT IS A BOUND ON `Leave`, NOT A PROTOCOL QUANTITY, and it is stated
// that way because there is no round trip to size it against: RFC 2131
// section 4.4.6 defines no answer to a DHCPRELEASE, and RFC 9915
// section 18.2.7's Reply is explicitly optional to wait for
// ("Implementations SHOULD retransmit one or more times but MAY choose
// to terminate the retransmission procedure early"). What is being
// waited on is the library's own request queue draining into one send
// on a socket that is already open, which is sub-millisecond work on an
// idle manager and single-digit milliseconds behind a burst of events.
//
// libnetwork blocks the container's teardown on `Leave`, so the cost of
// this number is paid by every `docker stop` on a releasing network
// when the send does not happen -- and when it does not happen, the
// lease is left to expire exactly as under `never`. One second buys
// three orders of magnitude over the work and keeps a stop that cannot
// release from being a stop that hangs.
// A VAR SO A UNIT TEST CAN SHORTEN IT. The failure arm of the wait is
// what the tests below drive most, and at a second apiece a table of
// them would be slow enough that someone would eventually delete it.
var releaseSendBudget = time.Second

// releaseSendPoll is how often the budget above is re-checked. The
// library offers no completion signal for a release -- the Lost event
// it produces is the machine's, not the socket's -- so the observer is
// the counter the library bumps AFTER a successful send, and this is
// how often it is read.
const releaseSendPoll = 5 * time.Millisecond

// parseReleaseLease normalises and validates the `release_lease`
// option. Empty is ReleaseNever.
//
// The refusal happens once, at CreateNetwork, so that everything after
// it deals in a value with two inhabitants -- the same rule
// conflict_check follows. A typo that silently selected the default
// would be a network an operator believes releases and that never does.
func parseReleaseLease(v string) (string, error) {
	switch v {
	case "", ReleaseNever:
		return ReleaseNever, nil
	case ReleaseOnStop:
		return ReleaseOnStop, nil
	case ReleaseOnRemove:
		return "", fmt.Errorf("%w: %s", util.ErrIPAM, releaseOnRemoveRefusal)
	default:
		return "", fmt.Errorf("%w: release_lease %q is not one of %s, %s",
			util.ErrIPAM, v, ReleaseNever, ReleaseOnStop)
	}
}

// releasesOnStop reports whether this network hands its leases back
// when an endpoint leaves.
//
// A METHOD AND NOT A COMPARISON AT EACH SITE. The release decision, the
// record's phase and the tombstone skip are three consequences of one
// answer, and three separate string comparisons are three places for
// them to disagree about what the network asked for.
func (o DHCPNetworkOptions) releasesOnStop() bool {
	return o.ReleaseLease == ReleaseOnStop
}

// releasingClient is what the teardown path asks a running DHCP client
// in order to hand its lease back. Two methods, both of which
// *dhcp.DHCPClient already has.
//
// The interface exists for the reason endpointClient does: the states
// that matter here -- a client holding a binding, a client that never
// bound, a client whose send fails -- live inside the library and no
// test in this package can construct one. Narrowing the field to what
// is called makes those states reachable, and it is what lets the
// counter pair be driven from the SEND rather than from the call.
type releasingClient interface {
	Stats() lease.Stats
	Release()
}

// setReleaseClient publishes one family's client to the teardown path.
func (m *dhcpManager) setReleaseClient(v6 bool, c releasingClient) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	if v6 {
		m.releaseV6 = c
		return
	}
	m.releaseV4 = c
}

// releaseClient is one family's client, or nil when this manager never
// started one -- a v4-only network asked for its v6 lease back, or a
// Start that failed before the client existed.
func (m *dhcpManager) releaseClient(v6 bool) releasingClient {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	c := m.releaseV4
	if v6 {
		c = m.releaseV6
	}
	// A nil interface holding a nil pointer is not nil, and the two
	// call sites that set this in a unit test hand it a typed nil.
	if c == nil {
		return nil
	}
	return c
}

// releasedAny reports whether either family's lease was handed back.
func (m *dhcpManager) releasedAny() bool {
	return m.releasedV4.Load() || m.releasedV6.Load()
}

// withdrawV6Address takes this endpoint's DHCPv6 address off the
// container link, which RFC 9915 section 18.2.7 requires before a
// Release exchange may begin.
//
// A missing address is not an error: the link is being torn down around
// this call and the kernel may have removed it already, and an address
// that is not on the interface satisfies the MUST by being absent.
func (m *dhcpManager) withdrawV6Address() error {
	_, last := m.lastIPs()
	if last == nil || last.IPNet == nil || last.IP == nil {
		return nil
	}
	if m.netHandle == nil || m.ctrLink == nil {
		return nil
	}
	if err := nlAddrDel(m.netHandle, m.ctrLink, last); err != nil {
		if errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.ENODEV) {
			return nil
		}
		return fmt.Errorf("failed to remove the DHCPv6 address %v before releasing it: %w", last, err)
	}
	return nil
}

// releaseHeldLease hands one family's lease back and reports whether a
// packet actually left the host.
//
// THE RETURN VALUE IS READ OFF THE LIBRARY'S SEND COUNTER AND NOT OFF
// THE CALL. `Release` does not block and does not report success; the
// library counts `ReleasesSent` where the send succeeded and nowhere
// else, and it counts nothing at all when the machine held no binding
// to relinquish. So "we asked" and "the server was told" are two
// different facts and only the second one is a release.
//
// RFC 9915 section 18.2.7 is why the v6 arm removes the address first:
//
//	The client MUST stop using all of the leases being released
//	before the client begins the Release message exchange process.
//	For an address, this means the address MUST have been removed
//	from the interface.
//
// and, in the same section:
//
//	The client MUST NOT use any of the addresses it is releasing as
//	the source address in the Release message or in any subsequently
//	transmitted message.
//
// The second MUST needs no code: the library's v6 transport sources
// from the interface's link-local address (runtime.Client6.Source),
// which is never an address this plugin leased. The first one does, and
// a removal that fails means the exchange may not begin -- so nothing
// is sent and the attempt is a failure, which leaves the address to
// expire on the server's clock exactly as under `never`.
//
// v4 has no such rule. RFC 2131 section 4.4.6 is the whole of what a
// DHCPRELEASE is:
//
//	If the client no longer requires use of its assigned network
//	address (e.g., the client is gracefully shut down), the client
//	sends a DHCPRELEASE message to the server.  Note that the
//	correct operation of DHCP does not depend on the transmission of
//	DHCPRELEASE messages.
//
// and section 4.4.4 is where the transmission rule lives:
//
//	The DHCP client broadcasts DHCPDISCOVER, DHCPREQUEST and
//	DHCPINFORM messages, unless the client knows the address of a
//	DHCP server.  The client unicasts DHCPRELEASE messages to the
//	server.
//
// Both are the library's to obey; the v4 address stays on the link
// because section 3.1(6) identifies the lease by 'ciaddr'.
func (m *dhcpManager) releaseHeldLease(v6 bool) bool {
	client := m.releaseClient(v6)
	if client == nil {
		return false
	}

	if v6 {
		if err := m.withdrawV6Address(); err != nil {
			log.WithError(err).WithFields(m.logFields(true)).
				Warn("Not releasing the DHCPv6 lease: the address could not be taken off the link first, " +
					"and RFC 9915 section 18.2.7 requires that before the exchange begins")
			return false
		}
	}

	before := client.Stats().ReleasesSent
	client.Release()

	deadline := time.Now().Add(releaseSendBudget)
	for {
		if client.Stats().ReleasesSent > before {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(releaseSendPoll)
	}
}

// releaseHeldLeases is the whole of what `release_lease=on_stop` does
// at teardown, for both families, and it reports whether ANY family's
// address was handed back.
//
// THE ANY IS DELIBERATE AND IT IS WHAT THE TOMBSTONE READS. The
// tombstone is one object carrying the MAC, the v4 address and the v6
// address together; a dual-stack endpoint that released only its v4
// lease must not leave behind a tombstone whose next consumer asks for
// the address just handed back. Each family's RECORD is still settled
// on its own outcome, because a v6 lease that was not released is a v6
// lease this endpoint may still resume.
func (m *dhcpManager) releaseHeldLeases() (releasedV4, releasedV6 bool) {
	releasedV4 = m.releaseHeldLease(false)
	m.countRelease(false, releasedV4)
	if m.opts.IPv6 {
		releasedV6 = m.releaseHeldLease(true)
		m.countRelease(true, releasedV6)
	}

	log.WithFields(m.logFields(false)).
		WithField("released_v4", releasedV4).
		WithField("released_v6", releasedV6).
		Info("release_lease=on_stop: handing this endpoint's lease back before the client stops")
	return releasedV4, releasedV6
}

// countRelease moves the per-family pair. Sent is a packet the library
// saw leave; failed is an attempt that produced none, whatever the
// reason -- a client that held no binding, a send that failed, a
// request dropped from a full queue, or a v6 address that could not be
// taken off the link.
func (m *dhcpManager) countRelease(v6 bool, sent bool) {
	if m.plugin == nil {
		return
	}
	if sent {
		bumpFamily(&m.plugin.releasesSentV4, &m.plugin.releasesSentV6, v6)
		return
	}
	bumpFamily(&m.plugin.releaseFailuresV4, &m.plugin.releaseFailuresV6, v6)
}
