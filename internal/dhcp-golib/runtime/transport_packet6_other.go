//go:build !linux

package runtime

import (
	"net"
	"net/netip"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// TransportStatsV6 is what a PacketTransportV6 has seen.
//
// The field list must match the Linux declaration, and NOT because the build
// says so — the fields' only readers are Linux-only test files, so deleting
// one here leaves `GOOS=darwin go build ./...` and `go vet` at rc=0. What
// holds it is TestTransportStatsV6DeclarationsAgree, which parses both files.
type TransportStatsV6 struct {
	Reads        uint64
	Skipped      uint64
	Sends        uint64
	Uncompleted  uint64
	ZeroChecksum uint64
	BadChecksum  uint64
	Foreign      uint64
	Dropped      uint64
}

// PacketTransportV6 is not available on this platform. Every method below
// fails or is empty; none of them pretends to transport anything.
//
// For PacketTransport's reason, applied to a family where it is if anything
// sharper: there is no portable way to send a DHCPv6 Solicit from a link-local
// source on an interface whose address has not finished duplicate address
// detection.
type PacketTransportV6 struct{}

// NewPacketTransportV6 always fails off Linux, with ErrUnsupportedPlatform.
func NewPacketTransportV6(string) (*PacketTransportV6, error) { return nil, ErrUnsupportedPlatform }

// Source returns the zero address off Linux.
func (*PacketTransportV6) Source() netip.Addr { return netip.Addr{} }

// HardwareAddr returns nothing off Linux.
func (*PacketTransportV6) HardwareAddr() net.HardwareAddr { return nil }

func (*PacketTransportV6) Send(proto.Dest, []byte) error { return ErrUnsupportedPlatform }

// Received returns a closed channel.
func (*PacketTransportV6) Received() <-chan lease.Inbound {
	ch := make(chan lease.Inbound)
	close(ch)
	return ch
}

func (*PacketTransportV6) Close() error { return nil }

func (*PacketTransportV6) Stats() TransportStatsV6 { return TransportStatsV6{} }
