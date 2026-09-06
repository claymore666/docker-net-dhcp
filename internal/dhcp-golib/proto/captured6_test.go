package proto

import (
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// The captured DHCPv6 frames this package's golden path is driven with.
//
// PROVENANCE: they are the same octets as wire/captured_v6_test.go's, and that
// file carries the full provenance block — the dnsmasq command line, the
// version banner, the tcpdump command, and the namespace the capture ran in.
// They are COPIED rather than shared because both files are _test.go in
// different packages and Go has no way to share a fixture between two test
// binaries without making it part of the library's exported surface. The
// duplication is bounded (four literals) and it is the direction that keeps
// the library free of test data; the handover records it.
//
// WHY THEY ARE HERE AT ALL: design §A.4's Trap 2 in its pure-machine form. A
// test that drove Machine6 with messages this library's own encoder built
// would be ring 1 agreeing with ring 0 about a shape neither had ever put on a
// wire. These bytes came off a link, from a server nobody here wrote, and the
// values asserted against them are the ones dnsmasq printed in its own log.
var (
	// #6  fe80::4416:80ff:fe75:e6ab.547 > fe80::e849:4eff:fee5:31ed.546
	// dnsmasq: "DHCPADVERTISE(v6srv) fd00:99::183 00:03:00:01:ea:49:4e:e5:31:ed"
	capAdvertise6 = mustHexBytes("021a2b3c" +
		"0001000a00030001ea494ee531ed" +
		"0002000e00010001322ecbbfea494ee531ed" +
		"000300280a0b0c0d0000009600000103" +
		"00050018fd0000990000000000000000000001830000012c0000012c" +
		"000d0009000073756363657373" +
		"00070001ff" +
		"00180011076669787475726507696e76616c69640000" +
		"170010fd000099000000000000000000000001")

	// #8  fe80::4416:80ff:fe75:e6ab.547 > fe80::e849:4eff:fee5:31ed.546
	// dnsmasq: "DHCPREPLY(v6srv) fd00:99::183 00:03:00:01:ea:49:4e:e5:31:ed"
	capReply6 = mustHexBytes("074d5e6f" +
		"0001000a00030001ea494ee531ed" +
		"0002000e00010001322ecbbfea494ee531ed" +
		"000300280a0b0c0d0000009600000103" +
		"00050018fd0000990000000000000000000001830000012c0000012c" +
		"000d0009000073756363657373" +
		"00180011076669787475726507696e76616c69640000" +
		"170010fd000099000000000000000000000001")

	// #5  the Solicit the capture's client sent, kept as the SHAPE the
	// machine's own Solicit is compared against. It is not this machine's
	// output: the client that produced it predates §21.24's ORO requirement,
	// which is the difference TestTheGoldenPathIsDrivenByDnsmasqsOwnBytes
	// names rather than papers over.
	capSolicit6 = mustHexBytes("011a2b3c" +
		"0001000a00030001ea494ee531ed" +
		"0008000200000003" +
		"000c0a0b0c0d0000000000000000" +
		"0006000400170018")
)

// The values dnsmasq itself printed or was configured with, as constants, so
// an assertion below reads against the SERVER's account and not against
// anything this package derived from the bytes it is checking.
const (
	// From the log line quoted above: "DHCPREPLY(v6srv) fd00:99::183".
	dnsmasqLeasedAddr = "fd00:99::183"
	// From the server's own command line: --dhcp-range=fd00:99::100,
	// fd00:99::1ff,64,300 — the trailing 300 is the lease time in seconds,
	// which dnsmasq puts in both IA Address lifetimes.
	dnsmasqLifetimeSeconds = 300
	// From --dhcp-option=option6:dns-server,[fd00:99::1] and
	// --dhcp-option=option6:domain-search,fixture.invalid.
	dnsmasqDNS    = "fd00:99::1"
	dnsmasqSearch = "fixture.invalid"
)

// receivedCaptured turns captured octets into the EvReceived they record,
// through the same decoder ring 3 would use.
func receivedCaptured(t *testing.T, raw []byte) Event {
	t.Helper()
	msg, err := wire.DecodeV6(raw)
	if err != nil {
		t.Fatalf("DecodeV6: %v", err)
	}
	return ReceivedV6(msg, raw)
}
