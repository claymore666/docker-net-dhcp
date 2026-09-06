//go:build !linux

package runtime

import (
	"net"
	"net/netip"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/wire"
)

// NDStats is what an NDSocket has seen.
//
// The field list must match the Linux declaration, and not because the build
// says so — the fields' only readers are Linux-only test files. What holds it
// is TestNDStatsDeclarationsAgree, which parses both files.
type NDStats struct {
	Present     bool
	Reads       uint64
	Sends       uint64
	Skipped     uint64
	Own         uint64
	BadChecksum uint64
	BadHopLimit uint64
	BadSource   uint64
	Dropped     uint64
}

// NDFrame is one validated Neighbor Discovery message. See the Linux
// declaration; the field list is repeated here so the duplicate-address
// detection runner's signature compiles off Linux.
type NDFrame struct {
	Src, Dst netip.Addr
	SenderHW net.HardwareAddr
	Type     uint8
	Body     []byte
}

// NDSocket is not available on this platform. Every method below fails or is
// empty; none of them pretends to carry a Neighbor Discovery message.
//
// It refuses for ARPSocket's reason, sharpened: a "portable" ND socket that
// quietly did nothing would make RFC 4862 section 5.4's duplicate address
// detection report every address free without looking, and RFC 9915 section
// 18.2.10.1 makes that check a MUST.
type NDSocket struct{}

// NewNDSocket always fails off Linux, with ErrUnsupportedPlatform.
func NewNDSocket(string) (*NDSocket, error) { return nil, ErrUnsupportedPlatform }

// HardwareAddr returns nothing off Linux.
func (*NDSocket) HardwareAddr() net.HardwareAddr { return nil }

func (*NDSocket) Send(wire.ICMPv6Packet) error { return ErrUnsupportedPlatform }

// Received returns a closed channel.
func (*NDSocket) Received() <-chan lease.NDInbound {
	ch := make(chan lease.NDInbound)
	close(ch)
	return ch
}

// Frames returns a closed channel.
func (*NDSocket) Frames() <-chan NDFrame {
	ch := make(chan NDFrame)
	close(ch)
	return ch
}

func (*NDSocket) Close() error { return nil }

func (*NDSocket) Stats() NDStats { return NDStats{} }
