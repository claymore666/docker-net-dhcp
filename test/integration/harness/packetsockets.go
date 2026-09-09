// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag, for the reason v6addrflags.go gives:
// this is a pure function over bytes, so it is driven in the fast lane
// against VERBATIM text read from the kernel the suite actually runs
// containers on.

package harness

import (
	"fmt"
	"strconv"
	"strings"
)

// PacketSocket is one row of /proc/net/packet: one AF_PACKET socket
// open in the network namespace the file was read from.
//
// # WHY THIS FILE IS THE EVIDENCE
//
// Until 2.0 "one DHCP client on the interface" was a question about
// PROCESSES: dhcpcd was a child, and a stray one was visible in the
// process table from anywhere on the host. Since 2.0 the client is a
// goroutine inside the plugin, and a displaced one that failed to stop
// leaves no process and no file behind. What it cannot hide is its
// SOCKET: the client speaks DHCP over AF_PACKET bound to the container
// link, and the kernel lists every such socket in the container's own
// network namespace.
//
// That makes this the outside evidence for a claim that has none
// otherwise. displaced_stops is the plugin's own opinion that it asked
// a client to stop. The DHCP server's log cannot settle it either: a
// client that has stopped sends nothing, and so does a client that is
// simply between renewals, so the two readings are identical for as
// long as the test is willing to wait. The socket table is not a
// question about traffic — it is the kernel naming the sockets that
// exist at the moment it is read.
type PacketSocket struct {
	// Proto is the protocol the socket is bound to, in host byte order
	// as the kernel prints it: 0x0800 ETH_P_IP (the DHCPv4 client),
	// 0x0806 ETH_P_ARP (RFC 5227 conflict detection), 0x86dd
	// ETH_P_IPV6 (the DHCPv6 client), 0x0003 ETH_P_ALL.
	Proto uint16
	// IfIndex is the interface the socket is bound to, or 0 for a
	// socket bound to every interface.
	IfIndex int
	// Inode is the socket's inode number as printed, kept as text
	// because it is an identity and never arithmetic. It is what makes
	// two rows for the same protocol and interface distinguishable in
	// a failure message.
	Inode string
}

// packetProtoColumn, packetIfaceColumn and packetInodeColumn are the
// columns of /proc/net/packet, which has carried the same nine since
// Linux 2.2 (net/packet/af_packet.c, packet_seq_show):
//
//	sk       RefCnt Type Proto  Iface R Rmem   User   Inode
//	000000006eb2ee44 3      3     0800  4     1 0      0        2103400
//
// Read by INDEX and not by header name on purpose: the header is
// whitespace-aligned rather than tab-separated, so a name-keyed reader
// would have to re-derive the columns from the alignment of the first
// line, and that alignment is what changes between kernels. The count
// is asserted per row instead, so a kernel that adds a column fails
// loudly here rather than reporting a plausible wrong number.
const (
	packetProtoColumn = 3
	packetIfaceColumn = 4
	packetInodeColumn = 8
	packetColumns     = 9
)

// PacketSocketsFromProc parses the contents of /proc/net/packet.
//
// It refuses rather than guesses: a row with the wrong number of
// columns, an unparseable protocol or an unparseable interface index is
// an error, because the caller's next step is to count what it finds
// and a silently dropped row reads exactly like a socket that is not
// there — which is the answer this file exists to disprove.
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

// EthPIP is ETH_P_IP, the protocol the DHCPv4 client's socket is bound
// to. Named rather than written as 0x0800 at the call site so the
// number appears once.
const EthPIP = 0x0800

// PacketSocketsOn returns the sockets bound to proto on ifIndex.
//
// A socket bound to interface 0 is bound to EVERY interface and is
// counted for any ifIndex: the kernel will deliver this link's frames
// to it, so for the question "how many clients could answer on this
// link" it is one of them.
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

// DescribePacketSockets renders rows for a failure message: the inodes
// are what distinguishes two clients from one read twice.
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
