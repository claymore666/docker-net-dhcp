// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/claymore666/dhcp-golib/wire"
)

// RFC 9915 section 11 says a DUID "SHOULD NOT change over time if at all possible" and the library refuses an empty
// one, so the chassis mints it once and the v6 record carries it across restarts through Bytes and ParseIdentity6
// (#911).

// Identity6 is one endpoint's DHCPv6 identity: the RFC 9915 section 21.2 DUID and the section 21.4 IAID.
type Identity6 struct {
	// DUID is the whole option 21.2 payload, type code included.
	DUID []byte
	// IAID is the identity association identifier; zero is legal, and no DUID means the empty Identity6.
	IAID uint32
}

// IsZero reports an identity that was never minted.
func (i Identity6) IsZero() bool { return len(i.DUID) == 0 }

// hwTypeEthernet is IANA hardware type 1, which RFC 9915 section 11.4's DUID-LL carries for Ethernet.
const hwTypeEthernet = 1

// iaidLen is the IAID's width on the wire (RFC 9915 section 21.4).
const iaidLen = 4

// The IAID goes last: the DUID is variable-length, and a fixed-width tail splits back without a length field (#911).

// Bytes renders the identity for the durable record: the DUID, then the IAID in network order.
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

// It is 1.9.0's identity unchanged, the value dhcpcd got as `duid`, so an endpoint upgraded from 1.x keeps its binding
// (#911).

// DUIDLL is RFC 9915 section 11.4's DUID-LL over an Ethernet address: 00:03:00:01 followed by the MAC.
func DUIDLL(mac net.HardwareAddr) ([]byte, error) {
	duid, err := wire.DUIDLL(hwTypeEthernet, mac)
	if err != nil {
		return nil, fmt.Errorf("dhcp: DUID-LL from %v: %w", mac, err)
	}
	return duid, nil
}

// Only for ipvlan: a slave inherits the parent's MAC, so a MAC-derived DUID would be the same for every container
// (#895, #219).

// DUIDUUID is RFC 9915 section 11.5's DUID-UUID over a caller-supplied sixteen octets.
func DUIDUUID(uuid []byte) ([]byte, error) {
	duid, err := wire.DUIDUUID(uuid)
	if err != nil {
		return nil, fmt.Errorf("dhcp: DUID-UUID: %w", err)
	}
	return duid, nil
}

// IAIDFromMAC is 1.9.0's IAID: the low four bytes of the MAC (#911).
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
