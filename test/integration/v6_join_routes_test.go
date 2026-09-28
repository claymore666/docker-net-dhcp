// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"maps"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The one-shot DHCPv6 exchange solicits a router at its start and never waits for the answer (dhcp-golib
// proto/machine6.go begin); a router answers the solicitation (RFC 4861 section 6.2.6), which the sender's
// four-second repeat alone does not, so its advertisement could miss the exchange (#1125).

// answerRouterSolicits re-sends the sender's advertisement on every Router Solicitation heard on iface.
func answerRouterSolicits(t *testing.T, iface string, sender *harness.RASender, spec harness.RASpec) *atomic.Int32 {
	t.Helper()
	link, err := net.InterfaceByName(iface)
	if err != nil {
		t.Fatalf("interface %s: %v", iface, err)
	}
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		t.Fatalf("ICMPv6 socket: %v", err)
	}
	var filter unix.ICMPv6Filter
	for i := range filter.Data {
		filter.Data[i] = 0xffffffff
	}
	const routerSolicit = 133
	filter.Data[routerSolicit>>5] &^= 1 << (routerSolicit & 31)
	mreq := &unix.IPv6Mreq{Interface: uint32(link.Index)}
	copy(mreq.Multiaddr[:], net.ParseIP("ff02::2"))
	timeout := unix.NsecToTimeval(int64(200 * time.Millisecond))
	for _, step := range []func() error{
		func() error { return unix.SetsockoptICMPv6Filter(fd, unix.IPPROTO_ICMPV6, unix.ICMPV6_FILTER, &filter) },
		func() error { return unix.BindToDevice(fd, iface) },
		func() error { return unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, mreq) },
		func() error { return unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout) },
	} {
		if err := step(); err != nil {
			unix.Close(fd)
			t.Fatalf("router solicitation listener on %s: %v", iface, err)
		}
	}

	var answered atomic.Int32
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, err := unix.Recvfrom(fd, buf, 0); err == nil {
				sender.Set(spec)
				answered.Add(1)
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		unix.Close(fd)
	})
	return &answered
}

// startWithRoutesAtStart starts a container whose first process saves its IPv6 routes, and returns them.
func startWithRoutesAtStart(t *testing.T, ctx context.Context, cli *docker.Client, netName string) (string, error) {
	t.Helper()
	ctrName := netName + "-ctr"
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Hostname: ctrName, Cmd: []string{"sh", "-c",
			"{ ip -6 route show; echo '#end'; } > /tmp/r && mv /tmp/r /tmp/routes-at-start; exec sleep infinity"}},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate(%s): %v", ctrName, err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, create.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := harness.ExecOutput(t, ctx, create.ID, "sh", "-c", "cat /tmp/routes-at-start 2>/dev/null")
		if strings.Contains(out, "#end") {
			return out, nil
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the container's first process wrote no route table in 10s: %q", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func countRoutesTo(lines []string, cidr string) int {
	n := 0
	for _, line := range lines {
		if strings.HasPrefix(line, cidr+" ") {
			n++
		}
	}
	return n
}

// TestJoin_TheAdvertisedRoutesAreInTheContainerAtStart reads the routes the container's first process sees: Join's
// answer, before the persistent client binds (#1125).
func TestJoin_TheAdvertisedRoutesAreInTheContainerAtStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	own, second := harness.V6SubnetV6CIDR, harness.AdvertOnLinkPrefix
	cases := []struct {
		name, net       string
		opts            map[string]string
		hostRouteToNext bool
	}{
		{"ipv6=true", "dh-itest-jr6", map[string]string{"ipv6": "true"}, false},
		{"ipv6_mode=dhcp alone", "dh-itest-jrmode", map[string]string{"ipv6_mode": "dhcp"}, false},
		{"host route to the second advertised prefix", "dh-itest-jrhost", map[string]string{"ipv6": "true"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := harness.RASpec{
				Managed:        true,
				RouterLifetime: harness.AdvertRouteLifetime,
				Prefixes: []harness.RAPrefix{
					advertPrefix(t, own, false, 1800, 1800),
					advertPrefix(t, second, false, 1800, 1800),
				},
			}
			f, sender := startSenderSegment(t, harness.V6PoolWithoutRAArgs(), spec)
			answered := answerRouterSolicits(t, f.Bridge(), sender, spec)

			if c.hostRouteToNext {
				bridge, err := netlink.LinkByName(f.Bridge())
				if err != nil {
					t.Fatalf("LinkByName(%s): %v", f.Bridge(), err)
				}
				_, dst, _ := net.ParseCIDR(second)
				route := &netlink.Route{LinkIndex: bridge.Attrs().Index, Dst: dst}
				if err := netlink.RouteAdd(route); err != nil {
					t.Fatalf("RouteAdd %s dev %s: %v", second, f.Bridge(), err)
				}
				t.Cleanup(func() {
					if err := netlink.RouteDel(route); err != nil {
						t.Logf("WARN: RouteDel %s dev %s: %v", second, f.Bridge(), err)
					}
				})
			}

			opts := map[string]string{"bridge": f.Bridge()}
			maps.Copy(opts, c.opts)
			harness.CreateNetwork(t, ctx, c.net, "bridge", opts)
			out, err := startWithRoutesAtStart(t, ctx, cli, c.net)
			if err != nil {
				t.Fatalf("ContainerStart: %v (router solicitations answered so far: %d)", err, answered.Load())
			}

			lines := pluginV6Routes(out)
			if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "default via fe80::") }) {
				t.Errorf("no IPv6 default route via the advertising router when the container started "+
					"(router solicitations answered: %d):\n%s", answered.Load(), out)
			}
			for _, cidr := range []string{own, second} {
				switch n := countRoutesTo(lines, cidr); {
				case n != 1:
					t.Errorf("%d routes to the advertised on-link prefix %s when the container started, want 1:\n%s",
						n, cidr, out)
				default:
					if line, _ := routeTo(lines, cidr); strings.Contains(line, " via ") {
						t.Errorf("the on-link prefix %s is routed via a gateway: %q", cidr, line)
					}
				}
			}
		})
	}
}
