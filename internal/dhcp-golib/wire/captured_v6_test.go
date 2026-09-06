package wire

// The captured frames every v6 test in this package is measured against, and
// where they came from.
//
// PROVENANCE. Hand-run 2026-09-05 on the session box, entirely inside
// `unshare -Urn` — a private network namespace with one veth pair, `v6srv` and
// `v6cli`, and no route to anything. No LAN traffic was involved and no LAN
// address appears here; the fixture prefix is fd00:99::/64, which is RFC 4193
// unique-local.
//
// The server:
//
//	LC_ALL=C /usr/sbin/dnsmasq --no-daemon --keep-in-foreground --log-dhcp \
//	  --log-facility=- --bind-interfaces --interface=v6srv \
//	  --except-interface=lo --port=0 --no-resolv --no-hosts \
//	  --dhcp-authoritative --enable-ra --ra-param=v6srv,60,300 \
//	  --dhcp-range=fd00:99::100,fd00:99::1ff,64,300 \
//	  --dhcp-option=option6:dns-server,[fd00:99::1] \
//	  --dhcp-option=option6:domain-search,fixture.invalid \
//	  --dhcp-leasefile=<tmp>
//
//	Dnsmasq Version 2.91  Copyright (c) 2000-2025 Simon Kelley
//	compile time options: IPv6 GNU-getopt DBus no-UBus i18n IDN2 DHCP DHCPv6
//	no-Lua TFTP conntrack ipset nftset auth DNSSEC loop-detect inotify dumpfile
//
// The wire:
//
//	tcpdump -Z root -i v6cli -s 0 -w v6cli.pcap \
//	  'icmp6 or udp port 546 or udp port 547'
//
// The client was a throwaway program that called this package's own encoders,
// sent from a plain UDP socket bound to fe80::e849:4eff:fee5:31ed%v6cli port
// 546, and printed what came back. dnsmasq answered it: its log carries
// DHCPSOLICIT, DHCPADVERTISE, DHCPREQUEST and DHCPREPLY for this exchange, and
// the handover quotes those lines verbatim. That is the round-trip proof for
// the encoder — a server nobody here wrote parsed our bytes and replied to
// them — and the bytes below are the decoder's fixture.
//
// WHAT THIS CANNOT SEE, stated rather than left to be discovered. Nothing
// re-runs the capture: it is a table of literals, so a change in dnsmasq's
// behaviour is invisible until M7c builds the netns fixture that repeats it.
// And dnsmasq set the IA Address preferred and valid lifetimes to the SAME
// value (300 each), and left Reachable Time and Retrans Timer at zero, so no
// captured frame can tell those pairs apart; the rows that separate them are
// synthetic and say so where they sit.

// The four DHCPv6 messages of one full exchange, in the order they crossed the
// link. `capSolicit` and `capRequest` are this package's own output, recorded
// off the wire rather than re-encoded, so a change to the encoder shows up
// here as a diff and not as a test that agrees with itself.
var (
	// #5  fe80::e849:4eff:fee5:31ed.546 > ff02::1:2.547
	capSolicit = mustHex("011a2b3c" +
		"0001000a00030001ea494ee531ed" +
		"0008000200000003" +
		"000c0a0b0c0d0000000000000000" +
		"0006000400170018")

	// #6  fe80::4416:80ff:fe75:e6ab.547 > fe80::e849:4eff:fee5:31ed.546
	// dnsmasq: "DHCPADVERTISE(v6srv) fd00:99::183 00:03:00:01:ea:49:4e:e5:31:ed"
	capAdvertise = mustHex("021a2b3c" +
		"0001000a00030001ea494ee531ed" +
		"0002000e00010001322ecbbfea494ee531ed" +
		"000300280a0b0c0d0000009600000103" +
		"00050018fd0000990000000000000000000001830000012c0000012c" +
		"000d0009000073756363657373" +
		"00070001ff" +
		"00180011076669787475726507696e76616c69640000" +
		"170010fd000099000000000000000000000001")

	// #7  fe80::e849:4eff:fee5:31ed.546 > ff02::1:2.547
	capRequest = mustHex("034d5e6f" +
		"0001000a00030001ea494ee531ed" +
		"0002000e00010001322ecbbfea494ee531ed" +
		"00080002000a" +
		"000300280a0b0c0d0000009600000103" +
		"00050018fd0000990000000000000000000001830000012c0000012c" +
		"0006000400170018")

	// #8  fe80::4416:80ff:fe75:e6ab.547 > fe80::e849:4eff:fee5:31ed.546
	// dnsmasq: "DHCPREPLY(v6srv) fd00:99::183 00:03:00:01:ea:49:4e:e5:31:ed"
	capReply = mustHex("074d5e6f" +
		"0001000a00030001ea494ee531ed" +
		"0002000e00010001322ecbbfea494ee531ed" +
		"000300280a0b0c0d0000009600000103" +
		"00050018fd0000990000000000000000000001830000012c0000012c" +
		"000d0009000073756363657373" +
		"00180011076669787475726507696e76616c69640000" +
		"170010fd000099000000000000000000000001")
)

// The ICMPv6 frames of the same run and of a second one that provoked a
// duplicate-address defence. Each is the ICMPv6 body exactly as it appeared
// after the 40-octet IPv6 header, with the source and destination that the
// checksum covers.
var (
	// #1  our own Router Solicitation, built by EncodeRouterSolicit and sent
	// on a raw ICMPv6 socket. The Linux kernel recomputes the checksum on such
	// a socket, and the octets on the wire are byte-for-byte the octets the
	// encoder produced — two implementations, one answer.
	capRouterSolicitSrc = "fe80::e849:4eff:fee5:31ed"
	capRouterSolicitDst = "ff02::2"
	capRouterSolicit    = mustHex("8500a8f5000000000101ea494ee531ed")

	// #4  the Router Advertisement dnsmasq sent back, unicast to the
	// solicitor. dnsmasq logged "RTR-SOLICIT(v6srv) ea:49:4e:e5:31:ed" and
	// then "RTR-ADVERT(v6srv) fd00:99::".
	capRouterAdvertSrc = "fe80::4416:80ff:fe75:e6ab"
	capRouterAdvertDst = "fe80::e849:4eff:fee5:31ed"
	capRouterAdvert    = mustHex("86005b1b40c0012c0000000000000000" +
		"030440800000012c0000012c00000000fd000099000000000000000000000000" +
		"05010000000005dc" +
		"010146168075e6ab" +
		"1f0400000000012c076669787475726507696e76616c696400000000000000" +
		"00" +
		"190300000000012cfd000099000000000000000000000001")

	// #9  the same server's periodic multicast Router Advertisement, one
	// second later. Same body, different checksum, because the destination is
	// part of the pseudo-header and nothing else changed.
	capRouterAdvertMcastSrc = "fe80::4416:80ff:fe75:e6ab"
	capRouterAdvertMcastDst = "ff02::1"
	capRouterAdvertMcast    = mustHex("8600c2b440c0012c0000000000000000" +
		"030440800000012c0000012c00000000fd000099000000000000000000000000" +
		"05010000000005dc" +
		"010146168075e6ab" +
		"1f0400000000012c076669787475726507696e76616c696400000000000000" +
		"00" +
		"190300000000012cfd000099000000000000000000000001")

	// #3  this host's own kernel answering a neighbour solicitation:
	// Solicited and Override set, Router clear.
	capNeighborAdvertSrc = "fe80::e849:4eff:fee5:31ed"
	capNeighborAdvertDst = "fe80::4416:80ff:fe75:e6ab"
	capNeighborAdvert    = mustHex("8800349460000000" +
		"fe80000000000000e8494efffee531ed" +
		"0201ea494ee531ed")

	// SECOND RUN, same namespace shape. `ip addr add fd00:99::5150/64 dev
	// v6cli` and the Linux kernel's own duplicate-address detection
	// solicitation for it, which carries RFC 7527's Nonce option (type 14) —
	// an option this package does not decode and must therefore skip.
	capKernelDADSrc = "::"
	capKernelDADDst = "ff02::1:ff00:5150"
	capKernelDAD    = mustHex("87005f5800000000" +
		"fd000099000000000000000000005150" +
		"0e0190689657464c")

	// SECOND RUN. Our own EncodeDADNeighborSolicit output for an address the
	// peer already held, sent as a complete Ethernet frame from an AF_PACKET
	// socket so that the checksum on the wire is the one ICMPv6Checksum
	// computed and no kernel touched it.
	capOurDADSrc = "::"
	capOurDADDst = "ff02::1:ff00:defe"
	capOurDAD    = mustHex("8700bf1000000000fd00009900000000000000000000defe")

	// SECOND RUN. The answer: RFC 4862 §5.4.3's defence, multicast to
	// ff02::1, Override set and Solicited clear because it answers a
	// solicitation from the unspecified address. This frame exists only
	// because the peer's stack accepted the frame above.
	capDefenceSrc = "fd00:99::defe"
	capDefenceDst = "ff02::1"
	capDefence    = mustHex("88007baf20000000" +
		"fd00009900000000000000000000defe" +
		"0201fa90458ce1a1")
)
