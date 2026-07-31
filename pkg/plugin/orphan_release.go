package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/devplayer0/docker-net-dhcp/pkg/dhcp"
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
		log.WithError(err).WithFields(fields).
			Warn("Could not release orphaned lease; it will be held until it expires")
		return
	}

	p.orphanedLeasesReleased.Add(1)
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

	mac := m.MacAddress
	if mac == nil {
		// Identity for v4 rides on the client-id (option 61), which we
		// still have, so a synthetic MAC is enough to get the link up
		// and the binding matched. Locally-administered so it is
		// recognisable as ephemeral if it shows up in a server's log.
		if mac, err = newProbeMAC(); err != nil {
			return fmt.Errorf("MAC generation: %w", err)
		}
	}

	link, err := p.releaseLink(m.opts, linkName, mac)
	if err != nil {
		return err
	}
	defer func() {
		if err := netlink.LinkDel(link); err != nil {
			log.WithError(err).WithField("link", linkName).
				Warn("Orphan-release link cleanup failed")
		}
	}()

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring release link up: %w", err)
	}

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
	if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: lease.IPNet}); err != nil {
		return fmt.Errorf("assign %v to release link: %w", addr, err)
	}

	client, err := dhcp.NewDHCPClient(linkName, &dhcp.DHCPClientOptions{
		// Same identity the one-shot used, so the server matches the
		// existing binding and hands back the same address rather than
		// allocating a second one.
		MAC:      mac,
		ClientID: resolveClientID(m.opts, m.joinReq.EndpointID),
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
//   - macvlan / ipvlan: a macvlan child of the parent NIC. ipvlan gets
//     macvlan here for the same reason the preflight probe does —
//     ipvlan slaves share the parent MAC, which would collide with the
//     endpoint MAC we are deliberately reproducing.
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
		la := netlink.NewLinkAttrs()
		la.Name = name
		la.ParentIndex = parent.Attrs().Index
		la.HardwareAddr = mac
		link := &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}
		if err := netlink.LinkAdd(link); err != nil {
			return nil, fmt.Errorf("create release macvlan on %q: %w", opts.Parent, err)
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
