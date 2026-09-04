//go:build !linux

package runtime

import (
	"net"

	"github.com/claymore666/dhcp-golib/lease"
)

// ARPStats is what an ARPSocket has seen.
//
// The field list must match the Linux declaration, and not because the build
// says so — the fields' only readers are Linux-only test files. What holds it
// is TestTransportStatsDeclarationsAgree's ARP counterpart, which parses both
// files.
type ARPStats struct {
	Present bool
	Reads   uint64
	Sends   uint64
	Dropped uint64
}

// ARPSocket is not available on this platform. Every method below fails or is
// empty; none of them pretends to carry an ARP packet.
//
// RFC 5227 needs a raw link socket, and there is no portable one. A "portable"
// ARP socket that quietly did nothing would make a client report that it had
// checked an address it had not looked at, which is the one failure this whole
// milestone exists to prevent — so off Linux the constructor refuses and the
// caller cannot build a client with conflict detection on at all.
type ARPSocket struct{}

// NewARPSocket always fails off Linux, with ErrUnsupportedPlatform — the same
// error NewPacketTransport returns, because it is the same reason.
func NewARPSocket(string) (*ARPSocket, error) { return nil, ErrUnsupportedPlatform }

// HardwareAddr returns nothing off Linux.
func (*ARPSocket) HardwareAddr() net.HardwareAddr { return nil }

func (*ARPSocket) Send([]byte) error { return ErrUnsupportedPlatform }

// Received returns a closed channel.
func (*ARPSocket) Received() <-chan lease.ARPInbound {
	ch := make(chan lease.ARPInbound)
	close(ch)
	return ch
}

func (*ARPSocket) Close() error { return nil }

func (*ARPSocket) Stats() ARPStats { return ARPStats{} }
