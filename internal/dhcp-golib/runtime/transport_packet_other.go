//go:build !linux

package runtime

import (
	"errors"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// ErrUnsupportedPlatform is returned by NewPacketTransport off Linux.
//
// An error rather than a build failure, so the pure rings and their tests stay
// usable on a non-Linux machine. It does not pretend to work: there is no
// portable way to send an IPv4 datagram with source 0.0.0.0 on an
// unconfigured interface, and a "portable" transport that quietly used an
// ordinary UDP socket would fail only against a real server.
var ErrUnsupportedPlatform = errors.New("runtime: AF_PACKET transport requires Linux")

// TransportStats is what a PacketTransport has seen.
//
// The field list must match the Linux declaration, and NOT because the build
// says so: the fields' only readers are Linux-only test files, so deleting one
// here leaves `GOOS=darwin go build ./...` and `go vet` at rc=0. What holds it
// is TestTransportStatsDeclarationsAgree, which parses both files.
type TransportStats struct {
	Reads       uint64
	Skipped     uint64
	Sends       uint64
	Uncompleted uint64
	Absent      uint64
	Dropped     uint64
}

// PacketTransport is not available on this platform. Every method below fails
// or is empty; none of them pretends to transport anything.
type PacketTransport struct{}

// NewPacketTransport always fails off Linux, with ErrUnsupportedPlatform.
func NewPacketTransport(string) (*PacketTransport, error) { return nil, ErrUnsupportedPlatform }

func (*PacketTransport) Send(proto.Dest, []byte) error { return ErrUnsupportedPlatform }

// Received returns a closed channel.
func (*PacketTransport) Received() <-chan lease.Inbound {
	ch := make(chan lease.Inbound)
	close(ch)
	return ch
}

func (*PacketTransport) Close() error { return nil }

func (*PacketTransport) Stats() TransportStats { return TransportStats{} }
