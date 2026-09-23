// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: a pure parser driven against verbatim kernel text in the unit job.

package harness

import (
	"fmt"
	"strconv"
	"strings"
)

// PacketSocket is one AF_PACKET socket row of /proc/net/packet in the reader's network namespace. Since 2.0 the DHCP
// client is a goroutine, so a displaced client that did not stop leaves only its socket as outside evidence (#682).
type PacketSocket struct {
	// Proto is the bound protocol in host order: 0x0800 ETH_P_IP, 0x0806 ETH_P_ARP, 0x86dd ETH_P_IPV6, 0x0003 ETH_P_ALL.
	Proto uint16
	// IfIndex is the bound interface, 0 for every interface.
	IfIndex int
	// Inode is the socket's inode as printed, an identity for failure messages.
	Inode string
}

// /proc/net/packet has had the same nine columns since Linux 2.2 (net/packet/af_packet.c, packet_seq_show); the header
// is space-aligned, so columns are read by index and the count is checked per row (#682).
const (
	packetProtoColumn = 3
	packetIfaceColumn = 4
	packetInodeColumn = 8
	packetColumns     = 9
)

// PacketSocketsFromProc parses /proc/net/packet, refusing a row it cannot read rather than dropping it (#682).
func PacketSocketsFromProc(text string) ([]PacketSocket, error) {
	var out []PacketSocket
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "sk" {
			continue
		}
		if len(f) != packetColumns {
			return nil, fmt.Errorf("/proc/net/packet line %d has %d columns, want %d: %q",
				i+1, len(f), packetColumns, line)
		}
		proto, err := strconv.ParseUint(f[packetProtoColumn], 16, 16)
		if err != nil {
			return nil, fmt.Errorf("/proc/net/packet line %d: protocol %q is not hexadecimal: %w",
				i+1, f[packetProtoColumn], err)
		}
		iface, err := strconv.Atoi(f[packetIfaceColumn])
		if err != nil {
			return nil, fmt.Errorf("/proc/net/packet line %d: interface index %q is not a number: %w",
				i+1, f[packetIfaceColumn], err)
		}
		out = append(out, PacketSocket{
			Proto:   uint16(proto),
			IfIndex: iface,
			Inode:   f[packetInodeColumn],
		})
	}
	return out, nil
}

// EthPIP is ETH_P_IP, the DHCPv4 client's protocol.
const EthPIP = 0x0800

// PacketSocketsOn returns the sockets bound to proto on ifIndex; a socket bound to interface 0 receives on every link and counts.
func PacketSocketsOn(rows []PacketSocket, proto uint16, ifIndex int) []PacketSocket {
	var out []PacketSocket
	for _, r := range rows {
		if r.Proto != proto {
			continue
		}
		if r.IfIndex != ifIndex && r.IfIndex != 0 {
			continue
		}
		out = append(out, r)
	}
	return out
}

// DescribePacketSockets renders rows with their inodes for a failure message.
func DescribePacketSockets(rows []PacketSocket) string {
	if len(rows) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("{proto=%04x iface=%d inode=%s}", r.Proto, r.IfIndex, r.Inode))
	}
	return strings.Join(parts, " ")
}
