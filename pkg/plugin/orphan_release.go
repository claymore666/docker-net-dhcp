// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// orphanReleaseBudget caps the whole synthesised release: bring up a
// temporary link, let dhcpcd reacquire the binding under the endpoint's
// identity, then SIGTERM it so it emits the DHCPRELEASE.
//
// It is deliberately generous. This runs off the container-teardown
// path, so nothing user-visible is waiting on it, and the alternative
// to waiting is a lease pinned until its own expiry.
const orphanReleaseBudget = 20 * time.Second

// releaseOrphanedLease frees a lease that the CreateEndpoint one-shot
// acquired and that no persistent client ever took ownership of.
//
// # Why an orphan is possible at all
//
// The one-shot client in CreateEndpoint runs with `-1 -p`: it acquires
// the address and deliberately does NOT release it on exit, so the
// address it reported to Docker is still held when the persistent
// client takes over at Join (pkg/dhcp/dhcpcd.go). Releasing is
// therefore the persistent client's job, via the `release` directive it
// alone emits.
//
// If the persistent client never starts, nobody holds that
// responsibility. The common way to get there is a container that exits
// between CreateEndpoint and the async Start in Join — the window is
// short, but "start, do one thing, exit" is an ordinary container
// lifecycle, not an exotic one. The lease then sits on the server until
// it expires: invisible to the operator, and on a small pool a genuine
// exhaustion risk. Observed on the integration suite at 17 of 32
// containers once #367 removed the accidental 10s stop grace that had
// been giving the persistent client time to win the race (#370).
//
// # Why it has to be synthesised
//
// By the time we know the lease is orphaned, the container's netns is
// gone and its interface with it, so there is nothing left to release
// FROM. Instead we rebuild just enough identity on the same segment —
// a temporary link carrying the endpoint's MAC, and dhcpcd configured
// with the endpoint's client-id and its address as `request` — and let
// a normal client lifecycle do the release. Same trick as the
// validate_dhcp preflight probe, in reverse.
//
// The reacquire is not wasted work: it is what makes the release
// well-formed. A DHCPRELEASE has to come from the client that holds the
// binding, so we become that client for as long as it takes to hand the
// address back.
//
// Best-effort by construction — it is called after the endpoint is
// already gone, so there is no caller to fail. Outcomes land on the
// health counters instead.
func (p *Plugin) releaseOrphanedLease(m *dhcpManager, endpointID string) {
	v4, _ := m.lastIPs()
	if v4 == nil || v4.IP == nil {
		// Nothing was ever acquired (the one-shot itself failed), so
		// there is no binding to hand back.
		return
	}
	addr := v4.IP.String()

	fields := log.Fields{
		"endpoint": shortID(endpointID),
		"ip":       addr,
	}

	ctx, cancel := context.WithTimeout(context.Background(), orphanReleaseBudget)
	defer cancel()

	if err := p.synthesiseRelease(ctx, m, v4); err != nil {
		p.orphanedLeaseReleaseFailures.Add(1)
		// Same ledger vocabulary as an ordinary teardown, because the
		// fact being recorded is the same one: this address was not
		// handed back. Until now the reclaim was invisible to the audit
		// log entirely — an endpoint that took this path left a `bound`
		// and then nothing, which reads as a lease still held whether
		// or not the reclaim worked.
		m.audit("release_failed", addr)
		log.WithError(err).WithFields(fields).
			Warn("Could not release orphaned lease; it will be held until it expires")
		return
	}

	p.orphanedLeasesReleased.Add(1)
	m.audit("release", addr)
	log.WithFields(fields).Info("Released orphaned lease")
}

// spawnOrphanRelease hands the reclaim to a tracked goroutine, exactly
// once per manager.
//
// The once matters: both abandon paths can fire for the same manager.
// Join's Start goroutine discovers the container is gone at the same
// moment a Leave may be taking the manager out of the registry, and the
// two do not otherwise serialise. Releasing twice would emit a
// DHCPRELEASE for an address the server has already freed and may
// already have reallocated — a stranger's lease, torn down by us.
func (p *Plugin) spawnOrphanRelease(m *dhcpManager) {
	if p == nil || m == nil {
		return
	}
	m.orphanReleaseOnce.Do(func() {
		p.orphanReleases.Add(1)
		go func() {
			defer p.orphanReleases.Done()
			p.releaseOrphanedLease(m, m.joinReq.EndpointID)
		}()
	})
}

// synthesiseRelease does the work described on releaseOrphanedLease:
// temporary link -> reacquire under the endpoint's identity -> release.
func (p *Plugin) synthesiseRelease(ctx context.Context, m *dhcpManager, lease *netlink.Addr) error {
	addr := lease.IP.String()

	linkName, err := newReleaseLinkName()
	if err != nil {
		return fmt.Errorf("name generation: %w", err)
	}

	plan, err := releaseMACPlan(m.opts, m.MacAddress, newProbeMAC)
	if err != nil {
		return fmt.Errorf("MAC generation: %w", err)
	}

	link, mac, err := p.upReleaseLink(ctx, m.opts, linkName, plan)
	if err != nil {
		return err
	}
	defer func() {
		if err := netlink.LinkDel(link); err != nil {
			log.WithError(err).WithField("link", linkName).
				Warn("Orphan-release link cleanup failed")
		}
	}()

	// Put the leased address on the link, and note that this is
	// LOAD-BEARING rather than tidiness.
	//
	// A DHCPRELEASE is unicast to the server sourced FROM the address
	// being given back — an unsourced release is not a release. dhcpcd
	// runs `--noconfigure` here as everywhere in this plugin, so it
	// never assigns anything itself; on a normal endpoint the plugin
	// has already configured the address inside the container, which is
	// why those releases reach the server. This link is brand new and
	// has nothing, so without this the whole sequence completes and
	// logs happily — solicit, offer, lease, "releasing lease of X" —
	// while the server sees no RELEASE at all. Observed exactly that on
	// the first CI run of this code: dnsmasq logged zero DHCPRELEASE
	// lines against a plugin that had counted three.
	//
	// Added before the client starts rather than after the bind: the
	// address is already known (it is the one we are handing back), and
	// doing it up front means no window where dhcpcd could decide to
	// release before the source address exists.
	//
	// Retried on EADDRINUSE for the same reason the link-up is (#408):
	// the endpoint's own child link is still there, still holding this
	// address, and DeleteEndpoint is about to remove it — observed
	// landing one second after the failed assignment. On ipvlan the
	// kernel enforces address uniqueness across every slave of the
	// parent, so the old child holding it blocks the new one outright;
	// this is the address-level twin of the MAC collision above.
	if err := addrAddAwaiting(ctx, link, &netlink.Addr{IPNet: lease.IPNet}, childLinkUpBudget); err != nil {
		return fmt.Errorf("assign %v to release link: %w", addr, err)
	}

	client, err := dhcp.NewDHCPClient(linkName, &dhcp.DHCPClientOptions{
		// Whatever MAC the link ended up wearing — the endpoint's when it
		// was free, a synthetic one when it was not (see releaseMACPlan).
		// The link's address and the DHCP identity are separate: the
		// server matches the binding on the client-id below, which is
		// the same either way.
		//
		// That client-id comes from m.clientID(), NOT from the local mac
		// above. Since #371 the id is MAC-derived in every mode but
		// ipvlan, and the local mac may be a synthetic fallback —
		// deriving the id from it would build an identity the server has
		// never seen, so the release would match no binding. The failure
		// would be silent, because dhcpcd reports a release it sent
		// regardless of what it actually freed.
		MAC:      mac,
		ClientID: m.clientID(),
		// `request ADDR`: ask for precisely the address we are trying
		// to give back.
		RequestedIP: addr,
		VendorClass: m.opts.VendorClass,
		// Not Once: only the persistent shape emits dhcpcd's `release`
		// directive, which is the entire point of this exercise.
	})
	if err != nil {
		return fmt.Errorf("create release client: %w", err)
	}

	events, err := client.Start()
	if err != nil {
		return fmt.Errorf("start release client: %w", err)
	}

	// Wait for the binding before signalling. dhcpcd only sends a
	// DHCPRELEASE for a lease it currently holds, so a SIGTERM that
	// arrives mid-DORA releases nothing and would report success
	// against an address still held upstream.
	bound := false
	for !bound {
		select {
		case event, ok := <-events:
			if !ok {
				// Client exited on its own without ever binding.
				_ = client.Wait(ctx)
				return fmt.Errorf("release client exited before acquiring %v", addr)
			}
			if event.Type == "bound" || event.Type == "renew" {
				bound = true
			}
		case <-ctx.Done():
			// Still signal: a client that is mid-exchange must not be
			// left running on a link we are about to delete.
			_ = client.Finish(context.Background())
			return fmt.Errorf("timed out waiting to reacquire %v for release: %w", addr, ctx.Err())
		}
	}

	if err := client.Finish(ctx); err != nil {
		return fmt.Errorf("release client shutdown: %w", err)
	}
	return nil
}

// releaseLink builds the temporary link the release is sent from,
// matching how the endpoint was attached in the first place:
//
//   - macvlan / ipvlan: a child of the parent NIC of the SAME KIND the
//     endpoint had. This paragraph used to say ipvlan gets a macvlan
//     here, "for the same reason the preflight probe does" — it no
//     longer does either, and the reason was wrong in both places: a
//     parent NIC is a macvlan port or an ipvlan port, never both, so
//     asking for the other kind is refused outright (#486). The shared
//     ipvlan MAC that motivated it is handled on identity instead, in
//     releaseMACPlan.
//   - bridge: a veth pair with the far end enslaved to the bridge, the
//     near end carrying the endpoint's MAC. Same shape CreateEndpoint
//     builds, minus the move into a container netns.
//
// The MAC being reused is safe precisely because this is only ever
// called once the container is gone: nothing else on the segment is
// answering for it.
func (p *Plugin) releaseLink(opts DHCPNetworkOptions, name string, mac net.HardwareAddr) (netlink.Link, error) {
	switch {
	case opts.Parent != "":
		parent, err := netlink.LinkByName(opts.Parent)
		if err != nil {
			return nil, fmt.Errorf("lookup parent %q: %w", opts.Parent, err)
		}
		mode := opts.effectiveMode()
		la := netlink.NewLinkAttrs()
		la.Name = name
		la.ParentIndex = parent.Attrs().Index
		// ipvlan children inherit the parent's address and the kernel
		// rejects any attempt to set one, so the plan's MAC is only
		// applied where it means something. It is still the right MAC to
		// hand the DHCP client below — what the client puts in chaddr
		// and what the link wears are separate.
		if mode != ModeIPvlan {
			la.HardwareAddr = mac
		}
		// The same KIND of link the endpoint had, not always a macvlan.
		// A parent NIC is a macvlan port or an ipvlan port, never both,
		// so building a macvlan release link on an ipvlan network asks
		// the kernel for something it will not do — which is the other
		// half of why an ipvlan orphaned lease has never been released
		// (#402).
		link := newChildLink(mode, la)
		if err := netlink.LinkAdd(link); err != nil {
			return nil, fmt.Errorf("create release %v on %q: %w", mode, opts.Parent, err)
		}
		return link, nil

	case opts.Bridge != "":
		bridge, err := netlink.LinkByName(opts.Bridge)
		if err != nil {
			return nil, fmt.Errorf("lookup bridge %q: %w", opts.Bridge, err)
		}
		la := netlink.NewLinkAttrs()
		la.Name = name
		la.HardwareAddr = mac
		link := &netlink.Veth{LinkAttrs: la, PeerName: name + "p"}
		if err := netlink.LinkAdd(link); err != nil {
			return nil, fmt.Errorf("create release veth on %q: %w", opts.Bridge, err)
		}
		peer, err := netlink.LinkByName(name + "p")
		if err != nil {
			_ = netlink.LinkDel(link)
			return nil, fmt.Errorf("lookup release veth peer: %w", err)
		}
		if err := netlink.LinkSetMaster(peer, bridge); err != nil {
			_ = netlink.LinkDel(link)
			return nil, fmt.Errorf("enslave release veth to %q: %w", opts.Bridge, err)
		}
		if err := netlink.LinkSetUp(peer); err != nil {
			_ = netlink.LinkDel(link)
			return nil, fmt.Errorf("bring release veth peer up: %w", err)
		}
		// Deleting one end of a veth removes both, so the deferred
		// LinkDel on the near end cleans the peer up too.
		return link, nil
	}

	return nil, fmt.Errorf("network has neither parent nor bridge; cannot synthesise a release path")
}

// newReleaseLinkName mirrors newProbeLinkName: a per-call unique name
// so concurrent releases cannot collide on it. "dh-rel-" + 6 hex is 13
// chars, inside IFNAMSIZ-1, and leaves room for the veth peer's
// trailing "p".
func newReleaseLinkName() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "dh-rel-" + hex.EncodeToString(b[:]), nil
}

// releaseMACRetryWindow is how long the release path waits for the
// endpoint's own MAC to come free before settling for a synthetic one.
//
// Well inside orphanReleaseBudget, which also has to cover a full DHCP
// exchange afterwards. The thing being waited for is Docker finishing a
// teardown it has already started, which is a matter of hundreds of
// milliseconds; a wait much longer than this is waiting for something
// that is not coming.
const releaseMACRetryWindow = 3 * time.Second

// releaseMACRetryInterval paces the retries within that window.
const releaseMACRetryInterval = 200 * time.Millisecond

// releaseMACPlan returns the hardware addresses to try for the release
// link, in order.
//
// The link's MAC and the DHCP identity are separate things, and only the
// second has to be faithful: the server matches the binding on the
// client-id (option 61) and on the requested address, both of which are
// unchanged whatever the link wears. That is what makes a fallback
// possible at all.
//
// Preferring the endpoint's own MAC is still right where it can be had.
// A server keyed on the hardware address rather than the client-id will
// only honour a release that carries it, and the plugin cannot know
// which kind of server it is talking to.
//
// Two cases skip straight to a synthetic address:
//
//   - Nothing was recorded. Pre-existing behaviour, unchanged.
//   - ipvlan. Its children share the parent NIC's address by kernel
//     design, so the recorded MAC *is* the parent's — and the kernel's
//     duplicate check tests the parent's own address explicitly. It can
//     never be free, so waiting for it would burn the window every time
//     and then fall back anyway (#402).
//
// Locally-administered, so a synthetic address is recognisable as
// ephemeral if it shows up in a server's log.
func releaseMACPlan(opts DHCPNetworkOptions, recorded net.HardwareAddr, synth func() (net.HardwareAddr, error)) ([]net.HardwareAddr, error) {
	fallback, err := synth()
	if err != nil {
		return nil, err
	}
	if len(recorded) == 0 || opts.effectiveMode() == ModeIPvlan {
		return []net.HardwareAddr{fallback}, nil
	}
	return []net.HardwareAddr{recorded, fallback}, nil
}

// upReleaseLink creates the release link and brings it up, working
// through the MAC plan until one succeeds. Returns the live link and the
// address it ended up wearing.
//
// The retry is on EADDRINUSE specifically, and it is not a guess about
// timing — it is the kernel refusing a hardware address that is still
// live on the parent. The endpoint's own child link is what holds it:
// created in the host netns at CreateEndpoint, removed at
// DeleteEndpoint, and this path runs from the failed-Join goroutine,
// which is not ordered against either. So the release routinely arrives
// while Docker is still tearing the endpoint down (#402).
//
// Asking the kernel rather than predicting the answer is deliberate. A
// scan of the host's links cannot see a child that Docker has already
// moved into a netns that is itself being destroyed, and that child
// still holds the address on the parent's port. Only the attempt is
// authoritative.
// This deliberately does not reuse linkUpAwaitingAddress (#408), which
// retries LinkSetUp on one link. A link's MAC is fixed when it is
// created, so working through a plan of addresses means destroying and
// rebuilding the link on each attempt — a different operation that only
// looks like the same one.
func (p *Plugin) upReleaseLink(ctx context.Context, opts DHCPNetworkOptions, linkName string, plan []net.HardwareAddr) (netlink.Link, net.HardwareAddr, error) {
	var lastErr error
	for i, mac := range plan {
		// Every MAC but the last gets the retry window; the last is the
		// fallback and there is nothing further to fall back to.
		deadline := time.Now()
		if i < len(plan)-1 {
			deadline = deadline.Add(releaseMACRetryWindow)
		}

		for {
			link, err := p.releaseLink(opts, linkName, mac)
			if err != nil {
				return nil, nil, err
			}
			if err := netlink.LinkSetUp(link); err == nil {
				if i > 0 {
					log.WithFields(log.Fields{"link": linkName, "mac": mac.String()}).
						Debug("Release link using a synthetic MAC; the endpoint's own was still in use")
				}
				return link, mac, nil
			} else {
				lastErr = err
				// Always remove the link we just made, whatever the
				// failure — a half-created link would collide with the
				// next attempt on its NAME rather than its MAC, turning
				// a recoverable problem into a stuck one.
				if delErr := netlink.LinkDel(link); delErr != nil {
					log.WithError(delErr).WithField("link", linkName).
						Debug("Could not remove a release link that failed to come up")
				}
				if !errors.Is(err, unix.EADDRINUSE) {
					return nil, nil, fmt.Errorf("bring release link up: %w", err)
				}
			}

			if !time.Now().Before(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, nil, fmt.Errorf("bring release link up: %w (last attempt: %w)", ctx.Err(), lastErr)
			case <-time.After(releaseMACRetryInterval):
			}
		}
	}
	return nil, nil, fmt.Errorf("bring release link up: %w", lastErr)
}

// addrAddAwaiting assigns an address to a link, waiting out the window
// where the link being replaced still holds it.
//
// The mirror of linkUpAwaitingAddress, one step later in the same
// sequence and for the same reason. Bringing the release link up
// contends for the MAC; putting the leased address on it contends for
// the address. Both are held by the endpoint's own child link, both are
// released when DeleteEndpoint removes it, and the reclaim runs from a
// goroutine that is ordered against neither.
//
// ipvlan makes this the harder half. Its slaves share the parent's MAC,
// so the kernel keeps them apart by address instead and enforces
// uniqueness across every slave of the port — the old child holding the
// address blocks the new one outright rather than occasionally.
//
// No fallback: the release has to be sourced FROM the address being
// given back, so there is no other address that would do. If the wait
// expires the reclaim fails and the lease sits until it expires, which
// is where it was before any of this existed.
func addrAddAwaiting(ctx context.Context, link netlink.Link, addr *netlink.Addr, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		err := nlAddrAdd(link, addr)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w (still held by the endpoint link this release replaces, "+
				"after waiting %v)", err, budget)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last attempt: %w)", ctx.Err(), err)
		case <-time.After(childLinkUpInterval):
		}
	}
}
