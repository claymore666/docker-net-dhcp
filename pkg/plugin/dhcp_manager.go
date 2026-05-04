package plugin

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

// linkAwaitTimeout caps how long Start waits for the macvlan child to
// reappear in the container netns under its post-rename name. Bridge mode
// keys off the veth peer index, which is symmetric across netns and
// available immediately, so it doesn't need this.
const linkAwaitTimeout = 30 * time.Second

const pollTime = 100 * time.Millisecond

// dhcpClientReapTimeout caps how long the udhcpc consumer waits to
// reap a self-exited child process before giving up and letting it
// linger as a zombie. The kernel's eventual reaping by init handles
// the worst case; this just bounds wall time on the cleanup path.
const dhcpClientReapTimeout = 5 * time.Second

// dhcpClientFinishTimeout caps how long Stop waits for SIGTERM ->
// DHCPRELEASE -> exit on the persistent udhcpc child. Long enough
// for a DHCPRELEASE round-trip on a healthy LAN; short enough that
// plugin shutdown / Leave isn't held hostage by an unresponsive
// upstream DHCP server.
const dhcpClientFinishTimeout = 5 * time.Second

type dhcpManager struct {
	docker  *docker.Client
	joinReq JoinRequest
	opts    DHCPNetworkOptions

	// ipMu guards lastIP / lastIPv6. Writes happen from the udhcpc
	// event goroutine (renew); reads happen from Leave after Stop has
	// drained that goroutine. The drain establishes happens-before in
	// practice, but the race detector doesn't always see the channel
	// pairing through `select`, and a future change to stop priority
	// could turn this into a real race. Cheap to make explicit.
	ipMu     sync.Mutex
	lastIP   *netlink.Addr
	lastIPv6 *netlink.Addr
	// MacAddress is set in macvlan mode so we can re-find the link inside
	// the container netns after Docker has moved and renamed it. Empty in
	// bridge mode.
	MacAddress net.HardwareAddr

	nsPath    string
	hostname  string
	nsHandle  netns.NsHandle
	netHandle *netlink.Handle
	ctrLink   netlink.Link

	stopChan  chan struct{}
	errChan   chan error
	errChanV6 chan error

	// startedCh is closed when Start has finished (success or failure);
	// startErr captures the result. This lets Stop be called against a
	// manager whose Start is still in flight (e.g. when Leave races
	// against the goroutine that Join spawned to call Start) — Stop
	// blocks until Start completes, then short-circuits if Start failed.
	startedCh chan struct{}
	startErr  error
}

func newDHCPManager(docker *docker.Client, r JoinRequest, opts DHCPNetworkOptions) *dhcpManager {
	return &dhcpManager{
		docker:  docker,
		joinReq: r,
		opts:    opts,

		stopChan:  make(chan struct{}),
		startedCh: make(chan struct{}),
	}
}

func (m *dhcpManager) logFields(v6 bool) log.Fields {
	return log.Fields{
		"network":  shortID(m.joinReq.NetworkID),
		"endpoint": shortID(m.joinReq.EndpointID),
		"sandbox":  m.joinReq.SandboxKey,
		"is_ipv6":  v6,
	}
}

// lastIPs returns the most recently observed v4/v6 leases under ipMu.
func (m *dhcpManager) lastIPs() (*netlink.Addr, *netlink.Addr) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return m.lastIP, m.lastIPv6
}

// setLastIP records a freshly-bound address under ipMu.
func (m *dhcpManager) setLastIP(v6 bool, addr *netlink.Addr) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	if v6 {
		m.lastIPv6 = addr
	} else {
		m.lastIP = addr
	}
}

func (m *dhcpManager) renew(v6 bool, info udhcpc.Info) error {
	v4, v6Last := m.lastIPs()
	lastIP := v4
	if v6 {
		lastIP = v6Last
	}

	ip, err := netlink.ParseAddr(info.IP)
	if err != nil {
		return fmt.Errorf("failed to parse IP address: %w", err)
	}

	if lastIP == nil || !ip.Equal(*lastIP) {
		// TODO: We can't deal with a different renewed IP for the same reason as described for `bound`
		log.
			WithFields(m.logFields(v6)).
			WithField("old_ip", lastIP).
			WithField("new_ip", ip).
			Warn("udhcpc renew with changed IP")
	}

	// Track the freshly-bound address so Leave can hand it to the
	// tombstone (and thus the next CreateEndpoint's `-r` hint).
	// Without this the manager keeps reporting whatever the very
	// first CreateEndpoint DISCOVER produced, even if udhcpc has
	// moved to a different lease since.
	m.setLastIP(v6, ip)

	// Skip gateway-from-DHCP renewal handling when the operator pinned a
	// gateway override on the network — leave their override in place.
	if !v6 && info.Gateway != "" && m.opts.Gateway == "" {
		newGateway := net.ParseIP(info.Gateway)

		routes, err := m.netHandle.RouteListFiltered(unix.AF_INET, &netlink.Route{
			LinkIndex: m.ctrLink.Attrs().Index,
			Dst:       nil,
		}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST)
		if err != nil {
			return fmt.Errorf("failed to list routes: %w", err)
		}

		if len(routes) == 0 {
			log.
				WithFields(m.logFields(v6)).
				WithField("gateway", newGateway).
				Info("udhcpc renew adding default route")

			if err := m.netHandle.RouteAdd(&netlink.Route{
				LinkIndex: m.ctrLink.Attrs().Index,
				Gw:        newGateway,
			}); err != nil {
				return fmt.Errorf("failed to add default route: %w", err)
			}
		} else if !newGateway.Equal(routes[0].Gw) {
			log.
				WithFields(m.logFields(v6)).
				WithField("old_gateway", routes[0].Gw).
				WithField("new_gateway", newGateway).
				Info("udhcpc renew replacing default route")

			routes[0].Gw = newGateway
			if err := m.netHandle.RouteReplace(&routes[0]); err != nil {
				return fmt.Errorf("failed to replace default route: %w", err)
			}
		}
	}

	return nil
}

func (m *dhcpManager) setupClient(v6 bool) (chan error, error) {
	v6Str := ""
	if v6 {
		v6Str = "v6"
	}

	log.
		WithFields(m.logFields(v6)).
		Info("Starting persistent DHCP client")

	// On plugin-restart recovery the persistent client should ask the
	// DHCP server for the IP the container is already using, instead
	// of doing a fresh DISCOVER that might return something different.
	// In the normal CreateEndpoint -> Join path lastIP / lastIPv6
	// already point at the IP we just acquired; passing it as -r is a
	// no-op (server still ACKs the same address). On recovery it's
	// what makes the lease "sticky".
	requestedIP := ""
	if !v6 {
		if v4Addr, _ := m.lastIPs(); v4Addr != nil && v4Addr.IP != nil {
			requestedIP = v4Addr.IP.String()
		}
	}
	client, err := udhcpc.NewDHCPClient(m.ctrLink.Attrs().Name, &udhcpc.DHCPClientOptions{
		Hostname:    m.hostname,
		V6:          v6,
		Namespace:   m.nsPath,
		RequestedIP: requestedIP,
		// Same client-id the initial DISCOVER used in CreateEndpoint,
		// so renewals are seen as the same client by the server.
		ClientID: clientIDFromEndpoint(m.joinReq.EndpointID),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP%v client: %w", v6Str, err)
	}

	events, err := client.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start DHCP%v client: %w", v6Str, err)
	}

	// Buffered: a partial-Start failure (v4 OK, v6 fails) bypasses Stop's
	// errChan reads; Stop short-circuits on m.startErr. Without a buffer
	// the goroutine here would block forever on the final write below.
	errChan := make(chan error, 1)
	go func() {
		for {
			select {
			case event, ok := <-events:
				if !ok {
					// udhcpc exited on its own (NAK, parent NIC vanished,
					// container netns torn down out from under us, etc.).
					// The scanner goroutine in udhcpc.Start closes events
					// when its read pipe hits EOF. Without this branch,
					// `<-events` on a closed channel returns the zero
					// Event{} every iteration, the switch matches nothing,
					// and we burn a CPU thread forever.
					log.
						WithFields(m.logFields(v6)).
						Warn("udhcpc event stream closed; client process exited")

					// Reap the child so it doesn't linger as a zombie:
					// cmd.Wait must be called exactly once per process,
					// and Stop's Finish path won't run if the consumer
					// returned first.
					reapCtx, reapCancel := context.WithTimeout(context.Background(), dhcpClientReapTimeout)
					if err := client.Wait(reapCtx); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							Debug("udhcpc reap returned error")
					}
					reapCancel()

					// Unblock Stop() if it's waiting on errChan. The
					// channel is buffered=1 so this never blocks; if
					// nobody's reading yet, the value sits until Stop
					// calls close(stopChan) and reads it.
					errChan <- nil
					return
				}
				switch event.Type {
				// TODO: We can't really allow the IP in the container to be deleted, it'll delete some of our previously
				// copied routes. Should this be handled somehow?
				//case "deconfig":
				//	ip := m.LastIP
				//	if v6 {
				//		ip = m.LastIPv6
				//	}

				//	log.
				//		WithFields(m.logFields(v6)).
				//		WithField("ip", ip).
				//		Info("udhcpc deconfiguring, deleting previously acquired IP")
				//	if err := m.netHandle.AddrDel(m.ctrLink, ip); err != nil {
				//		log.
				//			WithError(err).
				//			WithFields(m.logFields(v6)).
				//			WithField("ip", ip).
				//			Error("Failed to delete existing udhcpc address")
				//	}
				case "bound":
					// The persistent client's first DHCPACK can land
					// on a different IP than CreateEndpoint's initial
					// DISCOVER (some servers, including Fritz.Box,
					// hand out a fresh address per DISCOVER even for
					// the same MAC). Reuse the renew path so LastIP
					// reflects what's actually in the kernel.
					if err := m.renew(v6, event.Data); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							WithField("ip", event.Data.IP).
							Error("Failed to record initial bind")
					}
				case "renew":
					log.
						WithFields(m.logFields(v6)).
						Debug("udhcpc renew")

					if err := m.renew(v6, event.Data); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							WithField("gateway", event.Data.Gateway).
							WithField("new_ip", event.Data.IP).
							Error("Failed to execute IP renewal")
					}
				case "leasefail":
					log.WithFields(m.logFields(v6)).Warn("udhcpc failed to get a lease")
				case "nak":
					log.WithFields(m.logFields(v6)).Warn("udhcpc client received NAK")
				}

			case <-m.stopChan:
				log.
					WithFields(m.logFields(v6)).
					Info("Shutting down persistent DHCP client")

				ctx, cancel := context.WithTimeout(context.Background(), dhcpClientFinishTimeout)
				defer cancel()

				errChan <- client.Finish(ctx)
				return
			}
		}
	}()

	return errChan, nil
}

// locateContainerLink populates m.ctrLink with the post-Docker-move
// interface inside the container netns. The mechanism differs by mode:
//
//   - bridge: veth peer indexes are symmetric, so we read the host-side
//     veth's peer index and look that up in the sandbox netns. We also
//     wait for Docker's rename (the link must no longer carry the
//     pre-move name) so the persistent client doesn't race the move.
//   - macvlan / ipvlan: only one link is created and Docker moves it
//     wholesale, so we identify it by MAC after it reappears in the
//     sandbox. For ipvlan the child shares the parent's MAC, but the
//     parent is not in the container netns, so the MAC is still unique
//     within the search scope (loopback's MAC is all-zeros).
func (m *dhcpManager) locateContainerLink(ctx context.Context) error {
	if mode := m.opts.effectiveMode(); mode == ModeMacvlan || mode == ModeIPvlan {
		if len(m.MacAddress) == 0 {
			return fmt.Errorf("%v mode but no MAC address recorded for endpoint", mode)
		}

		awaitCtx, cancel := context.WithTimeout(ctx, linkAwaitTimeout)
		defer cancel()
		return util.AwaitCondition(awaitCtx, func() (bool, error) {
			link, err := findLinkByMAC(m.netHandle, m.MacAddress)
			if err != nil {
				// Not in the container netns yet — keep polling.
				return false, nil
			}
			m.ctrLink = link
			return true, nil
		}, pollTime)
	}

	hostName, oldCtrName := vethPairNames(m.joinReq.EndpointID)
	hostLink, err := netlink.LinkByName(hostName)
	if err != nil {
		return fmt.Errorf("failed to find host side of veth pair: %w", err)
	}
	hostVeth, ok := hostLink.(*netlink.Veth)
	if !ok {
		return util.ErrNotVEth
	}

	ctrIndex, err := netlink.VethPeerIndex(hostVeth)
	if err != nil {
		return fmt.Errorf("failed to get container side of veth's index: %w", err)
	}

	return util.AwaitCondition(ctx, func() (bool, error) {
		m.ctrLink, err = util.AwaitLinkByIndex(ctx, m.netHandle, ctrIndex, pollTime)
		if err != nil {
			return false, fmt.Errorf("failed to get link for container side of veth pair: %w", err)
		}
		return m.ctrLink.Attrs().Name != oldCtrName, nil
	}, pollTime)
}

func (m *dhcpManager) Start(ctx context.Context) (err error) {
	defer func() {
		m.startErr = err
		close(m.startedCh)
	}()
	var ctrID string
	if err := util.AwaitCondition(ctx, func() (bool, error) {
		dockerNet, err := m.docker.NetworkInspect(ctx, m.joinReq.NetworkID, dNetwork.InspectOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get Docker network info: %w", err)
		}

		for id, info := range dockerNet.Containers {
			if info.EndpointID == m.joinReq.EndpointID {
				ctrID = id
				break
			}
		}
		if ctrID == "" {
			return false, util.ErrNoContainer
		}

		// Seems like Docker makes the container ID just the endpoint until it's ready
		return !strings.HasPrefix(ctrID, "ep-"), nil
	}, pollTime); err != nil {
		return err
	}

	ctr, err := util.AwaitContainerInspect(ctx, m.docker, ctrID, pollTime)
	if err != nil {
		return fmt.Errorf("failed to get Docker container info: %w", err)
	}

	// Using the "sandbox key" directly causes issues on some platforms
	m.nsPath = fmt.Sprintf("/proc/%v/ns/net", ctr.State.Pid)
	m.hostname = ctr.Config.Hostname

	m.nsHandle, err = util.AwaitNetNS(ctx, m.nsPath, pollTime)
	if err != nil {
		return fmt.Errorf("failed to get sandbox network namespace: %w", err)
	}

	m.netHandle, err = netlink.NewHandleAt(m.nsHandle)
	if err != nil {
		m.nsHandle.Close()
		return fmt.Errorf("failed to open netlink handle in sandbox namespace: %w", err)
	}

	if err := func() error {
		if err := m.locateContainerLink(ctx); err != nil {
			return err
		}

		if m.errChan, err = m.setupClient(false); err != nil {
			close(m.stopChan)
			return err
		}

		if m.opts.IPv6 {
			if m.errChanV6, err = m.setupClient(true); err != nil {
				close(m.stopChan)
				return err
			}
		}

		return nil
	}(); err != nil {
		m.netHandle.Close()
		m.nsHandle.Close()
		return err
	}

	return nil
}

func (m *dhcpManager) Stop() error {
	// Wait for Start to finish so we don't tear down half-initialised
	// state. If Start failed there's nothing to clean up.
	<-m.startedCh
	if m.startErr != nil {
		return nil
	}

	// Guard against zero handles: Stop can be called against a manager
	// whose Start failed before AwaitNetNS / NewHandleAt set these
	// (see C-2 fix), in which case the deferred Close on the zero
	// value emits a noisy EBADF.
	defer func() {
		if m.nsHandle.IsOpen() {
			m.nsHandle.Close()
		}
	}()
	defer func() {
		if m.netHandle != nil {
			m.netHandle.Close()
		}
	}()

	close(m.stopChan)

	if err := <-m.errChan; err != nil {
		return fmt.Errorf("failed shut down DHCP client: %w", err)
	}
	if m.opts.IPv6 {
		if err := <-m.errChanV6; err != nil {
			return fmt.Errorf("failed shut down DHCPv6 client: %w", err)
		}
	}

	return nil
}
