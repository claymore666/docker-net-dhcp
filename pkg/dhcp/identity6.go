// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/claymore666/dhcp-golib/wire"
)

// Identity6 is one endpoint's DHCPv6 identity: RFC 9915 section 21.2's
// DUID as it goes on the wire, and section 21.4's IAID.
//
// THE CHASSIS MINTS IT AND THE LIBRARY CARRIES IT (D10, D30 Q4). The
// library refuses an empty DUID rather than inventing one, because RFC
// 9915 section 11 says a DUID "SHOULD NOT change over time if at all
// possible" and a value generated per process changes on every restart.
// Persistence is therefore this side's obligation: the identity is
// written once into the endpoint's v6 record and read back on every
// restart, and Bytes/ParseIdentity6 are the two halves of that.
type Identity6 struct {
	// DUID is the whole option 21.2 payload, type code included.
	DUID []byte
	// IAID is the identity association identifier. Zero is a legal
	// value on the wire and is not a sentinel; an Identity6 with no
	// DUID is the empty one.
	IAID uint32
}

// IsZero reports an identity that was never minted.
func (i Identity6) IsZero() bool { return len(i.DUID) == 0 }

// hwTypeEthernet is IANA's "Number Hardware Type (hrd)" 1, the value
// RFC 9915 section 11.4's DUID-LL carries for an Ethernet link.
const hwTypeEthernet = 1

// iaidLen is the width of an IAID on the wire (RFC 9915 section 21.4:
// "IAID: The unique identifier for this IA_NA", a 4-octet field).
const iaidLen = 4

// Bytes renders the identity for the durable record: the DUID followed
// by the IAID in network order.
//
// THE IAID GOES LAST because the DUID is variable-length and the record
// stores one opaque blob. A fixed-width tail is the only split that can
// be parsed back without a second length field, and a record that could
// not be parsed back would be a record of an identity nobody can reuse
// — which is the whole reason it is stored rather than re-derived.
func (i Identity6) Bytes() []byte {
	if i.IsZero() {
		return nil
	}
	out := make([]byte, 0, len(i.DUID)+iaidLen)
	out = append(out, i.DUID...)
	return binary.BigEndian.AppendUint32(out, i.IAID)
}

// ParseIdentity6 reads back what Bytes wrote.
func ParseIdentity6(b []byte) (Identity6, error) {
	if len(b) <= iaidLen {
		return Identity6{}, fmt.Errorf("dhcp: a stored DHCPv6 identity of %d bytes carries no DUID", len(b))
	}
	cut := len(b) - iaidLen
	return Identity6{
		DUID: append([]byte(nil), b[:cut]...),
		IAID: binary.BigEndian.Uint32(b[cut:]),
	}, nil
}

// DUIDLL is RFC 9915 section 11.4's DUID-LL over an Ethernet address:
// the four bytes 00:03:00:01 followed by the MAC.
//
// It is 1.9.0's identity unchanged (P-8.6). dhcpcd was told the same
// value as a `duid` directive, so a bridge or macvlan endpoint upgraded
// from 1.x presents the identity the server already has a binding for
// and keeps its address.
func DUIDLL(mac net.HardwareAddr) ([]byte, error) {
	duid, err := wire.DUIDLL(hwTypeEthernet, mac)
	if err != nil {
		return nil, fmt.Errorf("dhcp: DUID-LL from %v: %w", mac, err)
	}
	return duid, nil
}

// DUIDUUID is RFC 9915 section 11.5's DUID-UUID over a caller-supplied
// sixteen octets.
//
// It exists for ipvlan and for nothing else (#895, D30 Q4). An ipvlan
// slave inherits the parent's MAC by kernel design, so a MAC-derived
// DUID is IDENTICAL for every container on the network and they all
// claim one binding — the v6 form of the defect #219 names for v4.
func DUIDUUID(uuid []byte) ([]byte, error) {
	duid, err := wire.DUIDUUID(uuid)
	if err != nil {
		return nil, fmt.Errorf("dhcp: DUID-UUID: %w", err)
	}
	return duid, nil
}

// IAIDFromMAC is 1.9.0's IAID: the low four bytes of the MAC (P-8.6).
func IAIDFromMAC(mac net.HardwareAddr) (uint32, error) {
	if len(mac) < iaidLen {
		return 0, fmt.Errorf("dhcp: an IAID needs %d bytes of hardware address, got %d", iaidLen, len(mac))
	}
	return binary.BigEndian.Uint32(mac[len(mac)-iaidLen:]), nil
}

// IAIDFromBytes takes the first four bytes of a per-endpoint seed.
func IAIDFromBytes(seed []byte) (uint32, error) {
	if len(seed) < iaidLen {
		return 0, fmt.Errorf("dhcp: an IAID needs %d bytes of seed, got %d", iaidLen, len(seed))
	}
	return binary.BigEndian.Uint32(seed[:iaidLen]), nil
}
