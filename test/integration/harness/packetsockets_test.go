// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import "testing"

// Verbatim /proc/net/packet output from Linux 6.12; the multi-socket samples are AF_PACKET sockets opened under
// `unshare -rn`, SOCK_DGRAM and bound to one interface, the shape the plugin's clients produce (#682).
const (
	// One ARP socket, on the session box's own root namespace.
	procOneSocket = `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
000000002bee1efa 3      2    0806   2     1 0      0      3927
`
	// ETH_P_IP for DHCPv4, ETH_P_ARP for RFC 5227, ETH_P_IPV6 for DHCPv6: one dual-stack endpoint's sockets.
	procThreeProtocols = `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
00000000c4f0055c 2      2    0800   1     0 0      0      1739463
00000000faf658eb 2      2    0806   1     0 0      0      1739464
00000000deeaa38c 2      2    86dd   1     0 0      0      1739465
`
	// Two DHCPv4 clients on one interface: a displaced client that did not stop (#682).
	procTwoDHCPv4Clients = `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
000000006e12b26d 2      2    0800   1     0 0      0      1761563
00000000fe4d2285 2      2    0800   1     0 0      0      1761564
`
	// A header and no rows: an empty namespace.
	procNoSockets = `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
`
)

func TestPacketSocketsFromProc_ReadsTheColumns(t *testing.T) {
	rows, err := PacketSocketsFromProc(procOneSocket)
	if err != nil {
		t.Fatalf("PacketSocketsFromProc: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d row(s), want 1: %s", len(rows), DescribePacketSockets(rows))
	}
	want := PacketSocket{Proto: 0x0806, IfIndex: 2, Inode: "3927"}
	if rows[0] != want {
		t.Errorf("got %+v, want %+v", rows[0], want)
	}
}

func TestPacketSocketsFromProc_HeaderOnlyIsNoSockets(t *testing.T) {
	rows, err := PacketSocketsFromProc(procNoSockets)
	if err != nil {
		t.Fatalf("PacketSocketsFromProc: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a file with only a header parsed as %d socket(s): %s",
			len(rows), DescribePacketSockets(rows))
	}
}

func TestPacketSocketsFromProc_ProtocolIsHexadecimal(t *testing.T) {
	rows, err := PacketSocketsFromProc(procThreeProtocols)
	if err != nil {
		t.Fatalf("PacketSocketsFromProc: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d row(s), want 3: %s", len(rows), DescribePacketSockets(rows))
	}
	for i, want := range []uint16{0x0800, 0x0806, 0x86dd} {
		if rows[i].Proto != want {
			t.Errorf("row %d: proto=%#04x, want %#04x", i, rows[i].Proto, want)
		}
	}
	if got := PacketSocketsOn(rows, EthPIP, 1); len(got) != 1 {
		t.Errorf("one endpoint's three sockets counted as %d DHCPv4 client(s), want 1: %s",
			len(got), DescribePacketSockets(got))
	}
}

func TestPacketSocketsOn_TwoClientsOnOneInterfaceAreTwo(t *testing.T) {
	rows, err := PacketSocketsFromProc(procTwoDHCPv4Clients)
	if err != nil {
		t.Fatalf("PacketSocketsFromProc: %v", err)
	}
	got := PacketSocketsOn(rows, EthPIP, 1)
	if len(got) != 2 {
		t.Fatalf("two DHCPv4 sockets on interface 1 counted as %d: %s",
			len(got), DescribePacketSockets(got))
	}
	if got[0].Inode == got[1].Inode {
		t.Error("the two rows carry the same inode, so a failure message could not tell " +
			"two clients from one socket listed twice")
	}
}

func TestPacketSocketsOn_AnotherInterfaceIsNotThisOne(t *testing.T) {
	rows, err := PacketSocketsFromProc(procTwoDHCPv4Clients)
	if err != nil {
		t.Fatalf("PacketSocketsFromProc: %v", err)
	}
	if got := PacketSocketsOn(rows, EthPIP, 7); len(got) != 0 {
		t.Errorf("sockets bound to interface 1 counted as %d on interface 7: %s",
			len(got), DescribePacketSockets(got))
	}
	if got := PacketSocketsOn(rows, 0x0806, 1); len(got) != 0 {
		t.Errorf("ETH_P_IP sockets counted as %d ETH_P_ARP socket(s): %s",
			len(got), DescribePacketSockets(got))
	}
}

// A socket bound to interface 0 receives on every interface, so it counts for the link under test.
func TestPacketSocketsOn_InterfaceZeroIsEveryInterface(t *testing.T) {
	const anyIface = `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
000000006e12b26d 2      2    0800   0     0 0      0      1761563
`
	rows, err := PacketSocketsFromProc(anyIface)
	if err != nil {
		t.Fatalf("PacketSocketsFromProc: %v", err)
	}
	if got := PacketSocketsOn(rows, EthPIP, 4); len(got) != 1 {
		t.Errorf("a socket bound to every interface counted as %d on interface 4: %s",
			len(got), DescribePacketSockets(got))
	}
}

func TestPacketSocketsFromProc_RefusesRatherThanGuesses(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"a row with a column too few", `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
000000002bee1efa 3      2    0806   2     1 0      0
`},
		{"a row with a column too many", `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
000000002bee1efa 3      2    0806   2     1 0      0      3927 extra
`},
		{"a protocol that is not hexadecimal", `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
000000002bee1efa 3      2    zzzz   2     1 0      0      3927
`},
		{"an interface index that is not a number", `sk               RefCnt Type Proto  Iface R Rmem   User   Inode
000000002bee1efa 3      2    0806   eth0  1 0      0      3927
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, err := PacketSocketsFromProc(c.text)
			if err == nil {
				t.Fatalf("parsed without error as %s; a row this parser cannot read must not "+
					"become a socket that is not there", DescribePacketSockets(rows))
			}
		})
	}
}
