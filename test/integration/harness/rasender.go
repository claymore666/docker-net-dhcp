// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// RASenderInterval is RFC 4861 section 6.2.1's smallest MaxRtrAdvInterval; dnsmasq 2.91 emits no Route Information
// option (radv-protocol.h:53 defines it, radv.c never writes it), and its lifetimes floor at 120 s (#1016).
const RASenderInterval = 4 * time.Second

// The sender's prefixes sit beside the fixtures' fd00:6470:6863 to 6865 and on no bridge address, so every route to
// them is the advertisement's (#1016).
const (
	AdvertPrefixA       = "fd00:6470:6866::/64"
	AdvertPrefixB       = "fd00:6470:6867::/64"
	UnadvertisedPrefix  = "fd00:6470:6868::/64"
	AdvertOnLinkPrefix  = "fd00:6470:6869::/64"
	AdvertRoutePrefix   = "fd00:6470:686a::/48"
	AdvertRoutePrefix2  = "fd00:6470:686b::/48"
	AdvertRouteLifetime = 1800
)

// RASender advertises a swappable RASpec on one link from its link-local address, hop limit 255 (RFC 4861 6.1.2).
type RASender struct {
	t     V6FixtureT
	iface string
	fd    int
	dst   unix.SockaddrInet6

	mu   sync.Mutex
	spec RASpec
	sent []time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// StartRASender sends spec at once and then every RASenderInterval until the test ends.
func StartRASender(t V6FixtureT, iface string, spec RASpec) *RASender {
	t.Helper()
	link, err := net.InterfaceByName(iface)
	if err != nil {
		t.Fatalf("RA sender: interface %s: %v", iface, err)
	}
	fd, err := openRASocket(iface, link.Index)
	if err != nil {
		t.Fatalf("RA sender on %s: %v", iface, err)
	}
	s := &RASender{
		t:     t,
		iface: iface,
		fd:    fd,
		dst:   unix.SockaddrInet6{ZoneId: uint32(link.Index)},
		spec:  spec,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	copy(s.dst.Addr[:], net.IPv6linklocalallnodes)
	go s.run()
	t.Cleanup(s.Stop)
	if _, err := s.send(); err != nil {
		t.Fatalf("RA sender on %s: first advertisement: %v", iface, err)
	}
	return s
}

func openRASocket(iface string, index int) (int, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	opts := []struct {
		name int
		val  int
	}{
		{unix.IPV6_MULTICAST_HOPS, 255},
		{unix.IPV6_UNICAST_HOPS, 255},
		{unix.IPV6_MULTICAST_IF, index},
		{unix.IPV6_MULTICAST_LOOP, 0},
	}
	for _, o := range opts {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, o.name, o.val); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("setsockopt %d: %w", o.name, err)
		}
	}
	if err := unix.BindToDevice(fd, iface); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind to %s: %w", iface, err)
	}
	return fd, nil
}

func (s *RASender) run() {
	defer close(s.done)
	tick := time.NewTicker(RASenderInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			if _, err := s.send(); err != nil {
				s.t.Logf("RA sender on %s: %v", s.iface, err)
			}
		}
	}
}

func (s *RASender) send() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := time.Now()
	if err := unix.Sendto(s.fd, EncodeRA(s.spec), 0, &s.dst); err != nil {
		return at, fmt.Errorf("sendto: %w", err)
	}
	s.sent = append(s.sent, at)
	return at, nil
}

// Set replaces the advertised content and sends it at once, returning when it went out.
func (s *RASender) Set(spec RASpec) time.Time {
	s.t.Helper()
	s.mu.Lock()
	s.spec = spec
	s.mu.Unlock()
	at, err := s.send()
	if err != nil {
		s.t.Fatalf("RA sender on %s: %v", s.iface, err)
	}
	return at
}

// Stop ends the advertisements; safe to call twice.
func (s *RASender) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		<-s.done
		unix.Close(s.fd)
	})
}
